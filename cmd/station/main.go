package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/isaiah/ghost-radigo/internal/cache"
	"github.com/isaiah/ghost-radigo/internal/fetcher"
	"github.com/isaiah/ghost-radigo/internal/logger"
	"github.com/isaiah/ghost-radigo/internal/processor"
	"github.com/isaiah/ghost-radigo/internal/scanner"
	"github.com/isaiah/ghost-radigo/internal/server"
	"github.com/isaiah/ghost-radigo/internal/session"
)

const (
	addr           = ":8080"
	processorCount = 8
	seed           = 0 // 0 = derive from current time
)

// Gap lengths in seconds. Each encoded MP3 also carries ~50 ms of encoder
// padding, so the audible pauses run slightly longer.
var (
	wordGaps   = []float64{0.06, 0.09, 0.12, 0.15}
	phraseGaps = []float64{0.30, 0.40, 0.50, 0.60}
)

// encodeGaps pre-encodes the silences sessions place between pieces. A gap
// that fails to encode is skipped; with none, pieces simply butt together.
func encodeGaps(ctx context.Context) session.Gaps {
	encode := func(durs []float64) []processor.ProcessedClip {
		var out []processor.ProcessedClip
		for _, d := range durs {
			g, err := processor.EncodeSilence(ctx, d)
			if err != nil {
				logger.Warn("gap %.2fs: %v", d, err)
				continue
			}
			out = append(out, g)
		}
		return out
	}
	return session.Gaps{Word: encode(wordGaps), Phrase: encode(phraseGaps)}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Channels connecting pipeline stages.
	mediaC := make(chan fetcher.MediaItem, 16)
	jobC   := make(chan scanner.ClipJob, 8)
	clipC  := make(chan processor.ProcessedClip, 4)

	// Resolve seed.
	s := int64(seed)
	if s == 0 {
		s = time.Now().UnixNano()
	}

	// Set up disk cache.
	clipCache, err := cache.New(cache.DefaultDir())
	if err != nil {
		logger.Error("cache unavailable: %v — running without disk cache", err)
		clipCache = nil
	} else {
		logger.Info("clip cache: %s", cache.DefaultDir())
	}

	// Assemble pipeline.
	fetch    := fetcher.New(mediaC)
	scan     := scanner.New(mediaC, jobC, s)
	procPool := processor.NewPool(processorCount, jobC, clipC, clipCache)
	sessions := session.NewManager(clipC, encodeGaps(ctx))
	srv      := server.New(addr, sessions)

	// Run all stages concurrently.
	go func() {
		if err := fetch.Run(ctx); err != nil {
			logger.Error("fetcher: %v", err)
		}
	}()
	go func() {
		if err := scan.Run(ctx); err != nil {
			logger.Error("scanner: %v", err)
		}
	}()
	go func() {
		if err := procPool.Run(ctx); err != nil {
			logger.Error("processor pool: %v", err)
		}
	}()
	go func() {
		if err := sessions.Run(ctx); err != nil {
			logger.Error("session manager: %v", err)
		}
	}()

	logger.Info("ghost radio listening on %s", addr)
	if err := srv.Run(ctx); err != nil {
		logger.Error("server: %v", err)
	}
}
