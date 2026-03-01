package mixer

import (
	"context"
	"io"
	"time"

	"github.com/isaiah/ghost-radigo/internal/processor"
)

const (
	// How far ahead of wall-clock time the mixer is allowed to write before
	// pausing. Keeps client buffer depth predictable (~2 s).
	targetAhead = 2 * time.Second

	// How long to wait for the next clip before looping back to check pacing.
	clipWait = 500 * time.Millisecond
)

// Mixer consumes processed clips and writes a continuous audio byte stream.
type Mixer struct {
	in  <-chan processor.ProcessedClip
	out io.Writer
}

// New creates a Mixer that reads from in and writes the stream to out.
func New(in <-chan processor.ProcessedClip, out io.Writer) *Mixer {
	return &Mixer{in: in, out: out}
}

// Run starts the mix loop. It blocks until ctx is cancelled or out returns
// an error (e.g. all listeners disconnected).
func (m *Mixer) Run(ctx context.Context) error {
	start := time.Now()
	var audioWritten time.Duration // total audio time pushed downstream

	for {
		// Pace: if we're more than targetAhead of wall-clock, sleep until
		// we're only targetAhead/2 ahead. This keeps client buffers shallow
		// and prevents writing minutes of data in a burst.
		elapsed := time.Since(start)
		ahead := audioWritten - elapsed
		if ahead > targetAhead {
			sleep := ahead - targetAhead/2
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return nil
			}
		}

		select {
		case clip, ok := <-m.in:
			if !ok {
				return nil
			}
			if _, err := m.out.Write(clip.Data); err != nil {
				// Downstream writer failed (e.g. broadcast has no clients
				// and returned an error). Keep going — new clients may connect.
				_ = err
			}
			audioWritten += time.Duration(float64(time.Second) * clip.Duration)

		case <-time.After(clipWait):
			// No clip ready yet. Loop back; pacing will handle timing.

		case <-ctx.Done():
			return nil
		}
	}
}
