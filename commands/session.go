package commands

// SessionCue is dub-studio's app-level cue model (mirrors core.Cue),
// richer than the engine's own astisub-backed Item: it tracks editable
// spoken text separately from the original, and what was last actually
// sent to ElevenLabs, so a session can tell which cues need regenerating.
type SessionCue struct {
	Index                 int     `json:"index"`
	StartMs               int     `json:"start_ms"`
	EndMs                 int     `json:"end_ms"`
	Voice                 string  `json:"voice"`
	SourceText            string  `json:"source_text"`  // locked, original (tag stripped)
	CurrentText           string  `json:"current_text"` // editable spoken text
	Speed                 float64 `json:"speed"`
	SpeedOverride         bool    `json:"speed_override"`
	LastGeneratedText     string  `json:"last_generated_text"`
	LastGeneratedSpeed    float64 `json:"last_generated_speed"`
	LastGeneratedProvider string  `json:"last_generated_provider"` // "elevenlabs" | "audiocpp", set by the Generate call that produced AudioMs
	HasLastGenerated      bool    `json:"has_last_generated"`      // false until the first successful Generate
	AudioMs               *int    `json:"audio_ms"`
	CachePath             string  `json:"cache_path"`
	OverlapFlagged        bool    `json:"overlap_flagged"`
	NeedsHuman            bool    `json:"needs_human"`
	HumanReason           string  `json:"human_reason"`
	AiSuggestion          string  `json:"ai_suggestion"` // Claude's declined-but-attempted draft, see ReduceText
	AiNote                string  `json:"ai_note"`       // Claude's reason for declining, or "" once cleared
	Skipped               bool    `json:"skipped"`       // explicit "accept this line as-is" override, see UpdateCue
}

func (c *SessionCue) WindowMs() int { return c.EndMs - c.StartMs }

func (c *SessionCue) OverageMs() int {
	if c.AudioMs == nil {
		return 0
	}
	return *c.AudioMs - c.WindowMs()
}

// Dirty reports whether this cue needs regenerating: its text or speed
// changed since the last successful synthesis.
func (c *SessionCue) Dirty() bool {
	if !c.HasLastGenerated {
		return true
	}
	return c.CurrentText != c.LastGeneratedText || c.Speed != c.LastGeneratedSpeed
}

// Session is dub-studio's app-level unit of work: one uploaded subtitle
// file, tracked through cleanup/generate/autofix/export.
type Session struct {
	Name          string        `json:"name"`
	Cues          []*SessionCue `json:"cues"`
	RunCount      int           `json:"run_count"`
	ExportWavPath string        `json:"export_wav_path"`
	// Provider is the TTS provider the last Generate call used ("elevenlabs"
	// | "audiocpp"; "" before any Generate, treated as "elevenlabs"
	// everywhere it's read). Export reuses it without asking again, so it
	// always reproduces exactly what Generate last measured -- the same
	// guarantee dub-studio has always made, extended to cover provider
	// choice now that there's more than one.
	Provider        string         `json:"provider"`
	FirstPassReport map[string]any `json:"first_pass_report,omitempty"`
}

// Generated reports whether Generate has ever run (any cue has a measured
// duration).
func (s *Session) Generated() bool {
	for _, c := range s.Cues {
		if c.AudioMs != nil {
			return true
		}
	}
	return false
}

// Flagged returns cues whose measured overage exceeds toleranceMs -- the
// same tolerance the engine's own overlap detection uses (dub-studio's
// core.py treats these as literally the same constant, TOLERANCE_MS =
// config.OVERLAP_TOLERANCE_MS). A Skipped cue is excluded regardless of its
// real overage -- an explicit "accept this as-is" from the user overrides
// the automatic flag, the same way it overrides the basket below.
func (s *Session) Flagged(toleranceMs int) []*SessionCue {
	return filterCues(s.Cues, func(c *SessionCue) bool {
		return c.AudioMs != nil && c.OverageMs() > toleranceMs && !c.Skipped
	})
}

func (s *Session) Basket() []*SessionCue {
	return filterCues(s.Cues, func(c *SessionCue) bool { return c.NeedsHuman && !c.Skipped })
}

func filterCues(cues []*SessionCue, pred func(*SessionCue) bool) []*SessionCue {
	out := make([]*SessionCue, 0)
	for _, c := range cues {
		if pred(c) {
			out = append(out, c)
		}
	}
	return out
}

func cueByIndex(cues []*SessionCue, index int) *SessionCue {
	for _, c := range cues {
		if c.Index == index {
			return c
		}
	}
	return nil
}
