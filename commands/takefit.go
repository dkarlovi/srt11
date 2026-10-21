package commands

import (
	"math"
	"sort"
	"time"
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
	takeFitVersion          = "takefit-v2"
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

const (
	takeFitMaxSpeedup = 1.08  // largest wsola factor; Matko heard 15-24 % clearly, 8 % is fine
	wsolaFrame        = 0.030 // s
	wsolaSearch       = 0.010 // s, ± around the nominal analysis position
	wsolaMinWindowSum = 1e-6
)

// wsola shortens x by factor f (> 1 is faster) without changing its pitch:
// waveform-similarity overlap-add. Each 30 ms Hann frame is taken from
// around its nominal position (k * hop * f), shifted by up to ±10 ms to the
// offset that best continues the previous frame, and overlap-added at
// k * hop. This is what the POC got from ffmpeg's atempo, which this binary
// can't call. f <= 1, or x shorter than one frame, returns x unchanged.
func wsola(x []float64, rate int, f float64) []float64 {
	n := int(wsolaFrame * float64(rate))
	if f <= 1 || len(x) < n {
		return x
	}
	hs := n / 2
	tol := int(wsolaSearch * float64(rate))
	outLen := int(float64(len(x)) / f)

	win := make([]float64, n)
	for i := range win {
		win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	out := make([]float64, outLen+n)
	wsum := make([]float64, outLen+n)

	prev := 0
	for k := 0; k*hs < outLen; k++ {
		c := 0
		if k > 0 {
			nominal := int(math.Round(float64(k*hs) * f))
			c = bestContinuation(x, prev+hs, nominal, tol, hs, n)
			if c < 0 {
				break
			}
		}
		for i := 0; i < n && c+i < len(x); i++ {
			out[k*hs+i] += x[c+i] * win[i]
			wsum[k*hs+i] += win[i]
		}
		prev = c
	}
	for i := range out {
		if wsum[i] > wsolaMinWindowSum {
			out[i] /= wsum[i]
		}
	}
	return out[:outLen]
}

// bestContinuation returns the frame start within ±tol of nominal whose
// first overlap samples best match x[natural:] (normalized
// cross-correlation), or -1 when no full frame fits in x any more.
func bestContinuation(x []float64, natural, nominal, tol, overlap, n int) int {
	lo, hi := max(0, nominal-tol), min(len(x)-n, nominal+tol)
	if lo > hi {
		return -1
	}
	if natural+overlap > len(x) {
		return lo
	}
	ref := x[natural : natural+overlap]
	best, bestScore := lo, math.Inf(-1)
	for c := lo; c <= hi; c++ {
		var dot, energy float64
		for i, r := range ref {
			v := x[c+i]
			dot += v * r
			energy += v * v
		}
		if score := dot / math.Sqrt(energy+1e-12); score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

const (
	takeFitGap         = 120 * time.Millisecond // kept clear before the next cue
	takeFitLastCueRoom = time.Second            // room after the last cue's window
	takeFitGoodWER     = 0.10
)

// takeRoom is how long a take may run at natural speed: its window plus
// the pause after it, up to takeFitGap before the next cue of any speaker
// starts, but never less than the window itself. next is nil for the last
// cue.
func takeRoom(start, end time.Duration, next *time.Duration) time.Duration {
	window := end - start
	if next == nil {
		return window + takeFitLastCueRoom
	}
	return max(window, *next-takeFitGap-start)
}

type takeCandidate struct {
	Seed int
	Dur  time.Duration
	WER  float64
}

// selectTake picks a take the way fit_v2 did: among the good ones (WER at
// most takeFitGoodWER, or else the two with the lowest WER), the one whose
// length is nearest the window; ties go to the lower WER, then the lower
// seed. Returns an index into cands, or -1 if there are none.
func selectTake(cands []takeCandidate, window time.Duration) int {
	if len(cands) == 0 {
		return -1
	}
	idx := make([]int, 0, len(cands))
	for i, c := range cands {
		if c.WER <= takeFitGoodWER {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		idx = make([]int, len(cands))
		for i := range cands {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return cands[idx[a]].WER < cands[idx[b]].WER })
		idx = idx[:min(2, len(idx))]
	}
	lengthKey := func(c takeCandidate) float64 {
		return math.Round(math.Abs(c.Dur.Seconds()/window.Seconds()-1)*100) / 100
	}
	best := idx[0]
	for _, i := range idx[1:] {
		a, b := cands[i], cands[best]
		switch {
		case lengthKey(a) != lengthKey(b):
			if lengthKey(a) < lengthKey(b) {
				best = i
			}
		case a.WER != b.WER:
			if a.WER < b.WER {
				best = i
			}
		case a.Seed < b.Seed:
			best = i
		}
	}
	return best
}
