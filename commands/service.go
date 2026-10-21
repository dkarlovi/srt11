package commands

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Sentinel errors the HTTP layer classifies on. Generate and Export can
// each fail for two quite different reasons -- the caller has spent a
// quota or hasn't finished the work yet, versus ElevenLabs or the disk
// failing underneath us -- and those need different status codes. Matching
// on message text would be brittle, so the two caller-fault cases get
// sentinels and everything else is treated as upstream.
var (
	// ErrRunCapReached means the session has already spent its RunCap;
	// no audio was generated and nothing was billed.
	ErrRunCapReached = errors.New("run cap reached")

	// ErrExportBlocked means the session isn't exportable yet -- cues
	// still overlap past tolerance. Nothing is wrong with the service;
	// the caller has more editing to do.
	ErrExportBlocked = errors.New("export blocked")
)

// ServiceConfig is dub-studio's own app-level policy layered on top of the
// engine's config.yaml (auth key, voice models) -- tolerance/margin/speed
// caps, merge/overlap knobs, and where a session's working files live.
type ServiceConfig struct {
	Engine  *Config
	WorkDir string
	// CacheDir is where the engine looks up and writes cached mp3 takes.
	// Empty means the session vtt's own directory (i.e. WorkDir). serve
	// sets it to one directory shared by every session, so a per-session
	// WorkDir doesn't mean re-synthesizing identical lines for every new
	// browser -- see parseSubtitleFile on why sharing is safe.
	CacheDir           string
	MergeThresholdMs   int
	MergeMaxMs         int
	OverlapToleranceMs int
	NormalizeTargetDb  int
	AutoFix            AutoFixConfig
	RunCap             int // 0 = unlimited
	Reduce             ReduceServiceConfig
}

// ReduceServiceConfig configures the AI line-shortening step (see reduce.go)
// -- a separate Anthropic spend, not gated by RunCap. Client, when set,
// overrides APIKey/Model entirely (a test double); LocalService.Reduce
// constructs a real anthropicHTTPClient from APIKey/Model otherwise.
type ReduceServiceConfig struct {
	APIKey string
	Model  string
	Client AnthropicClient
}

// ExportResult reports the outcome of an Export call, mirroring dub-studio's
// app.py /export manifest.
type ExportResult struct {
	WavPath         string `json:"wav_path"`
	StillOverBudget []int  `json:"still_over_budget"`
	Basket          []int  `json:"basket"`
}

// Service is the orchestration contract for the stepped upload/cleanup/
// generate/autofix/export workflow dub-studio's app.py drives today over
// HTTP. LocalService below is the "files first" implementation -- it runs
// entirely in-process against the local engine. A future HTTP-backed
// implementation (or a mock, for testing) can satisfy the same interface,
// so callers -- the file-driven CLI today, an HTTP layer later -- don't
// need to change when the implementation does.
type Service interface {
	Upload(name, path string) (*Session, []TagViolation, error)
	Cleanup(s *Session) []CueEdit
	// Generate spends a run. provider selects the TTS provider for this
	// call ("elevenlabs" | "audiocpp" | "" to reuse whatever the session
	// last used, defaulting to "elevenlabs" if it's never generated before)
	// -- dkarlovi's requirement that either provider can be requested
	// per-request through the API, not only by a global config switch. The
	// choice is persisted on Session.Provider and Export reuses it.
	Generate(s *Session, provider string) (GenerateResult, error)
	AutoFix(s *Session) AutoFixResult
	Reduce(s *Session) (ReduceResult, error)
	Export(s *Session) (ExportResult, error)
	UpdateCue(s *Session, index int, text *string, speed *float64, skip *bool) (*SessionCue, error)
}

// LocalService implements Service by calling the engine's own parsing,
// generation, overlap-detection and mixing functions in-process. This is
// what replaces dub-studio's current approach of shelling out to a
// separately-built srt11 binary and scraping its printed report.
type LocalService struct {
	cfg ServiceConfig
}

func NewLocalService(cfg ServiceConfig) *LocalService {
	return &LocalService{cfg: cfg}
}

func (ls *LocalService) Upload(name, path string) (*Session, []TagViolation, error) {
	return NewSessionFromSubtitleFile(name, path, ls.cfg.Engine)
}

func (ls *LocalService) Cleanup(s *Session) []CueEdit {
	texts := make([]string, len(s.Cues))
	for i, c := range s.Cues {
		texts[i] = c.CurrentText
	}
	edits := mechanicalCleanup(texts)
	for _, e := range edits {
		s.Cues[e.Index-1].CurrentText = e.After
	}
	// Marks cleanup as done for this session, mirroring dub-studio's
	// app.py setting Session.first_pass_report after /cleanup -- callers
	// (e.g. the legacy frontend) use its presence to advance past the
	// "uploaded" phase, distinct from whether any cue's text actually
	// changed.
	s.FirstPassReport = map[string]any{"cleanup": edits}
	return edits
}

func (ls *LocalService) sessionVTTPath() string {
	return filepath.Join(ls.cfg.WorkDir, "_session.vtt")
}

func (ls *LocalService) writeAndParse(cues []*SessionCue, provider TTSProvider) ([]Item, error) {
	vttPath := ls.sessionVTTPath()
	if err := os.WriteFile(vttPath, writeSessionVTT(cues, ls.cfg.Engine.Default.Name), 0o644); err != nil {
		return nil, fmt.Errorf("writing session vtt: %w", err)
	}
	return parseSubtitleFile(ls.cfg.Engine, vttPath, ls.cfg.MergeThresholdMs, ls.cfg.MergeMaxMs, ls.cfg.CacheDir, provider)
}

// ErrProviderNotConfigured means the caller asked for a TTS provider this
// server has no config.yaml settings for -- a request/config problem, not
// an upstream failure, so the HTTP layer maps it to 400 rather than 502.
var ErrProviderNotConfigured = errors.New("provider not configured")

// resolveProvider is the session-layer equivalent of the package-level
// resolveProvider in run.go (used by the standalone run/CLI commands) --
// same provider names, same config.yaml fields, wrapped so its error can be
// classified as caller-fault by the HTTP layer (see ErrProviderNotConfigured
// and generateErrorStatus).
func (ls *LocalService) resolveProvider(name string) (TTSProvider, error) {
	provider, err := resolveProvider(name, ls.cfg.Engine)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, err)
	}
	return provider, nil
}

// maxConvergenceRounds caps how many internal speed-bump-then-remeasure
// cycles a single Generate call runs before giving up and reporting
// whatever state it reached. AutoFixDurations' own speed-cap logic already
// guarantees any one cue converges or gets basketed in a bounded number of
// rounds in the common case (ElevenLabs' speed parameter is close enough to
// linear that 1-2 corrective rounds are normal); this is a safety net
// against a pathological oscillation, not an expected ceiling.
const maxConvergenceRounds = 5

// generateRoundResult captures what one internal ElevenLabs round of
// Generate's automatic speed-convergence loop measured, before AutoFix
// decides whether another round is needed.
type generateRoundResult struct {
	billedPositions []int // 1-based positions in this round's post-merge item list that were freshly synthesized (cache miss)
	itemCount       int
	overlapConflict bool
	overlappingCues []int
}

// aggregateGenerateRounds combines every round's billing into one final
// report: the full set of distinct positions billed across all rounds (a
// cue that needed two speed-corrective rounds to converge is one billed
// cue, not two -- see Generate's doc comment below), and the final round's
// overlap state, since an earlier round's overlap may have resolved once
// speeds changed. Pure and separable from the network calls so it's
// testable without a fake ElevenLabs client.
func aggregateGenerateRounds(rounds []generateRoundResult) GenerateResult {
	billed := map[int]bool{}
	var last generateRoundResult
	for _, r := range rounds {
		for _, pos := range r.billedPositions {
			billed[pos] = true
		}
		last = r
	}
	generated := make([]int, 0, len(billed))
	for pos := range billed {
		generated = append(generated, pos)
	}
	sort.Ints(generated)
	return GenerateResult{
		Generated:       generated,
		SkippedCached:   last.itemCount - len(generated),
		OverlapConflict: last.overlapConflict,
		OverlappingCues: last.overlappingCues,
	}
}

// generateOnce is one TTS round against provider: parse+merge the current
// cues, synthesize whatever isn't already cached, measure real durations,
// and detect overlaps. Ported from dub-studio's core.generate_cues, adapted
// from subprocess+report-text scraping to direct in-process calls and
// struct access. Not independently unit-tested (it needs a real provider
// client) -- verified live, same as the rest of this file's network-touching
// code; aggregateGenerateRounds above carries the orchestration logic that
// *is* unit-tested.
func (ls *LocalService) generateOnce(s *Session, provider TTSProvider) (generateRoundResult, error) {
	items, err := ls.writeAndParse(s.Cues, provider)
	if err != nil {
		return generateRoundResult{}, err
	}

	audioFiles, err := generateMissingVoiceLines(provider, items)
	if err != nil {
		return generateRoundResult{}, err
	}

	overlapTolerance := time.Duration(ls.cfg.OverlapToleranceMs) * time.Millisecond
	overlapsByFirst, overlaps := annotateOverlaps(audioFiles, findOverlaps(audioFiles, overlapTolerance))

	// Session.Provider changes only here, together with the cues whose
	// measurements it describes: a Generate that fails before this point
	// leaves both as they were, so Export keeps reproducing the last
	// generation that actually succeeded.
	s.Cues = rebuildCuesAfterGenerate(s.Cues, audioFiles, overlapsByFirst, provider.Name())
	s.Provider = provider.Name()

	billedPositions := make([]int, 0)
	for i, item := range items {
		if item.Path.Path == "" {
			billedPositions = append(billedPositions, i+1)
		}
	}

	overlapping := make([]int, 0)
	for _, c := range s.Cues {
		if c.OverlapFlagged {
			overlapping = append(overlapping, c.Index)
		}
	}

	return generateRoundResult{
		billedPositions: billedPositions,
		itemCount:       len(items),
		overlapConflict: len(overlaps) > 0,
		overlappingCues: overlapping,
	}, nil
}

// runConvergenceLoop drives the round-counting/stopping logic for
// Generate's automatic speed-convergence loop: call generate, then
// autoFix; stop once autoFix makes no further speed changes (converged --
// everything either fits or is basketed) or maxConvergenceRounds is hit.
// Takes its two steps as closures so this control flow is unit-testable
// without a real ElevenLabs client or Anthropic key -- generateOnce and
// AutoFix themselves are not (see generateOnce's own doc comment).
func runConvergenceLoop(generate func() (generateRoundResult, error), autoFix func() AutoFixResult) ([]generateRoundResult, []AutoFixResult, error) {
	var rounds []generateRoundResult
	var afResults []AutoFixResult
	for round := 0; round < maxConvergenceRounds; round++ {
		roundResult, err := generate()
		if err != nil {
			return rounds, afResults, err
		}
		rounds = append(rounds, roundResult)

		afResult := autoFix()
		afResults = append(afResults, afResult)
		if len(afResult.Sped) == 0 {
			break
		}
	}
	return rounds, afResults, nil
}

// Generate is THE function that spends a run. Per Matko's 2026-09-18
// redesign, one Generate call no longer stops after a single round: it
// automatically loops generateOnce -> AutoFix -> (if AutoFix bumped any
// cue's speed) generateOnce again, silently, until AutoFix makes no
// further speed changes (converged: everything either fits or is
// basketed) or maxConvergenceRounds is hit. By the time Generate returns,
// the only cues still flagged are ones that genuinely can't be fixed by
// speed alone -- real candidates for Reduce or a human edit, not "just
// needs one more click." This still only enforces the run cap and
// increments RunCount ONCE per Generate call, regardless of how many
// internal rounds ran -- from the user's perspective they asked for one
// generation, not several.
func (ls *LocalService) Generate(s *Session, providerName string) (GenerateResult, error) {
	if ls.cfg.RunCap > 0 && s.RunCount >= ls.cfg.RunCap {
		return GenerateResult{}, fmt.Errorf(
			"%w (%d generations for this video); no audio was generated, reset the session to continue",
			ErrRunCapReached, ls.cfg.RunCap)
	}

	// "" means "whatever this session used last" (first-ever Generate on a
	// fresh session falls through to "elevenlabs", resolveProvider's own
	// default) -- so a caller that never mentions provider gets ElevenLabs
	// unchanged, and a caller that picked audio.cpp once doesn't have to
	// keep repeating it on every regenerate.
	effectiveProvider := providerName
	if effectiveProvider == "" {
		effectiveProvider = s.Provider
	}
	provider, err := ls.resolveProvider(effectiveProvider)
	if err != nil {
		return GenerateResult{}, err
	}

	rounds, afResults, err := runConvergenceLoop(
		func() (generateRoundResult, error) { return ls.generateOnce(s, provider) },
		func() AutoFixResult { return ls.AutoFix(s) },
	)
	if err != nil {
		return GenerateResult{}, err
	}

	result := aggregateGenerateRounds(rounds)
	if len(result.Generated) > 0 {
		s.RunCount++
	}
	result.RunNumber = s.RunCount
	result.Provider = provider.Name()

	spedCues := map[int]bool{}
	for _, af := range afResults {
		for _, sp := range af.Sped {
			spedCues[sp.Index] = true
		}
	}
	result.AutoSped = len(spedCues)
	result.StillBasketed = len(s.Basket())
	return result, nil
}

func (ls *LocalService) AutoFix(s *Session) AutoFixResult {
	afCues := toAutoFixCues(s.Cues)
	result := AutoFixDurations(afCues, ls.cfg.AutoFix)
	applyAutoFixResults(s.Cues, afCues)
	return result
}

// Reduce runs the AI line-shortening pass (see reduce.go) over every
// basketed cue. Mirrors dub-studio's app.py POST /reduce: a missing API key
// is the one error case that surfaces to the caller (a per-cue AI failure
// during ReduceText itself is reported as a decline, not an error here).
func (ls *LocalService) Reduce(s *Session) (ReduceResult, error) {
	client := ls.cfg.Reduce.Client
	if client == nil {
		if ls.cfg.Reduce.APIKey == "" {
			return ReduceResult{}, fmt.Errorf(
				"ANTHROPIC_API_KEY is not set. Get a key at console.anthropic.com and set it in the environment -- this is a separate account from ElevenLabs, and separate from any claude.ai or Claude Code plan.")
		}
		client = newAnthropicClient(ls.cfg.Reduce.APIKey, ls.cfg.Reduce.Model)
	}
	return ReduceText(s.Cues, client), nil
}

// buildExportResult decides whether a same-speaker overlap blocks export,
// and if not, summarizes what's still over budget or basketed. Pure and
// separable from the network call so it can be unit-tested with
// constructed AudioFile fixtures. Ported from dub-studio's core.export_audio.
func buildExportResult(cues []*SessionCue, overlaps []cueOverlap) (ExportResult, error) {
	if len(overlaps) > 0 {
		return ExportResult{}, fmt.Errorf(
			"%w: same-speaker cues still overlap past tolerance; fix or re-speed the flagged lines and Generate again first",
			ErrExportBlocked)
	}
	stillOver := make([]int, 0)
	basket := make([]int, 0)
	for _, c := range cues {
		if c.AudioMs != nil && c.OverageMs() > 0 {
			stillOver = append(stillOver, c.Index)
		}
		if c.NeedsHuman {
			basket = append(basket, c.Index)
		}
	}
	return ExportResult{StillOverBudget: stillOver, Basket: basket}, nil
}

// Export re-runs the identical generation as Generate, against the same
// provider Generate last used (Session.Provider -- never asked again, so
// it can't silently drift to a different provider's audio between the two)
// -- free (the cache means a matching prior generate already paid for
// every cue) so the exported audio matches exactly what was measured, then
// mixes the final WAV if nothing overlaps past tolerance. Ported from
// dub-studio's core.export_audio.
func (ls *LocalService) Export(s *Session) (ExportResult, error) {
	provider, err := ls.resolveProvider(s.Provider)
	if err != nil {
		return ExportResult{}, err
	}

	items, err := ls.writeAndParse(s.Cues, provider)
	if err != nil {
		return ExportResult{}, err
	}
	audioFiles, err := generateMissingVoiceLines(provider, items)
	if err != nil {
		return ExportResult{}, err
	}

	overlapTolerance := time.Duration(ls.cfg.OverlapToleranceMs) * time.Millisecond
	overlaps := findOverlaps(audioFiles, overlapTolerance)

	result, err := buildExportResult(s.Cues, overlaps)
	if err != nil {
		return ExportResult{}, err
	}

	outputPath := filepath.Join(ls.cfg.WorkDir,
		s.Name+"_"+time.Now().Format("2006-01-02-15-04-05")+".wav")
	if err := generateFinalAudioFile(provider, audioFiles, outputPath, float64(ls.cfg.NormalizeTargetDb)); err != nil {
		return ExportResult{}, fmt.Errorf("writing final audio track: %w", err)
	}
	s.ExportWavPath = outputPath
	result.WavPath = outputPath
	return result, nil
}

// UpdateCue mirrors dub-studio's app.py POST /cue/{index}: a manual text
// edit clears the basket flag (a human already looked at it), and a speed
// edit is always treated as an override so it's tagged on the next
// Generate instead of silently tracking the speaker's configured default.
// skip is independent of both: it's an explicit "accept this line as-is"
// override (see Session.Flagged/Basket, AutoFixDurations, ReduceText), and
// toggling it deliberately leaves NeedsHuman/HumanReason/text/speed alone
// so un-skipping later restores exactly what was there before.
func (ls *LocalService) UpdateCue(s *Session, index int, text *string, speed *float64, skip *bool) (*SessionCue, error) {
	c := cueByIndex(s.Cues, index)
	if c == nil {
		return nil, fmt.Errorf("no cue #%d", index)
	}
	if text != nil {
		c.CurrentText = *text
		c.NeedsHuman = false
		c.HumanReason = ""
	}
	if speed != nil {
		c.Speed = math.Max(float64(MinSpeed), math.Min(float64(MaxSpeed), *speed))
		c.SpeedOverride = true
	}
	if skip != nil {
		c.Skipped = *skip
	}
	return c, nil
}

func toAutoFixCues(cues []*SessionCue) []*AutoFixCue {
	out := make([]*AutoFixCue, 0, len(cues))
	for _, c := range cues {
		out = append(out, &AutoFixCue{
			Index: c.Index, StartMs: c.StartMs, EndMs: c.EndMs, Voice: c.Voice,
			Speed: c.Speed, SpeedOverride: c.SpeedOverride, AudioMs: c.AudioMs,
			OverlapFlagged: c.OverlapFlagged, Skipped: c.Skipped,
			Provider: c.LastGeneratedProvider,
		})
	}
	return out
}

func applyAutoFixResults(cues []*SessionCue, afCues []*AutoFixCue) {
	byIndex := make(map[int]*AutoFixCue, len(afCues))
	for _, af := range afCues {
		byIndex[af.Index] = af
	}
	for _, c := range cues {
		if af, ok := byIndex[c.Index]; ok {
			c.StartMs = af.StartMs
			c.Speed = af.Speed
			c.SpeedOverride = af.SpeedOverride
			c.NeedsHuman = af.NeedsHuman
			c.HumanReason = af.HumanReason
		}
	}
}
