// Package transcribe turns speech windows into timed words by shelling out
// to whisper.cpp's whisper-cli. It only analyses audio — decoding and
// resampling to the 16 kHz WAV whisper-cli reads is ffmpeg's job.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const (
	binary = "whisper-cli"

	// Each call loads the model and decodes ~12 s of audio: about 1.5 s with
	// tiny.en and 2.5 s with base.en on two CPU threads. Capping concurrent
	// calls keeps the processor pool from saturating the CPU.
	threads       = 2
	maxConcurrent = 4
	timeout       = 60 * time.Second
)

// Word is one transcribed word.
type Word struct {
	Text  string  // as Whisper wrote it, including punctuation, trimmed
	Start float64 // seconds from the start of the file
	End   float64
	Conf  float64 // lowest probability of the word's lexical tokens, 0–1
}

// NonSpeech reports whether Whisper marked this "word" as something other
// than speech: ♪ for singing or music, or part of a bracketed annotation
// such as [BLANK_AUDIO] or "(upbeat music)", which splits across words.
func (w Word) NonSpeech() bool {
	return strings.ContainsAny(w.Text, "♪[]()")
}

// Transcriber runs whisper-cli with a fixed model. A nil *Transcriber is
// valid and transcribes nothing.
type Transcriber struct {
	bin   string
	model string
	sem   chan struct{}
}

// New returns a Transcriber for the given ggml model file, or an error if
// whisper-cli isn't on $PATH or the model is missing.
func New(model string) (*Transcriber, error) {
	bin, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("transcribe: %w", err)
	}
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("transcribe: model: %w", err)
	}
	return &Transcriber{bin: bin, model: model, sem: make(chan struct{}, maxConcurrent)}, nil
}

// Words transcribes a 16 kHz mono WAV file into English words with
// timestamps. Safe to call on a nil *Transcriber (returns no words).
func (t *Transcriber) Words(ctx context.Context, wavPath string) ([]Word, error) {
	if t == nil {
		return nil, nil
	}
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outBase := wavPath + ".whisper"
	defer os.Remove(outBase + ".json")

	cmd := exec.CommandContext(tCtx, t.bin,
		"-m", t.model,
		"-f", wavPath,
		"-l", "en",
		"-t", fmt.Sprint(threads),
		// CPU only: matches the Docker image, and concurrent calls don't
		// each need to set up a GPU context.
		"-ng",
		// One segment per word, split on word boundaries, so every word
		// gets its own timestamps.
		"-ml", "1", "-sow",
		"-ojf", "-of", outBase,
		"-np",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("whisper: %s", lastLine(msg))
	}

	data, err := os.ReadFile(outBase + ".json")
	if err != nil {
		return nil, fmt.Errorf("whisper output: %w", err)
	}
	return parse(data)
}

// output mirrors the parts of whisper-cli's --output-json-full we use.
type output struct {
	Transcription []struct {
		Offsets struct {
			From int `json:"from"` // milliseconds
			To   int `json:"to"`
		} `json:"offsets"`
		Text   string `json:"text"`
		Tokens []struct {
			Text string  `json:"text"`
			P    float64 `json:"p"`
		} `json:"tokens"`
	} `json:"transcription"`
}

// parse converts whisper-cli JSON (run with one segment per word) to Words.
func parse(data []byte) ([]Word, error) {
	var out output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("whisper output: %w", err)
	}
	var words []Word
	for _, seg := range out.Transcription {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		conf, ok := 1.0, false
		for _, tok := range seg.Tokens {
			// Special tokens ([_BEG_], [_TT_123]) carry no word content, and
			// punctuation tokens score low even on clearly heard words.
			if strings.HasPrefix(tok.Text, "[_") || !strings.ContainsFunc(tok.Text, isWordRune) {
				continue
			}
			conf, ok = min(conf, tok.P), true
		}
		if !ok {
			continue
		}
		words = append(words, Word{
			Text:  text,
			Start: float64(seg.Offsets.From) / 1000,
			End:   float64(seg.Offsets.To) / 1000,
			Conf:  conf,
		})
	}
	return words, nil
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '♪' }

// lastLine keeps error messages to whisper-cli's final line of output.
func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
