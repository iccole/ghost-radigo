package cache

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Cache is a simple disk cache for the audio windows the processor analyses.
// Keys are deterministic so the same (identifier, offset, duration) region
// always maps to the same file, surviving process restarts.
//
// The offset is quantised into 5-second buckets and the duration is rounded
// to the nearest second; the scanner snaps window offsets to the same grid,
// so revisiting a region of a file skips the network fetch.
type Cache struct {
	dir string
}

// New opens (or creates) a cache rooted at dir.
func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	return &Cache{dir: dir}, nil
}

// DefaultDir returns the platform cache directory for ghost-radio, respecting
// GHOST_RADIO_CACHE_DIR if set.
func DefaultDir() string {
	if dir := os.Getenv("GHOST_RADIO_CACHE_DIR"); dir != "" {
		return dir
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "ghost-radio")
	}
	return filepath.Join(os.TempDir(), "ghost-radio")
}

// Get returns cached bytes for the given window parameters, or (nil, false) if
// not present. Safe to call on a nil *Cache.
func (c *Cache) Get(identifier string, offset, duration float64) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	data, err := os.ReadFile(c.path(identifier, offset, duration))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Put writes bytes to the cache atomically via a temp-file rename.
// Safe to call on a nil *Cache (no-op).
func (c *Cache) Put(identifier string, offset, duration float64, data []byte) error {
	if c == nil {
		return nil
	}
	p := c.path(identifier, offset, duration)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("cache write: %w", err)
	}
	return os.Rename(tmp, p)
}

// path returns the file path for a window.
func (c *Cache) path(identifier string, offset, duration float64) string {
	bucket := int(math.Round(offset/5)) * 5 // 5-second buckets
	durSec := int(math.Round(duration))     // nearest second
	name := fmt.Sprintf("%s_%ds_%ds.mp3", identifier, bucket, durSec)
	return filepath.Join(c.dir, name)
}
