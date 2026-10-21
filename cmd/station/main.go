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
	"github.com/isaiah/ghost-radigo/internal/pool"
	"github.com/isaiah/ghost-radigo/internal/processor"
	"github.com/isaiah/ghost-radigo/internal/scanner"
	"github.com/isaiah/ghost-radigo/internal/server"
)

const (
	addr           = ":8080"
	processorCount = 5
	seed           = 0 // 0 = derive from current time
)

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
	clipPool := pool.New(clipC)
	srv      := server.New(addr, clipPool)

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
		if err := clipPool.Run(ctx); err != nil {
			logger.Error("pool: %v", err)
		}
	}()

	logger.Info("ghost radio listening on %s", addr)
	if err := srv.Run(ctx); err != nil {
		logger.Error("server: %v", err)
	}
}
