package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/symfony-cli/console"
)

// builtinAutoFixConfig mirrors dub-studio's config.py defaults
// (TOLERANCE_MS = OVERLAP_TOLERANCE_MS = 120, AUTOFIX_MARGIN_MS = 40,
// SPEED_CAPS = {"hana": 1.15, "default": 1.2}) as the session-autofix
// command's out-of-the-box behavior when --autofix-config isn't given.
func builtinAutoFixConfig() AutoFixConfig {
	return AutoFixConfig{
		ToleranceMs:     120,
		AutofixMarginMs: 40,
		SpeedCaps:       map[string]float64{"default": 1.2},
	}
}

func sessionServiceConfig(c *console.Context, engine *Config) ServiceConfig {
	return ServiceConfig{
		Engine:             engine,
		WorkDir:            c.String("work-dir"),
		MergeThresholdMs:   c.Int("merge-lines-threshold-ms"),
		MergeMaxMs:         c.Int("merge-max-ms"),
		OverlapToleranceMs: c.Int("overlap-tolerance-ms"),
		NormalizeTargetDb:  c.Int("normalize-target-db"),
		RunCap:             c.Int("run-cap"),
		// Generate runs AutoFix internally; without this the session-*
		// commands ran it with a zero-value config (0ms tolerance, no speed
		// caps) while serve used these defaults. serve still overrides it
		// with --autofix-config when given.
		AutoFix: builtinAutoFixConfig(),
		Reduce: ReduceServiceConfig{
			APIKey: c.String("anthropic-api-key"),
			Model:  c.String("reduce-model"),
		},
	}
}

func sessionStore(c *console.Context) *FileSessionStore {
	return &FileSessionStore{Dir: c.String("work-dir")}
}

func loadActiveSession(store *FileSessionStore) (*Session, error) {
	s, err := store.Load()
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("no active session in %s -- run session-upload first", store.Dir)
	}
	return s, nil
}

func printJSON(c *console.Context, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(c.App.Writer, "%s\n", data)
	return nil
}

func sessionSummary(s *Session, toleranceMs int) map[string]any {
	flagged := len(s.Flagged(toleranceMs))
	basket := len(s.Basket())
	return map[string]any{
		"name":            s.Name,
		"run_count":       s.RunCount,
		"generated":       s.Generated(),
		"export_wav_path": s.ExportWavPath,
		"counts": map[string]any{
			"total":   len(s.Cues),
			"flagged": flagged,
			"basket":  basket,
			"cleared": len(s.Cues) - flagged,
		},
		"cues": s.Cues,
	}
}

func runSessionUpload(c *console.Context) error {
	path := c.Args().Get("file")
	if path == "" {
		return console.Exit("Missing subtitle file path", 1)
	}
	name := c.String("name")
	if name == "" {
		name = filepath.Base(path)
	}

	engine, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	ls := NewLocalService(sessionServiceConfig(c, engine))
	session, violations, err := ls.Upload(name, path)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error uploading %s: %v", path, err), 1)
	}
	if len(violations) > 0 {
		fmt.Fprintf(c.App.Writer, "Speaker tags must be at the very start of a cue:\n")
		for _, v := range violations {
			fmt.Fprintf(c.App.Writer, "  cue %d: tag %s is not at the start of the line\n", v.Index, v.Tag)
		}
		return console.Exit("Upload rejected", 1)
	}

	if err := sessionStore(c).Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	return printJSON(c, sessionSummary(session, c.Int("overlap-tolerance-ms")))
}

func runSessionCleanup(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	engine, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	ls := NewLocalService(sessionServiceConfig(c, engine))
	edits := ls.Cleanup(session)
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	out := sessionSummary(session, c.Int("overlap-tolerance-ms"))
	out["last_cleanup"] = edits
	return printJSON(c, out)
}

func runSessionGenerate(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	engine, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	ls := NewLocalService(sessionServiceConfig(c, engine))
	result, err := ls.Generate(session, c.String("provider"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error generating: %v", err), 1)
	}
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	out := sessionSummary(session, c.Int("overlap-tolerance-ms"))
	out["last_run"] = result
	return printJSON(c, out)
}

func runSessionAutoFix(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	afCfg := builtinAutoFixConfig()
	if path := c.String("autofix-config"); path != "" {
		afCfg, err = loadAutoFixConfigFile(path)
		if err != nil {
			return console.Exit(fmt.Sprintf("Error reading autofix config: %v", err), 1)
		}
	}

	ls := NewLocalService(ServiceConfig{AutoFix: afCfg})
	result := ls.AutoFix(session)
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	out := sessionSummary(session, c.Int("overlap-tolerance-ms"))
	out["last_autofix"] = result
	return printJSON(c, out)
}

func runSessionReduce(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	ls := NewLocalService(ServiceConfig{Reduce: ReduceServiceConfig{
		APIKey: c.String("anthropic-api-key"),
		Model:  c.String("reduce-model"),
	}})
	result, err := ls.Reduce(session)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reducing: %v", err), 1)
	}
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	out := sessionSummary(session, c.Int("overlap-tolerance-ms"))
	out["last_reduce"] = result
	return printJSON(c, out)
}

func runSessionExport(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	engine, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	ls := NewLocalService(sessionServiceConfig(c, engine))
	result, err := ls.Export(session)
	if err != nil {
		return console.Exit(fmt.Sprintf("Error exporting: %v", err), 1)
	}
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	out := sessionSummary(session, c.Int("overlap-tolerance-ms"))
	out["last_export"] = result
	return printJSON(c, out)
}

func runSessionUpdateCue(c *console.Context) error {
	store := sessionStore(c)
	session, err := loadActiveSession(store)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}

	index := c.Int("index")
	var textPtr *string
	if c.IsSet("text") {
		text := c.String("text")
		textPtr = &text
	}
	var speedPtr *float64
	if c.IsSet("speed") {
		speed := c.Float64("speed")
		speedPtr = &speed
	}
	var skipPtr *bool
	if c.IsSet("skip") {
		skip := c.Bool("skip")
		skipPtr = &skip
	}

	ls := NewLocalService(ServiceConfig{})
	cue, err := ls.UpdateCue(session, index, textPtr, speedPtr, skipPtr)
	if err != nil {
		return console.Exit(err.Error(), 1)
	}
	if err := store.Save(session); err != nil {
		return console.Exit(fmt.Sprintf("Error saving session: %v", err), 1)
	}
	return printJSON(c, cue)
}

func runSessionShow(c *console.Context) error {
	store := sessionStore(c)
	session, err := store.Load()
	if err != nil {
		return console.Exit(fmt.Sprintf("Error loading session: %v", err), 1)
	}
	if session == nil {
		return printJSON(c, map[string]any{"empty": true})
	}
	return printJSON(c, sessionSummary(session, c.Int("overlap-tolerance-ms")))
}

func runSessionReset(c *console.Context) error {
	if err := sessionStore(c).Reset(); err != nil {
		return console.Exit(fmt.Sprintf("Error resetting session: %v", err), 1)
	}
	return printJSON(c, map[string]any{"ok": true})
}
