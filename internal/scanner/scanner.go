package scanner

import (
	"context"
	"math"
	"math/rand"

	"github.com/isaiah/ghost-radigo/internal/fetcher"
)

const (
	bufferSize   = 32   // max MediaItems held locally
	minClip      = 1.5  // seconds
	maxClip      = 5.0  // seconds
	silenceProb  = 0.40 // 40% chance of emitting a noise gap between clips

	// Drift parameters.
	normalDriftMax  = 3.0  // max year change per clip under normal drift
	largeDriftMax   = 15.0 // max year change during an occasional jump
	largeDriftProb  = 0.10 // probability of a large drift step
	yearMin         = 1900.0
	yearMax         = 1980.0

	// Item selection: items within this many years of currentYear score well.
	yearWindow = 12.0
)

// ClipJob describes a segment to be extracted from a media item.
type ClipJob struct {
	Item     fetcher.MediaItem
	Offset   float64 // Start position in seconds
	Duration float64 // Clip length in seconds (3–20)
	Silence  bool    // If true, emit a silence gap instead of audio
}

// Scanner is the station dial: it maintains temporal state and emits ClipJobs.
type Scanner struct {
	in  <-chan fetcher.MediaItem
	Out chan<- ClipJob

	currentYear float64
	rng         *rand.Rand
	buf         []fetcher.MediaItem
}

// New creates a Scanner reading from in and writing jobs to out.
// seed controls the initial year and all random decisions.
func New(in <-chan fetcher.MediaItem, out chan<- ClipJob, seed int64) *Scanner {
	rng := rand.New(rand.NewSource(seed))
	// Start somewhere in the 1920–1960 sweet spot of the archive.
	startYear := 1920.0 + rng.Float64()*40.0
	return &Scanner{
		in:          in,
		Out:         out,
		currentYear: startYear,
		rng:         rng,
	}
}

// Run starts the scan loop. It blocks until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Occasionally emit a silence gap.
		if s.rng.Float64() < silenceProb {
			dur := minClip + s.rng.Float64()*(maxClip-minClip)
			select {
			case s.Out <- ClipJob{Silence: true, Duration: dur}:
			case <-ctx.Done():
				return nil
			}
			continue
		}

		// Fill the local buffer from the fetcher channel without blocking.
		s.drainFetcher()

		// If buffer is empty, block until at least one item arrives.
		if len(s.buf) == 0 {
			select {
			case item := <-s.in:
				s.buf = append(s.buf, item)
			case <-ctx.Done():
				return nil
			}
		}

		// Pick an item weighted by year proximity.
		idx := s.pickItemIndex()
		item := s.buf[idx]
		s.buf = append(s.buf[:idx], s.buf[idx+1:]...)

		// Select clip parameters.
		dur := minClip + s.rng.Float64()*(maxClip-minClip)
		offset := s.pickOffset(item, dur)

		select {
		case s.Out <- ClipJob{Item: item, Offset: offset, Duration: dur}:
		case <-ctx.Done():
			return nil
		}

		s.drift()
	}
}

// drainFetcher pulls as many items as possible from the fetcher into s.buf
// without blocking.
func (s *Scanner) drainFetcher() {
	for len(s.buf) < bufferSize {
		select {
		case item := <-s.in:
			s.buf = append(s.buf, item)
		default:
			return
		}
	}
}

// pickItemIndex returns the index of the selected item using a weighted random
// draw: items closer to currentYear have higher probability.
func (s *Scanner) pickItemIndex() int {
	if len(s.buf) == 1 {
		return 0
	}
	weights := make([]float64, len(s.buf))
	for i, item := range s.buf {
		if item.Year == 0 {
			weights[i] = 0.4 // unknown year: below-average but not excluded
		} else {
			dist := math.Abs(float64(item.Year) - s.currentYear)
			weights[i] = math.Exp(-dist / yearWindow)
		}
	}
	return weightedRandom(s.rng, weights)
}

// pickOffset returns a random start offset that leaves at least dur seconds
// remaining in the item.
func (s *Scanner) pickOffset(item fetcher.MediaItem, dur float64) float64 {
	if item.Duration <= 0 || item.Duration <= dur {
		// Unknown or short duration: pick within first 5 minutes.
		return s.rng.Float64() * 300
	}
	return s.rng.Float64() * (item.Duration - dur)
}

// drift advances currentYear by a random walk, with occasional larger jumps.
func (s *Scanner) drift() {
	var delta float64
	if s.rng.Float64() < largeDriftProb {
		delta = (s.rng.Float64()*2 - 1) * largeDriftMax
	} else {
		delta = (s.rng.Float64()*2 - 1) * normalDriftMax
	}
	s.currentYear = math.Max(yearMin, math.Min(yearMax, s.currentYear+delta))
}

// weightedRandom picks an index with probability proportional to weights.
func weightedRandom(rng *rand.Rand, weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total == 0 {
		return rng.Intn(len(weights))
	}
	r := rng.Float64() * total
	cumulative := 0.0
	for i, w := range weights {
		cumulative += w
		if r <= cumulative {
			return i
		}
	}
	return len(weights) - 1
}
