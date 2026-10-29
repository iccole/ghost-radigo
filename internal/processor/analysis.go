package processor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isaiah/ghost-radigo/internal/grammar"
)

const (
	// frameSecs is the loudness resolution: levelFilter resamples to 16 kHz
	// and groups 320 samples per frame, i.e. 20 ms.
	frameSecs = 0.02

	// centroidWinSecs is aspectralstats' default analysis window (2048
	// samples at 16 kHz). It hops by half that, so centroids arrive every
	// 64 ms with their own timestamps — not on the 20 ms loudness grid.
	centroidWinSecs = 2048.0 / 16000

	// Pause detection is relative to each window's own level distribution:
	// archive recordings carry hiss, crackle and audience noise, so a fixed
	// dB threshold almost never finds the gaps between words.
	floorPct     = 0.10 // percentile taken as the window's noise floor
	peakPct      = 0.95 // percentile taken as the window's speech level
	pauseFrac    = 0.35 // threshold sits this far from floor towards peak
	minRangeDB   = 8.0  // below this floor→peak range there are no real pauses
	minPauseSecs = 0.10 // shorter dips are treated as part of a word

	// Pieces are the spans between pauses. Spans touching the window edges
	// are discarded because they were cut mid-word by the window itself.
	minPieceSecs = 0.20
	maxPieceSecs = 2.00
	piecePadSecs = 0.04 // extend into each neighbouring pause for soft onsets/tails

	// longPauseSecs separates phrases from words: a piece preceded by a pause
	// this long starts a phrase, one followed by it ends a phrase (and
	// usually carries the falling pitch of a finished sentence). Pauses in
	// windows that pass the speech check have a median of ~0.18 s and an
	// upper quartile of ~0.40 s.
	longPauseSecs = 0.40

	// Speech/music discrimination: the spectral centroid of speech jumps
	// around as the mouth moves between vowels, fricatives and stops, while
	// sustained instruments hold a steady timbre. Calibrated on OTR, LibriVox,
	// comedy LPs and 78rpm dance/jazz records: music windows sat at ~0.07–0.23,
	// clean speech at 0.34–0.70, noisy comedy/OTR at 0.13–0.35.
	minCentroidCV = 0.22
	voicedRangeDB = 20.0 // centroids where the level is within this many dB of the peak count as voiced
	minVoicedFrac = 0.20 // windows that are mostly silence are rejected

	analysisTimeout = 20 * time.Second
)

// errNotSpeech is returned when a window doesn't look like speech or has no
// usable word-sized pieces. It is not a real error — callers skip silently.
var errNotSpeech = errors.New("window does not look like speech")

// envelope is the analysis of one window.
type envelope struct {
	levels    []float64  // voice-band RMS in dB per frameSecs; -120 for digital silence
	centroids []centroid // spectral centroid on aspectralstats' own grid
}

// centroid is one spectral centroid measurement.
type centroid struct {
	at float64 // window start, seconds from the beginning of the file
	hz float64
}

// Role is where a piece sat in the phrase it was cut from.
type Role int

const (
	RoleMiddle  Role = iota // short pauses on both sides
	RoleOpening             // long pause before: starts a phrase
	RoleEnding              // long pause after: ends a phrase
	RoleWhole               // long pauses on both sides: a phrase on its own
)

func (r Role) String() string {
	switch r {
	case RoleOpening:
		return "opening"
	case RoleEnding:
		return "ending"
	case RoleWhole:
		return "whole"
	default:
		return "middle"
	}
}

// Reversed returns the role a piece plays when played backwards: what began
// a phrase now ends one.
func (r Role) Reversed() Role {
	switch r {
	case RoleOpening:
		return RoleEnding
	case RoleEnding:
		return RoleOpening
	default:
		return r
	}
}

// span is a region of a window, in seconds from the window start.
type span struct {
	start, end  float64
	role        Role
	text        string      // transcript; empty when untranscribed or untrusted
	first, last grammar.Tag // parts of speech of the first and last words
}

func (s span) dur() float64 { return s.end - s.start }

// voiceBand limits analysis to the speech range at a fixed sample rate, so
// frame sizes below mean the same duration for every source.
const voiceBand = "aformat=channel_layouts=mono,highpass=f=150,lowpass=f=3500,aresample=16000,"

// levelFilter emits voice-band RMS for consecutive 20 ms frames.
const levelFilter = voiceBand +
	"asetnsamples=n=320:p=0," +
	"astats=metadata=1:reset=1:measure_perchannel=none:measure_overall=RMS_level," +
	"ametadata=mode=print:file=-"

// timbreFilter emits the spectral centroid. aspectralstats re-frames the
// audio into its own overlapping windows, which is why it can't share a pass
// with levelFilter without skewing the 20 ms grid.
const timbreFilter = voiceBand +
	"aspectralstats=measure=centroid," +
	"ametadata=mode=print:file=-"

// analyzeWindow runs the loudness and timbre passes over a local audio file.
// ffmpeg does the signal processing; Go only reads numbers.
func analyzeWindow(ctx context.Context, path string) (envelope, error) {
	aCtx, cancel := context.WithTimeout(ctx, analysisTimeout)
	defer cancel()

	levelOut, err := runMetadata(aCtx, path, levelFilter)
	if err != nil {
		return envelope{}, err
	}
	timbreOut, err := runMetadata(aCtx, path, timbreFilter)
	if err != nil {
		return envelope{}, err
	}

	var env envelope
	for _, m := range parseMetadata(levelOut, ".RMS_level") {
		// "-inf" (digital silence) and missing values become -120.
		env.levels = append(env.levels, math.Max(m.value, -120))
	}
	for _, m := range parseMetadata(timbreOut, ".centroid") {
		if m.value > 0 {
			env.centroids = append(env.centroids, centroid{at: m.at, hz: m.value})
		}
	}
	return env, nil
}

// runMetadata runs one ffmpeg analysis pass and returns its stdout.
func runMetadata(ctx context.Context, path, filter string) (*bytes.Buffer, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostats", "-loglevel", "error",
		"-i", path,
		"-af", filter,
		"-f", "null", "-",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("analysis: %s", msg)
	}
	return &stdout, nil
}

// measurement is one frame's value for a single metadata key.
type measurement struct {
	at    float64 // pts_time of the frame
	value float64 // NaN when the key was missing or unparseable
}

// parseMetadata reads ametadata print output, which looks like:
//
//	frame:0    pts:0       pts_time:0
//	lavfi.astats.Overall.RMS_level=-21.28
//
// and returns one measurement per frame for the key ending in keySuffix.
func parseMetadata(r io.Reader, keySuffix string) []measurement {
	var out []measurement
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "frame:") {
			m := measurement{value: math.NaN()}
			for _, field := range strings.Fields(line) {
				if t, ok := strings.CutPrefix(field, "pts_time:"); ok {
					m.at, _ = strconv.ParseFloat(t, 64)
				}
			}
			out = append(out, m)
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || len(out) == 0 || !strings.HasSuffix(key, keySuffix) {
			continue
		}
		// ParseFloat accepts "-inf" and "nan", which callers handle.
		if v, err := strconv.ParseFloat(val, 64); err == nil {
			out[len(out)-1].value = v
		}
	}
	return out
}

// speechScore returns the coefficient of variation of the spectral centroid
// across voiced measurements, and whether enough of the window is voiced.
// A centroid counts as voiced when the loudness at the middle of its
// analysis window is near the window's speech level.
func speechScore(env envelope) (cv float64, voiced bool) {
	if len(env.levels) == 0 || len(env.centroids) == 0 {
		return 0, false
	}
	peak := percentile(env.levels, peakPct)
	var hz []float64
	for _, c := range env.centroids {
		i := int((c.at + centroidWinSecs/2) / frameSecs)
		if i >= len(env.levels) {
			i = len(env.levels) - 1
		}
		if env.levels[i] > peak-voicedRangeDB {
			hz = append(hz, c.hz)
		}
	}
	if float64(len(hz)) < minVoicedFrac*float64(len(env.centroids)) {
		return 0, false
	}
	var sum, sq float64
	for _, h := range hz {
		sum += h
	}
	mean := sum / float64(len(hz))
	for _, h := range hz {
		sq += (h - mean) * (h - mean)
	}
	return math.Sqrt(sq/float64(len(hz))) / mean, true
}

// findPieces splits a window at its pauses and returns the word/phrase-sized
// spans that are bounded by a pause on both sides, tagged with their Role.
// levels are 20 ms loudness frames.
func findPieces(levels []float64) []span {
	if len(levels) == 0 {
		return nil
	}
	floor := percentile(levels, floorPct)
	peak := percentile(levels, peakPct)
	if peak-floor < minRangeDB {
		return nil
	}
	threshold := floor + pauseFrac*(peak-floor)
	minPause := int(math.Round(minPauseSecs / frameSecs))
	longPause := int(math.Round(longPauseSecs / frameSecs))

	// Collect pauses as [startFrame, endFrame) runs below threshold.
	type run struct{ start, end int }
	var pauses []run
	runStart := -1
	for i, l := range levels {
		quiet := l < threshold
		switch {
		case quiet && runStart < 0:
			runStart = i
		case !quiet && runStart >= 0:
			if i-runStart >= minPause {
				pauses = append(pauses, run{runStart, i})
			}
			runStart = -1
		}
	}
	if runStart >= 0 && len(levels)-runStart >= minPause {
		pauses = append(pauses, run{runStart, len(levels)})
	}

	var pieces []span
	for i := 1; i < len(pauses); i++ {
		prev, next := pauses[i-1], pauses[i]
		s := span{
			start: float64(prev.end) * frameSecs,
			end:   float64(next.start) * frameSecs,
		}
		if s.dur() < minPieceSecs || s.dur() > maxPieceSecs {
			continue
		}
		opens := prev.end-prev.start >= longPause
		ends := next.end-next.start >= longPause
		switch {
		case opens && ends:
			s.role = RoleWhole
		case opens:
			s.role = RoleOpening
		case ends:
			s.role = RoleEnding
		}
		// Pad into the pauses, but never past their midpoints so adjacent
		// pieces can't overlap.
		s.start -= math.Min(piecePadSecs, float64(prev.end-prev.start)*frameSecs/2)
		s.end += math.Min(piecePadSecs, float64(next.end-next.start)*frameSecs/2)
		pieces = append(pieces, s)
	}
	return pieces
}

// percentile returns the p-th percentile (0–1) of values.
func percentile(values []float64, p float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[int(p*float64(len(sorted)-1))]
}
