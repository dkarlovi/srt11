package commands

import (
	"math"
	"sort"
)

// Take fit: fitting audio.cpp takes to their cue windows after synthesis,
// for models that ignore duration_factor (Higgs). Ported from the POC's
// fit_v2.py/fit_v3.py (~/audio.cpp-bin/poc/multilang): make several takes,
// cut each at its ASR word timings, tighten its pauses, keep the best one,
// and speed it up by 8 % at most. Experimental -- see
// docs/superpowers/specs/2026-10-21-higgs-take-fit-design.md.
//
// Everything here is pure: samples are float64 in [-1, 1] at the take's own
// rate. audiocpp.go does the HTTP and the WAV I/O.

const (
	takeFitLeadMargin       = 0.060 // s before the first ASR word
	takeFitTailMargin       = 0.100 // s after the last ASR word
	takeFitMinCut           = 0.300 // s; a shorter cut keeps the whole take
	takeFitEdgeThresholdDb  = -40.0
	takeFitEdgePad          = 0.030 // s kept after trimming edge silence
	takeFitPauseThresholdDb = -42.0
	takeFitMaxPause         = 0.350 // s, longest inner pause kept
	takeFitPauseXfade       = 0.010 // s
	takeFitFrame            = 0.010 // s, analysis frame for the above
	takeFitFade             = 0.040 // s
	takeFitLoudFade         = 0.005 // s, never fade far into voice energy
	takeFitLoudEdgeDb       = 10.0  // edge this far above the noise floor counts as loud
	takeFitVersion          = "takefit-v1"
)

// asrWord is one word from /v1/audio/transcriptions/details, with sample
// positions at the take's own rate.
type asrWord struct {
	Word       string
	Start, End int
}

// frameDb returns the RMS level in dBFS of each whole, non-overlapping
// frame of x.
func frameDb(x []float64, frame int) []float64 {
	n := len(x) / frame
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for _, v := range x[i*frame : (i+1)*frame] {
			sum += v * v
		}
		out[i] = 20 * math.Log10(math.Sqrt(sum/float64(frame))+1e-9)
	}
	return out
}

// cleanTake cuts x at its word timings, trims edge silence and shortens
// long inner pauses, in that order (spec, data flow step 3).
func cleanTake(x []float64, rate int, words []asrWord) []float64 {
	if len(words) > 0 {
		a := max(0, words[0].Start-int(takeFitLeadMargin*float64(rate)))
		b := min(len(x), words[len(words)-1].End+int(takeFitTailMargin*float64(rate)))
		if b-a >= int(takeFitMinCut*float64(rate)) {
			x = x[a:b]
		}
	}
	x = trimEdgeSilence(x, rate)
	return shortenPauses(x, rate)
}

func trimEdgeSilence(x []float64, rate int) []float64 {
	frame := int(takeFitFrame * float64(rate))
	db := frameDb(x, frame)
	first, last := -1, -1
	for i, d := range db {
		if d >= takeFitEdgeThresholdDb {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		// Nothing above the threshold: keep the take rather than return
		// nothing; selection will score it down.
		return x
	}
	pad := int(takeFitEdgePad * float64(rate))
	a := max(0, first*frame-pad)
	b := min(len(x), (last+1)*frame+pad)
	if last == len(db)-1 {
		b = len(x) // keep the partial frame at the end
	}
	return x[a:b]
}

// shortenPauses keeps the first and last takeFitMaxPause/2 of every quiet
// run longer than takeFitMaxPause that doesn't touch an edge, joined with a
// short crossfade, so the take keeps its own room noise (fit_v3's
// shorten_pauses).
func shortenPauses(x []float64, rate int) []float64 {
	frame := int(takeFitFrame * float64(rate))
	db := frameDb(x, frame)
	keep := int(takeFitMaxPause / 2 * float64(rate))
	xf := int(takeFitPauseXfade * float64(rate))
	maxFrames := int(math.Round(takeFitMaxPause / takeFitFrame))

	var out []float64
	pos := 0 // samples of x already copied to out
	for i := 0; i < len(db); {
		j := i
		for j < len(db) && (db[j] < takeFitPauseThresholdDb) == (db[i] < takeFitPauseThresholdDb) {
			j++
		}
		if db[i] < takeFitPauseThresholdDb && i > 0 && j < len(db) && j-i > maxFrames {
			a, b := i*frame, j*frame
			out = append(out, x[pos:a+keep]...)
			// Crossfade the end of the kept head into the start of the
			// kept tail.
			tail := x[b-keep : b]
			head := out[len(out)-xf:]
			for k := 0; k < xf; k++ {
				r := float64(k) / float64(xf)
				head[k] = head[k]*(1-r) + tail[k]*r
			}
			out = append(out, tail[xf:]...)
			pos = b
		}
		i = j
	}
	return append(out, x[pos:]...)
}

// fadeEdges fades both ends of a copy of x: over takeFitLoudFade where the
// edge is loud (well above the take's noise floor), so a fade never eats
// into a word ("novità"), else over takeFitFade (spec step 8).
func fadeEdges(x []float64, rate int) []float64 {
	out := append([]float64(nil), x...)
	floorDb := percentile(frameDb(x, int(0.05*float64(rate))), 10)
	edge := int(takeFitFade * float64(rate))
	for side := 0; side < 2; side++ {
		seg := out[:min(edge, len(out))]
		if side == 1 {
			seg = out[max(0, len(out)-edge):]
		}
		n := int(takeFitFade * float64(rate))
		if lv := frameDb(seg, max(1, len(seg))); len(lv) > 0 && lv[0] > floorDb+takeFitLoudEdgeDb {
			n = int(takeFitLoudFade * float64(rate))
		}
		n = min(n, len(out))
		for k := 0; k < n; k++ {
			g := float64(k) / float64(n)
			if side == 0 {
				out[k] *= g
			} else {
				out[len(out)-1-k] *= g
			}
		}
	}
	return out
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -180
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[int(p/100*float64(len(s)-1))]
}
