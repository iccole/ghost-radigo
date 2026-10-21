package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/isaiah/ghost-radigo/internal/processor"
)

// MinClips is the number of composite clips produced before the session auto-stops.
const MinClips = 5

const (
	minStitchDur = 7.0  // seconds — minimum composite length
	maxStitchDur = 10.0 // seconds — maximum composite length
)

// Clip is a stitched composite of several raw processor clips concatenated
// into a single 7–10 second audio blob.
type Clip struct {
	Index      int
	Data       []byte
	Duration   float64
	Year       int
	Title      string
	Identifier string
}

// frag is a single raw processor output held in the accumulation buffer.
type frag struct {
	data       []byte
	duration   float64
	year       int
	title      string
	identifier string
}

// Session accumulates raw clips into composites and signals listeners.
type Session struct {
	ID         string
	composites []Clip
	mu         sync.RWMutex
	Notify     chan struct{} // receives a value each time a composite is ready
	Done       chan struct{} // closed when the session is stopped
	once       sync.Once

	// accumulation state — only accessed from Manager.Run goroutine, no mutex needed
	buf       []frag
	bufDur    float64
	targetDur float64
	rng       *mathrand.Rand
}

func newSession() *Session {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	return &Session{
		ID:        hex.EncodeToString(b),
		Notify:    make(chan struct{}, 64),
		Done:      make(chan struct{}),
		rng:       rng,
		targetDur: minStitchDur + rng.Float64()*(maxStitchDur-minStitchDur),
	}
}

// ingest adds a raw processor clip to the buffer. When the buffer reaches
// targetDur, the fragments are concatenated into a composite clip and stored.
// Must only be called from Manager.Run (single goroutine — no mutex on buf).
func (s *Session) ingest(f frag) {
	s.buf = append(s.buf, f)
	s.bufDur += f.duration

	if s.bufDur < s.targetDur {
		return
	}

	// Concatenate all fragment bytes into one audio blob.
	total := 0
	for _, fr := range s.buf {
		total += len(fr.data)
	}
	data := make([]byte, 0, total)
	last := s.buf[len(s.buf)-1]
	for _, fr := range s.buf {
		data = append(data, fr.data...)
	}

	composite := Clip{
		Data:       data,
		Duration:   s.bufDur,
		Year:       last.year,
		Title:      last.title,
		Identifier: last.identifier,
	}

	s.mu.Lock()
	composite.Index = len(s.composites)
	s.composites = append(s.composites, composite)
	s.mu.Unlock()

	// Reset buffer for next composite.
	s.buf = s.buf[:0]
	s.bufDur = 0
	s.targetDur = minStitchDur + s.rng.Float64()*(maxStitchDur-minStitchDur)

	select {
	case s.Notify <- struct{}{}:
	default:
	}
}

// Clip returns the composite clip at index n, or false if not yet available.
func (s *Session) Clip(n int) (Clip, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n < 0 || n >= len(s.composites) {
		return Clip{}, false
	}
	return s.composites[n], true
}

// Count returns how many composite clips have been produced so far.
func (s *Session) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.composites)
}

// Stop signals that the session should stop collecting.
func (s *Session) Stop() {
	s.once.Do(func() { close(s.Done) })
}

// Manager is the sole consumer of the processor output channel.
// It routes raw clips into active sessions for accumulation.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	in       <-chan processor.ProcessedClip
}

// NewManager creates a Manager that reads from the processor pool output.
func NewManager(in <-chan processor.ProcessedClip) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		in:       in,
	}
}

// Create registers and returns a new session.
func (m *Manager) Create() *Session {
	s := newSession()
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s
}

// Get looks up a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Remove deletes a session from the store.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// Run drains the processor channel and routes non-silence clips into all
// active sessions for accumulation. Blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	for {
		select {
		case clip, ok := <-m.in:
			if !ok {
				return nil
			}
			if clip.Silence {
				continue
			}
			f := frag{
				data:       clip.Data,
				duration:   clip.Duration,
				year:       clip.Year,
				title:      clip.Title,
				identifier: clip.Identifier,
			}
			m.mu.RLock()
			for _, s := range m.sessions {
				select {
				case <-s.Done:
				default:
					s.ingest(f)
				}
			}
			m.mu.RUnlock()
		case <-ctx.Done():
			return nil
		}
	}
}
