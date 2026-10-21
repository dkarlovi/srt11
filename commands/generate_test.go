package commands

import (
	"testing"
	"time"

	"github.com/asticode/go-astisub"
)

func TestRebuildCuesAfterGenerate(t *testing.T) {
	t.Run("one to one, no merge", func(t *testing.T) {
		oldCues := []*SessionCue{
			{Index: 1, StartMs: 0, EndMs: 2000, Voice: "Hana", CurrentText: "hi"},
			{Index: 2, StartMs: 2000, EndMs: 4000, Voice: "Matko", CurrentText: "yo"},
		}
		audioFiles := []AudioFile{
			{
				Item:     Item{Sub: &astisub.Item{StartAt: 0, EndAt: 2 * time.Second, Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "hi"}}}}}, Model: Model{name: "Hana", speed: 1.0}},
				Duration: 1800 * time.Millisecond,
			},
			{
				Item:     Item{Sub: &astisub.Item{StartAt: 2 * time.Second, EndAt: 4 * time.Second, Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "yo"}}}}}, Model: Model{name: "Matko", speed: 1.0}},
				Duration: 1500 * time.Millisecond,
			},
		}

		got := rebuildCuesAfterGenerate(oldCues, audioFiles, nil, "elevenlabs")
		if len(got) != 2 {
			t.Fatalf("want 2 cues, got %d", len(got))
		}
		if *got[0].AudioMs != 1800 || got[0].Voice != "Hana" {
			t.Errorf("cue 1 = %+v", got[0])
		}
		if !got[0].HasLastGenerated || got[0].LastGeneratedText != "hi" {
			t.Errorf("cue 1 last-generated state = %+v", got[0])
		}
		if got[0].LastGeneratedProvider != "elevenlabs" {
			t.Errorf("cue 1 LastGeneratedProvider = %q, want \"elevenlabs\"", got[0].LastGeneratedProvider)
		}
	})

	t.Run("a merged group collapses into one cue spanning the full window", func(t *testing.T) {
		oldCues := []*SessionCue{
			{Index: 1, StartMs: 2000, EndMs: 2500, Voice: "Matko", CurrentText: "First part", SpeedOverride: true},
			{Index: 2, StartMs: 2500, EndMs: 3000, Voice: "Matko", CurrentText: "Second part"},
		}
		audioFiles := []AudioFile{
			{
				Item: Item{
					Sub:   &astisub.Item{StartAt: 2 * time.Second, EndAt: 3 * time.Second, Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "First part Second part"}}}}},
					Model: Model{name: "Matko", speed: 1.0},
				},
				Duration: 1200 * time.Millisecond,
			},
		}

		got := rebuildCuesAfterGenerate(oldCues, audioFiles, map[int]cueOverlap{0: {First: 0, Second: 1, Duration: 200 * time.Millisecond}}, "elevenlabs")
		if len(got) != 1 {
			t.Fatalf("want 1 collapsed cue, got %d: %+v", len(got), got)
		}
		merged := got[0]
		if merged.StartMs != 2000 || merged.EndMs != 3000 {
			t.Errorf("window = %d-%d, want 2000-3000", merged.StartMs, merged.EndMs)
		}
		if *merged.AudioMs != 1200 {
			t.Errorf("audio_ms = %d, want 1200", *merged.AudioMs)
		}
		if !merged.OverlapFlagged {
			t.Error("want overlap_flagged true")
		}
		// override carried forward from either pre-merge source cue
		if !merged.SpeedOverride {
			t.Error("want speed_override carried forward from a merged source cue")
		}
		if merged.CurrentText != "First part Second part" {
			t.Errorf("current_text = %q", merged.CurrentText)
		}
	})
}
