package session

import (
	mathrand "math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/isaiah/ghost-radigo/internal/grammar"
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

// spoken returns a transcribed piece tagged from its first and last words.
func spoken(text, id string, role processor.Role) frag {
	f := piece(text, id, role)
	words := strings.Fields(text)
	f.text = text
	f.first = grammar.Of(words[0])
	f.last = grammar.Of(words[len(words)-1])
	return f
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

func TestTakeRespectsGrammar(t *testing.T) {
	s := testSession(100)
	s.lastTag = grammar.Of("the")
	s.pools[processor.RoleMiddle] = []frag{
		spoken("went down to", "a", processor.RoleMiddle),
		spoken("ferry boat from", "b", processor.RoleMiddle),
	}
	f, ok := s.take(processor.RoleMiddle)
	if !ok || f.text != "ferry boat from" {
		t.Fatalf("took %q after \"the\", want \"ferry boat from\"", f.text)
	}
	if f, ok := s.take(processor.RoleMiddle); ok {
		t.Errorf("took %q after \"the\"; a verb can't follow a determiner", f.text)
	}
}

func TestTakePrefersTranscribed(t *testing.T) {
	s := testSession(100)
	s.pools[processor.RoleOpening] = []frag{
		piece("hiss", "a", processor.RoleOpening),
		spoken("Do you know", "b", processor.RoleOpening),
	}
	if f, _ := s.take(processor.RoleOpening); f.text != "Do you know" {
		t.Errorf("took %q, want the transcribed piece", f.data)
	}
}

func TestFullPoolOverridesGrammar(t *testing.T) {
	s := testSession(100)
	s.ingest(spoken("look at the", "o", processor.RoleOpening))
	// Only verbs arrive for the middle slot after "the"; once their pool
	// fills, one stands in rather than stalling the session.
	for i := 0; i < poolCap; i++ {
		s.ingest(spoken("went away", "m", processor.RoleMiddle))
	}
	if len(s.buf) < 2 {
		t.Fatal("assembly stalled on a full pool of ungrammatical pieces")
	}
}

func TestTranscript(t *testing.T) {
	s := testSession(100)
	s.ingest(spoken("Do you know", "a", processor.RoleOpening))
	s.middlesLeft = 1
	s.ingest(piece("hiss", "b", processor.RoleMiddle))
	s.ingest(spoken("the river?", "c", processor.RoleEnding))
	s.ingest(spoken("Yes.", "d", processor.RoleWhole))
	want := `"Do you know" … "the river?" / "Yes."`
	if got := s.transcript(); got != want {
		t.Errorf("transcript = %s, want %s", got, want)
	}
}

func TestFallbackRelaxesRoleBeforeGrammar(t *testing.T) {
	s := testSession(100)
	s.lastTag = grammar.Of("the")
	s.pools[processor.RoleEnding] = []frag{spoken("old boat.", "e", processor.RoleEnding)}
	for i := 0; i < poolCap; i++ {
		s.pools[processor.RoleMiddle] = append(s.pools[processor.RoleMiddle],
			spoken("went away", "m", processor.RoleMiddle))
	}
	if f, _ := s.takeAny(); f.text != "old boat." {
		t.Errorf("fallback took %q, want the grammatical piece from another role", f.text)
	}
	if f, _ := s.takeAny(); f.text != "went away" {
		t.Errorf("fallback took %q, want an ungrammatical piece once nothing fits", f.text)
	}
}
