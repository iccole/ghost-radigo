package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/isaiah/ghost-radigo/internal/cache"
	"github.com/isaiah/ghost-radigo/internal/grammar"
	"github.com/isaiah/ghost-radigo/internal/logger"
	"github.com/isaiah/ghost-radigo/internal/scanner"
	"github.com/isaiah/ghost-radigo/internal/transcribe"
)

const (
	// Maximum time to wait for a window download from the archive.
	// 45 s is enough for well-behaved archive sources; anything slower is
	// likely a large-file range-seek that will never complete usefully.
	processorTimeout = 45 * time.Second

	// Piece encodes read a local file, so they should be near-instant.
	pieceTimeout = 15 * time.Second

	// Output encoding parameters — must match across all workers so session
	// composites (concatenated MP3 bytes) stay consistent.
	sampleRate = "44100"
	channels   = "2"
	bitrate    = "128k"
	format     = "mp3"

	// windowBitrate is higher than the output bitrate because pieces are
	// re-encoded from the window file.
	windowBitrate = "192k"

	// minWindowBytes is about one second of audio at windowBitrate. Smaller
	// window files are headers with little or no audio (a seek past EOF).
	minWindowBytes = 24 * 1024

	// maxPiecesPerWindow caps how many pieces one window contributes. The
	// session needs a varied pool to find grammatical joins, and it
	// interleaves recordings itself, so this can exceed a composite's share.
	maxPiecesPerWindow = 4

	// Anti-click fades on each piece (seconds).
	pieceFadeIn  = 0.015
	pieceFadeOut = 0.030

	// reversalProb is the probability that a piece is time-reversed before
	// playback — an EVP technique that creates disorienting backwards-voice
	// fragments while preserving the phoneme bed. Kept rare: backwards audio
	// breaks the sentence-like phrasing the session builds.
	reversalProb = 0.05
)

// ProcessedClip holds the encoded audio bytes for a single piece (or noise gap).
type ProcessedClip struct {
	Data       []byte
	Duration   float64 // seconds (may be 0 for silence if generation failed)
	Silence    bool
	Role       Role   // position in its source phrase; meaningless for silence
	Text       string // transcript; empty when untranscribed
	First      grammar.Tag
	Last       grammar.Tag // parts of speech of the first and last words
	Identifier string      // archive item ID; empty for noise gaps
	Title      string
	Year       int
}

// Worker pulls ClipJobs from in, runs ffmpeg, and pushes ProcessedClips to out.
// A single speech window can yield several ProcessedClips.
type Worker struct {
	id          int
	in          <-chan scanner.ClipJob
	out         chan<- ProcessedClip
	rng         *rand.Rand
	cache       *cache.Cache            // nil disables caching
	transcriber *transcribe.Transcriber // nil disables transcription
}

// NewWorker creates a processor worker.
func NewWorker(id int, in <-chan scanner.ClipJob, out chan<- ProcessedClip, c *cache.Cache, t *transcribe.Transcriber) *Worker {
	return &Worker{
		id:          id,
		in:          in,
		out:         out,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*1000)),
		cache:       c,
		transcriber: t,
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
			clips, err := w.process(ctx, job)
			if err != nil {
				if errors.Is(err, errNotSpeech) {
					logger.Debug("skip  %s  offset=%.1fs: %v", job.Item.Identifier, job.Offset, err)
					continue
				}
				logger.Error("processor[%d]: %s offset=%.1fs: %v",
					w.id, job.Item.Identifier, job.Offset, err)
				continue
			}
			for _, clip := range clips {
				select {
				case w.out <- clip:
				case <-ctx.Done():
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (w *Worker) process(ctx context.Context, job scanner.ClipJob) ([]ProcessedClip, error) {
	if job.Silence {
		logger.Info("clip  noise  dur=%.1fs", job.Duration)
		clip, err := generateNoise(ctx, job.Duration)
		if err != nil {
			return nil, err
		}
		return []ProcessedClip{clip}, nil
	}
	return w.extractPieces(ctx, job)
}

// extractPieces pulls a window of audio, checks that it sounds like speech,
// splits it at the pauses between words, and encodes up to
// maxPiecesPerWindow of those word/phrase-sized pieces.
//
// The window is fetched once (or read from the cache) into a local temp file,
// so the analysis pass and every piece encode avoid further network seeks.
func (w *Worker) extractPieces(ctx context.Context, job scanner.ClipJob) ([]ProcessedClip, error) {
	path, err := w.loadWindow(ctx, job)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)

	env, err := analyzeWindow(ctx, path)
	if err != nil {
		return nil, err
	}
	cv, voiced := speechScore(env)
	if !voiced || cv < minCentroidCV {
		return nil, fmt.Errorf("%w (cv=%.2f voiced=%v)", errNotSpeech, cv, voiced)
	}
	pieces := findPieces(env.levels)
	if len(pieces) == 0 {
		return nil, fmt.Errorf("%w (cv=%.2f, no pauses)", errNotSpeech, cv)
	}

	// Transcription is best-effort: without words, pieces still carry their
	// pause-based roles and the session falls back to those.
	words, err := w.transcribeWindow(ctx, path)
	if err != nil {
		logger.Warn("transcribe %s: %v", job.Item.Identifier, err)
	}
	annotate(pieces, words)

	found := len(pieces)
	w.rng.Shuffle(len(pieces), func(i, j int) { pieces[i], pieces[j] = pieces[j], pieces[i] })
	preferTranscribed(pieces)
	if len(pieces) > maxPiecesPerWindow {
		pieces = pieces[:maxPiecesPerWindow]
	}
	desc := make([]string, len(pieces))
	for i, p := range pieces {
		desc[i] = p.role.String()
		if p.text != "" {
			desc[i] += fmt.Sprintf(" %q", p.text)
		}
	}
	logger.Info("window  %s  year=%d  offset=%.0fs  cv=%.2f  pieces=%d/%d [%s]  title=%q",
		job.Item.Identifier, job.Item.Year, job.Offset, cv, len(pieces), found,
		strings.Join(desc, ", "), job.Item.Title)

	clips := make([]ProcessedClip, 0, len(pieces))
	for _, p := range pieces {
		reversed := w.rng.Float64() < reversalProb
		role := p.role
		if reversed {
			// Backwards audio has no readable words or grammar.
			role = role.Reversed()
			p.text, p.first, p.last = "", 0, 0
		}
		data, err := encodePiece(ctx, path, p, reversed)
		if err != nil {
			logger.Warn("piece %s @%.2fs: %v", job.Item.Identifier, job.Offset+p.start, err)
			continue
		}
		logger.Debug("piece  %s  at=%.2fs  dur=%.2fs  role=%v  rev=%v",
			job.Item.Identifier, job.Offset+p.start, p.dur(), role, reversed)
		clips = append(clips, ProcessedClip{
			Data:       data,
			Duration:   p.dur(),
			Role:       role,
			Text:       p.text,
			First:      p.first,
			Last:       p.last,
			Identifier: job.Item.Identifier,
			Title:      job.Item.Title,
			Year:       job.Item.Year,
		})
	}
	return clips, nil
}

// loadWindow writes the job's window of audio to a local temp file and
// returns its path; the caller removes it. The window is loudness-normalised
// and band-limited as a whole (loudnorm needs seconds of context to behave),
// and the encoded window is what the cache stores.
func (w *Worker) loadWindow(ctx context.Context, job scanner.ClipJob) (string, error) {
	tmp, err := os.CreateTemp("", "ghost-window-*.mp3")
	if err != nil {
		return "", fmt.Errorf("window temp file: %w", err)
	}
	path := tmp.Name()
	tmp.Close()

	if data, ok := w.cache.Get(job.Item.Identifier, job.Offset, job.Duration); ok {
		logger.Debug("cache hit  %s  offset=%.1fs", job.Item.Identifier, job.Offset)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("window temp file: %w", err)
		}
		return path, nil
	}

	args := []string{
		"-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", job.Offset), // fast seek before -i
		"-i", job.Item.FileURL,
		"-t", fmt.Sprintf("%.3f", job.Duration),
		"-vn", // discard video
		"-af", "loudnorm=I=-16:LRA=11:TP=-1.5,highpass=f=80,lowpass=f=8000",
		"-ar", sampleRate,
		"-ac", channels,
		"-b:a", windowBitrate,
		"-f", format,
		path,
	}

	winCtx, cancel := context.WithTimeout(ctx, processorTimeout)
	defer cancel()

	cmd := exec.CommandContext(winCtx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(path)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("ffmpeg: %s", msg)
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) < minWindowBytes {
		// Usually a seek past EOF on an item with unknown duration.
		os.Remove(path)
		return "", fmt.Errorf("%w (no audio at offset)", errNotSpeech)
	}
	if err := w.cache.Put(job.Item.Identifier, job.Offset, job.Duration, data); err != nil {
		logger.Warn("cache put %s: %v", job.Item.Identifier, err)
	}
	return path, nil
}

// encodePiece cuts one span out of a local window file. Fades are short —
// just enough to avoid clicks — because the span was already padded into the
// surrounding pauses.
func encodePiece(ctx context.Context, path string, s span, reversed bool) ([]byte, error) {
	af := fmt.Sprintf("afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f",
		pieceFadeIn, s.dur()-pieceFadeOut, pieceFadeOut)
	if reversed {
		// areverse first so the fades shape the backwards audio.
		af = "areverse," + af
	}

	args := []string{
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.3f", s.start),
		"-i", path,
		"-t", fmt.Sprintf("%.3f", s.dur()),
		"-af", af,
		"-ar", sampleRate,
		"-ac", channels,
		"-b:a", bitrate,
		"-f", format,
		"pipe:1",
	}

	pCtx, cancel := context.WithTimeout(ctx, pieceTimeout)
	defer cancel()

	cmd := exec.CommandContext(pCtx, "ffmpeg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg: %s", msg)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced no output")
	}
	return stdout.Bytes(), nil
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

// EncodeSilence returns dur seconds of digital silence encoded with the same
// parameters as pieces, for the session to place between them. Silence is
// encoded once at startup and reused, so concatenation stays byte-level.
func EncodeSilence(ctx context.Context, dur float64) (ProcessedClip, error) {
	args := []string{
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", fmt.Sprintf("anullsrc=r=%s:cl=stereo", sampleRate),
		"-t", fmt.Sprintf("%.3f", dur),
		"-b:a", bitrate,
		"-f", format,
		"pipe:1",
	}

	sCtx, cancel := context.WithTimeout(ctx, pieceTimeout)
	defer cancel()

	cmd := exec.CommandContext(sCtx, "ffmpeg", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ProcessedClip{}, fmt.Errorf("silence: %w", err)
	}
	return ProcessedClip{Data: stdout.Bytes(), Duration: dur, Silence: true}, nil
}

// Pool manages a fixed number of parallel processor workers.
type Pool struct {
	workers []*Worker
}

// NewPool creates n workers sharing the same input, output, cache and
// transcriber.
func NewPool(n int, in <-chan scanner.ClipJob, out chan<- ProcessedClip, c *cache.Cache, t *transcribe.Transcriber) *Pool {
	workers := make([]*Worker, n)
	for i := range workers {
		workers[i] = NewWorker(i, in, out, c, t)
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
