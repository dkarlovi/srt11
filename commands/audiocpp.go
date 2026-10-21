package commands

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	baseURL     string
	model       string
	fitDuration bool
	takeFit     bool
	takes       int
	client      *http.Client

	// refHashes memoizes each voice reference WAV's content hash, read once
	// per distinct file rather than once per cue. Scoped to this provider,
	// i.e. to one Generate/Export call (resolveProvider builds a fresh one
	// each time): a process-wide map would race between tenants and keep
	// serving a replaced WAV's old hash until restart.
	mu        sync.Mutex
	refHashes map[string]string
}

func newAudioCppProvider(cfg AudioCppConfig) *audioCppProvider {
	model := cfg.Model
	if model == "" {
		model = audioCppStructureModel
	}
	return &audioCppProvider{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		model:       model,
		fitDuration: cfg.FitDuration,
		takeFit:     cfg.TakeFit,
		takes:       cfg.takeCount(),
		client:      &http.Client{Timeout: audioCppTimeout},
		refHashes:   make(map[string]string),
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

func (p *audioCppProvider) voiceRefContentHash(path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.refHashes[path]; ok {
		return h, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading voice_ref %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	h := fmt.Sprintf("%x", sum[:8])
	p.refHashes[path] = h
	return h, nil
}

// CachePath's checksum covers the reference WAV's content (so re-recording
// or swapping a voice invalidates old takes), the reference text, the
// model, and whether takes are fitted. A fitted take also covers the cue's
// window duration and the clamp bounds -- duration_factor is derived from
// both, so a different window is a materially different take, unlike
// ElevenLabs where retiming a cue doesn't change its audio at all (see
// HANDOFF-audiocpp.local.md §9). An unfitted take doesn't depend on the
// window, so retiming reuses it. It deliberately does NOT cover speed --
// audio.cpp ignores @speed entirely -- or a seed: any successfully-verified
// take for the same key is equally valid to reuse regardless of which seed
// produced it, exactly like ElevenLabs' request ID never being part of its
// own cache key. The seed that actually produced a fresh take is recorded
// in the filename (via the %s template placeholder), mirroring ElevenLabs'
// request ID there.
func (p *audioCppProvider) CachePath(root string, sub *astisub.Item, model Model) Path {
	refHash, err := p.voiceRefContentHash(model.voiceRefPath)
	if err != nil {
		// A missing/unreadable voice_ref is a real configuration error, not
		// a cache-miss path -- Synthesize will hit the same file and fail
		// with a clearer message, so just render a cache key that can never
		// hit rather than crashing path computation itself.
		refHash = "unreadable"
	}
	// The clamp bounds are part of a fitted key: they decide which factor a
	// take was rendered at, so changing them must not reuse takes squeezed
	// under the old bounds.
	fit := "free"
	switch {
	case p.takeFit:
		fit = fmt.Sprintf("takes:%s:%d:%d:%d", takeFitVersion, p.takes, (sub.EndAt - sub.StartAt).Milliseconds(), model.roomMs)
	case p.fitDuration:
		fit = fmt.Sprintf("fit%d:%g-%g", (sub.EndAt - sub.StartAt).Milliseconds(), audioCppFactorMin, audioCppFactorMax)
	}
	checksum := md5.Sum([]byte(refHash + model.referenceText + p.model + fit + sub.String()))
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
	respBody, err := p.postASR("/v1/audio/transcriptions", wavBytes)
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

// postASR posts wavBytes as the multipart form both ASR endpoints take.
func (p *audioCppProvider) postASR(path string, wavBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", audioCppASRModel); err != nil {
		return nil, err
	}
	part, err := w.CreateFormFile("file", "clip.wav")
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(wavBytes); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return p.post(path, w.FormDataContentType(), &buf)
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
// word misses. Without fit_duration (a model that ignores duration_factor,
// e.g. Higgs) the free take is the one ASR-checked and kept: the cue is
// then not fitted to its window at all, and AutoFix baskets any overrun. items/idx are accepted to satisfy TTSProvider but unused --
// audio.cpp has no request-stitching equivalent (HANDOFF §9): each cue is
// synthesized independently.
func (p *audioCppProvider) Synthesize(items []Item, idx int) (string, time.Duration, error) {
	if p.takeFit {
		return p.synthesizeTakeFit(items[idx])
	}
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

		takeAudio, takeDur, err := p.speak(text, seed, voiceRef, refText, nil)
		if err != nil {
			return "", 0, fmt.Errorf("cue #%d free take (seed %d): %w", item.Sub.Index+1, seed, err)
		}

		factor, clamped := 1.0, false
		if p.fitDuration {
			factor, clamped = clampFactor(window.Seconds() / takeDur.Seconds())
			takeAudio, takeDur, err = p.speak(text, seed, voiceRef, refText, &factor)
			if err != nil {
				return "", 0, fmt.Errorf("cue #%d fitted take (seed %d): %w", item.Sub.Index+1, seed, err)
			}
		}

		heard, err := p.transcribe(takeAudio)
		if err != nil {
			return "", 0, fmt.Errorf("cue #%d ASR check (seed %d): %w", item.Sub.Index+1, seed, err)
		}
		misses := wordMisses(heard, text)

		if best == nil || misses < best.misses {
			best = &attempt{misses: misses, seed: seed, audio: takeAudio, dur: takeDur, factor: factor, clamped: clamped}
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
	fitNote := "unfitted"
	if p.fitDuration {
		fitNote = fmt.Sprintf("factor %.2f%s", best.factor, map[bool]string{true: ", CLAMPED", false: ""}[best.clamped])
	}
	log.Printf("Wrote %s (audio.cpp, %d word misses, %s)\n", path, best.misses, fitNote)

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

// transcribeDetails calls POST /v1/audio/transcriptions/details, which
// adds word timings to the transcript. See asrWordRate for which rate the
// positions are in; scaleWordSamples maps them to the take's.
func (p *audioCppProvider) transcribeDetails(wavBytes []byte) (string, []asrWord, int, error) {
	respBody, err := p.postASR("/v1/audio/transcriptions/details", wavBytes)
	if err != nil {
		return "", nil, 0, err
	}
	var resp struct {
		Text       string `json:"text"`
		SampleRate int    `json:"sample_rate"`
		Words      []struct {
			Word        string `json:"word"`
			StartSample int    `json:"start_sample"`
			EndSample   int    `json:"end_sample"`
		} `json:"words"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", nil, 0, fmt.Errorf("decoding transcription details: %w", err)
	}
	words := make([]asrWord, len(resp.Words))
	for i, w := range resp.Words {
		words[i] = asrWord{Word: w.Word, Start: w.StartSample, End: w.EndSample}
	}
	return resp.Text, words, resp.SampleRate, nil
}

// audioCppASRWordRate is the rate /details word positions are really in:
// Parakeet's own 16 kHz, whatever the response's sample_rate says (it
// reports the input's rate -- 24000 for a Higgs take). Measured
// 2026-10-21: reading them at 24 kHz ended every line a third early.
const audioCppASRWordRate = 16000

// asrWordRate picks the rate to read word positions at: 16 kHz, unless that
// would put the last word more than 5 % past the end of the take, in which
// case the server is taken at its word (fit_v3.py's heuristic, inverted).
func asrWordRate(words []asrWord, reported, takeLen, takeRate int) int {
	if len(words) == 0 {
		return reported
	}
	end := float64(words[len(words)-1].End) / audioCppASRWordRate
	if end <= 1.05*float64(takeLen)/float64(takeRate) {
		return audioCppASRWordRate
	}
	return reported
}

func scaleWordSamples(words []asrWord, from, to int) []asrWord {
	if from == to || from <= 0 {
		return words
	}
	out := make([]asrWord, len(words))
	for i, w := range words {
		out[i] = asrWord{Word: w.Word, Start: w.Start * to / from, End: w.End * to / from}
	}
	return out
}

// decodeWavFloat reads a WAV's first channel as float64 samples in [-1, 1].
func decodeWavFloat(b []byte) ([]float64, int, error) {
	buf, err := wav.NewDecoder(bytes.NewReader(b)).FullPCMBuffer()
	if err != nil {
		return nil, 0, err
	}
	ch := max(1, buf.Format.NumChannels)
	scale := float64(int(1) << (max(8, buf.SourceBitDepth) - 1))
	x := make([]float64, 0, len(buf.Data)/ch)
	for i := 0; i < len(buf.Data); i += ch {
		x = append(x, float64(buf.Data[i])/scale)
	}
	return x, buf.Format.SampleRate, nil
}

// encodeWavFloat writes x as a mono 16-bit PCM WAV. The header is written
// by hand: go-audio's encoder needs an io.WriteSeeker, and the cleaned take
// goes to ASR from memory before it's ever written to disk.
func encodeWavFloat(x []float64, rate int) []byte {
	var b bytes.Buffer
	dataLen := len(x) * 2
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(36 + dataLen))
	b.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(1)) // PCM
	le(uint16(1)) // mono
	le(uint32(rate))
	le(uint32(rate * 2))
	le(uint16(2))
	le(uint16(16))
	b.WriteString("data")
	le(uint32(dataLen))
	for _, v := range x {
		le(int16(math.Max(-32768, math.Min(32767, math.Round(v*32767)))))
	}
	return b.Bytes()
}

// synthesizeTakeFit is Synthesize under take_fit (spec data flow, steps
// 1-10): speak p.takes free takes, cut and tighten each at its word
// timings, score the cleaned take by ASR, keep the best, and speed it up by
// takeFitMaxSpeedup at most if it runs past its room. A failed take only
// costs that seed: Higgs sometimes never ends a short line.
func (p *audioCppProvider) synthesizeTakeFit(item Item) (string, time.Duration, error) {
	cue := item.Sub.Index + 1
	text := item.Sub.String()
	window := item.Sub.EndAt - item.Sub.StartAt
	room := time.Duration(item.Model.roomMs) * time.Millisecond
	voiceRef, refText := item.Model.voiceRefPath, item.Model.referenceText
	if voiceRef == "" || refText == "" {
		return "", 0, fmt.Errorf("cue #%d (%s): voice_ref_path/reference_text_path not configured for this speaker", cue, item.Model.name)
	}

	dir, err := p.takeCacheDir(item)
	if err != nil {
		return "", 0, err
	}
	var takes []cleanedTake
	var cands []takeCandidate
	var lastErr error
	nWords := max(1, len(words(text)))
	for seed := 1; seed <= p.takes; seed++ {
		tk, err := p.cleanedTakeFor(dir, text, seed, voiceRef, refText)
		if err != nil {
			lastErr = err
			log.Printf("audio.cpp cue #%d: take %d failed, skipping: %v", cue, seed, err)
			continue
		}
		takes = append(takes, tk)
		cands = append(cands, takeCandidate{
			Seed: seed,
			Dur:  time.Duration(float64(len(tk.samples)) / float64(tk.rate) * float64(time.Second)),
			WER:  float64(tk.Misses) / float64(nWords),
		})
	}

	best := selectTake(cands, window)
	if best < 0 {
		return "", 0, fmt.Errorf("cue #%d: all %d takes failed, last error: %w", cue, p.takes, lastErr)
	}
	chosen, c := takes[best], cands[best]

	x, factor := chosen.samples, 1.0
	if c.Dur > room {
		factor = math.Min(c.Dur.Seconds()/room.Seconds(), takeFitMaxSpeedup)
		x = wsola(x, chosen.rate, factor)
	}
	x = fadeEdges(x, chosen.rate)

	path := fmt.Sprintf(item.Path.Template, fmt.Sprintf("%d", c.Seed))
	encoded := encodeWavFloat(x, chosen.rate)
	if err := os.WriteFile(path, encoded, 0644); err != nil {
		return "", 0, fmt.Errorf("writing %s: %w", path, err)
	}
	// Measured the way ReadDuration measures a cache hit, so a fresh take
	// and its later reuse report the same duration.
	dur, err := wavDuration(bytes.NewReader(encoded))
	if err != nil {
		return "", 0, fmt.Errorf("reading duration of %s: %w", path, err)
	}
	log.Printf("Wrote %s (audio.cpp take fit: seed %d of %d ok, %d word misses, raw %s, cleaned %s, window %s, room %s, speed-up %.3f)\n",
		path, c.Seed, len(cands), chosen.Misses, chosen.RawDur.Round(time.Millisecond), c.Dur.Round(time.Millisecond),
		window, room, factor)
	return path, dur, nil
}

// cleanedTake is one take after cleanTake, plus what selection needs. It
// is cached on disk by takeCacheDir/cleanedTakeFor, keyed without the
// window or room: those only decide which cached take is picked and how
// far it's stretched, so a retime (AutoFix starting a line earlier, which
// also shrinks the previous line's room) rebuilds the final file from
// these without calling the server -- and Export keeps playing takes that
// Generate measured.
type cleanedTake struct {
	samples []float64
	rate    int
	Misses  int           `json:"misses"`
	RawDur  time.Duration `json:"raw_duration"`
	Failed  string        `json:"failed,omitempty"` // the error, for a seed the server couldn't produce
}

// takeCacheDir is where item's cleaned takes live: next to the final
// takes, keyed on everything that changes a take except the window and
// room.
func (p *audioCppProvider) takeCacheDir(item Item) (string, error) {
	refHash, err := p.voiceRefContentHash(item.Model.voiceRefPath)
	if err != nil {
		return "", err
	}
	text := item.Sub.String()
	sum := md5.Sum([]byte(refHash + item.Model.referenceText + p.model + takeFitVersion + text))
	dir := filepath.Join(filepath.Dir(item.Path.Template),
		fmt.Sprintf("%X-%s-%s.takes", sum[:4], item.Model.name, audioCppSlug(text)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating take cache %s: %w", dir, err)
	}
	return dir, nil
}

// cleanedTakeFor returns seed's cleaned take from dir, or speaks, cleans,
// scores and caches it. A seed that failed is cached as failed too, so a
// rebuild never goes back to the server for it.
func (p *audioCppProvider) cleanedTakeFor(dir, text string, seed int, voiceRef, refText string) (cleanedTake, error) {
	metaPath := filepath.Join(dir, fmt.Sprintf("seed%d.json", seed))
	wavPath := filepath.Join(dir, fmt.Sprintf("seed%d.wav", seed))
	var tk cleanedTake
	if meta, err := os.ReadFile(metaPath); err == nil && json.Unmarshal(meta, &tk) == nil {
		if tk.Failed != "" {
			return tk, errors.New(tk.Failed)
		}
		if b, err := os.ReadFile(wavPath); err == nil {
			if tk.samples, tk.rate, err = decodeWavFloat(b); err == nil {
				return tk, nil
			}
		}
	}

	tk, err := p.makeCleanedTake(text, seed, voiceRef, refText)
	if err != nil {
		var apiErr *audioCppAPIError
		if !errors.As(err, &apiErr) {
			return tk, err // transient (server down, timeout): try again next time
		}
		tk.Failed = err.Error()
	} else if err := os.WriteFile(wavPath, encodeWavFloat(tk.samples, tk.rate), 0o644); err != nil {
		return tk, fmt.Errorf("writing %s: %w", wavPath, err)
	}
	meta, _ := json.Marshal(tk)
	if werr := os.WriteFile(metaPath, meta, 0o644); werr != nil {
		return tk, fmt.Errorf("writing %s: %w", metaPath, werr)
	}
	return tk, err
}

// makeCleanedTake speaks one free take, cuts and tightens it at its word
// timings, and scores the cleaned result by ASR (spec steps 1-4).
func (p *audioCppProvider) makeCleanedTake(text string, seed int, voiceRef, refText string) (cleanedTake, error) {
	audio, rawDur, err := p.speak(text, seed, voiceRef, refText, nil)
	if err != nil {
		return cleanedTake{}, err
	}
	x, rate, err := decodeWavFloat(audio)
	if err != nil {
		return cleanedTake{}, err
	}
	_, ws, asrRate, err := p.transcribeDetails(audio)
	if err != nil {
		return cleanedTake{}, err
	}
	x = cleanTake(x, rate, scaleWordSamples(ws, asrWordRate(ws, asrRate, len(x), rate), rate))
	heard, err := p.transcribe(encodeWavFloat(x, rate))
	if err != nil {
		return cleanedTake{}, err
	}
	return cleanedTake{samples: x, rate: rate, Misses: wordMisses(heard, text), RawDur: rawDur}, nil
}
