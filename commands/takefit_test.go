package commands

import (
	"math"
	"testing"
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
