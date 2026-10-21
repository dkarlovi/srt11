# Higgs Take Fit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fit Higgs takes to their cue windows after synthesis (N takes, clean, select, ≤ 8 % WSOLA speed-up), behind `audio_cpp.take_fit`.

**Architecture:** Pure DSP and selection functions in a new `commands/takefit.go`; the audio.cpp provider calls them from a new take-fit branch of `Synthesize`. `parseSubtitleFile` sets each item's `Model.roomMs` in a second pass so `CachePath` can key on it. AutoFix/Export/session code is unchanged.

**Tech Stack:** Go, `github.com/go-audio/wav` + `github.com/go-audio/audio` (already vendored), no new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-21-higgs-take-fit-design.md`

## Global Constraints

- Pure Go, `CGO_ENABLED=0`, no `os/exec`, no new module dependency.
- Constants exactly as the spec's table: lead 60 ms, tail 100 ms, edge −40 dBFS / 30 ms pad, pause −42 dBFS / 350 ms max / 10 ms xfade, speed-up ≤ 1.08, gap 120 ms, last-cue room +1 s, good WER 0.10, fades 40 ms / 5 ms, version `"takefit-v1"`.
- `take_fit: false` (default) must leave every existing code path byte-identical in behaviour.
- After every task: `go build ./... && go vet ./... && gofmt -l . && go test ./...` clean.
- Branch `feat/higgs-take-fit`, local only. Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- A take that is all silence or has no ASR words → `cleanTake` must return a non-empty take, never panic on empty slices (Task 2 test `TestCleanTake_NoWordsSilentTake`).
- A take shorter than one WSOLA frame (30 ms) → `wsola` returns it unchanged (Task 3 test `TestWSOLA_ShorterThanFrame`).
- Two items with the same start (overlapping/abutting next cue) → room = window, never smaller than the window (Task 4 `takeRoom` table).
- `/details` returning `sample_rate` ≠ take rate → word positions must be rescaled to the take's rate (Task 5 test `TestScaleWordSamples`).
- Every take failing → one clear error naming the cue, no write, no nil dereference on `best` (Task 5 provider test).

---

### Task 1: Config fields and validation

**Files:**
- Modify: `commands/run.go` (`AudioCppConfig`, `readConfig`)
- Modify: `config.yaml.dist` (document the two fields under the `audio_cpp` example)
- Test: `commands/run_test.go`

**Interfaces:**
- Produces: `AudioCppConfig.TakeFit bool` (`yaml:"take_fit"`), `AudioCppConfig.Takes int` (`yaml:"takes"`), `func (c AudioCppConfig) takeCount() int` (0 → 6), `func validateAudioCppConfig(c *AudioCppConfig) error` (nil-safe), called at the end of `readConfig`.

- [ ] **Step 1: Write failing test** `TestValidateAudioCppConfig`: nil → nil; `{TakeFit:true, FitDuration:true}` → error containing `take_fit`; `{TakeFit:true, Takes:-1}` → error; `{TakeFit:true}.takeCount()` == 6; `{Takes:3}.takeCount()` == 3.
- [ ] **Step 2: Run** `go test ./commands/ -run TestValidateAudioCppConfig` → FAIL (undefined).
- [ ] **Step 3: Implement** fields, `takeCount`, `validateAudioCppConfig`, call it from `readConfig` (return its error wrapped with the filename). Add the two fields, commented, to `config.yaml.dist`.
- [ ] **Step 4: Run** full check → PASS.
- [ ] **Step 5: Commit** `feat: take_fit and takes config for audio.cpp`.

### Task 2: Take cleaning and edge fades

**Files:**
- Create: `commands/takefit.go`
- Test: `commands/takefit_test.go`

**Interfaces:**
- Produces:
  - `type asrWord struct { Word string; Start, End int }` (sample positions at the take's rate)
  - `func cleanTake(x []float64, rate int, words []asrWord) []float64` — steps 3a–3c of the spec, in that order.
  - `func fadeEdges(x []float64, rate int) []float64` — spec step 8; returns a copy.
  - `func frameDb(x []float64, frame int) []float64` — RMS dBFS per non-overlapping frame (helper shared by both).
  - Samples are float64 in [−1, 1].

- [ ] **Step 1: Write failing tests** (24 kHz, signals built from a 220 Hz sine at amplitude 0.3 for "voice" and zeros for "silence"):
  - `TestCleanTake_CutsAtWordMargins`: 1 s silence + 1 s voice + 1 s silence, words `[{Start:24000, End:48000}]` → length within ±15 ms of 1 s + 60 ms + 100 ms, then the edge trim removes the silent margins down to 30 ms pads → final length 1.06 s ± 20 ms.
  - `TestCleanTake_ShortCutKeepsWholeTake`: words spanning 100 ms → the cut is skipped (output still contains the voice, length ≥ voice length).
  - `TestCleanTake_ShortensLongInnerPause`: voice 0.5 s + silence 1 s + voice 0.5 s, words covering all → inner pause 350 ms ± 10 ms (total 1.35 s ± 20 ms).
  - `TestCleanTake_KeepsShortInnerPause`: same with a 200 ms pause → total 1.2 s ± 20 ms.
  - `TestCleanTake_NoWordsSilentTake`: 0.5 s of zeros, no words → non-empty, no panic.
  - `TestFadeEdges`: a take starting with loud voice → first sample is 0 and sample at 5 ms is at full envelope (≥ 0.9 × unfaded); a take starting with 100 ms silence then voice → fade spans 40 ms (sample at 20 ms is < 0.6 × unfaded noise-free reference). Use a constant 0.3 DC "voice" for the fade test so the envelope is exact.
- [ ] **Step 2: Run** `go test ./commands/ -run 'TestCleanTake|TestFadeEdges'` → FAIL.
- [ ] **Step 3: Implement** in `commands/takefit.go` with the spec's constants as named `const`s. Frames are 10 ms. Edge trim keeps at least one frame if every frame is below threshold. Pause shortening: runs of quiet frames strictly inside the take (not touching index 0 or the last frame), length > 350 ms → keep first and last 175 ms, linear 10 ms crossfade. Loud-edge test for fades: edge 40 ms RMS dB > (10th percentile of 50 ms frame dB) + 10.
- [ ] **Step 4: Run** full check → PASS.
- [ ] **Step 5: Commit** `feat: take cleaning for the Higgs take fit`.

### Task 3: WSOLA time compression

**Files:**
- Modify: `commands/takefit.go`
- Test: `commands/takefit_test.go`

**Interfaces:**
- Produces: `func wsola(x []float64, rate int, f float64) []float64`.

- [ ] **Step 1: Write failing tests:**
  - `TestWSOLA_IdentityAtOne`: f = 1.0 and f = 0.95 return a slice equal to the input.
  - `TestWSOLA_Length`: 2 s 220 Hz sine, f = 1.08 → `len` within ±1 % of `n/1.08`.
  - `TestWSOLA_PreservesPitch`: same input → zero-crossing rate of the middle 1 s of output is 220 Hz ± 2 % (count sign changes / 2 per second).
  - `TestWSOLA_ShorterThanFrame`: 10 ms input, f = 1.08 → unchanged.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** per the spec's WSOLA section: frame N = 30 ms, Hann, Hs = N/2, Ha = Hs·f, search ±10 ms for maximum normalized cross-correlation between the candidate frame's first Hs samples and the previous frame's natural continuation (the input samples at prevPos+Hs), overlap-add, divide by the summed window (skip where window sum < 1e-6). Output length `int(float64(len(x))/f)`, truncate or zero-pad the tail to it.
- [ ] **Step 4: Run** full check → PASS.
- [ ] **Step 5: Commit** `feat: pure-Go WSOLA time compression`.

### Task 4: Room and take selection

**Files:**
- Modify: `commands/takefit.go`
- Test: `commands/takefit_test.go`

**Interfaces:**
- Produces:
  - `func takeRoom(start, end time.Duration, next *time.Duration) time.Duration` — `next == nil` for the last cue.
  - `type takeCandidate struct { Seed int; Dur time.Duration; WER float64 }`
  - `func selectTake(cands []takeCandidate, window time.Duration) int` — returns the index into `cands`; `-1` if `cands` is empty.

- [ ] **Step 1: Write failing tests:**
  - `TestTakeRoom` table: window 0–3 s, next 4 s → 3.88 s; next 3 s → 3 s; next 2.5 s → 3 s; nil → 4 s.
  - `TestSelectTake`: (a) WER {0, 0.05, 0.3} with durs {3.5, 3.1, 3.0} s, window 3 s → index 1; (b) all WER > 0.1: {0.5, 0.2, 0.3} durs {3.0, 4.0, 3.2} → best-2 are WER 0.2 and 0.3 → index 2 (nearer length); (c) tie on rounded length → lower WER, then lower seed; (d) empty → −1.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**; the length key is `math.Round(|dur/window−1|*100)/100`.
- [ ] **Step 4: Run** full check → PASS.
- [ ] **Step 5: Commit** `feat: room and take selection for the Higgs take fit`.

### Task 5: Provider take-fit mode

**Files:**
- Modify: `commands/run.go` (`Model.roomMs int64`; second pass at the end of `parseSubtitleFile`)
- Modify: `commands/audiocpp.go` (provider fields, `transcribeDetails`, `CachePath`, `Synthesize`, WAV helpers)
- Modify: `CLAUDE.md` (one paragraph under the audio.cpp provider bullet)
- Test: `commands/audiocpp_test.go`, `commands/run_test.go`

**Interfaces:**
- Consumes: Task 1 `takeCount`, Task 2 `cleanTake`/`fadeEdges`/`asrWord`, Task 3 `wsola`, Task 4 `takeRoom`/`selectTake`/`takeCandidate`.
- Produces:
  - `audioCppProvider.takeFit bool`, `audioCppProvider.takes int` (from config).
  - `func (p *audioCppProvider) transcribeDetails(wav []byte) (text string, words []asrWord, sampleRate int, err error)` — POST `/v1/audio/transcriptions/details`, same multipart as `transcribe`.
  - `func scaleWordSamples(words []asrWord, from, to int) []asrWord`.
  - `func decodeWavFloat(b []byte) ([]float64, int, error)` / `func encodeWavFloat(x []float64, rate int) ([]byte, error)` (mono 16-bit).
  - `func (p *audioCppProvider) synthesizeTakeFit(items []Item, idx int) (string, time.Duration, error)`, called from `Synthesize` when `p.takeFit`.

- [ ] **Step 1: Write failing tests:**
  - `TestScaleWordSamples`: 16000 → 24000 doubles-and-a-half positions (8000 → 12000).
  - `TestParseSubtitleFile_TakeFitRoom` (run_test.go): two `[Hana]` cues 0–3 s and 4–6 s, provider `newAudioCppProvider(AudioCppConfig{TakeFit:true})`, speaker with voice ref (temp WAV) and reference text (temp file) → item 0 `Model.roomMs` == 3880, item 1 == 3000; with `TakeFit:false` both are 0.
  - `TestAudioCppProvider_CachePath` new subtest: take-fit mode template differs from free and fitted; changes when `model.roomMs` changes.
  - Extend `audioCppTestServer` with `/v1/audio/transcriptions/details` returning `{"text", "sample_rate": 22050, "words": [one word spanning the whole clip]}` and a `failSpeak map[int]bool` keyed by call number that returns 400 (non-retryable, so the test doesn't sleep through retry backoff).
  - `TestAudioCppProvider_SynthesizeTakeFit` subtests: (a) 3 takes, take 1 fails with 400, others succeed → returns a path with seed 2 or 3, no error; (b) all 3 fail → error containing `cue #1`; (c) takes of 4 s with window 3 s and `roomMs` 3000 → returned duration ≤ 4 s / 1.08 + 50 ms.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
  - `parseSubtitleFile`: after the loop, if the provider is an `*audioCppProvider` with `takeFit`, for each `i` set `items[i].Model.roomMs = takeRoom(start, end, next).Milliseconds()` (next = items[i+1].Sub.StartAt, nil for the last) and recompute `items[i].Path = provider.CachePath(root, items[i].Sub, items[i].Model)`.
  - `CachePath`: fit field `"takes" + takeFitVersion + N + windowMs + roomMs` when `takeFit`.
  - `synthesizeTakeFit`: spec data-flow steps 1–10. A `speak` or `transcribeDetails` error skips that seed (log it). Score with `transcribe` on the cleaned take (encoded at the take's rate). Window = cue end − start; room from `item.Model.roomMs`; stretch only when `dur > room`. Write with `encodeWavFloat` to `fmt.Sprintf(item.Path.Template, strconv.Itoa(seed))`.
  - `CLAUDE.md`: describe `take_fit` in the audio.cpp provider bullet (what it does, that it's an experiment branch, constants in `takefit.go`).
- [ ] **Step 4: Run** full check → PASS.
- [ ] **Step 5: Commit** `feat: take-fit mode in the audio.cpp provider`.

### Task 6: A/B/C experiment

**Files:** none in the repo. Work in the scratchpad: `$S` = this session's scratchpad dir.

- [ ] **Step 1:** Build `$S/dyson_en.vtt` from `~/audio.cpp-bin/poc/multilang/cues.json` and the 13 `GROUPS` in `fit_v3.py`: one cue per group, start of first − T0 to end of last − T0, `en` texts joined by a space, prefixed `[Hana] `.
- [ ] **Step 2:** Write `$S/a/config.yaml` and `$S/b/config.yaml`: default speaker Hana with `voice_ref_path: ~/audio.cpp-bin/poc/multilang/refs/hana_hr.wav` (absolute path), `reference_text_path: …/hana_hr.txt`, `audio_cpp: {base_url: http://127.0.0.1:8081, model: higgs-tts}`; B adds `take_fit: true`. `auth_key` is a dummy (ElevenLabs is not called).
- [ ] **Step 3:** For each of A and B: `session-upload`, `session-generate --provider audiocpp`, `session-export`, with `--config` and `--work-dir` set to that run's dir. Capture logs.
- [ ] **Step 4:** Copy the exports to `~/audio.cpp-bin/poc/multilang/listen/34 Higgs dubstudio – English unfitted.wav` and `35 Higgs dubstudio – English take fit.wav`.
- [ ] **Step 5:** Report per run: basketed lines, word misses per line, speed-ups, largest overrun past the window, lead shifts (from `session-show` JSON and the logs).
