package transcribe

import (
	"context"
	"os"
	"testing"
)

func TestParse(t *testing.T) {
	// Real whisper-cli --output-json-full output, trimmed to its first words.
	data, err := os.ReadFile("testdata/words.json")
	if err != nil {
		t.Fatal(err)
	}
	words, err := parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Yes,", "yes,", "I", "am", "English."}
	if len(words) != len(want) {
		t.Fatalf("got %d words %+v, want %d", len(words), words, len(want))
	}
	for i, w := range want {
		if words[i].Text != w {
			t.Errorf("word %d = %q, want %q", i, words[i].Text, w)
		}
		if words[i].End < words[i].Start || words[i].Conf <= 0 || words[i].Conf > 1 {
			t.Errorf("word %d has bad timing or confidence: %+v", i, words[i])
		}
	}
	// "English." is 0.92 for the word and 0.48 for the full stop; only the
	// word counts.
	if words[4].Conf != 0.9224 {
		t.Errorf("English. conf = %v, want 0.9224 (punctuation ignored)", words[4].Conf)
	}
	if words[2].Start != 0.9 || words[2].End != 0.99 {
		t.Errorf("word 2 timing = %.2f–%.2f, want 0.90–0.99", words[2].Start, words[2].End)
	}
}

func TestNonSpeech(t *testing.T) {
	for text, want := range map[string]bool{
		"♪":             true,
		"[BLANK_AUDIO]": true,
		"(laughs)":      true,
		"music)":        true,
		"boat":          false,
	} {
		if got := (Word{Text: text}).NonSpeech(); got != want {
			t.Errorf("NonSpeech(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestNilTranscriber(t *testing.T) {
	var tr *Transcriber
	words, err := tr.Words(context.Background(), "missing.wav")
	if words != nil || err != nil {
		t.Errorf("nil Transcriber returned %v, %v; want nil, nil", words, err)
	}
}
