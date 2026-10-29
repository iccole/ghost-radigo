package processor

import (
	"math"
	"strings"
	"testing"
)

// levels builds a 20 ms loudness sequence from (seconds, dB) segments.
func levels(segs ...[2]float64) []float64 {
	var out []float64
	for _, s := range segs {
		n := int(math.Round(s[0] / frameSecs))
		for i := 0; i < n; i++ {
			out = append(out, s[1])
		}
	}
	return out
}

// centroids builds measurements on aspectralstats' 64 ms hop from
// (seconds, Hz) segments.
func centroids(segs ...[2]float64) []centroid {
	const hop = centroidWinSecs / 2
	var out []centroid
	at := 0.0
	for _, s := range segs {
		for end := at + s[0]; at < end-1e-9; at += hop {
			out = append(out, centroid{at: at, hz: s[1]})
		}
	}
	return out
}

func TestParseMetadata(t *testing.T) {
	out := strings.NewReader(`frame:0    pts:0       pts_time:0
lavfi.astats.Overall.RMS_level=-inf
frame:1    pts:1024    pts_time:0.064
lavfi.aspectralstats.1.centroid=1458.03
lavfi.astats.Overall.RMS_level=-21.5
frame:2    pts:2048    pts_time:0.128
`)
	got := parseMetadata(out, ".RMS_level")
	if len(got) != 3 {
		t.Fatalf("got %d measurements, want 3", len(got))
	}
	if !math.IsInf(got[0].value, -1) {
		t.Errorf("frame 0 = %v, want -inf", got[0].value)
	}
	if got[1].at != 0.064 || got[1].value != -21.5 {
		t.Errorf("frame 1 = %+v, want {at:0.064 value:-21.5}", got[1])
	}
	if !math.IsNaN(got[2].value) {
		t.Errorf("frame 2 = %v, want NaN for a missing key", got[2].value)
	}
}

func TestFindPieces(t *testing.T) {
	// edge word | pause | word 0.5s | pause | phrase 1.2s | pause | too long | pause | edge
	got := findPieces(levels(
		[2]float64{0.3, -20},
		[2]float64{0.2, -50},
		[2]float64{0.5, -20},
		[2]float64{0.3, -50},
		[2]float64{1.2, -20},
		[2]float64{0.2, -50},
		[2]float64{3.0, -20},
		[2]float64{0.2, -50},
		[2]float64{0.4, -20},
	))
	want := []span{
		{start: 0.5 - piecePadSecs, end: 1.0 + piecePadSecs, role: RoleMiddle},
		{start: 1.3 - piecePadSecs, end: 2.5 + piecePadSecs, role: RoleMiddle},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pieces %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if math.Abs(got[i].start-want[i].start) > 1e-9 || math.Abs(got[i].end-want[i].end) > 1e-9 ||
			got[i].role != want[i].role {
			t.Errorf("piece %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFindPiecesRoles(t *testing.T) {
	// edge | long | A | short | B | short | C | long | D | long | edge
	got := findPieces(levels(
		[2]float64{0.3, -20},
		[2]float64{0.5, -50},
		[2]float64{0.4, -20}, // A: opening
		[2]float64{0.2, -50},
		[2]float64{0.4, -20}, // B: middle
		[2]float64{0.2, -50},
		[2]float64{0.4, -20}, // C: ending
		[2]float64{0.6, -50},
		[2]float64{0.8, -20}, // D: whole
		[2]float64{0.5, -50},
		[2]float64{0.3, -20},
	))
	want := []Role{RoleOpening, RoleMiddle, RoleEnding, RoleWhole}
	if len(got) != len(want) {
		t.Fatalf("got %d pieces %+v, want %d", len(got), got, len(want))
	}
	for i, r := range want {
		if got[i].role != r {
			t.Errorf("piece %d role = %v, want %v", i, got[i].role, r)
		}
	}
}

func TestFindPiecesIgnoresShortDips(t *testing.T) {
	// A 60 ms dip is inside a word, not a pause between words.
	got := findPieces(levels(
		[2]float64{0.2, -50},
		[2]float64{0.4, -20},
		[2]float64{0.06, -50},
		[2]float64{0.4, -20},
		[2]float64{0.2, -50},
	))
	if len(got) != 1 || math.Abs(got[0].dur()-(0.86+2*piecePadSecs)) > 1e-9 {
		t.Fatalf("got %+v, want one 0.86 s piece plus padding", got)
	}
}

func TestFindPiecesFlatSignal(t *testing.T) {
	// Wall-to-wall sound (dense music, constant hiss) has no usable pauses.
	if got := findPieces(levels([2]float64{12, -20})); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestSpeechScore(t *testing.T) {
	loud := levels([2]float64{4, -20})
	steady := envelope{loud, centroids([2]float64{2, 1000}, [2]float64{2, 1050})}
	moving := envelope{loud, centroids(
		[2]float64{0.5, 600}, [2]float64{0.5, 2400},
		[2]float64{0.5, 900}, [2]float64{0.5, 1800},
		[2]float64{0.5, 600}, [2]float64{0.5, 2400},
		[2]float64{0.5, 900}, [2]float64{0.5, 1800},
	)}
	// Bright, varied timbre, but only during the one loud second.
	mostlySilent := envelope{
		levels([2]float64{1, -20}, [2]float64{3, -120}),
		centroids([2]float64{4, 1000}),
	}

	if cv, voiced := speechScore(steady); !voiced || cv >= minCentroidCV {
		t.Errorf("steady timbre: cv=%.2f voiced=%v, want voiced and below %.2f", cv, voiced, minCentroidCV)
	}
	if cv, voiced := speechScore(moving); !voiced || cv < minCentroidCV {
		t.Errorf("moving timbre: cv=%.2f voiced=%v, want voiced and at least %.2f", cv, voiced, minCentroidCV)
	}
	if _, voiced := speechScore(envelope{levels: levels([2]float64{4, -120})}); voiced {
		t.Error("window with no centroids reported as voiced")
	}
	if _, voiced := speechScore(mostlySilent); !voiced {
		t.Error("a quarter-voiced window should still count as voiced")
	}
}
