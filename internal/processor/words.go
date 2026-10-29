package processor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/isaiah/ghost-radigo/internal/grammar"
	"github.com/isaiah/ghost-radigo/internal/transcribe"
)

// minPieceConf is the mean word confidence a piece needs for its transcript
// to be trusted. Below it the words are likely misheard (old, noisy audio)
// or not English at all, and the piece is treated as untranscribed.
const minPieceConf = 0.5

// transcribeWindow converts a window to the 16 kHz mono WAV whisper-cli
// expects and returns its words. ffmpeg does the conversion.
func (w *Worker) transcribeWindow(ctx context.Context, path string) ([]transcribe.Word, error) {
	if w.transcriber == nil {
		return nil, nil
	}
	wav := strings.TrimSuffix(path, ".mp3") + ".wav"
	defer os.Remove(wav)

	wCtx, cancel := context.WithTimeout(ctx, pieceTimeout)
	defer cancel()

	cmd := exec.CommandContext(wCtx, "ffmpeg",
		"-loglevel", "error", "-y",
		"-i", path,
		"-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le",
		wav,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("wav: %s", msg)
	}
	return w.transcriber.Words(ctx, wav)
}

// annotate attaches transcribed words to the pieces cut from the same window.
// A word belongs to the piece containing its midpoint. Pieces whose words are
// confident speech get their text and the grammar tags of their first and
// last words, and their role is re-derived from the transcript:
//
//   - opening: follows sentence-final punctuation, or starts with a capital
//   - ending: ends with sentence-final punctuation, or sits before a long
//     pause and ends on a word that can end a sentence (not "the", "of"…)
func annotate(pieces []span, words []transcribe.Word) {
	for i := range pieces {
		p := &pieces[i]
		first, last := -1, -1
		for j, w := range words {
			mid := (w.Start + w.End) / 2
			if mid >= p.start && mid <= p.end {
				if first < 0 {
					first = j
				}
				last = j
			}
		}
		if first < 0 {
			continue
		}

		var conf float64
		texts := make([]string, 0, last-first+1)
		speech := true
		for _, w := range words[first : last+1] {
			conf += w.Conf
			texts = append(texts, w.Text)
			speech = speech && !w.NonSpeech()
		}
		if !speech || conf/float64(len(texts)) < minPieceConf {
			continue
		}

		p.text = strings.Join(texts, " ")
		p.first = grammar.Of(words[first].Text)
		p.last = grammar.Of(words[last].Text)

		// The window's first word has no transcribed predecessor (and Whisper
		// capitalises it regardless), so fall back to what the pauses said.
		pauseOpens := p.role == RoleOpening || p.role == RoleWhole
		pauseEnds := p.role == RoleEnding || p.role == RoleWhole
		opens := pauseOpens
		if first > 0 {
			opens = grammar.EndsSentence(words[first-1].Text) || grammar.Capitalized(words[first].Text)
		}
		lastText := words[last].Text
		ends := grammar.EndsSentence(lastText) ||
			(pauseEnds && grammar.CanEnd(p.last) && !strings.HasSuffix(lastText, ","))
		switch {
		case opens && ends:
			p.role = RoleWhole
		case opens:
			p.role = RoleOpening
		case ends:
			p.role = RoleEnding
		default:
			p.role = RoleMiddle
		}
	}
}

// preferTranscribed moves transcribed pieces ahead of untranscribed ones,
// keeping the existing (shuffled) order within each group.
func preferTranscribed(pieces []span) {
	sort.SliceStable(pieces, func(i, j int) bool {
		return pieces[i].text != "" && pieces[j].text == ""
	})
}
