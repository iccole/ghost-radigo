package processor

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/isaiah/ghost-radigo/internal/logger"
	"github.com/isaiah/ghost-radigo/internal/scanner"
)

const (
	// Maximum time to wait for a single ffmpeg invocation.
	// Remote sources can be slow; give ample headroom.
	processorTimeout = 2 * time.Minute

	// Output encoding parameters — must match across all workers so the
	// stream stays consistent for the mixer.
	sampleRate = "44100"
	channels   = "2"
	bitrate    = "128k"
	format     = "mp3"
)

// ProcessedClip holds the encoded audio bytes for a single clip.
type ProcessedClip struct {
	Data     []byte
	Duration float64 // seconds (may be 0 for silence if generation failed)
	Silence  bool
}

// Worker pulls ClipJobs from in, runs ffmpeg, and pushes ProcessedClips to out.
type Worker struct {
	id  int
	in  <-chan scanner.ClipJob
	out chan<- ProcessedClip
	rng *rand.Rand
}

// NewWorker creates a processor worker.
func NewWorker(id int, in <-chan scanner.ClipJob, out chan<- ProcessedClip) *Worker {
	return &Worker{
		id:  id,
		in:  in,
		out: out,
		rng: rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*1000)),
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
			if job.Silence {
				logger.Info("clip  noise  dur=%.1fs", job.Duration)
			} else {
				logger.Info("clip  %s  year=%d  offset=%.1fs  dur=%.1fs  title=%q  url=%s",
					job.Item.Identifier, job.Item.Year, job.Offset, job.Duration, job.Item.Title, job.Item.FileURL)
			}
			clip, err := w.process(ctx, job)
			if err != nil {
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
		return generateNoise(ctx, job.Duration)
	}
	return w.extractClip(ctx, job)
}

// extractClip runs ffmpeg to pull a segment from a remote URL and encode it
// to MP3 with fades, loudness normalisation, and a light bandpass filter.
func (w *Worker) extractClip(ctx context.Context, job scanner.ClipJob) (ProcessedClip, error) {
	fadeIn := 0.3 + w.rng.Float64()*0.5  // 0.3–0.8 s
	fadeOut := 0.3 + w.rng.Float64()*0.5 // 0.3–0.8 s
	// Ensure fades fit inside the clip.
	if fadeIn+fadeOut >= job.Duration {
		fadeIn = job.Duration * 0.2
		fadeOut = job.Duration * 0.2
	}
	fadeOutStart := job.Duration - fadeOut

	// Build the audio filter chain:
	//   afade in/out       — soft clip boundaries
	//   loudnorm           — EBU R128 single-pass normalisation
	//   highpass/lowpass   — gentle bandpass for that archival warmth
	afFilter := fmt.Sprintf(
		"afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f,loudnorm=I=-16:LRA=11:TP=-1.5,highpass=f=80,lowpass=f=8000",
		fadeIn, fadeOutStart, fadeOut,
	)

	args := []string{
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.3f", job.Offset), // fast seek before -i
		"-i", job.Item.FileURL,
		"-t", fmt.Sprintf("%.3f", job.Duration),
		"-vn",          // discard video
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

	return ProcessedClip{
		Data:     stdout.Bytes(),
		Duration: job.Duration,
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

// NewPool creates n workers sharing the same input and output channels.
func NewPool(n int, in <-chan scanner.ClipJob, out chan<- ProcessedClip) *Pool {
	workers := make([]*Worker, n)
	for i := range workers {
		workers[i] = NewWorker(i, in, out)
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
