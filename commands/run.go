package commands

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/symfony-cli/console"
	"gopkg.in/yaml.v3"
)

type SpeakerConfig struct {
	Model    string  `yaml:"model"` // ElevenLabs voice ID
	Name     string  `yaml:"name"`
	Speed    float32 `yaml:"speed"`
	TTSModel string  `yaml:"tts_model"` // optional: ElevenLabs model ID override for this speaker

	// audio.cpp fields, used only when the audiocpp provider is selected
	// for a Generate/Export call. VoiceRefPath is a local WAV file (the
	// audio.cpp server always runs on the same machine, per Matko's
	// 2026-09-22 decision -- see HANDOFF-audiocpp.local.md -- so this is a
	// plain path, not something uploaded). ReferenceTextPath is that WAV's
	// exact transcript, sent as reference_text; audio.cpp requires it and
	// silently produces near-silence without it (FINDINGS §3), so it's
	// loaded from disk once at parse time rather than trusted to be right
	// inline in this YAML.
	VoiceRefPath      string `yaml:"voice_ref_path"`
	ReferenceTextPath string `yaml:"reference_text_path"`
}

type Config struct {
	AuthKey               string                   `yaml:"auth_key"`
	TTSModel              string                   `yaml:"tts_model"` // optional: default ElevenLabs model ID for all speakers
	Default               SpeakerConfig            `yaml:"default"`
	Models                map[string]SpeakerConfig `yaml:"models"`
	MergeLinesThresholdMs int                      `yaml:"merge_lines_threshold_ms"` // optional
	AudioCpp              *AudioCppConfig          `yaml:"audio_cpp"`                // optional: nil disables the audiocpp provider entirely
}

// AudioCppConfig configures the audio.cpp TTS provider. Never hardcode a
// base URL elsewhere (dkarlovi's requirement #3) -- this is the only place
// it comes from.
type AudioCppConfig struct {
	BaseURL string `yaml:"base_url"` // e.g. "http://127.0.0.1:8081"
	Model   string `yaml:"model"`    // e.g. "indextts-clone"
	// FitDuration sends duration_factor to fit each take to its cue's
	// window. Only IndexTTS honours it; other models (e.g. Higgs) return
	// the same audio for any factor, so it's off unless set explicitly.
	FitDuration bool `yaml:"fit_duration"`
	// TakeFit fits takes after synthesis instead, for models without
	// duration control (Higgs): several takes, cleaned, the best one picked,
	// sped up by 8 % at most (see takefit.go). Experimental.
	TakeFit bool `yaml:"take_fit"`
	Takes   int  `yaml:"takes"` // takes per cue under take_fit; 0 means defaultTakes
}

const defaultTakes = 6

func (c AudioCppConfig) takeCount() int {
	if c.Takes == 0 {
		return defaultTakes
	}
	return c.Takes
}

// validateAudioCppConfig rejects combinations readConfig's YAML decoding
// can't: the two fit modes are alternatives, not layers.
func validateAudioCppConfig(c *AudioCppConfig) error {
	if c == nil {
		return nil
	}
	if c.TakeFit && c.FitDuration {
		return fmt.Errorf("audio_cpp: take_fit and fit_duration cannot both be true")
	}
	if c.Takes < 0 {
		return fmt.Errorf("audio_cpp: takes must not be negative, got %d", c.Takes)
	}
	return nil
}

// defaultTTSModel is used when no tts_model is set anywhere in the config.
// Kept as the historical default so existing configs behave identically.
const defaultTTSModel = "eleven_multilingual_v2"

// resolveTTSModel picks the ElevenLabs model ID for a speaker, in order of
// precedence: per-speaker tts_model, top-level tts_model, built-in default.
func resolveTTSModel(speaker SpeakerConfig, cfg *Config) string {
	if speaker.TTSModel != "" {
		return speaker.TTSModel
	}
	if cfg.TTSModel != "" {
		return cfg.TTSModel
	}
	return defaultTTSModel
}

type Model struct {
	model    string
	name     string
	offset   int
	speed    float32
	ttsModel string

	// audio.cpp fields (empty when this speaker/call uses ElevenLabs).
	voiceRefPath  string
	referenceText string // loaded contents of the speaker's ReferenceTextPath
}

type Path struct {
	Path     string
	Template string
	Id       string
}

type Item struct {
	Sub        *astisub.Item
	Model      Model
	Path       Path
	MergedFrom []string // timings of merged-from lines
}

type AudioFile struct {
	Item     Item
	Duration time.Duration
	Offset   time.Duration
	Channel  int
	Overlap  time.Duration
}

// All returns all available commands
func All() []*console.Command {
	return []*console.Command{
		{
			Name:        "run",
			Usage:       "Convert subtitle files to audio using ElevenLabs TTS",
			Description: "Convert subtitle files to audio",
			Args: []*console.Arg{
				{
					Name:        "file",
					Description: "Path to the .srt or .vtt subtitle file",
				},
			},
			Flags: []console.Flag{
				&console.IntFlag{
					Name:    "merge-lines-threshold-ms",
					Aliases: []string{"m"},
					Usage:   "Merge lines if same speaker and gap is below this threshold (ms)",
				},
				&console.IntFlag{
					Name:  "merge-max-ms",
					Usage: "Cap a merged cue's total window (first start to last end) to this many ms; once folding the next line in would exceed it, start a new cue instead (0 = unlimited, the default)",
				},
				&console.IntFlag{
					Name:    "overlap-tolerance-ms",
					Aliases: []string{"t"},
					Usage:   "Allow same-speaker overlaps up to this many ms without failing; the final audio is still written (0 = require no overlap, the default)",
				},
				&console.IntFlag{
					Name:         "cross-overlap-tolerance-ms",
					Usage:        "Gate on cross-speaker overlaps past this many ms, like --overlap-tolerance-ms; cross-overlaps are always reported per-cue regardless (default -1 = report only, never fail)",
					DefaultValue: -1,
				},
				&console.IntFlag{
					Name:         "normalize-target-db",
					Usage:        "Gain each cue toward this RMS level (dBFS) before mixing, evening out level swings between ElevenLabs generations; 0 or above disables normalization",
					DefaultValue: -20,
				},
				&console.StringFlag{
					Name:         "provider",
					Usage:        "TTS provider: \"elevenlabs\" (default) or \"audiocpp\" (requires audio_cpp.base_url in config)",
					DefaultValue: "elevenlabs",
				},
			},
			Action: Run,
		},
		{
			Name:        "autofix-parity",
			Usage:       "Run dub-studio's auto-fix scheduling policy against JSON cues for migration parity checks",
			Description: "Read cues + policy config as JSON, run AutoFixDurations, and write a JSON result for parity validation against the Python PoC",
			Flags: []console.Flag{
				&console.StringFlag{
					Name:  "cues",
					Usage: "Path to a JSON array of input cues ({index,start_ms,end_ms,voice,speed,audio_ms,overlap_flagged})",
				},
				&console.StringFlag{
					Name:  "autofix-config",
					Usage: "Path to a JSON object ({tolerance_ms,autofix_margin_ms,speed_caps})",
				},
				&console.StringFlag{
					Name:         "out-dir",
					Usage:        "Directory to write the autofix parity summary JSON into",
					DefaultValue: ".",
				},
			},
			Action: runAutoFixParity,
		},
		{
			Name:        "session-upload",
			Usage:       "Start a new dub-studio session from a subtitle file",
			Description: "Parse a subtitle file into a session, rejecting it if any speaker tag isn't at the very start of a cue",
			Args: []*console.Arg{
				{Name: "file", Description: "Path to the .srt or .vtt subtitle file"},
			},
			Flags: append([]console.Flag{
				&console.StringFlag{Name: "name", Usage: "Session name (defaults to the file's basename)"},
			}, sessionWorkDirFlags()...),
			Action: runSessionUpload,
		},
		{
			Name:   "session-cleanup",
			Usage:  "Apply deterministic filler/hedge-phrase cleanup to the active session",
			Flags:  sessionWorkDirFlags(),
			Action: runSessionCleanup,
		},
		{
			Name:   "session-generate",
			Usage:  "Generate audio for the active session's dirty/uncached cues",
			Flags:  append(sessionWorkDirFlags(), sessionGenerationFlags()...),
			Action: runSessionGenerate,
		},
		{
			Name:  "session-autofix",
			Usage: "Run the auto-fix scheduling policy (speed bump or basket) on the active session",
			Flags: append([]console.Flag{
				&console.StringFlag{Name: "autofix-config", Usage: "Path to a JSON object ({tolerance_ms,autofix_margin_ms,speed_caps}); built-in defaults are used if omitted"},
			}, sessionWorkDirFlags()...),
			Action: runSessionAutoFix,
		},
		{
			Name:   "session-reduce",
			Usage:  "Let Claude draft a shortened version of every basketed cue",
			Flags:  append(sessionWorkDirFlags(), reduceFlags()...),
			Action: runSessionReduce,
		},
		{
			Name:   "session-export",
			Usage:  "Export the active session's final mixed WAV",
			Flags:  append(sessionWorkDirFlags(), sessionGenerationFlags()...),
			Action: runSessionExport,
		},
		{
			Name:  "session-update-cue",
			Usage: "Manually edit one cue's text and/or speed, or skip/un-skip it",
			Flags: append([]console.Flag{
				&console.IntFlag{Name: "index", Usage: "1-based cue index to edit"},
				&console.StringFlag{Name: "text", Usage: "New spoken text for the cue"},
				&console.Float64Flag{Name: "speed", Usage: "New speed override for the cue"},
				&console.BoolFlag{Name: "skip", Usage: "Accept this cue as-is, excluding it from flagged/basket counts and Reduce; --skip=false un-skips it"},
			}, sessionWorkDirFlags()...),
			Action: runSessionUpdateCue,
		},
		{
			Name:   "session-show",
			Usage:  "Print the active session's current state",
			Flags:  sessionWorkDirFlags(),
			Action: runSessionShow,
		},
		{
			Name:   "session-reset",
			Usage:  "Clear the active session",
			Flags:  sessionWorkDirFlags(),
			Action: runSessionReset,
		},
		{
			Name:        "serve",
			Usage:       "Run the HTTP API for the session workflow (upload/cleanup/generate/autofix/reduce/export)",
			Description: "Route-for-route equivalent of dub-studio's app.py",
			Flags: append([]console.Flag{
				&console.StringFlag{
					Name:         "addr",
					Usage:        "Address to listen on",
					DefaultValue: ":8080",
				},
				&console.StringFlag{
					Name:  "static-dir",
					Usage: "Directory containing an index.html (and any other static assets) to serve at / and /static/ instead of the built-in embedded frontend; only useful for iterating on the frontend locally without a rebuild",
				},
				&console.StringFlag{
					Name:  "autofix-config",
					Usage: "Path to a JSON object ({tolerance_ms,autofix_margin_ms,speed_caps}); built-in defaults are used if omitted",
				},
			}, append(sessionWorkDirFlags(), append(sessionGenerationFlags(), reduceFlags()...)...)...),
			Action: runServe,
		},
	}
}

// sessionWorkDirFlags are common to every session-* command: where the
// session lives, and the tolerance used both to gate same-voice overlaps
// at Generate/Export time and to decide whether a cue counts as "flagged"
// in any session summary -- dub-studio's core.py treats these as literally
// the same constant (TOLERANCE_MS = config.OVERLAP_TOLERANCE_MS).
func sessionWorkDirFlags() []console.Flag {
	return []console.Flag{
		&console.StringFlag{
			Name:         "work-dir",
			Usage:        "Directory holding session state, working VTT, and exported audio; serve puts each browser session in a sessions/<id> subdirectory of it and shares one cache/ subdirectory across them",
			DefaultValue: ".",
		},
		&console.IntFlag{
			Name:         "overlap-tolerance-ms",
			Aliases:      []string{"t"},
			Usage:        "Allow same-voice overlaps up to this many ms; also the threshold for a cue counting as \"flagged\"",
			DefaultValue: 120,
		},
	}
}

// sessionGenerationFlags mirror dub-studio's config.py defaults
// (MERGE_THRESHOLD_MS, MERGE_MAX_MS) as out-of-the-box behavior for
// session-generate/session-export, on top of sessionWorkDirFlags.
func sessionGenerationFlags() []console.Flag {
	return []console.Flag{
		&console.IntFlag{
			Name:         "merge-lines-threshold-ms",
			Aliases:      []string{"m"},
			Usage:        "Merge lines if same speaker and gap is below this threshold (ms)",
			DefaultValue: 120,
		},
		&console.IntFlag{
			Name:         "merge-max-ms",
			Usage:        "Cap a merged cue's total window to this many ms (0 = unlimited)",
			DefaultValue: 6300,
		},
		&console.IntFlag{
			Name:         "normalize-target-db",
			Usage:        "Gain each cue toward this target RMS before mixing (0 or above disables)",
			DefaultValue: -20,
		},
		&console.IntFlag{
			Name:         "run-cap",
			Usage:        "Refuse to Generate past this many runs for the session (0 = unlimited)",
			DefaultValue: 10,
		},
		&console.StringFlag{
			Name:  "provider",
			Usage: "TTS provider for this Generate call: \"elevenlabs\" (default) or \"audiocpp\"; empty reuses whatever the session last used",
		},
	}
}

// reduceFlags configure the AI line-shortening step (see reduce.go).
// --anthropic-api-key falls back to the ANTHROPIC_API_KEY environment
// variable, mirroring dub-studio's config.py (a real Anthropic key, billed
// separately from ElevenLabs -- never printed or logged).
func reduceFlags() []console.Flag {
	return []console.Flag{
		&console.StringFlag{
			Name:    "anthropic-api-key",
			Usage:   "Anthropic API key for the AI line-shortening step; required only when /reduce or session-reduce is actually used",
			EnvVars: []string{"ANTHROPIC_API_KEY"},
		},
		&console.StringFlag{
			Name:         "reduce-model",
			Usage:        "Anthropic model used for AI line-shortening",
			DefaultValue: defaultReduceModel,
		},
	}
}

func Run(c *console.Context) error {
	path := c.Args().Get("file")

	config, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	threshold := config.MergeLinesThresholdMs
	if c.Int("merge-lines-threshold-ms") > 0 {
		threshold = c.Int("merge-lines-threshold-ms")
	}
	if threshold > 0 {
		log.Printf("Using merge threshold: %d", threshold)
	} else {
		log.Printf("No merge threshold set, not merging lines")
	}

	mergeMaxMs := c.Int("merge-max-ms")
	if mergeMaxMs > 0 {
		log.Printf("Using merge max window: %dms", mergeMaxMs)
	}

	overlapTolerance := time.Duration(c.Int("overlap-tolerance-ms")) * time.Millisecond
	if overlapTolerance > 0 {
		log.Printf("Using overlap tolerance: %s", overlapTolerance)
	}

	crossOverlapToleranceMs := c.Int("cross-overlap-tolerance-ms")
	if crossOverlapToleranceMs >= 0 {
		log.Printf("Using cross-overlap tolerance: %dms", crossOverlapToleranceMs)
	}

	normalizeTargetDb := c.Int("normalize-target-db")
	if normalizeTargetDb < 0 {
		log.Printf("Normalizing each cue toward %ddB RMS before mixing", normalizeTargetDb)
	} else {
		log.Printf("Normalization disabled")
	}

	provider, err := resolveProvider(c.String("provider"), config)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error resolving TTS provider: %v", err), 1)
	}

	items, err := parseSubtitleFile(config, path, threshold, mergeMaxMs, "", provider)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error parsing subtitles: %v", err), 1)
	}

	audioFiles, err := generateMissingVoiceLines(provider, items)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error generating audio: %v", err), 1)
	}

	overlapsByFirst, overlaps := annotateOverlaps(audioFiles, findOverlaps(audioFiles, overlapTolerance))

	// Cross-overlaps are always reported per-cue at tolerance 0, regardless of
	// whether they gate the run.
	crossOverlapsByFirst, _ := annotateOverlaps(audioFiles, findCrossOverlaps(audioFiles, 0))

	var crossOverlaps []AudioFile
	if crossOverlapToleranceMs >= 0 {
		crossTolerance := time.Duration(crossOverlapToleranceMs) * time.Millisecond
		_, crossOverlaps = annotateOverlaps(audioFiles, findCrossOverlaps(audioFiles, crossTolerance))
	}

	for i, file := range audioFiles {
		fileEndAt := file.Offset + file.Duration
		var overlapText string
		if ov, ok := overlapsByFirst[i]; ok {
			overlapText += fmt.Sprintf(" (<fg=yellow>OVERLAP %s</>)", ov.Duration.Round(time.Millisecond))
		}
		if ov, ok := crossOverlapsByFirst[i]; ok {
			overlapText += fmt.Sprintf(" (<fg=cyan>CROSS-OVERLAP %s</>)", ov.Duration.Round(time.Millisecond))
		}

		fmt.Fprintf(c.App.Writer,
			"#%03d\n<info>%s</>\nSpeaker:  <comment>%s</>, speed: %.2f\nSubtitle: <fg=yellow>%s</> --> <fg=yellow>%s</> (duration <fg=yellow>%s</>)\nAudio:    <fg=yellow>%s</> --> <fg=yellow>%s</> (duration <fg=yellow>%s</>)%s\nPath:     <fg=default>%s</>\n",
			file.Item.Sub.Index+1,
			file.Item.Sub.String(),
			file.Item.Model.name,
			file.Item.Model.speed,
			file.Item.Sub.StartAt.Round(time.Millisecond),
			file.Item.Sub.EndAt.Round(time.Millisecond),
			(file.Item.Sub.EndAt - file.Item.Sub.StartAt).Round(time.Millisecond),
			file.Offset.Round(time.Millisecond),
			fileEndAt.Round(time.Millisecond),
			file.Duration.Round(time.Millisecond),
			overlapText,
			file.Item.Path.Path,
		)
		// Print merged-from info if present
		if len(file.Item.MergedFrom) > 1 {
			fmt.Fprintf(c.App.Writer, "Merged from:\n")
			for _, line := range file.Item.MergedFrom {
				parts := strings.SplitN(line, " | ", 2)
				if len(parts) == 2 {
					fmt.Fprintf(c.App.Writer,
						"    %s\n    %s\n",
						parts[1], parts[0],
					)
				} else {
					fmt.Fprintf(c.App.Writer, "    %s\n", line)
				}
			}
		}
		fmt.Fprintf(c.App.Writer, "\n")
	}

	if len(overlaps) > 0 {
		fmt.Fprintf(c.App.Writer, "<fg=yellow>Overlaps detected:</>\n")
		for _, overlap := range overlaps {
			fmt.Fprintf(c.App.Writer,
				"#%03d <fg=yellow>%s</>\n<info>%s</>\n\n",
				overlap.Item.Sub.Index+1,
				overlap.Overlap.Round(time.Millisecond),
				overlap.Item.Sub.String(),
			)
		}
	}
	if len(crossOverlaps) > 0 {
		fmt.Fprintf(c.App.Writer, "<fg=cyan>Cross-overlaps detected:</>\n")
		for _, overlap := range crossOverlaps {
			fmt.Fprintf(c.App.Writer,
				"#%03d <fg=cyan>%s</>\n<info>%s</>\n\n",
				overlap.Item.Sub.Index+1,
				overlap.Overlap.Round(time.Millisecond),
				overlap.Item.Sub.String(),
			)
		}
	}
	if len(overlaps) > 0 || len(crossOverlaps) > 0 {
		fmt.Fprintf(c.App.Writer, "Fix and rerun the script to generate the final audio file.\n")
		os.Exit(1)
	}

	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_" + time.Now().Format("2006-01-02-15-04-05") + ".wav"
	if err := generateFinalAudioFile(provider, audioFiles, outputPath, float64(normalizeTargetDb)); err != nil {
		return console.Exit(fmt.Sprintf("Error writing final audio track: %v", err), 1)
	}
	log.Printf("Final audio track written to %s\n", outputPath)
	return nil
}

func readConfig(filename string) (*Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var config Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		var typeError *yaml.TypeError
		if errors.As(err, &typeError) {
			msg := ""
			for _, field := range typeError.Errors {
				msg += fmt.Sprintf("  - <fg=red>%s</>\n", field)
			}
			return nil, fmt.Errorf("error parsing config file <info>%s</>:\n%s", filename, msg)
		}
		return nil, err
	}
	if err := validateAudioCppConfig(config.AudioCpp); err != nil {
		return nil, fmt.Errorf("error in config file %s: %w", filename, err)
	}
	return &config, nil
}

// loadReferenceText reads an audio.cpp voice's reference transcript
// (SpeakerConfig.ReferenceTextPath), memoized in cache since the same
// speaker's file is read once per cue that speaker has. audio.cpp requires
// reference_text and silently produces near-silence without it (FINDINGS
// §3), so an empty or missing path is a hard error, not a quiet fallback.
func loadReferenceText(path string, cache map[string]string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("reference_text_path is not set")
	}
	if text, ok := cache[path]; ok {
		return text, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(data))
	cache[path] = text
	return text, nil
}

func generateModelChannelMap(config *Config) map[string]int {
	channels := make(map[string]int)
	// Default model always goes to channel 0
	channels[config.Default.Name] = 0

	currentChannel := 1
	// Assign unique channels to each distinct model
	for _, model := range config.Models {
		if _, exists := channels[model.Name]; !exists {
			channels[model.Name] = currentChannel
			currentChannel++
		}
	}
	return channels
}

// resolveProvider maps a provider name from a flag/request into a
// TTSProvider, using config for whatever that provider needs
// (config.AuthKey for ElevenLabs, config.AudioCpp for audio.cpp). "" means
// "elevenlabs" -- the feature is additive and off by default, so an empty
// or omitted provider preserves today's behavior exactly.
func resolveProvider(name string, config *Config) (TTSProvider, error) {
	switch name {
	case "", "elevenlabs":
		return newElevenLabsProvider(config.AuthKey), nil
	case "audiocpp":
		if config.AudioCpp == nil || config.AudioCpp.BaseURL == "" {
			return nil, fmt.Errorf(
				"provider \"audiocpp\" requires audio_cpp.base_url to be set in config.yaml")
		}
		return newAudioCppProvider(*config.AudioCpp), nil
	default:
		return nil, fmt.Errorf("unknown TTS provider %q (want \"elevenlabs\" or \"audiocpp\")", name)
	}
}

// newestFile returns the path with the most recent modification time among
// files, which must be non-empty. Used when a cache glob matches more than
// one file -- e.g. a cache miss got regenerated with a new request ID while
// an older file for the same voice+model+speed+text was never cleaned up --
// so the most recently generated take is reused instead of glob's arbitrary
// (not chronological) ordering silently picking a stale one.
func newestFile(files []string) (string, error) {
	newest := files[0]
	newestModTime, err := fileModTime(newest)
	if err != nil {
		return "", err
	}
	for _, f := range files[1:] {
		modTime, err := fileModTime(f)
		if err != nil {
			return "", err
		}
		if modTime.After(newestModTime) {
			newest = f
			newestModTime = modTime
		}
	}
	return newest, nil
}

func fileModTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// canMergeCue reports whether the next cue may be folded into the current
// merge group. Cross-voice cues never merge, regardless of gap or cap.
// mergeThresholdMs caps the gap between the group's current end and the
// next cue's start. mergeMaxMs (0 = unlimited) separately caps the total
// window from the group's first start to the candidate end -- so a long
// run of abutting same-voice lines still gets split into multiple cues
// once folding the next one in would make the group too long.
func canMergeCue(curSpeaker, nextSpeaker string, mergedStart, mergedEnd, nextStart, nextEnd time.Duration, mergeThresholdMs, mergeMaxMs int) bool {
	if curSpeaker != nextSpeaker {
		return false
	}
	gap := nextStart - mergedEnd
	if gap < 0 || gap.Milliseconds() > int64(mergeThresholdMs) {
		return false
	}
	if mergeMaxMs > 0 {
		window := nextEnd - mergedStart
		if window.Milliseconds() > int64(mergeMaxMs) {
			return false
		}
	}
	return true
}

// parseSubtitleFile parses path, resolves each cue's speaker/speed, merges
// adjacent same-speaker cues, and computes each cue's cache path.
//
// cacheDir, when non-empty, is where cached takes are looked up and
// written; empty means the subtitle file's own directory, which is the
// engine's standalone behavior and what the run command passes. The
// session/app layer overrides it so several per-session work directories
// can share one cache -- each provider's cache key is content-addressed
// (see TTSProvider.CachePath), so a hit written by one session is
// byte-identical to what another would generate.
//
// provider resolves each cue's cache path (and, for audio.cpp, its voice
// reference) -- see TTSProvider.CachePath.
//
// Every failure is returned, never fatal: serve calls this for every
// tenant, and a speaker missing its audio.cpp reference used to log.Fatal
// the whole process. That one wraps ErrProviderNotConfigured, so the HTTP
// layer reports it as a 400 config problem rather than a 502.
func parseSubtitleFile(config *Config, path string, mergeLinesThresholdMs, mergeMaxMs int, cacheDir string, provider TTSProvider) ([]Item, error) {
	subs, err := astisub.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("parsing subtitle file: %w", err)
	}

	modelChannels := generateModelChannelMap(config)
	items := make([]Item, 0)
	referenceTextCache := make(map[string]string)
	root, _ := filepath.Abs(filepath.Dir(path))
	if cacheDir != "" {
		root, _ = filepath.Abs(cacheDir)
	}

	// Merge logic
	type mergedResult struct {
		item       *astisub.Item
		mergedFrom []string
	}
	mergedSubs := make([]mergedResult, 0)
	i := 0
	for i < len(subs.Items) {
		cur := subs.Items[i]
		// Determine speaker for current line
		var curSpeaker string
		if cur.Lines[0].VoiceName != "" {
			curSpeaker = cur.Lines[0].VoiceName
		} else if len(cur.Comments) > 0 {
			curSpeaker = cur.Comments[0]
		} else {
			curSpeaker, _, _ = resolveSpeaker(cur.String())
		}
		// Prepare to merge into a single line
		mergedText := cur.String()
		mergedStart := cur.StartAt
		mergedEnd := cur.EndAt
		mergedVoiceName := cur.Lines[0].VoiceName
		mergedComments := cur.Comments
		mergedFrom := []string{
			fmt.Sprintf("<fg=yellow>%s</> --> <fg=yellow>%s</> (duration <fg=yellow>%s</>) | <info>%s</>",
				cur.StartAt.Round(time.Millisecond),
				cur.EndAt.Round(time.Millisecond),
				(cur.EndAt - cur.StartAt).Round(time.Millisecond),
				strings.TrimSpace(cur.String()),
			),
		}
		for {
			// Try to merge with next lines if threshold is set
			if mergeLinesThresholdMs > 0 && i+1 < len(subs.Items) {
				next := subs.Items[i+1]
				var nextSpeaker string
				if next.Lines[0].VoiceName != "" {
					nextSpeaker = next.Lines[0].VoiceName
				} else if len(next.Comments) > 0 {
					nextSpeaker = next.Comments[0]
				} else {
					nextSpeaker, _, _ = resolveSpeaker(next.String())
				}
				if canMergeCue(curSpeaker, nextSpeaker, mergedStart, mergedEnd, next.StartAt, next.EndAt, mergeLinesThresholdMs, mergeMaxMs) {
					// Merge: extend end time, concat text. Strip next's own
					// leading speaker tag (if it has one) before folding it
					// in -- resolveSpeaker only strips a tag anchored at the
					// very start of a string, so without this, a tag from a
					// merged-in line lands mid-string and is never stripped,
					// ending up spoken literally.
					_, nextDialogue, _ := resolveSpeaker(next.String())
					mergedEnd = next.EndAt
					mergedText = strings.TrimSpace(mergedText) + " " + strings.TrimSpace(nextDialogue)
					mergedFrom = append(mergedFrom, fmt.Sprintf("<fg=yellow>%s</> --> <fg=yellow>%s</> (duration <fg=yellow>%s</>) | <info>%s</>",
						next.StartAt.Round(time.Millisecond),
						next.EndAt.Round(time.Millisecond),
						(next.EndAt-next.StartAt).Round(time.Millisecond),
						strings.TrimSpace(next.String()),
					))
					i++
					continue
				}
			}
			break
		}
		// Create a new astisub.Item with the merged text as a single line
		mergedItem := &astisub.Item{
			StartAt: mergedStart,
			EndAt:   mergedEnd,
			Lines: []astisub.Line{
				{
					VoiceName: mergedVoiceName,
					Items: []astisub.LineItem{
						{Text: mergedText},
					},
				},
			},
			Comments: mergedComments,
		}
		mergedSubs = append(mergedSubs, mergedResult{item: mergedItem, mergedFrom: mergedFrom})
		i++
	}

	for i, res := range mergedSubs {
		sub := res.item
		sub.Index = i
		var modelName string
		if sub.Lines[0].VoiceName != "" {
			modelName = sub.Lines[0].VoiceName
		} else if len(sub.Comments) > 0 {
			modelName = sub.Comments[0]
		} else if name, dialogue, tagged := resolveSpeaker(sub.String()); tagged {
			modelName = name
			sub.Lines[0].Items[0].Text = dialogue
		}

		// A speaker tag may carry a per-line speed, e.g. [Matko@1.15] or, for
		// the default speaker, [@1.15]. Split it off before looking the speaker
		// up, so "Matko@1.15" resolves against the configured "Matko".
		modelName, lineSpeed, hasLineSpeed, specErr := splitSpeakerSpec(modelName)
		if specErr != nil {
			return nil, fmt.Errorf("subtitle #%d: %w", i+1, specErr)
		}

		var speakerConfig SpeakerConfig
		offset := 0
		if modelName != "" {
			var err error
			speakerConfig, err = lookupSpeakerModel(modelName, config)
			if err != nil {
				return nil, fmt.Errorf("subtitle #%d: %w", i+1, err)
			}
			offset = modelChannels[modelName]
		} else {
			speakerConfig = config.Default
		}

		model := Model{name: speakerConfig.Name, model: speakerConfig.Model, offset: offset,
			speed: speakerConfig.Speed, ttsModel: resolveTTSModel(speakerConfig, config)}

		// Applied before provider.CachePath: the effective speed is already
		// part of ElevenLabs' cache checksum, so each speed of a line is its
		// own file and every take stays on disk. audio.cpp ignores it (see
		// HANDOFF-audiocpp.local.md §2 #3: fitting to the cue already sets
		// the pace, so a separate speed knob has no meaning for that
		// provider) -- it's still recorded on the model for display/session
		// purposes, just never sent to audio.cpp or folded into its cache key.
		if hasLineSpeed {
			model.speed = lineSpeed
		}

		if provider.Name() == "audiocpp" {
			if speakerConfig.VoiceRefPath == "" {
				return nil, fmt.Errorf("%w: subtitle #%d: voice_ref_path is not set for speaker %q",
					ErrProviderNotConfigured, i+1, speakerConfig.Name)
			}
			var err error
			model.voiceRefPath = speakerConfig.VoiceRefPath
			model.referenceText, err = loadReferenceText(speakerConfig.ReferenceTextPath, referenceTextCache)
			if err != nil {
				return nil, fmt.Errorf("%w: subtitle #%d: loading reference text for speaker %q: %v",
					ErrProviderNotConfigured, i+1, speakerConfig.Name, err)
			}
		}

		item := Item{
			Sub:        sub,
			Model:      model,
			Path:       provider.CachePath(root, sub, model),
			MergedFrom: res.mergedFrom,
		}

		items = append(items, item)
	}

	return items, nil
}

// generateMissingVoiceLines synthesizes every cue that isn't already
// cached via provider, measures each cue's real duration, and returns them
// in subtitle order.
//
// Every failure is returned rather than fatal. This used to log.Fatal,
// which was survivable for the run command (one process, one subtitle
// file) but not for serve: one bad voice ID or one expired key took the
// whole HTTP server down mid-request, for every session using it, without
// writing a response.
func generateMissingVoiceLines(provider TTSProvider, items []Item) ([]AudioFile, error) {
	audioFiles := make([]AudioFile, 0)
	for i, item := range items {
		if item.Path.Path != "" {
			duration, err := provider.ReadDuration(item.Path.Path)
			if err != nil {
				return nil, fmt.Errorf("reading cached audio %s duration: %w", item.Path.Path, err)
			}
			audioFiles = append(audioFiles, AudioFile{
				Item:     item,
				Offset:   item.Sub.StartAt,
				Channel:  item.Model.offset,
				Duration: duration,
			})
			continue
		}

		path, duration, err := provider.Synthesize(items, i)
		if err != nil {
			return nil, fmt.Errorf("synthesizing cue #%d as %s via %s: %w", item.Sub.Index+1, item.Model.name, provider.Name(), err)
		}
		item.Path.Path = path

		audioFiles = append(audioFiles, AudioFile{
			Item:     item,
			Offset:   item.Sub.StartAt,
			Channel:  item.Model.offset,
			Duration: duration,
		})
	}

	return audioFiles, nil
}

// normalizeCeilingDb and normalizeMaxBoostDb bound normalizationGain: the
// ceiling stops a gained-up clip from clipping, and the boost cap stops a
// near-silent or broken generation (mostly noise floor, no real signal)
// from being amplified into audible noise instead of being left as an
// obvious outlier for manual review.
const (
	normalizeCeilingDb  = -1.0
	normalizeMaxBoostDb = 24.0
)

// resampleLinear converts samples from srcRate to dstRate by linear
// interpolation. Adequate for speech (per HANDOFF-audiocpp.local.md §4);
// a windowed-sinc resampler would sound marginally better but isn't needed
// for anything currently mixed into this engine's output -- audio.cpp's
// IndexTTS model returns a clean 22,050 Hz, exactly half the mix's 44,100
// Hz, so every source sample lands exactly on an output sample and the
// interpolated points are the midpoints between them.
func resampleLinear(samples []int, srcRate, dstRate int) []int {
	if srcRate == dstRate || len(samples) == 0 {
		return samples
	}
	outLen := int(float64(len(samples)) * float64(dstRate) / float64(srcRate))
	out := make([]int, outLen)
	for i := range out {
		srcPos := float64(i) * float64(srcRate) / float64(dstRate)
		i0 := int(srcPos)
		frac := srcPos - float64(i0)
		if i0 >= len(samples)-1 {
			out[i] = samples[len(samples)-1]
			continue
		}
		out[i] = int(float64(samples[i0])*(1-frac) + float64(samples[i0+1])*frac)
	}
	return out
}

// normalizationGain computes the linear gain to bring samples' RMS level to
// targetDb (dBFS, negative). ElevenLabs generations vary widely in level
// from clip to clip -- gaps of 70dB+ between cues in the same track are not
// unusual -- so mixing them unmodified leaves some lines near-inaudible and
// others painfully loud right next to each other.
func normalizationGain(samples []int, targetDb float64) float64 {
	if len(samples) == 0 {
		return 1
	}

	var sumSquares float64
	peak := 0
	for _, s := range samples {
		sumSquares += float64(s) * float64(s)
		abs := s
		if abs < 0 {
			abs = -abs
		}
		if abs > peak {
			peak = abs
		}
	}
	if peak == 0 {
		return 1 // true silence: nothing to normalize
	}

	const fullScale = 32768.0
	rms := math.Sqrt(sumSquares / float64(len(samples)))
	rmsDb := 20 * math.Log10(rms/fullScale)
	peakDb := 20 * math.Log10(float64(peak)/fullScale)

	gainDb := targetDb - rmsDb
	if gainDb > normalizeMaxBoostDb {
		gainDb = normalizeMaxBoostDb
	}
	if peakDb+gainDb > normalizeCeilingDb {
		gainDb = normalizeCeilingDb - peakDb
	}

	return math.Pow(10, gainDb/20)
}

func generateFinalAudioFile(provider TTSProvider, files []AudioFile, outputPath string, normalizeTargetDb float64) error {
	const sampleRate = 44100
	const bitDepth = 16

	numChannels := 0
	for _, file := range files {
		numChannels = max(numChannels, file.Channel+1)
	}

	var maxEndTime time.Duration
	for _, file := range files {
		maxEndTime = max(maxEndTime, file.Offset+file.Duration)
	}

	totalFrames := int(maxEndTime.Seconds() * float64(sampleRate))
	totalSamples := totalFrames * numChannels
	mixBuffer := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: numChannels,
			SampleRate:  sampleRate,
		},
		Data:           make([]int, totalSamples),
		SourceBitDepth: bitDepth,
	}

	for _, file := range files {
		path := file.Item.Path.Path
		samples, srcRate, err := provider.DecodeSamples(path)
		if err != nil {
			return fmt.Errorf("failed to read audio data from %s: %w", path, err)
		}
		samples = resampleLinear(samples, srcRate, sampleRate)

		gain := 1.0
		if normalizeTargetDb < 0 {
			gain = normalizationGain(samples, normalizeTargetDb)
		}

		startFrame := int(file.Offset.Seconds() * float64(sampleRate))
		for i, sample := range samples {
			frame := startFrame + i
			if frame >= totalFrames {
				break
			}
			pos := (frame * numChannels) + file.Channel
			if pos < len(mixBuffer.Data) {
				mixBuffer.Data[pos] += int(math.Round(float64(sample) * gain))
			}
		}
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer out.Close()

	enc := wav.NewEncoder(out, sampleRate, bitDepth, numChannels, 1)
	defer enc.Close()

	return enc.Write(mixBuffer)
}
