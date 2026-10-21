package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/isaiah/ghost-radigo/internal/cache"
	"github.com/isaiah/ghost-radigo/internal/logger"
	"github.com/isaiah/ghost-radigo/internal/scanner"
)

const (
	// Maximum time to wait for a single ffmpeg invocation.
	// 45 s is enough for well-behaved archive sources; anything slower is
	// likely a large-file range-seek that will never complete usefully.
	processorTimeout = 45 * time.Second

	// Output encoding parameters — must match across all workers so the
	// stream stays consistent for the mixer.
	sampleRate = "44100"
	channels   = "2"
	bitrate    = "128k"
	format     = "mp3"

	// reversalProb is the probability that a clip is time-reversed before
	// playback — an EVP technique that creates disorienting backwards-voice
	// fragments while preserving the phoneme bed.
	reversalProb = 0.15
)

// ProcessedClip holds the encoded audio bytes for a single clip.
type ProcessedClip struct {
	Data       []byte
	Duration   float64 // seconds (may be 0 for silence if generation failed)
	Silence    bool
	Identifier string // archive item ID; empty for noise gaps
	Title      string
	Year       int
}

// Worker pulls ClipJobs from in, runs ffmpeg, and pushes ProcessedClips to out.
type Worker struct {
	id    int
	in    <-chan scanner.ClipJob
	out   chan<- ProcessedClip
	rng   *rand.Rand
	cache *cache.Cache // nil disables caching
}

// NewWorker creates a processor worker.
func NewWorker(id int, in <-chan scanner.ClipJob, out chan<- ProcessedClip, c *cache.Cache) *Worker {
	return &Worker{
		id:    id,
		in:    in,
		out:   out,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*1000)),
		cache: c,
	}
}

// Run starts the processing loop. It blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case job, ok := <-w.in:
			if !ok {
				return nil
			}
			clip, err := w.process(ctx, job)
			if err != nil {
				if errors.Is(err, errVADReject) {
					logger.Debug("vad skip  %s  offset=%.1fs", job.Item.Identifier, job.Offset)
					continue
				}
				logger.Error("processor[%d]: %s offset=%.1fs: %v",
					w.id, job.Item.Identifier, job.Offset, err)
				continue
			}
			select {
			case w.out <- clip:
			case <-ctx.Done():
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (w *Worker) process(ctx context.Context, job scanner.ClipJob) (ProcessedClip, error) {
	if job.Silence {
		logger.Info("clip  noise  dur=%.1fs", job.Duration)
		return generateNoise(ctx, job.Duration)
	}
	return w.extractClip(ctx, job)
}

// extractClip runs ffmpeg to pull a segment from a remote URL and encode it
// to MP3 with fades, loudness normalisation, and a light bandpass filter.
//
// Before encoding, a VAD probe checks for voice-band energy. Clips that score
// below vadThreshold are rejected with errVADReject. On probe failure the clip
// is accepted anyway — we prefer false positives over dropping valid content.
//
// A random 15% of clips are time-reversed (areverse) for an EVP-style effect.
func (w *Worker) extractClip(ctx context.Context, job scanner.ClipJob) (ProcessedClip, error) {
	// Cache hit — skip VAD probe and ffmpeg encode entirely.
	if data, ok := w.cache.Get(job.Item.Identifier, job.Offset, job.Duration); ok {
		logger.Debug("cache hit  %s  offset=%.1fs", job.Item.Identifier, job.Offset)
		return ProcessedClip{
			Data:       data,
			Duration:   job.Duration,
			Identifier: job.Item.Identifier,
			Title:      job.Item.Title,
			Year:       job.Item.Year,
		}, nil
	}

	// VAD probe — analysis-only pass, no encoding.
	score, err := vadScore(ctx, job.Item.FileURL, job.Offset, job.Duration)
	if err != nil {
		// Probe failed (network hiccup, unsupported format, etc.) — accept.
		logger.Warn("vad probe %s: %v — accepting", job.Item.Identifier, err)
	} else if score < vadThreshold {
		return ProcessedClip{}, errVADReject
	}

	reversed := w.rng.Float64() < reversalProb
	logger.Info("clip  %s  year=%d  offset=%.1fs  dur=%.1fs  vad=%.2f  rev=%v  title=%q",
		job.Item.Identifier, job.Item.Year, job.Offset, job.Duration, score, reversed, job.Item.Title)

	fadeIn := 0.3 + w.rng.Float64()*0.5  // 0.3–0.8 s
	fadeOut := 0.3 + w.rng.Float64()*0.5 // 0.3–0.8 s
	if fadeIn+fadeOut >= job.Duration {
		fadeIn = job.Duration * 0.2
		fadeOut = job.Duration * 0.2
	}
	fadeOutStart := job.Duration - fadeOut

	// areverse (when used) comes first so fades are applied to the
	// already-reversed audio, giving natural in/out on the backwards clip.
	var afFilter string
	if reversed {
		afFilter = fmt.Sprintf(
			"areverse,afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f,loudnorm=I=-16:LRA=11:TP=-1.5,highpass=f=80,lowpass=f=8000",
			fadeIn, fadeOutStart, fadeOut,
		)
	} else {
		afFilter = fmt.Sprintf(
			"afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f,loudnorm=I=-16:LRA=11:TP=-1.5,highpass=f=80,lowpass=f=8000",
			fadeIn, fadeOutStart, fadeOut,
		)
	}

	args := []string{
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.3f", job.Offset), // fast seek before -i
		"-i", job.Item.FileURL,
		"-t", fmt.Sprintf("%.3f", job.Duration),
		"-vn", // discard video
		"-af", afFilter,
		"-ar", sampleRate,
		"-ac", channels,
		"-b:a", bitrate,
		"-f", format,
		"pipe:1",
	}

	clipCtx, cancel := context.WithTimeout(ctx, processorTimeout)
	defer cancel()

	cmd := exec.CommandContext(clipCtx, "ffmpeg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return ProcessedClip{}, fmt.Errorf("ffmpeg: %s", msg)
	}
	if stdout.Len() == 0 {
		return ProcessedClip{}, fmt.Errorf("ffmpeg produced no output")
	}

	data := stdout.Bytes()
	if err := w.cache.Put(job.Item.Identifier, job.Offset, job.Duration, data); err != nil {
		logger.Warn("cache put %s: %v", job.Item.Identifier, err)
	}
	return ProcessedClip{
		Data:       data,
		Duration:   job.Duration,
		Identifier: job.Item.Identifier,
		Title:      job.Item.Title,
		Year:       job.Item.Year,
	}, nil
}

// generateNoise uses ffmpeg's lavfi anoisesrc to produce a quiet pink-noise
// segment — more atmospheric than dead silence between archive clips.
func generateNoise(ctx context.Context, dur float64) (ProcessedClip, error) {
	fade := 0.4
	if dur < 1.2 {
		fade = dur * 0.2
	}
	fadeOutStart := dur - fade

	afFilter := fmt.Sprintf(
		"afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f,lowpass=f=3500",
		fade, fadeOutStart, fade,
	)

	args := []string{
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", fmt.Sprintf("anoisesrc=r=%s:color=pink:amplitude=0.018", sampleRate),
		"-t", fmt.Sprintf("%.3f", dur),
		"-ac", channels,
		"-af", afFilter,
		"-b:a", bitrate,
		"-f", format,
		"pipe:1",
	}

	noiseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(noiseCtx, "ffmpeg", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return ProcessedClip{}, fmt.Errorf("noise: %w", err)
	}

	return ProcessedClip{
		Data:     stdout.Bytes(),
		Duration: dur,
		Silence:  true,
	}, nil
}

// Pool manages a fixed number of parallel processor workers.
type Pool struct {
	workers []*Worker
}

// NewPool creates n workers sharing the same input, output, and cache.
func NewPool(n int, in <-chan scanner.ClipJob, out chan<- ProcessedClip, c *cache.Cache) *Pool {
	workers := make([]*Worker, n)
	for i := range workers {
		workers[i] = NewWorker(i, in, out, c)
	}
	return &Pool{workers: workers}
}

// Run starts all workers concurrently. It blocks until ctx is cancelled and
// all workers have exited.
func (p *Pool) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, w := range p.workers {
		wg.Add(1)
		w := w
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil {
				logger.Error("processor worker %d: %v", w.id, err)
			}
		}()
	}
	wg.Wait()
	return nil
}
