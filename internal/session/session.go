package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"strings"
	"sync"
	"time"

	"github.com/isaiah/ghost-radigo/internal/grammar"
	"github.com/isaiah/ghost-radigo/internal/logger"
	"github.com/isaiah/ghost-radigo/internal/processor"
)

// MinClips is the number of composite clips produced before the session auto-stops.
const MinClips = 5

const (
	minStitchDur = 7.0  // seconds — target composite length is drawn from
	maxStitchDur = 10.0 // [min, max]; the phrase that crosses it is finished

	maxMiddles = 3 // middle pieces per phrase, drawn from 1..maxMiddles

	// poolCap is how many pieces are held per role; the oldest is dropped
	// when full. A full pool also relaxes the phrase pattern (see step).
	poolCap = 10
)

// Gaps holds pre-encoded silences that the session places between pieces.
// Either slice may be empty, in which case pieces butt together.
type Gaps struct {
	Word   []processor.ProcessedClip // between pieces within a phrase
	Phrase []processor.ProcessedClip // between phrases
}

// slot is the role the phrase being built needs next.
type slot int

const (
	slotOpening slot = iota
	slotMiddle
	slotEnding
)

// Clip is a stitched composite: a few sentence-like phrases built from pieces
// of many recordings, concatenated into a single audio blob.
type Clip struct {
	Index      int
	Data       []byte
	Duration   float64
	Year       int
	Title      string
	Identifier string
}

// frag is a single raw processor output held in a pool or the composite buffer.
type frag struct {
	data        []byte
	duration    float64
	role        processor.Role
	text        string      // transcript; empty when untranscribed (and for gaps)
	phraseGap   bool        // a silence between phrases (for transcript logs)
	first, last grammar.Tag // parts of speech at the piece's edges
	year        int
	title       string
	identifier  string
}

// Session assembles pieces into composites and signals listeners.
//
// Each composite is a run of phrases. A phrase is an opening piece, 1–3
// middle pieces and an ending piece (or a single whole-phrase piece), with
// short word gaps inside the phrase and longer gaps between phrases. Pieces
// wait in per-role pools until the phrase being built needs them, and a
// transcribed piece is only placed where its first word can grammatically
// follow the previous piece's last word.
type Session struct {
	ID         string
	composites []Clip
	mu         sync.RWMutex
	Notify     chan struct{} // receives a value each time a composite is ready
	Done       chan struct{} // closed when the session is stopped
	once       sync.Once

	// assembly state — only accessed from Manager.Run goroutine, no mutex needed
	pools       [4][]frag // indexed by processor.Role
	buf         []frag    // composite in progress, gaps included
	bufDur      float64
	targetDur   float64
	next        slot
	middlesLeft int
	lastID      string      // identifier of the last piece placed
	lastTag     grammar.Tag // part of speech of the last word placed; 0 at a sentence start
	gaps        Gaps
	rng         *mathrand.Rand
}

func newSession(gaps Gaps) *Session {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	return &Session{
		ID:        hex.EncodeToString(b),
		Notify:    make(chan struct{}, 64),
		Done:      make(chan struct{}),
		rng:       rng,
		gaps:      gaps,
		targetDur: minStitchDur + rng.Float64()*(maxStitchDur-minStitchDur),
	}
}

// ingest pools a piece by role, then places pooled pieces into the phrase
// being built for as long as the next slot can be filled.
// Must only be called from Manager.Run (single goroutine — no mutex on pools).
func (s *Session) ingest(f frag) {
	pool := &s.pools[f.role]
	if len(*pool) >= poolCap {
		*pool = (*pool)[1:]
	}
	*pool = append(*pool, f)

	for s.step() {
	}
}

// step fills the next slot of the current phrase if it can, and reports
// whether it made progress.
func (s *Session) step() bool {
	var f frag
	var ok bool
	switch s.next {
	case slotOpening:
		if f, ok = s.take(processor.RoleOpening); !ok {
			f, ok = s.take(processor.RoleWhole)
		}
	case slotMiddle:
		f, ok = s.take(processor.RoleMiddle)
	case slotEnding:
		if f, ok = s.take(processor.RoleEnding); !ok {
			f, ok = s.take(processor.RoleWhole)
		}
	}
	if !ok {
		// No piece of the slot's role fits. Once some pool is full — pieces
		// are about to be dropped — let any piece stand in rather than stall.
		if !s.poolFull() {
			return false
		}
		f, _ = s.takeAny()
	}
	s.fill(f)
	return true
}

// fill places f in the current slot and advances the phrase.
func (s *Session) fill(f frag) {
	switch s.next {
	case slotOpening:
		s.place(f, true)
		if f.role == processor.RoleWhole {
			s.endPhrase()
			return
		}
		s.next = slotMiddle
		s.middlesLeft = 1 + s.rng.Intn(maxMiddles)
	case slotMiddle:
		s.place(f, false)
		// Wrap the phrase up early once the composite is long enough, so
		// composites don't overshoot their target by several middles.
		if s.middlesLeft--; s.middlesLeft == 0 || s.bufDur >= s.targetDur {
			s.next = slotEnding
		}
	case slotEnding:
		s.place(f, false)
		s.endPhrase()
	}
}

// take removes the best piece of the given role whose first word can follow
// the last word placed (see score).
func (s *Session) take(role processor.Role) (frag, bool) {
	if i := s.best(role, true); i >= 0 {
		return s.remove(role, i), true
	}
	return frag{}, false
}

// takeAny is the fallback when a pool is full: it relaxes the role first,
// taking the best grammatical piece of any role, and only then grammar,
// taking from the fullest pool.
func (s *Session) takeAny() (frag, bool) {
	bestRole, bestIdx, bestScore := processor.Role(0), -1, -1
	for r := range s.pools {
		role := processor.Role(r)
		if i := s.best(role, true); i >= 0 && s.score(s.pools[r][i]) > bestScore {
			bestRole, bestIdx, bestScore = role, i, s.score(s.pools[r][i])
		}
	}
	if bestIdx >= 0 {
		return s.remove(bestRole, bestIdx), true
	}
	fullest := processor.Role(0)
	for r := range s.pools {
		if len(s.pools[r]) > len(s.pools[fullest]) {
			fullest = processor.Role(r)
		}
	}
	if i := s.best(fullest, false); i >= 0 {
		return s.remove(fullest, i), true
	}
	return frag{}, false
}

// best returns the index of the highest-scoring piece in a role's pool, or
// -1. With grammatical set, only pieces that can follow the last word count.
func (s *Session) best(role processor.Role, grammatical bool) int {
	best, bestScore := -1, -1
	for i, f := range s.pools[role] {
		if grammatical && !grammar.CanFollow(s.lastTag, f.first) {
			continue
		}
		if sc := s.score(f); sc > bestScore {
			best, bestScore = i, sc
		}
	}
	return best
}

// score ranks candidate pieces: transcribed beats untranscribed (whose
// grammar is unknown), a different recording than the last piece beats the
// same one so sources interleave, and longer pieces beat shorter ones since
// they carry real grammar inside them.
func (s *Session) score(f frag) int {
	sc := 0
	if f.text != "" {
		sc += 4
	}
	if f.identifier != s.lastID {
		sc += 2
	}
	if strings.Count(f.text, " ") >= 2 { // three words or more
		sc++
	}
	return sc
}

func (s *Session) remove(role processor.Role, i int) frag {
	pool := s.pools[role]
	f := pool[i]
	s.pools[role] = append(pool[:i], pool[i+1:]...)
	return f
}

// poolFull reports whether any role's pool is at capacity.
func (s *Session) poolFull() bool {
	for _, p := range s.pools {
		if len(p) >= poolCap {
			return true
		}
	}
	return false
}

// place appends a piece to the composite, preceded by a random phrase or
// word gap unless it is the composite's first piece.
func (s *Session) place(f frag, newPhrase bool) {
	gaps := s.gaps.Word
	if newPhrase {
		gaps = s.gaps.Phrase
	}
	if len(s.buf) > 0 && len(gaps) > 0 {
		g := gaps[s.rng.Intn(len(gaps))]
		s.buf = append(s.buf, frag{data: g.Data, duration: g.Duration, phraseGap: newPhrase})
		s.bufDur += g.Duration
	}
	s.buf = append(s.buf, f)
	s.bufDur += f.duration
	s.lastID = f.identifier
	s.lastTag = f.last
}

// endPhrase closes the current phrase and publishes the composite once it
// has reached its target length.
func (s *Session) endPhrase() {
	s.next = slotOpening
	s.lastTag = 0 // a new sentence may start with anything
	if s.bufDur >= s.targetDur {
		s.publish()
	}
}

// publish concatenates the buffer into a composite, stores it and notifies
// listeners. Gaps carry no identifier, so metadata comes from the last piece.
func (s *Session) publish() {
	total := 0
	for _, fr := range s.buf {
		total += len(fr.data)
	}
	data := make([]byte, 0, total)
	for _, fr := range s.buf {
		data = append(data, fr.data...)
	}
	last := s.buf[len(s.buf)-1]

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

	logger.Info("composite  %s #%d  %.1fs  %s", s.ID, composite.Index, s.bufDur, s.transcript())

	// Reset buffer for next composite.
	s.buf = s.buf[:0]
	s.bufDur = 0
	s.targetDur = minStitchDur + s.rng.Float64()*(maxStitchDur-minStitchDur)

	select {
	case s.Notify <- struct{}{}:
	default:
	}
}

// transcript renders the buffer for logs: piece text in quotes, "…" for an
// untranscribed piece, "/" between phrases.
func (s *Session) transcript() string {
	var b strings.Builder
	for _, f := range s.buf {
		switch {
		case f.phraseGap:
			b.WriteString(" /")
		case f.identifier == "": // word gap
		case f.text != "":
			fmt.Fprintf(&b, " %q", f.text)
		default:
			b.WriteString(" …")
		}
	}
	return strings.TrimSpace(b.String())
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
// It routes pieces into active sessions for assembly.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	in       <-chan processor.ProcessedClip
	gaps     Gaps
}

// NewManager creates a Manager that reads from the processor pool output and
// gives every session the same pre-encoded gaps.
func NewManager(in <-chan processor.ProcessedClip, gaps Gaps) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		in:       in,
		gaps:     gaps,
	}
}

// Create registers and returns a new session.
func (m *Manager) Create() *Session {
	s := newSession(m.gaps)
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

// Run drains the processor channel and routes non-silence pieces into all
// active sessions for assembly. Blocks until ctx is cancelled.
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
				role:       clip.Role,
				text:       clip.Text,
				first:      clip.First,
				last:       clip.Last,
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
