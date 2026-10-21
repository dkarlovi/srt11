package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// writeTestWAV writes a mono, 16-bit PCM WAV at rate containing n samples
// (a simple square wave so it's non-silent), returning its path.
func writeTestWAV(t *testing.T, path string, rate, n int) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer f.Close()

	buf := &audio.IntBuffer{
		Format:         &audio.Format{NumChannels: 1, SampleRate: rate},
		Data:           make([]int, n),
		SourceBitDepth: 16,
	}
	for i := range buf.Data {
		if i%2 == 0 {
			buf.Data[i] = 10000
		} else {
			buf.Data[i] = -10000
		}
	}
	enc := wav.NewEncoder(f, rate, 16, 1, 1)
	if err := enc.Write(buf); err != nil {
		t.Fatalf("writing wav: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("closing encoder: %v", err)
	}
	return path
}

func TestWordMisses(t *testing.T) {
	cases := []struct {
		name, asr, text string
		want            int
	}{
		{"exact match", "plaque builds up on your teeth", "plaque builds up on your teeth", 0},
		{"one substituted word", "flack builds up on your teeth", "plaque builds up on your teeth", 1},
		{"one dropped word", "builds up on your teeth", "plaque builds up on your teeth", 1},
		{"one inserted word", "plaque really builds up on your teeth", "plaque builds up on your teeth", 1},
		{"contraction expansion is not a miss", "who is that", "who's that", 0},
		{"case and punctuation are not misses", "Plaque Builds Up!", "plaque builds up", 0},
		{"curly apostrophe contraction", "you are right", "you’re right", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wordMisses(c.asr, c.text); got != c.want {
				t.Errorf("wordMisses(%q, %q) = %d, want %d", c.asr, c.text, got, c.want)
			}
		})
	}
}

func TestClampFactor(t *testing.T) {
	cases := []struct {
		raw         float64
		wantFactor  float64
		wantClamped bool
	}{
		{1.0, 1.0, false},
		{audioCppFactorMin, audioCppFactorMin, false},
		{audioCppFactorMax, audioCppFactorMax, false},
		{0.3, audioCppFactorMin, true},
		{0.73, audioCppFactorMin, true}, // sample_205 cue 2's squeeze, which distorted Hana's voice
		{3.0, audioCppFactorMax, true},
	}
	for _, c := range cases {
		factor, clamped := clampFactor(c.raw)
		if factor != c.wantFactor || clamped != c.wantClamped {
			t.Errorf("clampFactor(%v) = (%v, %v), want (%v, %v)", c.raw, factor, clamped, c.wantFactor, c.wantClamped)
		}
	}
}

func TestAudioCppProvider_CachePath(t *testing.T) {
	dir := t.TempDir()
	voiceRef := writeTestWAV(t, filepath.Join(dir, "matko.wav"), 22050, 1000)
	p := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081", FitDuration: true})

	item := &astisub.Item{
		StartAt: 0, EndAt: 3 * time.Second,
		Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "hello there"}}}},
	}
	model := Model{name: "Matko", voiceRefPath: voiceRef, referenceText: "a reference transcript"}

	first := p.CachePath(dir, item, model)
	if first.Path != "" {
		t.Fatalf("want no cache hit yet, got %+v", first)
	}

	// A fresh take lands at the template with some seed suffix.
	takePath := fmt.Sprintf(first.Template, "3")
	if err := os.WriteFile(takePath, []byte("fake wav bytes"), 0o644); err != nil {
		t.Fatalf("writing fake take: %v", err)
	}

	second := p.CachePath(dir, item, model)
	if second.Path != takePath || second.Id != "3" {
		t.Errorf("want cache hit at %q with id \"3\", got %+v", takePath, second)
	}

	t.Run("a different window changes the cache key", func(t *testing.T) {
		longer := &astisub.Item{StartAt: 0, EndAt: 5 * time.Second, Lines: item.Lines}
		got := p.CachePath(dir, longer, model)
		if got.Template == first.Template {
			t.Error("want a different window to produce a different cache template (duration_factor depends on it)")
		}
	})

	t.Run("different reference text changes the cache key", func(t *testing.T) {
		otherModel := model
		otherModel.referenceText = "a completely different transcript"
		got := p.CachePath(dir, item, otherModel)
		if got.Template == first.Template {
			t.Error("want different reference_text to produce a different cache template")
		}
	})

	t.Run("speed is not part of the cache key", func(t *testing.T) {
		spedModel := model
		spedModel.speed = 1.15
		got := p.CachePath(dir, item, spedModel)
		if got.Template != first.Template {
			t.Error("want speed to have no effect on the audiocpp cache key -- that provider ignores @speed")
		}
	})

	t.Run("take-fit takes are keyed apart and on the room", func(t *testing.T) {
		tf := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081", TakeFit: true})
		roomy := model
		roomy.roomMs = 3000
		got := tf.CachePath(dir, item, roomy)
		free := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081"}).CachePath(dir, item, roomy)
		if got.Template == first.Template || got.Template == free.Template {
			t.Error("want take-fit takes never to reuse fitted or free ones")
		}
		roomy.roomMs = 3500
		if tf.CachePath(dir, item, roomy).Template == got.Template {
			t.Error("want a different room to change the take-fit cache key")
		}
	})

	t.Run("unfitted takes are keyed apart from fitted ones and ignore the window", func(t *testing.T) {
		free := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081"})
		got := free.CachePath(dir, item, model)
		if got.Template == first.Template {
			t.Error("want an unfitted take to never reuse a fitted one")
		}
		longer := &astisub.Item{StartAt: 0, EndAt: 5 * time.Second, Lines: item.Lines}
		if free.CachePath(dir, longer, model).Template != got.Template {
			t.Error("want the window to have no effect on an unfitted take's key")
		}
	})
}

func TestAudioCppProvider_VoiceRefContentHash(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWAV(t, filepath.Join(dir, "matko.wav"), 22050, 1000)

	t.Run("concurrent lookups are safe", func(t *testing.T) {
		// Run under -race: this used to be a package-global map shared by
		// every tenant's Generate.
		p := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081"})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := p.voiceRefContentHash(path); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("a replaced reference is rehashed by the next provider", func(t *testing.T) {
		before, err := newAudioCppProvider(AudioCppConfig{}).voiceRefContentHash(path)
		if err != nil {
			t.Fatal(err)
		}
		writeTestWAV(t, path, 22050, 2000)
		after, err := newAudioCppProvider(AudioCppConfig{}).voiceRefContentHash(path)
		if err != nil {
			t.Fatal(err)
		}
		if before == after {
			t.Error("want a re-recorded voice_ref to produce a new hash without a restart")
		}
	})
}

func TestResampleLinear(t *testing.T) {
	t.Run("identity when rates match", func(t *testing.T) {
		in := []int{1, 2, 3, 4}
		out := resampleLinear(in, 44100, 44100)
		if len(out) != len(in) {
			t.Fatalf("want unchanged length, got %d", len(out))
		}
	})

	t.Run("exact 2x upsample interpolates midpoints", func(t *testing.T) {
		in := []int{0, 100, 200}
		out := resampleLinear(in, 22050, 44100)
		if len(out) != 6 {
			t.Fatalf("want 6 output samples, got %d: %v", len(out), out)
		}
		// out[0]=in[0], out[1]=midpoint(in[0],in[1]), out[2]=in[1], ...
		want := []int{0, 50, 100, 150, 200, 200}
		for i, w := range want {
			if out[i] != w {
				t.Errorf("out[%d] = %d, want %d (full: %v)", i, out[i], w, out)
			}
		}
	})
}

func TestAudioCppProvider_ReadDurationAndDecodeSamples(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWAV(t, filepath.Join(dir, "clip.wav"), 22050, 2205) // 0.1s at 22050Hz
	p := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:8081"})

	dur, err := p.ReadDuration(path)
	if err != nil {
		t.Fatalf("ReadDuration: %v", err)
	}
	if dur < 95*time.Millisecond || dur > 105*time.Millisecond {
		t.Errorf("duration = %s, want ~100ms", dur)
	}

	samples, rate, err := p.DecodeSamples(path)
	if err != nil {
		t.Fatalf("DecodeSamples: %v", err)
	}
	if rate != 22050 {
		t.Errorf("rate = %d, want 22050", rate)
	}
	if len(samples) != 2205 {
		t.Errorf("len(samples) = %d, want 2205", len(samples))
	}
	if samples[0] != 10000 {
		t.Errorf("samples[0] = %d, want 10000", samples[0])
	}
}

// audioCppTestServer fakes the two audio.cpp endpoints Synthesize uses.
// speakDurMs is consumed in call order, one per /v1/audio/speech hit (wraps
// around via modulo); transcripts likewise for /v1/audio/transcriptions.
type audioCppTestServer struct {
	speakCalls  int
	speakDurMs  []int
	failSpeak   map[int]bool // 1-based speak call numbers answered with a 400
	asrCalls    int
	transcripts []string
}

func (s *audioCppTestServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/speech":
			durMs := s.speakDurMs[s.speakCalls%len(s.speakDurMs)]
			s.speakCalls++
			if s.failSpeak[s.speakCalls] {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":{"message":"max_tokens reached"}}`))
				return
			}
			n := durMs * 22050 / 1000
			tmp := filepath.Join(t.TempDir(), fmt.Sprintf("resp-%d.wav", s.speakCalls))
			writeTestWAV(t, tmp, 22050, n)
			data, err := os.ReadFile(tmp)
			if err != nil {
				t.Fatalf("reading generated fixture: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(data)
		case "/v1/audio/transcriptions/details":
			// One word spanning the whole clip, at the clip's own rate.
			r.ParseMultipartForm(32 << 20)
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("details: no file: %v", err)
			}
			data, _ := io.ReadAll(f)
			n := (len(data) - 44) / 2
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"text":        s.transcripts[0],
				"sample_rate": 22050,
				"words":       []map[string]any{{"word": "all", "start_sample": 0, "end_sample": n}},
			})
		case "/v1/audio/transcriptions":
			text := s.transcripts[s.asrCalls%len(s.transcripts)]
			s.asrCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"text": text})
		default:
			http.NotFound(w, r)
		}
	}
}

func TestAudioCppProvider_Synthesize(t *testing.T) {
	dir := t.TempDir()
	voiceRef := writeTestWAV(t, filepath.Join(dir, "matko.wav"), 22050, 1000)

	buildItems := func(cacheDir string, provider TTSProvider) []Item {
		sub := &astisub.Item{
			StartAt: 0, EndAt: 3 * time.Second,
			Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "plaque builds up on your teeth"}}}},
		}
		model := Model{name: "Matko", voiceRefPath: voiceRef, referenceText: "plaque builds up on your teeth"}
		return []Item{{Sub: sub, Model: model, Path: provider.CachePath(cacheDir, sub, model)}}
	}

	t.Run("an exact ASR match on the first attempt succeeds without retrying", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{2900, 3000}, transcripts: []string{"plaque builds up on your teeth"}}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()

		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, FitDuration: true})
		items := buildItems(t.TempDir(), p)

		path, dur, err := p.Synthesize(items, 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("want the winning take written to disk: %v", err)
		}
		if dur < 2900*time.Millisecond || dur > 3100*time.Millisecond {
			t.Errorf("duration = %s, want ~3s", dur)
		}
		if fake.speakCalls != 2 { // one free + one fitted take, single attempt
			t.Errorf("speakCalls = %d, want 2 (no retry needed)", fake.speakCalls)
		}
	})

	t.Run("an ASR mismatch retries with the next seed and keeps the better take", func(t *testing.T) {
		fake := &audioCppTestServer{
			speakDurMs:  []int{2900, 3000, 2900, 3000}, // seed 1: free+fitted, seed 2: free+fitted
			transcripts: []string{"flack builds up on your teeth", "plaque builds up on your teeth"},
		}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()

		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, FitDuration: true})
		items := buildItems(t.TempDir(), p)

		path, _, err := p.Synthesize(items, 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if !strings.Contains(path, ".2.wav") {
			t.Errorf("want the seed-2 take's path (the exact-match retry), got %s", path)
		}
		if fake.asrCalls != 2 {
			t.Errorf("asrCalls = %d, want 2 (one mismatch, one exact match)", fake.asrCalls)
		}
	})

	t.Run("all retries mismatch: keeps whichever attempt had the fewest word misses", func(t *testing.T) {
		fake := &audioCppTestServer{
			speakDurMs: []int{2900, 3000, 2900, 3000, 2900, 3000},
			transcripts: []string{
				"flack flack up on your teeth",      // 2 misses
				"plaque builds up on your teeth zz", // 1 miss (inserted word)
				"flack flack flack up your teeth",   // 3 misses
			},
		}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()

		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, FitDuration: true})
		items := buildItems(t.TempDir(), p)

		path, _, err := p.Synthesize(items, 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if !strings.Contains(path, ".2.wav") {
			t.Errorf("want the seed-2 take (fewest word misses among all three attempts), got %s", path)
		}
	})

	t.Run("a wildly mismatched window clamps the factor but still writes a take", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{100, 100}, transcripts: []string{"plaque builds up on your teeth"}}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()

		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, FitDuration: true})
		sub := &astisub.Item{
			StartAt: 0, EndAt: 3 * time.Second, // 3s window against a 100ms "free" take -> raw factor 30, clamped to 1.95
			Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "plaque builds up on your teeth"}}}},
		}
		model := Model{name: "Matko", voiceRefPath: voiceRef, referenceText: "plaque builds up on your teeth"}
		items := []Item{{Sub: sub, Model: model, Path: p.CachePath(t.TempDir(), sub, model)}}

		path, _, err := p.Synthesize(items, 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("want a take still written even when clamped: %v", err)
		}
	})

	t.Run("without fit_duration one free take is verified and kept, no duration_factor sent", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{4200}, transcripts: []string{"plaque builds up on your teeth"}}
		var sawFactor bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/audio/speech" {
				var req speechRequest
				body, _ := io.ReadAll(r.Body)
				json.Unmarshal(body, &req)
				sawFactor = sawFactor || req.Options != nil
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			fake.handler(t)(w, r)
		}))
		defer srv.Close()

		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL})
		items := buildItems(t.TempDir(), p)

		_, dur, err := p.Synthesize(items, 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if fake.speakCalls != 1 || sawFactor {
			t.Errorf("speakCalls = %d, sawFactor = %v; want one unfitted take", fake.speakCalls, sawFactor)
		}
		if dur < 4100*time.Millisecond {
			t.Errorf("duration = %s, want the free take's ~4.2s, not the 3s window", dur)
		}
	})

	t.Run("missing voice_ref/reference_text is a clear error, not an HTTP call", func(t *testing.T) {
		p := newAudioCppProvider(AudioCppConfig{BaseURL: "http://127.0.0.1:1"}) // unreachable on purpose
		sub := &astisub.Item{StartAt: 0, EndAt: time.Second, Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "hi"}}}}}
		items := []Item{{Sub: sub, Model: Model{name: "Hana"}}}
		if _, _, err := p.Synthesize(items, 0); err == nil {
			t.Fatal("want an error for a speaker with no voice_ref/reference_text configured")
		}
	})
}

type fakeNetError struct{}

func (fakeNetError) Error() string { return "connection refused" }

func TestRetryableAudioCppError(t *testing.T) {
	if retryableAudioCppError(nil) {
		t.Error("nil error should not be retryable")
	}
	if retryableAudioCppError(&audioCppAPIError{status: 400, message: "bad request"}) {
		t.Error("a 4xx (deterministic rejection) should not be retried")
	}
	if !retryableAudioCppError(&audioCppAPIError{status: 500, message: "internal error"}) {
		t.Error("a 5xx should be retried")
	}
	if !retryableAudioCppError(fakeNetError{}) {
		t.Error("a plain connection/timeout error should be retried")
	}
}

func TestScaleWordSamples(t *testing.T) {
	got := scaleWordSamples([]asrWord{{Word: "a", Start: 8000, End: 16000}}, 16000, 24000)
	if got[0].Start != 12000 || got[0].End != 24000 {
		t.Errorf("got %+v, want 12000..24000", got[0])
	}
}

func TestAudioCppProvider_SynthesizeTakeFit(t *testing.T) {
	dir := t.TempDir()
	voiceRef := writeTestWAV(t, filepath.Join(dir, "hana.wav"), 22050, 1000)
	const text = "plaque builds up on your teeth"
	buildItems := func(p *audioCppProvider) []Item {
		sub := &astisub.Item{StartAt: 0, EndAt: 3 * time.Second,
			Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: text}}}}}
		model := Model{name: "Hana", voiceRefPath: voiceRef, referenceText: "ref", roomMs: 3000}
		return []Item{{Sub: sub, Model: model, Path: p.CachePath(t.TempDir(), sub, model)}}
	}

	t.Run("a failed take is skipped", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{2800}, failSpeak: map[int]bool{1: true}, transcripts: []string{text}}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, TakeFit: true, Takes: 3})

		path, _, err := p.Synthesize(buildItems(p), 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if !strings.Contains(path, ".2.wav") {
			t.Errorf("path = %s, want seed 2 (seed 1 failed, 2 and 3 tie)", path)
		}
		if fake.speakCalls != 3 {
			t.Errorf("speakCalls = %d, want 3", fake.speakCalls)
		}
	})

	t.Run("all takes failing is an error", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{2800}, failSpeak: map[int]bool{1: true, 2: true, 3: true}, transcripts: []string{text}}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, TakeFit: true, Takes: 3})

		_, _, err := p.Synthesize(buildItems(p), 0)
		if err == nil || !strings.Contains(err.Error(), "cue #1") {
			t.Fatalf("err = %v, want an error naming cue #1", err)
		}
	})

	t.Run("a take longer than its room is sped up by 8 % at most", func(t *testing.T) {
		fake := &audioCppTestServer{speakDurMs: []int{4000}, transcripts: []string{text}}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		p := newAudioCppProvider(AudioCppConfig{BaseURL: srv.URL, TakeFit: true, Takes: 1})

		path, dur, err := p.Synthesize(buildItems(p), 0)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		want := 3704 * time.Millisecond // 4 s / 1.08
		if dur > want+50*time.Millisecond || dur < want-50*time.Millisecond {
			t.Errorf("duration = %s, want %s ± 50ms", dur, want)
		}
		if got, err := p.ReadDuration(path); err != nil || got != dur {
			t.Errorf("file duration = %s (%v), want the returned %s", got, err, dur)
		}
	})
}
