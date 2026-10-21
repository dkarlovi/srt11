package commands

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/go-audio/wav"
)

// audio.cpp provider: local voice cloning via IndexTTS2, running on a
// separately-started server on the same machine (HANDOFF-audiocpp.local.md
// §5). This package never starts that server or shells out to anything --
// it only ever talks to an already-running server over HTTP (constraint in
// §4: pure Go, CGO_ENABLED=0, no os/exec).
//
// Ported from the POC's Python reference implementation,
// ~/audio.cpp-bin/poc/dialogue_ab.py (speak/transcribe/word_misses, the
// stage-1 loop in main) -- stage 1 (IndexTTS fit) only; stage 2 (a second
// clone through Fish for "style") was evaluated and rejected, see
// HANDOFF-audiocpp.local.md §11.

const (
	audioCppStructureModel = "indextts-clone"
	audioCppASRModel       = "parakeet-asr"
	audioCppOutRate        = 22050             // IndexTTS's fixed output rate (HANDOFF §4)
	audioCppTimeout        = 900 * time.Second // lazy_load: the first request to a model loads multi-GB weights (HANDOFF §5)
	// audioCppFactorMin allows only a small speed-up. IndexTTS accepts factors
	// down to ~0.55, but squeezing a voice that hard distorts it, not just its
	// tempo: at 0.73-0.83 Hana's clone came out Indian-accented and
	// over-eager, while the same line at natural pace was right
	// (HANDOFF-audiocpp.local.md §11, 2026-09-23). A line that needs more
	// than this is too long for its slot and must be shortened, not squeezed.
	audioCppFactorMin = 0.9
	audioCppFactorMax = 1.95
	audioCppRetries   = 2 // 3 total attempts (k=0..2), matching the POC's default
	audioCppBaseSeed  = 1
)

type audioCppProvider struct {
	baseURL string
	model   string
	client  *http.Client
}

func newAudioCppProvider(cfg AudioCppConfig) *audioCppProvider {
	model := cfg.Model
	if model == "" {
		model = audioCppStructureModel
	}
	return &audioCppProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: audioCppTimeout},
	}
}

func (p *audioCppProvider) Name() string { return "audiocpp" }

// audioCppCacheSlug mirrors elevenLabsProvider.CachePath's filename
// sanitizing (same regex, same 50-rune cap) so cache directories stay
// consistent between providers; kept as its own function since the two
// providers' checksums cover different fields (see CachePath below).
var audioCppSlugRE = regexp.MustCompile(`[,.!?'<>:"/\\|?*\x00-\x1F]`)

func audioCppSlug(text string) string {
	slug := audioCppSlugRE.ReplaceAllString(text, "")
	slug = strings.ToLower(slug)
	slug = strings.ReplaceAll(slug, " ", "_")
	slug = strings.TrimSpace(slug)
	if runes := []rune(slug); len(runes) > 50 {
		slug = string(runes[:50])
	}
	return slug
}

// voiceRefHashCache memoizes each voice reference WAV's content hash --
// read once per distinct file rather than once per cue.
var voiceRefHashCache = make(map[string]string)

func voiceRefContentHash(path string) (string, error) {
	if h, ok := voiceRefHashCache[path]; ok {
		return h, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading voice_ref %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	h := fmt.Sprintf("%x", sum[:8])
	voiceRefHashCache[path] = h
	return h, nil
}

// CachePath's checksum covers the reference WAV's content (so re-recording
// or swapping a voice invalidates old takes), the reference text, the
// model, and the cue's window duration -- duration_factor is derived from
// the window, so a different window is a materially different take, unlike
// ElevenLabs where retiming a cue doesn't change its audio at all (see
// HANDOFF-audiocpp.local.md §9). It deliberately does NOT cover speed --
// audio.cpp ignores @speed entirely (fitting to the cue already sets the
// pace) -- or a seed: any successfully-verified take for the same
// (voice, text, window) is equally valid to reuse regardless of which seed
// produced it, exactly like ElevenLabs' request ID never being part of its
// own cache key. The seed that actually produced a fresh take is recorded
// in the filename (via the %s template placeholder), mirroring ElevenLabs'
// request ID there.
func (p *audioCppProvider) CachePath(root string, sub *astisub.Item, model Model) Path {
	windowMs := (sub.EndAt - sub.StartAt).Milliseconds()
	refHash, err := voiceRefContentHash(model.voiceRefPath)
	if err != nil {
		// A missing/unreadable voice_ref is a real configuration error, not
		// a cache-miss path -- Synthesize will hit the same file and fail
		// with a clearer message, so just render a cache key that can never
		// hit rather than crashing path computation itself.
		refHash = "unreadable"
	}
	// The clamp bounds are part of the key: they decide which factor a take
	// was rendered at, so changing them must not reuse takes squeezed under
	// the old bounds.
	checksum := md5.Sum([]byte(refHash + model.referenceText + p.model + fmt.Sprintf("%d", windowMs) +
		fmt.Sprintf("%g-%g", audioCppFactorMin, audioCppFactorMax) + sub.String()))
	template := filepath.Join(root, fmt.Sprintf("%X-%s-%s.%%s.wav", checksum[:4], model.name, audioCppSlug(sub.String())))

	glob := fmt.Sprintf(template, "*")
	if files, err := filepath.Glob(glob); err == nil && len(files) > 0 {
		chosen := files[0]
		if len(files) > 1 {
			if newest, err := newestFile(files); err == nil {
				chosen = newest
			}
		}
		re := regexp.MustCompile(`([^.]+)\.wav$`)
		if match := re.FindStringSubmatch(filepath.Base(chosen)); len(match) > 1 {
			return Path{Path: chosen, Template: template, Id: match[1]}
		}
	}
	return Path{Template: template}
}

// speechRequest/speechResponse mirror POST /v1/audio/speech
// (HANDOFF-audiocpp.local.md §7).
type speechRequest struct {
	Model         string         `json:"model"`
	Input         string         `json:"input"`
	VoiceRef      string         `json:"voice_ref"`
	ReferenceText string         `json:"reference_text"`
	Seed          int            `json:"seed"`
	Options       *speechOptions `json:"options,omitempty"`
}

type speechOptions struct {
	DurationFactor float64 `json:"duration_factor"`
}

type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// speak calls POST /v1/audio/speech and returns the WAV bytes plus their
// duration. factor == nil means a "free" (unfitted) take.
func (p *audioCppProvider) speak(text string, seed int, voiceRef, referenceText string, factor *float64) ([]byte, time.Duration, error) {
	req := speechRequest{Model: p.model, Input: text, VoiceRef: voiceRef, ReferenceText: referenceText, Seed: seed}
	if factor != nil {
		req.Options = &speechOptions{DurationFactor: *factor}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}

	audio, err := p.post("/v1/audio/speech", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	duration, err := wavDuration(bytes.NewReader(audio))
	if err != nil {
		return nil, 0, fmt.Errorf("reading duration of synthesized audio: %w", err)
	}
	return audio, duration, nil
}

// transcribe calls POST /v1/audio/transcriptions (multipart form, per
// HANDOFF-audiocpp.local.md §8). The server accepts IndexTTS's 22,050 Hz
// WAV directly, so no resampling is needed here.
func (p *audioCppProvider) transcribe(wavBytes []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", audioCppASRModel); err != nil {
		return "", err
	}
	part, err := w.CreateFormFile("file", "clip.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wavBytes); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	respBody, err := p.post("/v1/audio/transcriptions", w.FormDataContentType(), &buf)
	if err != nil {
		return "", err
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("decoding transcription response: %w", err)
	}
	return resp.Text, nil
}

// post issues one HTTP POST with retry on transient failure (same policy
// shape as ElevenLabs, see retry.go), and unwraps {"error":{"message":...}}
// on a non-2xx response.
func (p *audioCppProvider) post(path, contentType string, body io.Reader) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}

	var result []byte
	err := withRetry(defaultTTSRetryPolicy(), retryableAudioCppError, time.Sleep, func() error {
		resp, err := p.client.Post(p.baseURL+path, contentType, bytes.NewReader(bodyBytes))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var apiErr apiErrorBody
			if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error.Message != "" {
				return &audioCppAPIError{status: resp.StatusCode, message: apiErr.Error.Message}
			}
			return &audioCppAPIError{status: resp.StatusCode, message: string(respBody)}
		}
		result = respBody
		return nil
	})
	return result, err
}

// audioCppAPIError is a deterministic request problem (bad voice_ref, text
// the model rejects) -- the audio.cpp analogue of *elevenlabs.APIError.
// Never retried; see retryableAudioCppError.
type audioCppAPIError struct {
	status  int
	message string
}

func (e *audioCppAPIError) Error() string {
	return fmt.Sprintf("audio.cpp returned %d: %s", e.status, e.message)
}

// retryableAudioCppError mirrors retryableTTSError's reasoning: a typed
// audioCppAPIError is a request the server understood and rejected for a
// reason that won't change on retry (4xx). Everything else -- connection
// refused (server not running), a timeout, a 5xx -- is worth another
// attempt.
func retryableAudioCppError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *audioCppAPIError
	if e, ok := err.(*audioCppAPIError); ok {
		apiErr = e
	}
	if apiErr != nil && apiErr.status >= 400 && apiErr.status < 500 {
		return false
	}
	return true
}

// clampFactor bounds a raw duration_factor to what the model accepts
// (HANDOFF-audiocpp.local.md §8). clamped reports whether the raw value
// needed bounding -- when it does, the text doesn't fit its window no
// matter what factor is applied, and the caller should flag the cue rather
// than trust the audio to actually match the timeline.
func clampFactor(raw float64) (factor float64, clamped bool) {
	factor = raw
	if factor < audioCppFactorMin {
		factor = audioCppFactorMin
	}
	if factor > audioCppFactorMax {
		factor = audioCppFactorMax
	}
	return factor, factor != raw
}

// Synthesize runs the fit + ASR-verify + retry loop (HANDOFF-audiocpp.local.md
// §8, ported from dialogue_ab.py's stage-1 loop): speak a free take, derive
// a duration_factor from the cue's real window, speak the fitted take,
// ASR-check it, and keep retrying with the next seed until a zero-miss take
// is found or retries run out, keeping whichever attempt had the fewest
// word misses. items/idx are accepted to satisfy TTSProvider but unused --
// audio.cpp has no request-stitching equivalent (HANDOFF §9): each cue is
// synthesized independently.
func (p *audioCppProvider) Synthesize(items []Item, idx int) (string, time.Duration, error) {
	item := items[idx]
	text := item.Sub.String()
	window := item.Sub.EndAt - item.Sub.StartAt
	voiceRef := item.Model.voiceRefPath
	refText := item.Model.referenceText
	if voiceRef == "" || refText == "" {
		return "", 0, fmt.Errorf("cue #%d (%s): voice_ref_path/reference_text_path not configured for this speaker", item.Sub.Index+1, item.Model.name)
	}

	type attempt struct {
		misses  int
		seed    int
		audio   []byte
		dur     time.Duration
		factor  float64
		clamped bool
	}
	var best *attempt

	for k := 0; k <= audioCppRetries; k++ {
		seed := audioCppBaseSeed + k

		_, freeDur, err := p.speak(text, seed, voiceRef, refText, nil)
		if err != nil {
			return "", 0, fmt.Errorf("cue #%d free take (seed %d): %w", item.Sub.Index+1, seed, err)
		}

		raw := window.Seconds() / freeDur.Seconds()
		factor, clamped := clampFactor(raw)

		fittedAudio, fittedDur, err := p.speak(text, seed, voiceRef, refText, &factor)
		if err != nil {
			return "", 0, fmt.Errorf("cue #%d fitted take (seed %d): %w", item.Sub.Index+1, seed, err)
		}

		heard, err := p.transcribe(fittedAudio)
		if err != nil {
			return "", 0, fmt.Errorf("cue #%d ASR check (seed %d): %w", item.Sub.Index+1, seed, err)
		}
		misses := wordMisses(heard, text)

		if best == nil || misses < best.misses {
			best = &attempt{misses: misses, seed: seed, audio: fittedAudio, dur: fittedDur, factor: factor, clamped: clamped}
		}
		if misses == 0 {
			break
		}
		if k < audioCppRetries {
			log.Printf("audio.cpp cue #%d: seed %d ASR mismatch (%d word misses) -- retrying", item.Sub.Index+1, seed, misses)
		}
	}

	if best.clamped {
		log.Printf("audio.cpp cue #%d: duration_factor clamped to %.2f -- text does not fit its %s window, needs shortening", item.Sub.Index+1, best.factor, window)
	}

	path := fmt.Sprintf(item.Path.Template, fmt.Sprintf("%d", best.seed))
	if err := os.WriteFile(path, best.audio, 0644); err != nil {
		return "", 0, fmt.Errorf("writing %s: %w", path, err)
	}
	log.Printf("Wrote %s (audio.cpp, %d word misses, factor %.2f%s)\n", path, best.misses, best.factor,
		map[bool]string{true: ", CLAMPED", false: ""}[best.clamped])

	return path, best.dur, nil
}

func (p *audioCppProvider) ReadDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return wavDuration(f)
}

func wavDuration(r io.ReadSeeker) (time.Duration, error) {
	d := wav.NewDecoder(r)
	d.ReadInfo()
	if err := d.Err(); err != nil {
		return 0, err
	}
	return d.Duration()
}

// DecodeSamples reads a WAV file's left channel (mono voice references
// produce mono output, so this is every sample) via go-audio/wav -- a real
// vetted dependency already vendored in this module (it encodes the final
// mix), so this needed no hand-written WAV parser despite
// HANDOFF-audiocpp.local.md §4 assuming one would be necessary.
func (p *audioCppProvider) DecodeSamples(path string) ([]int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	d := wav.NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		return nil, 0, fmt.Errorf("decoding %s: %w", path, err)
	}

	channels := buf.Format.NumChannels
	if channels <= 1 {
		return buf.Data, buf.Format.SampleRate, nil
	}
	// Left channel only, matching elevenLabsProvider.DecodeSamples' existing
	// mp3 behavior for a stereo source.
	samples := make([]int, 0, len(buf.Data)/channels)
	for i := 0; i < len(buf.Data); i += channels {
		samples = append(samples, buf.Data[i])
	}
	return samples, buf.Format.SampleRate, nil
}

var contractionExpansions = map[string]string{
	"who's": "who is", "you're": "you are", "let's": "let us", "gonna": "going to",
	"i'm": "i am", "it's": "it is", "that's": "that is", "aren't": "are not",
}

var nonLetterRE = regexp.MustCompile(`[^a-z ]`)

// words lowercases and expands contractions before splitting on whitespace,
// so ASR transcription style ("who's" vs "who is") isn't scored as a word
// miss. Ported from dialogue_ab.py's words().
func words(s string) []string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "’", "'") // curly apostrophe -> straight
	for k, v := range contractionExpansions {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(k) + `\b`)
		s = re.ReplaceAllString(s, v)
	}
	s = nonLetterRE.ReplaceAllString(s, " ")
	return strings.Fields(s)
}

// wordMisses counts words inserted, dropped or changed between what was
// heard (ASR) and the script (0 = exact). Ported from dialogue_ab.py's
// word_misses, which uses Python's difflib.SequenceMatcher; Go has no
// stdlib equivalent, so this computes the same quantity via a standard
// word-level edit distance (Wagner-Fischer). The two agree exactly for a
// single-word substitution (the overwhelmingly common ASR-mismatch case --
// one garbled or missing word) and are conservative-equivalent for the
// rarer multi-word block edits difflib's opcode-max formula handles
// slightly differently; either way, 0 means "exact" in both, which is the
// only value Synthesize's retry loop branches on.
func wordMisses(asr, text string) int {
	a, b := words(text), words(asr)
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
		dp[i][0] = i
	}
	for j := 0; j <= m; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1]
				continue
			}
			dp[i][j] = 1 + min(dp[i-1][j-1], min(dp[i-1][j], dp[i][j-1]))
		}
	}
	return dp[n][m]
}
