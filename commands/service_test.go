package commands

import (
	"errors"
	"fmt"
	"testing"
)

func testServiceConfig() ServiceConfig {
	return ServiceConfig{
		Engine:             uploadTestConfig(),
		OverlapToleranceMs: 0,
		AutoFix:            AutoFixConfig{ToleranceMs: 120, AutofixMarginMs: 40, SpeedCaps: testSpeedCaps},
	}
}

func TestLocalService_Cleanup(t *testing.T) {
	s := &Session{Cues: []*SessionCue{
		{Index: 1, CurrentText: "So, um, this is, you know, a test."},
		{Index: 2, CurrentText: "A perfectly clean sentence."},
	}}
	ls := NewLocalService(testServiceConfig())

	edits := ls.Cleanup(s)

	if len(edits) != 1 || edits[0].Index != 1 {
		t.Fatalf("edits = %+v, want 1 edit on cue 1", edits)
	}
	if s.Cues[0].CurrentText != "This is, a test." {
		t.Errorf("cue 1 current_text = %q", s.Cues[0].CurrentText)
	}
	if s.Cues[1].CurrentText != "A perfectly clean sentence." {
		t.Errorf("cue 2 current_text changed unexpectedly: %q", s.Cues[1].CurrentText)
	}
}

func TestLocalService_AutoFix(t *testing.T) {
	audioMs := 2350
	s := &Session{Cues: []*SessionCue{
		{Index: 1, StartMs: 0, EndMs: 2000, Voice: "hana", Speed: 1.0, AudioMs: &audioMs},
		{Index: 2, StartMs: 2200, EndMs: 4000, Voice: "hana", Speed: 1.0},
	}}
	ls := NewLocalService(testServiceConfig())

	result := ls.AutoFix(s)

	if len(result.Sped) != 1 || result.Sped[0].Index != 1 {
		t.Fatalf("sped = %+v, want cue 1 sped up", result.Sped)
	}
	if s.Cues[0].Speed != 1.09 || !s.Cues[0].SpeedOverride {
		t.Errorf("cue 1 = %+v, want speed 1.09 override", s.Cues[0])
	}
}

func TestLocalService_UpdateCue(t *testing.T) {
	s := &Session{Cues: []*SessionCue{
		{Index: 1, CurrentText: "old", Speed: 1.0, NeedsHuman: true, HumanReason: "was basketed"},
	}}
	ls := NewLocalService(testServiceConfig())

	t.Run("text edit clears the basket flag", func(t *testing.T) {
		newText := "edited"
		got, err := ls.UpdateCue(s, 1, &newText, nil, nil)
		if err != nil {
			t.Fatalf("UpdateCue() error: %v", err)
		}
		if got.CurrentText != "edited" || got.NeedsHuman || got.HumanReason != "" {
			t.Errorf("cue = %+v", got)
		}
	})

	t.Run("speed edit is clamped and marked an override", func(t *testing.T) {
		tooHigh := 5.0
		got, err := ls.UpdateCue(s, 1, nil, &tooHigh, nil)
		if err != nil {
			t.Fatalf("UpdateCue() error: %v", err)
		}
		if got.Speed != float64(MaxSpeed) || !got.SpeedOverride {
			t.Errorf("cue = %+v, want speed clamped to %v and overridden", got, MaxSpeed)
		}
	})

	t.Run("skip toggles independently of text/speed, and preserves the basket reason for a later un-skip", func(t *testing.T) {
		s2 := &Session{Cues: []*SessionCue{
			{Index: 1, CurrentText: "too long", Speed: 1.0, NeedsHuman: true, HumanReason: "past the cap"},
		}}
		skip := true
		got, err := ls.UpdateCue(s2, 1, nil, nil, &skip)
		if err != nil {
			t.Fatalf("UpdateCue() error: %v", err)
		}
		if !got.Skipped || !got.NeedsHuman || got.HumanReason != "past the cap" || got.CurrentText != "too long" {
			t.Errorf("cue = %+v, want only Skipped set, everything else untouched", got)
		}

		unskip := false
		got, err = ls.UpdateCue(s2, 1, nil, nil, &unskip)
		if err != nil {
			t.Fatalf("UpdateCue() error: %v", err)
		}
		if got.Skipped {
			t.Errorf("cue = %+v, want Skipped cleared", got)
		}
	})

	t.Run("unknown index errors", func(t *testing.T) {
		if _, err := ls.UpdateCue(s, 99, nil, nil, nil); err == nil {
			t.Error("want an error for an unknown cue index")
		}
	})
}

func TestAggregateGenerateRounds(t *testing.T) {
	t.Run("a cue billed in two rounds (needed a second speed correction) counts once, not twice", func(t *testing.T) {
		rounds := []generateRoundResult{
			{billedPositions: []int{1, 3}, itemCount: 5, overlapConflict: true, overlappingCues: []int{3}},
			{billedPositions: []int{3, 5}, itemCount: 5, overlapConflict: false, overlappingCues: []int{}},
		}
		got := aggregateGenerateRounds(rounds)
		if len(got.Generated) != 3 || got.Generated[0] != 1 || got.Generated[1] != 3 || got.Generated[2] != 5 {
			t.Fatalf("Generated = %v, want [1 3 5] deduped and sorted", got.Generated)
		}
		if got.SkippedCached != 2 {
			t.Errorf("SkippedCached = %d, want 2 (5 items - 3 distinct billed)", got.SkippedCached)
		}
	})

	t.Run("overlap state reflects the final round, not an earlier one that has since resolved", func(t *testing.T) {
		rounds := []generateRoundResult{
			{billedPositions: []int{1}, itemCount: 2, overlapConflict: true, overlappingCues: []int{1}},
			{billedPositions: []int{1}, itemCount: 2, overlapConflict: false, overlappingCues: []int{}},
		}
		got := aggregateGenerateRounds(rounds)
		if got.OverlapConflict || len(got.OverlappingCues) != 0 {
			t.Errorf("got = %+v, want the resolved (final round) overlap state", got)
		}
	})

	t.Run("a single round behaves exactly like the old one-shot Generate", func(t *testing.T) {
		rounds := []generateRoundResult{
			{billedPositions: []int{2}, itemCount: 4, overlapConflict: false, overlappingCues: []int{}},
		}
		got := aggregateGenerateRounds(rounds)
		if len(got.Generated) != 1 || got.Generated[0] != 2 || got.SkippedCached != 3 {
			t.Errorf("got = %+v", got)
		}
	})
}

func TestRunConvergenceLoop(t *testing.T) {
	t.Run("stops as soon as AutoFix makes no further speed changes", func(t *testing.T) {
		generateCalls := 0
		generate := func() (generateRoundResult, error) {
			generateCalls++
			return generateRoundResult{itemCount: 1}, nil
		}
		afCalls := 0
		autoFix := func() AutoFixResult {
			afCalls++
			if afCalls < 3 {
				return AutoFixResult{Sped: []SpeedChange{{Index: 1}}}
			}
			return AutoFixResult{} // converged on the 3rd AutoFix call
		}

		rounds, afResults, err := runConvergenceLoop(generate, autoFix)
		if err != nil {
			t.Fatalf("runConvergenceLoop() error: %v", err)
		}
		if generateCalls != 3 || afCalls != 3 {
			t.Fatalf("generate calls = %d, autoFix calls = %d, want 3 and 3 (stop right after convergence, not one extra generate)", generateCalls, afCalls)
		}
		if len(rounds) != 3 || len(afResults) != 3 {
			t.Errorf("rounds = %d, afResults = %d, want 3 each", len(rounds), len(afResults))
		}
	})

	t.Run("a single round that already converges makes exactly one generate call", func(t *testing.T) {
		generateCalls := 0
		generate := func() (generateRoundResult, error) { generateCalls++; return generateRoundResult{}, nil }
		autoFix := func() AutoFixResult { return AutoFixResult{} }

		rounds, _, err := runConvergenceLoop(generate, autoFix)
		if err != nil {
			t.Fatalf("runConvergenceLoop() error: %v", err)
		}
		if generateCalls != 1 || len(rounds) != 1 {
			t.Fatalf("generate calls = %d, rounds = %d, want 1 each", generateCalls, len(rounds))
		}
	})

	t.Run("a pathological non-converging AutoFix is stopped by the round cap, not left to loop forever", func(t *testing.T) {
		calls := 0
		generate := func() (generateRoundResult, error) { calls++; return generateRoundResult{}, nil }
		autoFix := func() AutoFixResult { return AutoFixResult{Sped: []SpeedChange{{Index: 1}}} } // never converges

		rounds, afResults, err := runConvergenceLoop(generate, autoFix)
		if err != nil {
			t.Fatalf("runConvergenceLoop() error: %v", err)
		}
		if calls != maxConvergenceRounds || len(rounds) != maxConvergenceRounds || len(afResults) != maxConvergenceRounds {
			t.Fatalf("calls = %d, want exactly maxConvergenceRounds (%d)", calls, maxConvergenceRounds)
		}
	})

	t.Run("a generate error on a later round stops the loop and surfaces the error, keeping earlier rounds", func(t *testing.T) {
		errBoom := fmt.Errorf("network exploded")
		calls := 0
		generate := func() (generateRoundResult, error) {
			calls++
			if calls == 2 {
				return generateRoundResult{}, errBoom
			}
			return generateRoundResult{}, nil
		}
		autoFix := func() AutoFixResult { return AutoFixResult{Sped: []SpeedChange{{Index: 1}}} }

		rounds, _, err := runConvergenceLoop(generate, autoFix)
		if err != errBoom {
			t.Fatalf("err = %v, want errBoom", err)
		}
		if len(rounds) != 1 {
			t.Fatalf("rounds = %d, want 1 (the successful round before the error)", len(rounds))
		}
	})
}

func TestLocalService_Reduce(t *testing.T) {
	t.Run("missing API key and no injected client is a clear error, not a network attempt", func(t *testing.T) {
		s := &Session{Cues: []*SessionCue{{Index: 1, NeedsHuman: true, CurrentText: "x"}}}
		ls := NewLocalService(testServiceConfig())

		if _, err := ls.Reduce(s); err == nil {
			t.Fatal("want an error when Reduce.APIKey and Reduce.Client are both unset")
		}
	})

	t.Run("an injected client is used even with no API key, for testability", func(t *testing.T) {
		s := &Session{Cues: []*SessionCue{
			{Index: 1, StartMs: 0, EndMs: 2000, Voice: "hana", CurrentText: "too long", NeedsHuman: true},
		}}
		cfg := testServiceConfig()
		cfg.Reduce.Client = &fakeAnthropicClient{results: []LineShortenResult{
			{Action: "shorten", Text: "short", Reason: ""},
		}}
		ls := NewLocalService(cfg)

		result, err := ls.Reduce(s)
		if err != nil {
			t.Fatalf("Reduce() error: %v", err)
		}
		if len(result.Shortened) != 1 || s.Cues[0].CurrentText != "short" {
			t.Errorf("result = %+v, cue = %+v", result, s.Cues[0])
		}
	})
}

func TestBuildExportResult(t *testing.T) {
	t.Run("an overlap blocks export", func(t *testing.T) {
		_, err := buildExportResult(nil, []cueOverlap{{First: 0, Second: 1}})
		if err == nil {
			t.Fatal("want an error when overlaps remain")
		}
	})

	t.Run("summarizes still-over-budget and basketed cues", func(t *testing.T) {
		overMs := 3000
		underMs := 100
		cues := []*SessionCue{
			{Index: 1, EndMs: 2000, AudioMs: &overMs},
			{Index: 2, EndMs: 2000, AudioMs: &underMs},
			{Index: 3, NeedsHuman: true},
		}
		result, err := buildExportResult(cues, nil)
		if err != nil {
			t.Fatalf("buildExportResult() error: %v", err)
		}
		if len(result.StillOverBudget) != 1 || result.StillOverBudget[0] != 1 {
			t.Errorf("still_over_budget = %v, want [1]", result.StillOverBudget)
		}
		if len(result.Basket) != 1 || result.Basket[0] != 3 {
			t.Errorf("basket = %v, want [3]", result.Basket)
		}
	})
}

func TestLocalService_Generate_FailureKeepsLastProvider(t *testing.T) {
	// audio.cpp is configured, but no speaker has a voice reference: the
	// switch fails while parsing, before any synthesis. That used to
	// log.Fatal the whole serve process, and before that it had already
	// switched Session.Provider, so the next Export would have resolved
	// audio.cpp against cues ElevenLabs measured.
	cfg := testServiceConfig()
	engine := *cfg.Engine
	engine.AudioCpp = &AudioCppConfig{BaseURL: "http://127.0.0.1:1"}
	cfg.Engine = &engine
	cfg.WorkDir = t.TempDir()
	ls := NewLocalService(cfg)

	audioMs := 900
	s := &Session{Provider: "elevenlabs", Cues: []*SessionCue{
		{Index: 1, StartMs: 0, EndMs: 1000, Voice: engine.Default.Name, CurrentText: "hello", Speed: 1.0,
			AudioMs: &audioMs, LastGeneratedProvider: "elevenlabs"},
	}}

	_, err := ls.Generate(s, "audiocpp")
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("err = %v, want ErrProviderNotConfigured (a 400, not a crash or a 502)", err)
	}
	if s.Provider != "elevenlabs" {
		t.Errorf("Session.Provider = %q, want it left at the last successful generation's \"elevenlabs\"", s.Provider)
	}
	if s.Cues[0].LastGeneratedProvider != "elevenlabs" || *s.Cues[0].AudioMs != 900 {
		t.Errorf("cue = %+v, want its ElevenLabs measurement untouched", s.Cues[0])
	}
}
