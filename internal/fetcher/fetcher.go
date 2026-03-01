package fetcher

import (
	"context"
	"math/rand"
	"net/http"
	"time"

	"github.com/isaiah/ghost-radigo/internal/logger"
)

const (
	interItemDelay = 500 * time.Millisecond
	errorDelay     = 5 * time.Second
	rateLimitDelay = 30 * time.Second
)

// MediaItem represents a single streamable item retrieved from the Internet Archive.
type MediaItem struct {
	Identifier  string    // Archive item identifier, e.g. "78_jazz-record_1923"
	Title       string
	Year        int     // Best-effort publication year
	MediaType   string  // "audio" or "movies" (video treated as audio)
	FileURL     string  // Direct download URL for the selected file
	Duration    float64 // Seconds; 0 if unknown
	RetrievedAt time.Time
}

// Fetcher periodically queries the Internet Archive and pushes MediaItems into Out.
type Fetcher struct {
	Out    chan<- MediaItem
	client *http.Client
	rng    *rand.Rand
}

// New creates a Fetcher that writes to out.
func New(out chan<- MediaItem) *Fetcher {
	return &Fetcher{
		Out:    out,
		client: &http.Client{Timeout: 20 * time.Second},
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Run starts the fetch loop. It blocks until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		query := searchQueries[f.rng.Intn(len(searchQueries))]
		start := f.rng.Intn(maxPageStart/pageSize) * pageSize

		docs, err := f.search(ctx, query, start)
		if err != nil {
			logger.Warn("fetcher: search %q page %d: %v", query, start/pageSize, err)
			if !sleepCtx(ctx, errorDelay) {
				return nil
			}
			continue
		}

		// Shuffle so consecutive items within a page arrive in random order.
		f.rng.Shuffle(len(docs), func(i, j int) { docs[i], docs[j] = docs[j], docs[i] })

		for _, doc := range docs {
			if ctx.Err() != nil {
				return nil
			}

			item, err := f.resolveItem(ctx, doc)
			if err != nil {
				logger.Warn("fetcher: skip %s: %v", doc.Identifier, err)
				continue
			}

			select {
			case f.Out <- *item:
			case <-ctx.Done():
				return nil
			}

			if !sleepCtx(ctx, interItemDelay) {
				return nil
			}
		}
	}
}

// sleepCtx sleeps for d or until ctx is done. Returns false if context ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
