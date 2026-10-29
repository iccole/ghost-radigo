package processor

import (
	"testing"

	"github.com/isaiah/ghost-radigo/internal/grammar"
	"github.com/isaiah/ghost-radigo/internal/transcribe"
)

func word(text string, start, end, conf float64) transcribe.Word {
	return transcribe.Word{Text: text, Start: start, End: end, Conf: conf}
}

func TestAnnotate(t *testing.T) {
	// Timings from a real whisper-cli run over a Father Brown episode.
	words := []transcribe.Word{
		word("I", 0.90, 0.99, 0.9),
		word("am", 0.99, 1.17, 0.9),
		word("English.", 1.17, 1.96, 0.9),
		word("I", 1.96, 2.00, 0.9),
		word("thought", 2.00, 2.29, 0.9),
		word("you", 2.29, 2.41, 0.9),
		word("were", 2.41, 2.64, 0.9),
		word("French.", 2.64, 2.93, 0.9),
		word("Do", 6.90, 6.99, 0.9),
		word("you", 6.99, 7.14, 0.9),
		word("know", 7.14, 7.34, 0.9),
		word("where", 7.34, 7.59, 0.9),
		word("we", 7.59, 7.69, 0.9),
		word("can", 7.69, 7.84, 0.9),
		word("find", 7.84, 8.04, 0.9),
		word("the", 8.04, 8.19, 0.9),
		word("ferry", 8.19, 8.50, 0.9),
		word("boat", 8.50, 8.64, 0.9),
		word("Poland", 8.64, 9.00, 0.9),
		word("today", 9.00, 9.30, 0.9),
	}
	pieces := []span{
		{start: 0.85, end: 1.70, role: RoleOpening}, // first words of the window
		{start: 1.90, end: 2.40, role: RoleMiddle},  // starts after "English."
		{start: 2.40, end: 3.00, role: RoleOpening}, // ends with "French."
		{start: 6.85, end: 7.60, role: RoleMiddle},  // starts after "French."
		{start: 4.00, end: 5.00, role: RoleEnding},  // no words: untouched
		{start: 7.60, end: 8.10, role: RoleEnding},  // long pause after, ends on a verb: ending
		{start: 8.10, end: 8.30, role: RoleEnding},  // long pause after, but "the" can't end a sentence
		{start: 8.30, end: 8.60, role: RoleEnding},  // long pause after, ends on a noun: ending
		{start: 8.60, end: 9.40, role: RoleMiddle},  // capitalised proper noun: opening
	}
	annotate(pieces, words)

	want := []struct {
		text string
		role Role
	}{
		{"I am English.", RoleWhole},
		{"I thought you", RoleOpening},
		{"were French.", RoleEnding},
		{"Do you know where", RoleOpening},
		{"", RoleEnding},
		{"we can find", RoleEnding},
		{"the", RoleMiddle},
		{"ferry boat", RoleEnding},
		{"Poland today", RoleOpening},
	}
	for i, w := range want {
		if pieces[i].text != w.text || pieces[i].role != w.role {
			t.Errorf("piece %d = %q %v, want %q %v", i, pieces[i].text, pieces[i].role, w.text, w.role)
		}
	}
	if pieces[1].first&grammar.Pron == 0 || pieces[1].last&grammar.Pron == 0 {
		t.Errorf("piece 1 tags = %b…%b, want pronouns at both ends", pieces[1].first, pieces[1].last)
	}
}

func TestAnnotateRejectsUntrustedText(t *testing.T) {
	pieces := []span{
		{start: 0, end: 1, role: RoleMiddle},
		{start: 1, end: 2, role: RoleMiddle},
	}
	annotate(pieces, []transcribe.Word{
		word("Little", 0.1, 0.4, 0.8),
		word("♪", 0.4, 0.6, 0.9),  // singing
		word("Le", 1.1, 1.3, 0.2), // misheard / not English
		word("pere", 1.3, 1.6, 0.3),
	})
	for i, p := range pieces {
		if p.text != "" {
			t.Errorf("piece %d kept untrusted text %q", i, p.text)
		}
	}
}

func TestPreferTranscribed(t *testing.T) {
	pieces := []span{{text: ""}, {text: "a"}, {text: ""}, {text: "b"}}
	preferTranscribed(pieces)
	got := pieces[0].text + pieces[1].text + pieces[2].text + pieces[3].text
	if got != "ab" || pieces[0].text != "a" {
		t.Errorf("order = %+v, want transcribed pieces first in original order", pieces)
	}
}
