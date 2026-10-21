package commands

import (
	"math"
	"testing"
	"time"
)

const tfRate = 24000

func tfSec(s float64) int { return int(s * tfRate) }

// tfVoice is a 220 Hz sine at amplitude 0.3: loud enough to count as speech.
func tfVoice(sec float64) []float64 {
	x := make([]float64, tfSec(sec))
	for i := range x {
		x[i] = 0.3 * math.Sin(2*math.Pi*220*float64(i)/tfRate)
	}
	return x
}

func tfConst(sec, v float64) []float64 {
	x := make([]float64, tfSec(sec))
	for i := range x {
		x[i] = v
	}
	return x
}

func tfJoin(parts ...[]float64) []float64 {
	var out []float64
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func tfAssertSec(t *testing.T, name string, x []float64, want, tol float64) {
	t.Helper()
	got := float64(len(x)) / tfRate
	if math.Abs(got-want) > tol {
		t.Errorf("%s: length %.3fs, want %.3fs ± %.3fs", name, got, want, tol)
	}
}

func TestCleanTake_CutsAtWordMargins(t *testing.T) {
	x := tfJoin(tfConst(1, 0), tfVoice(1), tfConst(1, 0))
	out := cleanTake(x, tfRate, []asrWord{{Word: "hi", Start: tfSec(1), End: tfSec(2)}})
	// Cut 0.94-2.10 s, then the silent margins are trimmed down to 30 ms pads.
	tfAssertSec(t, "cleaned", out, 1.06, 0.02)
}

func TestCleanTake_ShortCutKeepsWholeTake(t *testing.T) {
	x := tfVoice(0.5)
	out := cleanTake(x, tfRate, []asrWord{{Word: "hi", Start: 0, End: tfSec(0.1)}})
	tfAssertSec(t, "cleaned", out, 0.5, 0.02)
}

func TestCleanTake_ShortensLongInnerPause(t *testing.T) {
	x := tfJoin(tfVoice(0.5), tfConst(1, 0), tfVoice(0.5))
	out := cleanTake(x, tfRate, []asrWord{{Word: "a", Start: 0, End: tfSec(2)}})
	tfAssertSec(t, "cleaned", out, 1.35, 0.02)
}

func TestCleanTake_KeepsShortInnerPause(t *testing.T) {
	x := tfJoin(tfVoice(0.5), tfConst(0.2, 0), tfVoice(0.5))
	out := cleanTake(x, tfRate, []asrWord{{Word: "a", Start: 0, End: tfSec(1.2)}})
	tfAssertSec(t, "cleaned", out, 1.2, 0.02)
}

func TestCleanTake_NoWordsSilentTake(t *testing.T) {
	out := cleanTake(tfConst(0.5, 0), tfRate, nil)
	if len(out) == 0 {
		t.Fatal("want a non-empty take")
	}
}

func TestFadeEdges(t *testing.T) {
	ms := func(m float64) int { return int(m * tfRate / 1000) }

	t.Run("loud edges get a 5 ms fade", func(t *testing.T) {
		x := tfJoin(tfConst(0.1, 0.3), tfConst(0.4, 0.001), tfConst(0.1, 0.3))
		out := fadeEdges(x, tfRate)
		if out[0] != 0 || out[len(out)-1] != 0 {
			t.Errorf("edges = %g, %g; want 0", out[0], out[len(out)-1])
		}
		if out[ms(5)] < 0.9*0.3 || out[len(out)-1-ms(5)] < 0.9*0.3 {
			t.Errorf("want full level 5 ms in, got %g / %g", out[ms(5)], out[len(out)-1-ms(5)])
		}
		if x[0] != 0.3 {
			t.Error("fadeEdges must not modify its input")
		}
	})

	t.Run("quiet edges get a 40 ms fade", func(t *testing.T) {
		x := tfJoin(tfConst(0.1, 0.001), tfConst(0.4, 0.3), tfConst(0.1, 0.001))
		out := fadeEdges(x, tfRate)
		if v := out[ms(20)]; v > 0.6*0.001 || v < 0.4*0.001 {
			t.Errorf("20 ms into a 40 ms fade: %g, want about half of 0.001", v)
		}
		if out[ms(45)] != 0.001 {
			t.Errorf("past the fade: %g, want 0.001", out[ms(45)])
		}
	})
}

func TestWSOLA_IdentityAtOne(t *testing.T) {
	x := tfVoice(0.5)
	for _, f := range []float64{1.0, 0.95} {
		out := wsola(x, tfRate, f)
		if len(out) != len(x) || out[100] != x[100] {
			t.Errorf("f=%g: want the input unchanged", f)
		}
	}
}

func TestWSOLA_Length(t *testing.T) {
	x := tfVoice(2)
	out := wsola(x, tfRate, 1.08)
	want := float64(len(x)) / 1.08
	if math.Abs(float64(len(out))-want)/want > 0.01 {
		t.Errorf("len = %d, want %.0f ± 1%%", len(out), want)
	}
}

func TestWSOLA_PreservesPitch(t *testing.T) {
	out := wsola(tfVoice(2), tfRate, 1.08)
	mid := out[len(out)/2-tfRate/2 : len(out)/2+tfRate/2]
	crossings := 0
	for i := 1; i < len(mid); i++ {
		if (mid[i-1] < 0) != (mid[i] < 0) {
			crossings++
		}
	}
	if hz := float64(crossings) / 2; math.Abs(hz-220)/220 > 0.02 {
		t.Errorf("pitch = %.1f Hz, want 220 Hz ± 2%%", hz)
	}
}

func TestWSOLA_ShorterThanFrame(t *testing.T) {
	x := tfVoice(0.01)
	if out := wsola(x, tfRate, 1.08); len(out) != len(x) {
		t.Errorf("len = %d, want the %d-sample input unchanged", len(out), len(x))
	}
}

func TestWSOLA_KeepsLevelToTheEnd(t *testing.T) {
	out := wsola(tfVoice(2), tfRate, 1.08)
	for _, part := range []struct {
		name string
		x    []float64
	}{{"middle", out[len(out)/2 : len(out)/2+tfSec(0.1)]}, {"last 50 ms", out[len(out)-tfSec(0.05):]}} {
		if db := frameDb(part.x, len(part.x))[0]; math.Abs(db-(-13.5)) > 1.5 {
			t.Errorf("%s: %.1f dBFS, want the input's -13.5 ± 1.5", part.name, db)
		}
	}
}

func TestTakeRoom(t *testing.T) {
	d := func(s float64) *time.Duration { v := time.Duration(s * float64(time.Second)); return &v }
	for _, tc := range []struct {
		name string
		next *time.Duration
		want time.Duration
	}{
		{"pause after the window", d(4), 3880 * time.Millisecond},
		{"next cue abuts", d(3), 3 * time.Second},
		{"next cue overlaps", d(2.5), 3 * time.Second},
		{"last cue", nil, 4 * time.Second},
	} {
		if got := takeRoom(0, 3*time.Second, tc.next); got != tc.want {
			t.Errorf("%s: room = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSelectTake(t *testing.T) {
	s := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
	window := 3 * time.Second

	t.Run("nearest length among good takes", func(t *testing.T) {
		got := selectTake([]takeCandidate{{1, s(3.5), 0}, {2, s(3.1), 0.05}, {3, s(3.0), 0.3}}, window)
		if got != 1 {
			t.Errorf("got %d, want 1 (take 3 is nearer but not good)", got)
		}
	})
	t.Run("no good take: best two by WER, then nearest length", func(t *testing.T) {
		got := selectTake([]takeCandidate{{1, s(3.0), 0.5}, {2, s(4.0), 0.2}, {3, s(3.2), 0.3}}, window)
		if got != 2 {
			t.Errorf("got %d, want 2", got)
		}
	})
	t.Run("length tie: lower WER, then lower seed", func(t *testing.T) {
		got := selectTake([]takeCandidate{{3, s(3.1), 0}, {1, s(2.9), 0.05}, {2, s(3.1), 0}}, window)
		if got != 2 {
			t.Errorf("got %d, want 2 (WER 0, seed 2)", got)
		}
	})
	t.Run("no takes", func(t *testing.T) {
		if got := selectTake(nil, window); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})
}
