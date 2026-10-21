# Higgs take fit — design

Date: 2026-10-21. Branch: `feat/higgs-take-fit` (from `feat/audiocpp-provider`, PR #60).
Status: **experiment**. Local branch only. No push and no PR until Matko's listening test shows it helps.

## Goal

Make Higgs Audio v3 takes fit their cue windows inside dubstudio. Higgs has no duration control
(`duration_factor` is ignored, see PR #60 review item 6), so the fit is done on the audio after synthesis.
This ports parts of the method that worked in the POC (`~/audio.cpp-bin/poc/multilang/fit_v2.py`,
`fit_v3.py`, TEST-PLAN.md §v2/§v3b).

Success: in an A/B listening test on the same passage, Matko prefers B (take fit) over A (unfitted Higgs).

## Scope

In scope (POC parts 1, 2, 4):

1. N takes per cue, select by ASR word error rate, then by length.
2. Clean each take: cut at ASR word timings, trim edge silence, shorten inner pauses.
4. Timing: natural speed inside the room after the window, else a speed-up of 8 % or less.
   Any remaining overrun goes to AutoFix.

Out of scope (later sub-projects, each with its own spec):

- 3. Breath removal with ASR verification.
- 5. Recorded room-tone bed under the full track (needs a room-tone recording that does not exist yet).
- 6. One take per sentence (a new merge rule). The experiment groups cues into sentences by hand instead.
- ECAPA speaker similarity as a tie-breaker (the audio.cpp server has no such endpoint).
- The POC's "drift" (a later start for the next line). AutoFix's existing earlier start
  (`leadingGapsMs`, `MaxLeadShiftMs`) is used instead.

## Config

New optional fields on `AudioCppConfig` (`audio_cpp` in config.yaml):

| Field | Type | Default | Meaning |
|---|---|---|---|
| `take_fit` | bool | false | Turn on the take-fit mode described here. |
| `takes` | int | 6 | Takes per cue (N). Only read when `take_fit` is true. 0 or omitted means 6. |

`take_fit: true` together with `fit_duration: true` is a config error, reported by `readConfig`.
`takes` < 0 is also an error.

Method constants (not config, in `commands/takefit.go`):

| Constant | Value | Source |
|---|---|---|
| `takeFitLeadMargin` | 60 ms | before the first ASR word (fit_v3) |
| `takeFitTailMargin` | 100 ms | after the last ASR word (fit_v3) |
| `takeFitEdgeThresholdDb` | -40 dBFS | edge-silence trim threshold (fit_v2 `THR_DB`) |
| `takeFitEdgePad` | 30 ms | kept after trimming edge silence (fit_v2 `PAD`) |
| `takeFitPauseThresholdDb` | -42 dBFS | inner-pause detection, 10 ms frames (fit_v3 `shorten_pauses`) |
| `takeFitMaxPause` | 350 ms | longest inner pause kept (fit_v3 `MAX_PAUSE`) |
| `takeFitPauseXfade` | 10 ms | crossfade where a pause's middle is removed |
| `takeFitMaxSpeedup` | 1.08 | largest time-stretch factor (fit_v3 `MAX_SPEEDUP`) |
| `takeFitGap` | 120 ms | kept clear before the next cue (fit_v3 `GAP`) |
| `takeFitLastCueRoom` | 1 s | room after the last cue's window (fit_v3 `e_cue + 1.0`) |
| `takeFitGoodWER` | 0.10 | "good" take threshold (fit_v2) |
| `takeFitFade` / `takeFitLoudFade` | 40 ms / 5 ms | edge fades; 5 ms when the edge is loud (fit_v3) |
| `takeFitVersion` | `"takefit-v1"` | in the cache key; bump on any method change |

## Data flow (per cue, inside `audioCppProvider.Synthesize`)

When `take_fit` is false, `Synthesize` is unchanged.

When `take_fit` is true:

1. **Takes.** For seed `1..N`: `speak(text, seed, voiceRef, refText, nil)` (no `options`).
   A failed `speak` (any error, e.g. Higgs `max_tokens` on a short line) is logged and skips that seed.
   If all N fail, return an error that names the cue and the last failure.
2. **Word timings.** `POST /v1/audio/transcriptions/details` (multipart, same fields as `transcribe`).
   Response (verified 2026-10-21 against the local server):
   `{"text": "...", "sample_rate": 24000, "words": [{"word", "start_sample", "end_sample", "confidence"}], "timing": {...}}`.
   Word sample positions are at Parakeet's 16 kHz, **not** at `sample_rate` (which reports the input's
   rate): found in the first experiment run, where reading them at 24 kHz cut the last third of every line.
   `asrWordRate` uses 16 kHz unless that puts the last word > 5 % past the take's end. Positions come at
   80 ms steps; the first word can start at 0 even after leading silence.
   If `words` is empty, skip the cut (step 3a) for that take.
3. **Clean** (`cleanTake`, pure):
   a. Cut from `firstWord.start − takeFitLeadMargin` to `lastWord.end + takeFitTailMargin`, clamped to the take.
      If the cut would leave less than 300 ms, keep the whole take (fit_v3 rule).
   b. Trim edge silence: drop leading and trailing 10 ms frames below `takeFitEdgeThresholdDb`,
      keeping `takeFitEdgePad`.
   c. Shorten inner pauses: a run of 10 ms frames below `takeFitPauseThresholdDb` that is longer than
      `takeFitMaxPause` and is not at an edge keeps its first and last `takeFitMaxPause/2`, joined with a
      `takeFitPauseXfade` linear crossfade.
4. **Score.** `transcribe` the cleaned take. `misses = wordMisses(heard, text)`,
   `wer = misses / max(1, len(words(text)))`.
5. **Select** (`selectTake`, pure): `good` = takes with `wer ≤ takeFitGoodWER`; if none, the 2 takes with
   the lowest `wer`. From `good`, pick the minimum of `(round(|dur/window − 1|, 2), wer, seed)`.
6. **Room.** Read `item.Model.roomMs`, which `parseSubtitleFile` set with `takeRoom` (see Cache key).
   `takeRoom` (pure): `next` = start of the next item (any speaker).
   `room = max(window, next − takeFitGap − cueStart)`. Last cue: `room = window + takeFitLastCueRoom`.
7. **Stretch.** If `dur > room`: `f = min(dur/room, takeFitMaxSpeedup)`, and the take is `wsola(take, f)`.
   Otherwise `f = 1` and the take is unchanged.
8. **Fades** (`fadeEdges`, pure): for each edge, if the RMS of the edge's 40 ms is more than 10 dB above the
   take's 10th-percentile 50 ms level, fade over `takeFitLoudFade`, else over `takeFitFade`.
9. **Write** a mono 16-bit WAV at the take's own sample rate (24 kHz for Higgs) via `go-audio/wav`, at
   `fmt.Sprintf(item.Path.Template, seed)`. Return its path and duration.
   The existing mix resamples it to 44.1 kHz (`resampleLinear`).
10. **Log** one line per cue: seed, misses, raw and cleaned duration, window, room, factor,
    and the takes that failed.

AutoFix, Export and the session layer have no changes. AutoFix sees `Provider == "audiocpp"`: no speed bump;
an overrun is absorbed by an earlier start where possible, else basketed.

## WSOLA (`wsola`, pure, `commands/takefit.go`)

Pitch-preserving time compression for factors `1.0 < f ≤ 1.08`; `f ≤ 1.0` returns the input unchanged.

- Frame 30 ms, Hann window, synthesis hop = frame/2.
- Analysis hop = synthesis hop × f.
- For each frame, search ±10 ms around the nominal analysis position for the offset whose frame best
  continues the previous output (maximum normalized cross-correlation over the overlap), then overlap-add.
- Normalize by the summed window, so the output level matches the input.
- Output length ≈ `len(in) / f`.

## Cache key

`audioCppProvider.CachePath` in take-fit mode hashes: reference hash, reference text, model,
`takeFitVersion`, N, window ms, room ms, and the text. `room` depends on the next cue's start, so
retiming a neighbour makes the take again. Speed and seed stay out of the key (as today).
The fit field changes from `"free"` / `"fit…"` to a three-way value: `"free"`, `"fit…"`, `"takes…"`.

`CachePath` needs the next cue's start for `room`. Today it gets only `(root, sub, model)`.
`parseSubtitleFile` computes `Path` per cue in its loop, before the next cue is known, so the take-fit
cache path is computed in a second pass: after the loop, for each item, recompute `Path` with
`room` from `items[i+1]`. To keep the `TTSProvider` interface unchanged, the room is passed in on the
`Model` (new field `roomMs`, set in that second pass, zero for every other provider and mode).

**Take cache (added after the final review).** AutoFix retimes cues inside Generate (an earlier start),
which changes the cue's window and the previous cue's room. With only the key above, Export then made new,
unmeasured takes for both. So each cleaned take is also cached, with its ASR misses, in
`<cache>/<hash>-<speaker>-<slug>.takes/seed<k>.{wav,json}`, keyed on reference hash, reference text, model,
`takeFitVersion` and text — no window, no room. A changed window or room rebuilds the final file from these
(selection, stretch, fades) with no server calls. A seed that failed with a 4xx is cached as failed; a
transient failure is not, so it is tried again.

## Error handling

- One failed take: logged, skipped.
- All takes failed: error from `Synthesize` (→ `generateMissingVoiceLines` → 502 in serve, exit 1 in run).
- `/details` failure: treated like a failed take for that seed.
- Config conflicts: `readConfig` error at startup.

## Tests

Unit tests, no server (`commands/takefit_test.go`):

- `cleanTake`: margins around word timings; whole take kept when the cut is under 300 ms;
  edge trim keeps 30 ms; a 1 s inner pause becomes 350 ms ± 10 ms; a 200 ms pause is unchanged;
  edge pauses are not shortened.
- `wsola`: f = 1.0 returns the input unchanged; f = 1.08 gives length `n/1.08` ± 1 %;
  a 220 Hz sine stays at 220 Hz ± 2 % (zero-crossing rate).
- `takeRoom`: next cue after a pause; next cue abutting (room = window); next cue overlapping
  (room = window); last cue.
- `selectTake`: good-rule; fallback to best 2; nearest length; tie-break by WER then seed.
- `fadeEdges`: loud edge gets 5 ms, quiet edge gets 40 ms.

Provider tests (`commands/audiocpp_test.go`, fake server with `/details` added):

- One failing seed is skipped and another seed is chosen.
- All seeds failing is an error.
- The written take is cut and, when longer than `room`, stretched (duration ≤ raw/1.08 + tolerance).
- The cache template changes when the next cue's start moves.

Config test: `take_fit` + `fit_duration` is an error; `takes: -1` is an error; `takes` omitted gives 6.

Parser test: in take-fit mode, each item's `Model.roomMs` comes from the next item's start.

## Experiment (A/B/C)

Material: the Dyson passage, English, from `~/audio.cpp-bin/poc/multilang/cues.json`, grouped by the POC's
13 sentence `GROUPS` (`fit_v3.py`) into 13 cues: start of the group's first cue to end of its last cue,
texts joined with a space. Times shifted so the first cue starts at 0. Every cue tagged `[Hana]`.
Voice: `refs/hana_hr.wav` + `refs/hana_hr.txt`, model `higgs-tts`.

Each run uses the session flow (upload → generate → export, so AutoFix runs) in its own work directory,
merging off (`merge_lines_threshold_ms` unset):

- **A** — Higgs unfitted: no fit flags.
- **B** — Higgs take fit: `take_fit: true`.
- **C** — POC reference: `listen/10 Higgs clone – English v3b (breaths removed).wav` (not regenerated).

A and B exports are copied to `~/audio.cpp-bin/poc/multilang/listen/` as
`34 Higgs dubstudio – English unfitted.wav` and `35 Higgs dubstudio – English take fit.wav`.

Numbers reported for A and B: lines basketed, ASR word misses per line, speed-up factors used,
largest overrun past the window, and AutoFix lead shifts.

Known bias: C has a room-tone bed and its breaths removed; A and B have neither. The question
"does the fit help" is answered by **A against B**. C shows the remaining distance to the POC.

## Decision after the test

- B preferred over A: keep the branch; next sub-projects are breaths (3), room tone (5), sentence merge (6).
- No clear preference: report the numbers, and decide with Matko whether to tune or drop.
- A preferred: drop the branch.
