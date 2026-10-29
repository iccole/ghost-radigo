package session

import (
	mathrand "math/rand"
	"regexp"
	"testing"

	"github.com/isaiah/ghost-radigo/internal/processor"
)

// testSession returns a deterministic session whose gaps encode as "w"
// (word) and "|" (phrase), so a composite's bytes spell out its structure.
func testSession(target float64) *Session {
	s := newSession(Gaps{
		Word:   []processor.ProcessedClip{{Data: []byte("w"), Duration: 0.1}},
		Phrase: []processor.ProcessedClip{{Data: []byte("|"), Duration: 0.4}},
	})
	s.rng = mathrand.New(mathrand.NewSource(1))
	s.targetDur = target
	return s
}

func piece(name, id string, role processor.Role) frag {
	return frag{data: []byte(name), duration: 1, role: role, identifier: id}
}

func composite(t *testing.T, s *Session, n int) string {
	t.Helper()
	c, ok := s.Clip(n)
	if !ok {
		t.Fatalf("composite %d not published (have %d)", n, s.Count())
	}
	return string(c.Data)
}

func TestPhrasePattern(t *testing.T) {
	s := testSession(0.1)
	// Arrival order doesn't matter: the ending and middles wait in their
	// pools until an opening starts the phrase.
	s.ingest(piece("E1", "e", processor.RoleEnding))
	s.ingest(piece("M1", "m1", processor.RoleMiddle))
	s.ingest(piece("M2", "m2", processor.RoleMiddle))
	s.ingest(piece("M3", "m3", processor.RoleMiddle))
	if s.Count() != 0 {
		t.Fatal("published before any opening arrived")
	}
	s.ingest(piece("O1", "o", processor.RoleOpening))

	got := composite(t, s, 0)
	if !regexp.MustCompile(`^O1(wM\d){1,3}wE1$`).MatchString(got) {
		t.Errorf("composite = %q, want opening, 1–3 middles, ending", got)
	}
}

func TestWholePhrasesJoinWithPhraseGaps(t *testing.T) {
	s := testSession(1.5)
	s.ingest(piece("W1", "a", processor.RoleWhole))
	if s.Count() != 0 {
		t.Fatal("published below target length")
	}
	s.ingest(piece("W2", "b", processor.RoleWhole))
	if got := composite(t, s, 0); got != "W1|W2" {
		t.Errorf("composite = %q, want %q", got, "W1|W2")
	}
}

func TestPhraseEndsEarlyAtTarget(t *testing.T) {
	s := testSession(1.5)
	s.ingest(piece("O1", "o", processor.RoleOpening))
	s.middlesLeft = maxMiddles
	s.ingest(piece("M1", "m1", processor.RoleMiddle))
	s.ingest(piece("M2", "m2", processor.RoleMiddle))
	s.ingest(piece("E1", "e", processor.RoleEnding))

	// O1 + gap + M1 is past the target, so the phrase goes straight to its
	// ending and M2 waits for the next composite.
	if got := composite(t, s, 0); got != "O1wM1wE1" {
		t.Errorf("composite = %q, want %q", got, "O1wM1wE1")
	}
}

func TestTakePrefersDifferentSource(t *testing.T) {
	s := testSession(100)
	s.lastID = "same"
	s.pools[processor.RoleMiddle] = []frag{
		piece("A", "same", processor.RoleMiddle),
		piece("B", "other", processor.RoleMiddle),
	}
	f, ok := s.take(processor.RoleMiddle)
	if !ok || string(f.data) != "B" {
		t.Errorf("took %q, want the piece from a different recording", f.data)
	}
	// With only same-source pieces left, it still takes one.
	if f, ok := s.take(processor.RoleMiddle); !ok || string(f.data) != "A" {
		t.Errorf("took %q, want the remaining piece", f.data)
	}
}

func TestStallRelaxesPattern(t *testing.T) {
	s := testSession(100)
	// No openings ever arrive; once the middle pool fills, they stand in.
	for i := 0; i < poolCap; i++ {
		s.ingest(piece("M", "m", processor.RoleMiddle))
	}
	if len(s.buf) == 0 {
		t.Fatal("assembly stalled with a full pool")
	}
}

func TestPoolCap(t *testing.T) {
	s := testSession(100)
	for i := 0; i < poolCap+3; i++ {
		s.ingest(piece("E", "e", processor.RoleEnding))
	}
	// A full pool also relaxes the pattern and drains, so only the bound
	// is fixed.
	if n := len(s.pools[processor.RoleEnding]); n > poolCap {
		t.Errorf("ending pool holds %d, want at most %d", n, poolCap)
	}
}
