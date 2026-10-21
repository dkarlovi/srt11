package commands

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/symfony-cli/console"
)

// httpServer is the HTTP transport for the Service/SessionStore contract:
// it depends only on those interfaces, not on LocalService/FileSessionStore
// concretely, so a different implementation of either can be swapped in
// without this file changing. Mirrors dub-studio's app.py route-for-route
// (see that file's own docstring), now including /reduce.
//
// Unlike app.py, this is not single-tenant: each browser gets its own
// session, service and store, keyed by an opaque id in an HttpOnly cookie
// (see resolveTenant). Everything a tenant writes -- session.json, the
// session vtt, the exported wav -- lives under its own work directory, so
// two people hitting one serve process no longer share session state and
// overwrite each other's output. The mp3 cache is deliberately the one
// thing they still share; see localTenantFunc.
//
// The frontend itself is embedded (embeddedIndexHTML, see webassets.go) so
// this binary is fully self-contained and needs no ~/dub-studio checkout at
// runtime; staticDir, when set, overrides that with a directory on disk --
// only useful for iterating on the frontend locally without a rebuild.
type httpServer struct {
	// mu guards tenants only. Session state is locked per tenant, so one
	// browser's Generate -- minutes of ElevenLabs round trips, holding
	// its tenant lock the whole way -- doesn't block anyone else.
	mu      sync.Mutex
	tenants map[string]*tenant

	// newTenant builds the state for a session id on first contact. A
	// field rather than a method so tests can inject a fake service
	// without a config or a real work directory.
	newTenant newTenantFunc

	staticDir   string
	runCap      int
	toleranceMs int
}

// tenant is one browser's isolated slice of server state. mu guards
// session against concurrent requests from that same browser.
type tenant struct {
	mu      sync.Mutex
	id      string
	session *Session
	service Service
	store   SessionStore
}

type newTenantFunc func(id string) (*tenant, error)

// localTenantFunc builds the real on-disk tenant for a session id:
// baseCfg with WorkDir moved down into <work-dir>/sessions/<id>, and the
// session file store alongside it. Since the session vtt, the mp3 cache
// and the exported wav all derive from WorkDir, rewriting that one field
// scopes all of them per session.
//
// All but the cache, that is: CacheDir is pinned to <work-dir>/cache, one
// directory shared by every tenant, on purpose. The engine's cache key is
// content-addressed -- md5(voiceID+ttsModel+speed+text), see
// generatePathTemplate -- so a take cached by one session is byte-identical
// to what any other session would synthesize for the same line by the same
// voice. Scoping the cache per session would mean every new cookie
// re-synthesizes the entire video from scratch.
func localTenantFunc(baseCfg ServiceConfig) newTenantFunc {
	root := baseCfg.WorkDir
	return func(id string) (*tenant, error) {
		cfg := baseCfg
		cfg.WorkDir = filepath.Join(root, "sessions", id)
		if cfg.CacheDir == "" {
			cfg.CacheDir = filepath.Join(root, "cache")
		}
		// Both are directories this process invents, so nothing else
		// has created them; the engine and the store only ever write
		// individual files into them.
		for _, dir := range []string{cfg.WorkDir, cfg.CacheDir} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("creating %s: %w", dir, err)
			}
		}
		store := &FileSessionStore{Dir: cfg.WorkDir}
		session, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("loading session: %w", err)
		}
		return &tenant{
			id:      id,
			session: session,
			service: NewLocalService(cfg),
			store:   store,
		}, nil
	}
}

func newHTTPServer(newTenant newTenantFunc, staticDir string, runCap, toleranceMs int) *httpServer {
	return &httpServer{
		tenants:     map[string]*tenant{},
		newTenant:   newTenant,
		staticDir:   staticDir,
		runCap:      runCap,
		toleranceMs: toleranceMs,
	}
}

// sessionCookieName carries a browser's opaque session id. HttpOnly: the
// frontend has no reason to read it, and doesn't need to -- every call in
// index.html is a relative-URL fetch, which defaults to same-origin
// credentials and so returns the cookie automatically. That's why
// per-session state needed no frontend change at all.
const sessionCookieName = "dubstudio_session"

// newSessionID returns 16 crypto/rand bytes, hex encoded.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validSessionID gates a cookie value before it is ever used as a path
// element. The id becomes a directory name under <work-dir>/sessions, and
// the cookie is entirely client-controlled, so an unvalidated value is a
// path traversal ("../../../etc") handed straight to MkdirAll. Accepts
// only what newSessionID itself produces: exactly 32 lowercase hex
// characters.
func validSessionID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveTenant returns the calling browser's tenant, creating it and
// issuing a cookie on first contact. A missing, malformed or unrecognized
// cookie value is all the same thing: the caller gets a fresh id rather
// than an error, so a stale cookie from an earlier serve process quietly
// starts a new session instead of failing every request.
//
// Handlers must call this before writing any response body, because a
// freshly issued cookie is a header and has to precede WriteHeader.
func (h *httpServer) resolveTenant(w http.ResponseWriter, r *http.Request) (*tenant, bool) {
	id := ""
	if c, err := r.Cookie(sessionCookieName); err == nil && validSessionID(c.Value) {
		id = c.Value
	}
	if id == "" {
		var err error
		if id, err = newSessionID(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return nil, false
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    id,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	// newTenant runs under h.mu so two concurrent first requests from one
	// browser share a tenant rather than racing to build one each. It only
	// does a couple of MkdirAlls and a session.json read, so holding the
	// map lock across it costs nothing measurable.
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tenants[id]; ok {
		return t, true
	}
	t, err := h.newTenant(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	h.tenants[id] = t
	return t, true
}

func (h *httpServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /session", h.handleGetSession)
	mux.HandleFunc("POST /upload", h.handleUpload)
	mux.HandleFunc("POST /cleanup", h.handleCleanup)
	mux.HandleFunc("POST /generate", h.handleGenerate)
	mux.HandleFunc("POST /autofix", h.handleAutoFix)
	mux.HandleFunc("POST /reduce", h.handleReduce)
	mux.HandleFunc("POST /cue/{index}", h.handleUpdateCue)
	mux.HandleFunc("POST /export", h.handleExport)
	mux.HandleFunc("GET /export/download", h.handleExportDownload)
	mux.HandleFunc("POST /reset", h.handleReset)

	mux.HandleFunc("GET /{$}", h.handleIndex)
	if h.staticDir != "" {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(h.staticDir))))
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"detail": message})
}

// handleIndex serves the frontend: from staticDir when explicitly given
// (local dev iteration on a working copy of the frontend without a
// rebuild), otherwise the embedded default, which is what makes this
// binary self-contained -- no ~/dub-studio checkout required at runtime.
func (h *httpServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if h.staticDir != "" {
		http.ServeFile(w, r, filepath.Join(h.staticDir, "index.html"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(embeddedIndexHTML)
}

func (h *httpServer) handleGetSession(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	writeJSON(w, http.StatusOK, sessionDTO(t.session, h.toleranceMs, h.runCap))
}

func (h *httpServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no file uploaded: "+err.Error())
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "dubstudio-upload-*.vtt")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp.Close()

	session, violations, err := t.service.Upload(header.Filename, tmp.Name())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(violations) > 0 {
		var detail strings.Builder
		for i, v := range violations {
			if i > 0 {
				detail.WriteString("; ")
			}
			fmt.Fprintf(&detail, "cue %d: tag %s is not at the start of the line", v.Index, v.Tag)
		}
		writeError(w, http.StatusBadRequest,
			"Speaker tags must be at the very start of a cue. "+detail.String())
		return
	}

	t.session = session
	if !t.save(w) {
		return
	}
	writeJSON(w, http.StatusOK, sessionDTO(t.session, h.toleranceMs, h.runCap))
}

// save persists the current session, writing a 500 and returning false on
// failure so callers can bail out of their handler.
func (t *tenant) save(w http.ResponseWriter) bool {
	if err := t.store.Save(t.session); err != nil {
		writeError(w, http.StatusInternalServerError, "saving session: "+err.Error())
		return false
	}
	return true
}

func (t *tenant) requireSession(w http.ResponseWriter) bool {
	if t.session == nil {
		writeError(w, http.StatusBadRequest, "No active session.")
		return false
	}
	return true
}

func (h *httpServer) handleCleanup(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	edits := t.service.Cleanup(t.session)
	if !t.save(w) {
		return
	}
	out := sessionDTO(t.session, h.toleranceMs, h.runCap)
	out["last_cleanup"] = map[string]any{"cleanup": edits}
	writeJSON(w, http.StatusOK, out)
}

func (h *httpServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	// provider selects the TTS provider for this call -- dkarlovi's
	// requirement that either provider can be requested per-request
	// through the API. Omitted or "" reuses whatever this session last
	// used, defaulting to ElevenLabs (see LocalService.Generate).
	var payload struct {
		Provider string `json:"provider"`
	}
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	result, err := t.service.Generate(t.session, payload.Provider)
	if err != nil {
		writeError(w, generateErrorStatus(err), err.Error())
		return
	}
	if !t.save(w) {
		return
	}
	out := sessionDTO(t.session, h.toleranceMs, h.runCap)
	out["last_run"] = result
	writeJSON(w, http.StatusOK, out)
}

func (h *httpServer) handleAutoFix(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	result := t.service.AutoFix(t.session)
	if !t.save(w) {
		return
	}
	out := sessionDTO(t.session, h.toleranceMs, h.runCap)
	out["last_autofix"] = result
	writeJSON(w, http.StatusOK, out)
}

func (h *httpServer) handleReduce(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	result, err := t.service.Reduce(t.session)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !t.save(w) {
		return
	}
	out := sessionDTO(t.session, h.toleranceMs, h.runCap)
	out["last_reduce"] = result
	writeJSON(w, http.StatusOK, out)
}

func (h *httpServer) handleExport(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	stillOver := t.session.Flagged(h.toleranceMs)
	basket := t.session.Basket()

	result, err := t.service.Export(t.session)
	if err != nil {
		writeError(w, exportErrorStatus(err), err.Error())
		return
	}
	if !t.save(w) {
		return
	}

	manifest := map[string]any{
		"video":             t.session.Name,
		"cue_count":         len(t.session.Cues),
		"still_over_budget": indices(stillOver),
		"basket":            indices(basket),
		"wav_path":          result.WavPath,
	}
	out := sessionDTO(t.session, h.toleranceMs, h.runCap)
	out["last_export"] = manifest
	writeJSON(w, http.StatusOK, out)
}

func (h *httpServer) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	session := t.session
	t.mu.Unlock()

	if session == nil || session.ExportWavPath == "" {
		writeError(w, http.StatusNotFound, "No exported audio yet.")
		return
	}
	if _, err := os.Stat(session.ExportWavPath); err != nil {
		writeError(w, http.StatusNotFound, "Exported file is no longer on disk.")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(session.ExportWavPath)))
	http.ServeFile(w, r, session.ExportWavPath)
}

func (h *httpServer) handleUpdateCue(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requireSession(w) {
		return
	}

	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cue index")
		return
	}

	var payload struct {
		Text  *string  `json:"text"`
		Speed *float64 `json:"speed"`
		Skip  *bool    `json:"skip"`
	}
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	cue, err := t.service.UpdateCue(t.session, index, payload.Text, payload.Speed, payload.Skip)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("No cue #%d.", index))
		return
	}
	if !t.save(w) {
		return
	}
	writeJSON(w, http.StatusOK, cueDTO(cue))
}

func (h *httpServer) handleReset(w http.ResponseWriter, r *http.Request) {
	t, ok := h.resolveTenant(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.session = nil
	if err := t.store.Reset(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// generateErrorStatus and exportErrorStatus separate "the caller has more
// to do" from "something failed underneath us".
//
// Both handlers used to return one fixed status for every failure --
// Generate always 429, Export always 409 -- which was accurate only while
// the only reachable failure was the caller's own quota or unresolved
// overlaps. Since generateMissingVoiceLines returns its errors instead of
// killing the process, an ElevenLabs outage or a bad voice ID reaches
// here too, and reporting that as 429 would drive the frontend's
// 429-specific branch (index.html's call(): a sticky "you've used all
// your runs" toast plus a refresh) to say something false about an
// upstream failure. 502 falls through to its generic !r.ok branch, which
// toasts whatever is in detail -- so no frontend change is needed.
func generateErrorStatus(err error) int {
	if errors.Is(err, ErrRunCapReached) {
		return http.StatusTooManyRequests
	}
	if errors.Is(err, ErrProviderNotConfigured) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func exportErrorStatus(err error) int {
	if errors.Is(err, ErrExportBlocked) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrProviderNotConfigured) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func indices(cues []*SessionCue) []int {
	out := make([]int, 0, len(cues))
	for _, c := range cues {
		out = append(out, c.Index)
	}
	return out
}

// cueDTO adapts the general SessionCue model to the exact shape dub-
// studio's existing frontend (static/index.html) expects: a lowercase
// voice name (that UI is hardcoded to the matko/hana pair), and derived
// flagged/est_flagged/dirty fields dub-studio's own core.Cue exposes as
// properties. est_flagged/est_overage_ms are always zero-valued -- this Go
// service has no mock/estimate mode.
//
// flagged means NeedsHuman, not "runs past its own subtitle window" --
// found live 2026-09-18: AutoFix's real budget (time until the same voice
// speaks again, see realBudgetsMs in autofix.go) is deliberately more
// generous than the subtitle's own window, which exists for reading text
// on screen, not for constraining an audio-only voice track. On a real
// 132-cue session, 46 cues ran past their own window but well within real
// budget and were correctly left alone by AutoFix -- displaying those as
// "flagged" (the old behavior, using OverageMs()/toleranceMs) buried the
// 4 cues that actually needed attention in noise. NeedsHuman is already
// computed against the correct (real budget) metric, so it's the right
// signal here; Session.Flagged (used for the export manifest) keeps the
// separate raw-window meaning, which is a legitimate video-sync concern
// distinct from "does this need Reduce/a human."
func cueDTO(c *SessionCue) map[string]any {
	overageMs := c.OverageMs()
	flagged := c.AudioMs != nil && c.NeedsHuman && !c.Skipped
	return map[string]any{
		"index":           c.Index,
		"start_ms":        c.StartMs,
		"end_ms":          c.EndMs,
		"voice":           strings.ToLower(c.Voice),
		"source_text":     c.SourceText,
		"current_text":    c.CurrentText,
		"speed":           c.Speed,
		"speed_override":  c.SpeedOverride,
		"audio_ms":        c.AudioMs,
		"cache_path":      c.CachePath,
		"overlap_flagged": c.OverlapFlagged,
		"needs_human":     c.NeedsHuman,
		"human_reason":    c.HumanReason,
		"skipped":         c.Skipped,
		"ai_suggestion":   c.AiSuggestion,
		"ai_note":         c.AiNote,
		"window_ms":       c.WindowMs(),
		"overage_ms":      overageMs,
		"est_overage_ms":  0,
		"flagged":         flagged,
		"est_flagged":     false,
		"dirty":           c.Dirty(),
	}
}

func sessionDTO(s *Session, toleranceMs, runCap int) map[string]any {
	if s == nil {
		return map[string]any{"empty": true, "mode": "real"}
	}

	cues := make([]map[string]any, 0, len(s.Cues))
	flaggedCount, basketCount := 0, 0
	for _, c := range s.Cues {
		d := cueDTO(c)
		cues = append(cues, d)
		if d["flagged"].(bool) {
			flaggedCount++
		}
		if c.NeedsHuman && !c.Skipped {
			basketCount++
		}
	}

	return map[string]any{
		"name":            s.Name,
		"run_count":       s.RunCount,
		"cap":             runCap,
		"tolerance_ms":    toleranceMs,
		"mode":            "real",
		"generated":       s.Generated(),
		"export_wav_path": s.ExportWavPath,
		"provider":        s.Provider,
		"counts": map[string]any{
			"total":       len(s.Cues),
			"flagged":     flaggedCount,
			"est_flagged": 0,
			"basket":      basketCount,
			"cleared":     len(s.Cues) - flaggedCount,
		},
		"first_pass_report": s.FirstPassReport,
		"cues":              cues,
	}
}

func runServe(c *console.Context) error {
	engine, err := readConfig(c.String("config"))
	if err != nil {
		return console.Exit(fmt.Sprintf("Error reading config: %v", err), 1)
	}

	afCfg := builtinAutoFixConfig()
	if path := c.String("autofix-config"); path != "" {
		afCfg, err = loadAutoFixConfigFile(path)
		if err != nil {
			return console.Exit(fmt.Sprintf("Error reading autofix config: %v", err), 1)
		}
	}

	svcCfg := sessionServiceConfig(c, engine)
	svcCfg.AutoFix = afCfg

	// Per-session state is built lazily per browser, not once here: see
	// localTenantFunc for how each session gets its own work directory
	// while still sharing one mp3 cache.
	server := newHTTPServer(localTenantFunc(svcCfg), c.String("static-dir"), c.Int("run-cap"), c.Int("overlap-tolerance-ms"))

	addr := c.String("addr")
	fmt.Fprintf(c.App.Writer, "Listening on %s (work-dir=%s)\n", addr, svcCfg.WorkDir)
	srv := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(server.mux()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		return console.Exit(fmt.Sprintf("Server error: %v", err), 1)
	}
	return nil
}
