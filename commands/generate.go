package commands

import (
	"strings"
)

// GenerateResult reports what a Generate call did, mirroring dub-studio's
// core.generate_cues return dict.
type GenerateResult struct {
	RunNumber       int    `json:"run_number"`
	Provider        string `json:"provider"` // "elevenlabs" | "audiocpp", whichever this call actually used
	Generated       []int  `json:"generated"`
	SkippedCached   int    `json:"skipped_cached"`
	OverlapConflict bool   `json:"overlap_conflict"`
	OverlappingCues []int  `json:"overlapping_cues"`
	// AutoSped and StillBasketed summarize the automatic speed-convergence
	// loop (see LocalService.Generate) that now runs inside every Generate
	// call: how many distinct cues got a speed bump at some point during
	// convergence, and how many are left basketed once it settled.
	AutoSped      int `json:"auto_sped"`
	StillBasketed int `json:"still_basketed"`
}

// rebuildCuesAfterGenerate replaces oldCues with one SessionCue per
// resulting AudioFile. This deliberately collapses a merged group of
// original cues into a single cue spanning its full window: keeping the
// pre-merge cues around with the group's one shared audio_ms would compare
// that shared duration against each cue's own much narrower window and
// flag every one of them as wildly over. The merged group is the real unit
// of scheduling review from here on. Ported from dub-studio's
// core._cues_from_report, adapted from report-text scraping to direct
// struct access since Generate now runs the engine in-process.
func rebuildCuesAfterGenerate(oldCues []*SessionCue, audioFiles []AudioFile, overlapsByFirst map[int]cueOverlap, provider string) []*SessionCue {
	newCues := make([]*SessionCue, 0, len(audioFiles))
	origIdx := 0
	for i, af := range audioFiles {
		groupEndMs := int(af.Item.Sub.EndAt.Milliseconds())

		var group []*SessionCue
		for origIdx < len(oldCues) {
			c := oldCues[origIdx]
			group = append(group, c)
			origIdx++
			if c.EndMs >= groupEndMs {
				break
			}
		}

		override := false
		for _, c := range group {
			if c.SpeedOverride {
				override = true
				break
			}
		}

		text := strings.TrimSpace(af.Item.Sub.String())
		audioMs := int(af.Duration.Milliseconds())
		speed := round2(float64(af.Item.Model.speed))
		_, overlapped := overlapsByFirst[i]

		newCues = append(newCues, &SessionCue{
			Index:   i + 1,
			StartMs: int(af.Item.Sub.StartAt.Milliseconds()), EndMs: groupEndMs,
			Voice:                 af.Item.Model.name,
			SourceText:            text,
			CurrentText:           text,
			Speed:                 speed,
			SpeedOverride:         override,
			LastGeneratedText:     text,
			LastGeneratedSpeed:    speed,
			LastGeneratedProvider: provider,
			HasLastGenerated:      true,
			AudioMs:               &audioMs,
			CachePath:             af.Item.Path.Path,
			OverlapFlagged:        overlapped,
		})
	}
	return newCues
}
