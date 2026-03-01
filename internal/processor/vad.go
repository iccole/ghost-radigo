package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// vadThreshold is the minimum voice-band score [0,1] a clip must reach to
	// be played. 0.30 is deliberately loose — we want phoneme fragments and
	// partial syllables, not just clear intelligible speech.
	vadThreshold = 0.30

	// vadSilenceFloor is the dB reference for an effectively silent signal.
	// Scores are linearly mapped from [vadSilenceFloor, 0 dB] → [0, 1].
	vadSilenceFloor = -70.0

	// vadProbeSecs caps how much audio we download for the analysis pass.
	vadProbeSecs = 3.0

	vadProbeTimeout = 30 * time.Second
)

// errVADReject is returned when a clip fails the voice-band energy check.
// It is not a real error — callers should skip the clip silently.
var errVADReject = errors.New("vad: insufficient voice-band content")

// reMeanVol matches ffmpeg volumedetect output:
//
//	[Parsed_volumedetect_N @ 0x...] mean_volume: -32.5 dB
var reMeanVol = regexp.MustCompile(`mean_volume:\s*([-\d.]+)\s*dB`)

// vadScore estimates voice-band energy for a segment of a remote audio file.
// It runs a short ffmpeg probe: bandpass 80–3500 Hz then volumedetect, and
// maps the mean RMS level to a score in [0, 1].
//
// On probe failure the error is returned and the caller should decide whether
// to accept or reject the clip — failing open is preferred.
func vadScore(ctx context.Context, url string, offset, dur float64) (float64, error) {
	probe := dur
	if probe > vadProbeSecs {
		probe = vadProbeSecs
	}

	args := []string{
		// volumedetect output is emitted at INFO level, so we need -loglevel info.
		// All other ffmpeg chatter is collateral but the regex filters it out.
		"-loglevel", "info",
		"-ss", fmt.Sprintf("%.3f", offset),
		"-i", url,
		"-t", fmt.Sprintf("%.3f", probe),
		"-af", "highpass=f=80,lowpass=f=3500,volumedetect",
		"-f", "null", "-",
	}

	pCtx, cancel := context.WithTimeout(ctx, vadProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(pCtx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// HTTP auth failures (401/403) will also fail the encode — reject now
		// rather than wasting a second round-trip. Any other error (network
		// hiccup, unsupported format) fails open so we don't drop valid content.
		se := stderr.String()
		if strings.Contains(se, "401") || strings.Contains(se, "403") ||
			strings.Contains(se, "Unauthorized") || strings.Contains(se, "Forbidden") {
			return 0, nil // score 0 → below threshold → rejected cleanly
		}
		return 0, fmt.Errorf("vad probe: %w", err)
	}

	m := reMeanVol.FindSubmatch(stderr.Bytes())
	if m == nil {
		return 0, fmt.Errorf("vad: volumedetect output not found in ffmpeg stderr")
	}

	db, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return 0, fmt.Errorf("vad: parse dB %q: %w", m[1], err)
	}

	// Linear map: vadSilenceFloor → 0.0, 0 dB → 1.0
	// At threshold 0.30 this accepts signals above ~-49 dB in the voice band —
	// loose enough to capture breath, vowels, and degraded phoneme fragments.
	score := (db - vadSilenceFloor) / (-vadSilenceFloor)
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	return score, nil
}
