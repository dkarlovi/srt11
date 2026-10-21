package commands

import (
	"strings"
	"testing"
)

// Mirrors dub-studio's tests/test_scheduling.py.

func autoFixCue(index, startMs, endMs int, voice string, audioMs *int) *AutoFixCue {
	return &AutoFixCue{Index: index, StartMs: startMs, EndMs: endMs, Voice: voice, Speed: 1.0, AudioMs: audioMs}
}

func ms(v int) *int { return &v }

var testSpeedCaps = map[string]float64{"hana": 1.15, "default": 1.2}

func defaultAutoFixConfig() AutoFixConfig {
	return AutoFixConfig{ToleranceMs: 120, AutofixMarginMs: 40, SpeedCaps: testSpeedCaps}
}

func TestRealBudgetsMs(t *testing.T) {
	t.Run("budget is time until the same voice speaks again", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "hana", nil),
			autoFixCue(2, 2000, 4000, "matko", nil),
			autoFixCue(3, 6000, 8000, "hana", nil),
		}
		budgets := realBudgetsMs(cues)
		want := min(6000-0, 2000+MaxDriftMs)
		if budgets[1] != want {
			t.Errorf("budgets[1] = %d, want %d", budgets[1], want)
		}
	})

	t.Run("budget falls back to window plus drift cap with no later same voice cue", func(t *testing.T) {
		cues := []*AutoFixCue{autoFixCue(1, 0, 2000, "hana", nil)}
		budgets := realBudgetsMs(cues)
		if want := 2000 + MaxDriftMs; budgets[1] != want {
			t.Errorf("budgets[1] = %d, want %d", budgets[1], want)
		}
	})

	t.Run("cross voice neighbor does not constrain the budget", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "hana", nil),
			autoFixCue(2, 2000, 2500, "matko", nil),
			autoFixCue(3, 2500, 4500, "hana", nil),
			autoFixCue(4, 9000, 11000, "matko", nil),
		}
		budgets := realBudgetsMs(cues)
		want := min(9000-2000, 500+MaxDriftMs)
		if budgets[2] != want {
			t.Errorf("budgets[2] = %d, want %d", budgets[2], want)
		}
	})
}

func TestAutoFixDurations(t *testing.T) {
	t.Run("a cue within its real budget is left untouched", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "hana", ms(2300)),
			autoFixCue(2, 2200, 4000, "hana", ms(1500)),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())
		if len(result.Sped) != 0 || len(result.Basketed) != 0 {
			t.Fatalf("want no sped/basketed, got %+v", result)
		}
		if cues[0].NeedsHuman || cues[0].Speed != 1.0 {
			t.Errorf("cue 1 = %+v, want untouched", cues[0])
		}
	})

	t.Run("a moderate overage gets sped up within the speaker cap", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "hana", ms(2350)),
			autoFixCue(2, 2200, 4000, "hana", ms(1500)),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())
		if len(result.Sped) != 1 || result.Sped[0].Index != 1 || result.Sped[0].From != 1.0 || result.Sped[0].To != 1.09 {
			t.Fatalf("sped = %+v, want [{1 1.0 1.09}]", result.Sped)
		}
		if cues[0].Speed != 1.09 || !cues[0].SpeedOverride || cues[0].NeedsHuman {
			t.Errorf("cue 1 = %+v", cues[0])
		}
	})

	t.Run("an audiocpp cue over budget is basketed, never speed-bumped -- that provider has no speed lever", func(t *testing.T) {
		c1 := autoFixCue(1, 0, 2000, "hana", ms(2350)) // same overage as the "moderate overage" case above, which sped it up for elevenlabs
		c1.Provider = "audiocpp"
		c2 := autoFixCue(2, 2200, 4000, "hana", ms(1500))
		result := AutoFixDurations([]*AutoFixCue{c1, c2}, defaultAutoFixConfig())
		if len(result.Sped) != 0 {
			t.Fatalf("want no speed bump for an audiocpp cue, got %+v", result.Sped)
		}
		if len(result.Basketed) != 1 || result.Basketed[0].Index != 1 {
			t.Fatalf("want cue 1 basketed, got %+v", result)
		}
		if c1.Speed != 1.0 || c1.SpeedOverride {
			t.Errorf("cue 1 speed = %+v, want untouched (audio.cpp ignores it)", c1)
		}
		if !strings.Contains(c1.HumanReason, "clamped") {
			t.Errorf("human_reason = %q, want it to explain the fit was clamped, not mention a speed cap", c1.HumanReason)
		}
	})

	t.Run("an overage past the speaker cap is basketed not sped", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "hana", ms(2500)),
			autoFixCue(2, 2200, 4000, "hana", ms(1500)),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())
		if len(result.Sped) != 0 || len(result.Basketed) != 1 || result.Basketed[0].Index != 1 {
			t.Fatalf("want 1 basketed cue 1, got %+v", result)
		}
		if !cues[0].NeedsHuman {
			t.Error("want needs_human")
		}
		if want := "1.15x cap for hana"; !strings.Contains(cues[0].HumanReason, want) {
			t.Errorf("human_reason = %q, want it to contain %q", cues[0].HumanReason, want)
		}
		if cues[0].Speed != 1.0 {
			t.Errorf("speed = %v, want untouched at 1.0", cues[0].Speed)
		}
	})

	t.Run("a skipped cue is left completely untouched, even one that would otherwise be basketed", func(t *testing.T) {
		c1 := autoFixCue(1, 0, 2000, "hana", ms(2500))
		c1.Skipped = true
		c1.NeedsHuman = true
		c1.HumanReason = "previously basketed, before being skipped"
		c2 := autoFixCue(2, 2200, 4000, "hana", ms(1500))
		result := AutoFixDurations([]*AutoFixCue{c1, c2}, defaultAutoFixConfig())
		if len(result.Basketed) != 0 || len(result.Sped) != 0 {
			t.Fatalf("want no fresh sped/basketed entries for the skipped cue, got %+v", result)
		}
		if c1.Speed != 1.0 || !c1.NeedsHuman || c1.HumanReason != "previously basketed, before being skipped" {
			t.Errorf("skipped cue = %+v, want its prior state left exactly as it was", c1)
		}
	})

	t.Run("engine flagged overlap is named in the basket reason", func(t *testing.T) {
		c1 := autoFixCue(1, 0, 2000, "hana", ms(2500))
		c1.OverlapFlagged = true
		c2 := autoFixCue(2, 2200, 4000, "hana", ms(1500))
		result := AutoFixDurations([]*AutoFixCue{c1, c2}, defaultAutoFixConfig())
		if len(result.Basketed) != 1 {
			t.Fatalf("want 1 basketed, got %+v", result.Basketed)
		}
		if !strings.Contains(c1.HumanReason, "Overlaps the next line") || !strings.Contains(c1.HumanReason, "past the available time") {
			t.Errorf("human_reason = %q", c1.HumanReason)
		}
	})

	t.Run("a cue with no generated audio yet is never touched", func(t *testing.T) {
		c := autoFixCue(1, 0, 100, "hana", nil)
		result := AutoFixDurations([]*AutoFixCue{c}, defaultAutoFixConfig())
		if len(result.Sped) != 0 || len(result.Basketed) != 0 || len(result.Retimed) != 0 || result.StillOpen != 0 {
			t.Fatalf("want all-empty result, got %+v", result)
		}
		if c.NeedsHuman {
			t.Error("want needs_human false")
		}
	})

	t.Run("matko uses the default speed cap not hanas", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 2000, "matko", ms(2500)),
			autoFixCue(2, 2200, 4000, "matko", ms(1500)),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())
		if len(result.Basketed) != 0 {
			t.Fatalf("want no basketed, got %+v", result.Basketed)
		}
		if len(result.Sped) != 1 {
			t.Fatalf("want 1 sped, got %+v", result.Sped)
		}
		if cues[0].Speed > testSpeedCaps["default"] {
			t.Errorf("speed = %v, want <= %v", cues[0].Speed, testSpeedCaps["default"])
		}
	})
}

func TestLeadingGapAbsorption(t *testing.T) {
	t.Run("a gap that fully covers the overage resolves it with no speed change", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 1000, "matko", ms(800)),
			autoFixCue(2, 1500, 1960, "hana", ms(810)),
			autoFixCue(3, 2100, 4100, "hana", nil),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())

		if len(result.Retimed) != 1 || result.Retimed[0].Index != 2 || result.Retimed[0].ShiftedMs != 210 {
			t.Fatalf("retimed = %+v, want [{2 210}]", result.Retimed)
		}
		if cues[1].StartMs != 1290 {
			t.Errorf("start_ms = %d, want 1290", cues[1].StartMs)
		}
		if len(result.Sped) != 0 || len(result.Basketed) != 0 || cues[1].NeedsHuman {
			t.Fatalf("want no sped/basketed/needs_human, got %+v", result)
		}
	})

	t.Run("a gap that only partly covers the overage reduces the needed speed bump", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 1000, "matko", ms(900)),
			autoFixCue(2, 1300, 5000, "hana", ms(7500)),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())

		if len(result.Retimed) != 1 || result.Retimed[0].Index != 2 || result.Retimed[0].ShiftedMs != 400 {
			t.Fatalf("retimed = %+v, want [{2 400}]", result.Retimed)
		}
		if cues[1].StartMs != 900 {
			t.Errorf("start_ms = %d, want 900", cues[1].StartMs)
		}
		if len(result.Sped) != 1 || result.Sped[0].Index != 2 || result.Sped[0].From != 1.0 || result.Sped[0].To != 1.06 {
			t.Fatalf("sped = %+v, want [{2 1.0 1.06}]", result.Sped)
		}
		if len(result.Basketed) != 0 {
			t.Fatalf("want no basketed, got %+v", result.Basketed)
		}
	})

	t.Run("the shift never exceeds the lead shift cap even with more gap available", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 1000, "matko", ms(500)),
			autoFixCue(2, 10000, 10100, "hana", ms(6000)),
		}
		AutoFixDurations(cues, defaultAutoFixConfig())
		if want := 10000 - MaxLeadShiftMs; cues[1].StartMs != want {
			t.Errorf("start_ms = %d, want %d", cues[1].StartMs, want)
		}
	})

	t.Run("a zero gap stranded tail still gets basketed not retimed", func(t *testing.T) {
		cues := []*AutoFixCue{
			autoFixCue(1, 0, 1000, "hana", ms(1000)),
			autoFixCue(2, 1000, 1140, "hana", ms(888)),
			autoFixCue(3, 1420, 3000, "hana", nil),
		}
		result := AutoFixDurations(cues, defaultAutoFixConfig())
		if len(result.Retimed) != 0 {
			t.Fatalf("want no retimed, got %+v", result.Retimed)
		}
		if cues[1].StartMs != 1000 {
			t.Errorf("start_ms = %d, want 1000", cues[1].StartMs)
		}
		if len(result.Basketed) != 1 || result.Basketed[0].Index != 2 {
			t.Fatalf("basketed = %+v, want [{2 ...}]", result.Basketed)
		}
	})
}
