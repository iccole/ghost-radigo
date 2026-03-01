package pool

import (
	"context"
	"math/rand"

	"github.com/isaiah/ghost-radigo/internal/processor"
)

const (
	// capacity is the number of sweep chunks buffered and ready to stream.
	capacity = 128

	// chunkBytes is the number of MP3 bytes per sweep chunk.
	// At 128 kbps: 128000 bits/s ÷ 8 = 16000 bytes/s × 0.150 s ≈ 2400 bytes.
	chunkBytes = 2400

	// chunkSecs is the audio duration represented by one chunk.
	chunkSecs = 0.150

	// minChunks / maxChunks bounds the random burst length taken from each
	// clip before switching sources. 1–6 chunks = 150 ms – 900 ms per source.
	minChunks = 1
	maxChunks = 6
)

// Pool slices pre-encoded clips into ~150 ms sweep chunks so the HTTP handler
// can stitch sources together at ghost-box speed.
type Pool struct {
	clips chan processor.ProcessedClip
	in    <-chan processor.ProcessedClip
}

// New creates a Pool that reads from the processor output channel.
func New(in <-chan processor.ProcessedClip) *Pool {
	return &Pool{
		clips: make(chan processor.ProcessedClip, capacity),
		in:    in,
	}
}

// Run slices each incoming clip into chunkBytes-sized pieces and enqueues
// them. Blocks until ctx is cancelled or in is closed.
func (p *Pool) Run(ctx context.Context) error {
	for {
		select {
		case clip, ok := <-p.in:
			if !ok {
				return nil
			}
			n := minChunks + rand.Intn(maxChunks-minChunks+1)
			data := clip.Data
			if len(data) > chunkBytes {
				offset := rand.Intn(len(data)-chunkBytes) / chunkBytes * chunkBytes
				data = data[offset:]
			}
			for i := 0; i < n && len(data) > 0; i++ {
				end := chunkBytes
				if end > len(data) {
					end = len(data)
				}
				chunk := processor.ProcessedClip{
					Data:       data[:end],
					Duration:   chunkSecs,
					Silence:    clip.Silence,
					Identifier: clip.Identifier,
					Title:      clip.Title,
					Year:       clip.Year,
				}
				data = data[end:]
				select {
				case p.clips <- chunk:
				case <-ctx.Done():
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// C returns the receive-only chunk channel for streaming handlers.
func (p *Pool) C() <-chan processor.ProcessedClip {
	return p.clips
}
