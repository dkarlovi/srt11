package commands

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Ported from dub-studio's core.py (_real_budgets_ms, _leading_gaps_ms,
// _occupied_end_ms, auto_fix_durations). This is dub-studio's app-level
// scheduling policy layered on top of srt11's own engine (overlap
// detection, merging, etc.), not part of the engine itself -- callers
// supply their own tolerance/margin/speed-cap policy via AutoFixConfig.

// Never let a cue run this far past its own subtitle window, even when the
// real same-voice budget would allow much more -- a guardrail against a
// line sounding badly out of sync with the picture, independent of whether
// it technically collides with anything.
const MaxDriftMs = 3000

// How far a single cue's start_ms may be pulled earlier to absorb real dead
// air before it. Bounded well below MaxDriftMs -- this is a small, local
// nudge into a confirmed silence, not a general "let it run long" allowance.
const MaxLeadShiftMs = 1500

// AutoFixCue is the minimal, mutable shape auto-fix scheduling needs.
// AudioMs is nil until a real generation has measured it.
type AutoFixCue struct {
	Index          int
	StartMs        int
	EndMs          int
	Voice          string
	Speed          float64
	SpeedOverride  bool
	AudioMs        *int
	OverlapFlagged bool
	NeedsHuman     bool
	HumanReason    string
	Skipped        bool // explicit "accept this line as-is" override, never re-evaluated
	// Provider is which TTS provider generated AudioMs ("elevenlabs" |
	// "audiocpp" | "" before any generate). audio.cpp already fits each
	// cue's duration via duration_factor at synthesis time -- a
	// still-overrunning audio.cpp cue means the fit was clamped (the text
	// doesn't fit no matter what factor is applied), so a speed bump would
	// be attempting a fix that provider has no lever for; see the gate in
	// AutoFixDurations below.
	Provider string
}

func (c *AutoFixCue) windowMs() int { return c.EndMs - c.StartMs }

// occupiedEndMs is the real end of this cue's audio, not just its subtitle
// box -- an already-overrunning cue keeps the next cue's leading gap from
// starting until its own audio is actually done.
func occupiedEndMs(c *AutoFixCue) int {
	if c.AudioMs != nil {
		return c.StartMs + *c.AudioMs
	}
	return c.EndMs
}

func sortedByStart(cues []*AutoFixCue) []*AutoFixCue {
	ordered := make([]*AutoFixCue, len(cues))
	copy(ordered, cues)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartMs < ordered[j].StartMs })
	return ordered
}

// realBudgetsMs: a cue's honest time budget isn't its own subtitle window --
// that boundary exists for reading text on screen, not for an audio-only
// voice track. The real constraint is however long until this SAME voice
// needs to speak again (the same adjacency the engine's own same-voice
// overlap check uses), capped by MaxDriftMs so a line never drifts
// arbitrarily far just because its voice happens to be quiet for a while.
func realBudgetsMs(cues []*AutoFixCue) map[int]int {
	ordered := sortedByStart(cues)
	budgets := make(map[int]int, len(ordered))
	for i, c := range ordered {
		ceiling := c.windowMs() + MaxDriftMs
		budget := ceiling
		for _, other := range ordered[i+1:] {
			if other.Voice == c.Voice {
				budget = min(other.StartMs-c.StartMs, ceiling)
				break
			}
		}
		budgets[c.Index] = budget
	}
	return budgets
}

// leadingGapsMs: real, unused silence immediately before each cue,
// regardless of voice -- dead air nobody is occupying. The first cue in the
// timeline has no known predecessor, so it gets none to absorb.
func leadingGapsMs(cues []*AutoFixCue) map[int]int {
	ordered := sortedByStart(cues)
	gaps := make(map[int]int, len(ordered))
	prevOccupiedEnd := 0
	for i, c := range ordered {
		if i == 0 {
			gaps[c.Index] = 0
		} else {
			gaps[c.Index] = max(0, c.StartMs-prevOccupiedEnd)
		}
		prevOccupiedEnd = occupiedEndMs(c)
	}
	return gaps
}

func speedCapFor(voice string, caps map[string]float64) float64 {
	if v, ok := caps[voice]; ok {
		return v
	}
	return caps["default"]
}

// AutoFixConfig is dub-studio's own scheduling policy, not srt11's engine
// config: how much overage to tolerate before acting, how far under budget
// to aim when speeding up, and each voice's speed ceiling ("default" is
// required for voices with no specific entry).
type AutoFixConfig struct {
	ToleranceMs     int
	AutofixMarginMs int
	SpeedCaps       map[string]float64
}

type SpeedChange struct {
	Index int     `json:"index"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
}

type BasketEntry struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

type RetimeEntry struct {
	Index     int `json:"index"`
	ShiftedMs int `json:"shifted_ms"`
}

type AutoFixResult struct {
	Sped      []SpeedChange `json:"sped"`
	Basketed  []BasketEntry `json:"basketed"`
	Retimed   []RetimeEntry `json:"retimed"`
	StillOpen int           `json:"still_open"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// AutoFixDurations works off the REAL measured durations (and, in real
// mode, the engine's own overlap detection) from the last generate:
//   - fits its real budget, or engine says fine -> untouched
//   - runs over / engine flagged it              -> bump speed, capped per speaker
//   - cap isn't enough                            -> basket (needs rewording, human call)
//
// Before reaching for a speed bump, an overrunning cue first absorbs any
// real dead air sitting immediately before it by pulling its own start_ms
// earlier -- free (no re-synthesis) and strictly safe (bounded by the gap,
// so it can never newly collide with anything).
func AutoFixDurations(cues []*AutoFixCue, cfg AutoFixConfig) AutoFixResult {
	result := AutoFixResult{Sped: []SpeedChange{}, Basketed: []BasketEntry{}, Retimed: []RetimeEntry{}}

	budgets := realBudgetsMs(cues)
	leadingGaps := leadingGapsMs(cues)

	for _, c := range cues {
		if c.Skipped {
			continue // an explicit user override -- leave Speed/NeedsHuman/HumanReason exactly as they were
		}
		c.NeedsHuman = false
		c.HumanReason = ""

		budget, ok := budgets[c.Index]
		if !ok {
			budget = c.windowMs()
		}
		realOverageMs := 0
		if c.AudioMs != nil {
			realOverageMs = *c.AudioMs - budget
		}
		needsAttention := realOverageMs > cfg.ToleranceMs || c.OverlapFlagged
		if c.AudioMs == nil || !needsAttention {
			continue
		}

		if realOverageMs > cfg.ToleranceMs {
			shift := min(leadingGaps[c.Index], MaxLeadShiftMs, realOverageMs)
			if shift > 0 {
				c.StartMs -= shift
				budget += shift
				realOverageMs -= shift
				result.Retimed = append(result.Retimed, RetimeEntry{Index: c.Index, ShiftedMs: shift})
				if realOverageMs <= cfg.ToleranceMs && !c.OverlapFlagged {
					continue
				}
			}
		}

		// audio.cpp already fit this cue's duration to its window via
		// duration_factor at synthesis time -- it has no speed knob, so a
		// still-overrunning audio.cpp cue means that fit was clamped (the
		// text is too long no matter what factor is applied). There's
		// nothing a Go-side speed bump can do about that; basket it
		// straight away rather than attempt a "fix" this provider has no
		// lever for.
		canSpeedBump := c.Provider != "audiocpp"

		cap := speedCapFor(c.Voice, cfg.SpeedCaps)
		target := math.Max(1, float64(budget-cfg.AutofixMarginMs))
		needed := c.Speed * (float64(*c.AudioMs) / target)
		if canSpeedBump && needed <= cap {
			newSpeed := round2(math.Min(cap, math.Max(needed, c.Speed)))
			if newSpeed > c.Speed {
				result.Sped = append(result.Sped, SpeedChange{Index: c.Index, From: c.Speed, To: newSpeed})
				c.Speed = newSpeed
				c.SpeedOverride = true
			}
		} else {
			var reasons []string
			if c.OverlapFlagged {
				reasons = append(reasons, "overlaps the next line (engine-detected)")
			}
			if realOverageMs > cfg.ToleranceMs {
				reasons = append(reasons, fmt.Sprintf("runs %d ms past the available time before %s speaks again", realOverageMs, c.Voice))
			}
			c.NeedsHuman = true
			reason := strings.Join(reasons, ", ")
			reason = strings.ToUpper(reason[:1]) + reason[1:]
			if canSpeedBump {
				c.HumanReason = fmt.Sprintf("%s — past the %.2fx cap for %s. Needs rewording.", reason, cap, c.Voice)
			} else {
				c.HumanReason = fmt.Sprintf("%s — audio.cpp's fit-to-window was clamped, it can't be sped up further. Needs rewording.", reason)
			}
			result.Basketed = append(result.Basketed, BasketEntry{Index: c.Index, Reason: c.HumanReason})
		}
	}

	result.StillOpen = len(result.Basketed)
	return result
}
