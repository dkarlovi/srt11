# dubstudio

`dubstudio` uses ElevenLabs Text-to-Speech (TTS) to convert an `.srt` (or `.vtt`) subtitle file into a WAV audio track, matching the subtitle timings. This enables easy replacement of audio tracks in videos using only subtitles.

## Download

Pre-built binaries for Linux, Windows, and Mac are available at:  
[https://github.com/dkarlovi/dubstudio/releases/latest](https://github.com/dkarlovi/dubstudio/releases/latest)

## Usage

1. Create the config file [`config.yaml`](./config.yaml.dist) as so:
   ```yaml
   auth_key: "sk_your_auth_key"
   default:
     model: "model_id"
     name: "Speaker name"
     speed: 1.1
   # Optional: merge lines if same speaker and gap is below this threshold (in ms)
   merge_lines_threshold_ms: 50
   models:
     # Optional: add custom speakers
     # https://github.com/dkarlovi/dubstudio?tab=readme-ov-file#speakers
     Joe:
       model: "joe_model_id"
       name: "Joe"
   ```
2. create [the ElevenLabs API key](https://elevenlabs.io/app/settings/api-keys) and paste as `auth_key` into the config file
3. download the [latest release](https://github.com/dkarlovi/dubstudio/releases/latest) appropriate for your system and unpack somewhere
4. from the folder where you keep your `config.yaml`, run the binary like so:
   ```sh
   dubstudio run data/130_EN.vtt
   ```
   The `run` command takes a single required argument: the path to the `.srt` or `.vtt` file. If your config lives elsewhere, point at it with `-c` / `--config`:
   ```sh
   dubstudio run --config /path/to/config.yaml data/130_EN.vtt
   ```
5. this will process the VTT/SRT file and produce output similar to this:
   ```
   2025/05/23 16:07:17 No merge threshold set, not merging lines
   #001
   ...so you shouldn't eat any teeth half an hour before...
   Speaker:  Hana, speed: 1.00
   Subtitle: 0s --> 2.69s (duration 2.69s)
   Audio:    0s --> 3.657s (duration 3.657s)
   Path:     /home/dkarlovi/Development/OSS/dubstudio/data/96EB5561-Hana-so_you_shouldnt_eat_any_teeth_half_an_hour_before.jajumpwyk0xFZQn4P41i.mp3

   (...stuff...)

   2025/05/23 16:07:18 Final audio track written to data/130_EN_2025-05-23-16-07-17.wav
   ```
6. the file is ready to be used

## Speakers

By default, all lines are read by the `default` speaker. You can override this per line in one of these ways (they are mutually exclusive and detected in this order):

1. Add a VTT speaker (only `.vtt` files):
   ```
   00:22.980 --> 00:23.300
   <v Matko>What do we do?</v>
   ```
2. Add a VTT comment (only `.vtt` files):
   ```
   NOTE Matko
   00:22.980 --> 00:23.300
   What do we do?
   ```
3. Add in square brackets in front of the line:
   ```
   00:22.980 --> 00:23.300
   [Matko]What do we do?
   ```

Each named speaker must be defined in the `config.yaml` file. The default speaker is used if no speaker is defined.

## Speed

The speed of the audio can be adjusted in the config file. The default is 1.0, but you can set it to any value between 0.7 and 1.2. The speed is per speaker.

### Per-line speed

A single line can override its speaker's speed by appending `@speed` to the speaker tag. This works with all three speaker forms, and with no speaker name at all for the default speaker:

```
[Matko@1.15]What do we do?

<v Matko@1.15>What do we do?</v>

NOTE Matko@1.15
What do we do?

[@1.15]A line for the default speaker, a little faster.
```

The speed is resolved per line in this order: the line's own `@speed`, then the speaker's `speed`, then `default.speed`, then `1.0`. A value outside 0.7-1.2 is rejected when the subtitle file is parsed, rather than being clamped silently.

The effective speed is part of the cache key, so each speed of a line is kept as its own file. Both takes stay on disk and the final track is assembled from whichever speed is currently set.

A line with a per-line speed is treated as a branch off [request stitching](https://elevenlabs.io/docs/eleven-api/guides/how-to/text-to-speech/request-stitching) rather than a link in the chain: lines at the speaker's normal speed step over it, so changing one line's speed does not force the rest of the file to regenerate. Consecutive lines sharing the same override stitch onto each other.

## TTS providers

`dubstudio` supports two TTS providers behind one interface: **ElevenLabs** (the default, described throughout this README) and **[audio.cpp](https://github.com/dkarlovi/audio.cpp)**, local voice cloning via IndexTTS2 that runs entirely on your own machine. Everything below this section — speakers, speed, merge lines, overlap detection, normalization — describes the ElevenLabs provider; using audio.cpp instead changes only how each cue's audio gets synthesized.

audio.cpp is off by default. To enable it, add an `audio_cpp` section to your config and give each speaker that should use it a voice reference:

```yaml
auth_key: "sk_your_auth_key"   # still required: at least one speaker may still use ElevenLabs
default:
  model: "model_id"
  name: "Hana"
  speed: 1.1
  # A local WAV (~28s, mono) plus its exact transcript -- audio.cpp clones
  # this voice on every request. Only used when a call selects "audiocpp".
  voice_ref_path: "/path/to/hana.wav"
  reference_text_path: "/path/to/hana.txt"
models:
  Matko:
    model: "matko_voice_id"
    name: "Matko"
    voice_ref_path: "/path/to/matko.wav"
    reference_text_path: "/path/to/matko.txt"

# Never hardcoded -- the audio.cpp server must already be running here.
audio_cpp:
  base_url: "http://127.0.0.1:8081"
  model: "indextts-clone"
```

The provider is chosen **per call**, not by a global switch: `run`/`session-generate`/`session-export`/`serve` all take a `--provider` flag (`elevenlabs`, the default, or `audiocpp`), and the HTTP API's `POST /generate` takes the same choice as a `"provider"` field in its JSON body. Once a session has generated with a provider, Export reuses it automatically.

A few things work differently under audio.cpp:

- **No speed knob.** ElevenLabs paces a line with its `speed` setting; audio.cpp instead fits each line to its exact subtitle window at synthesis time (`duration_factor`), so `speed`/per-line `@speed` tags have no effect under this provider. If a line genuinely can't be shortened enough to fit its window, the fit is clamped and the line is flagged for a human to reword — there's no speed lever left to try.
- **No request stitching.** Every cue is synthesized independently; ElevenLabs' cross-line continuity has no equivalent here.
- **Runs entirely on your machine, for free** — no ElevenLabs credits spent, and the audio.cpp server itself must already be running (`voice_ref_path` is a local file path, not uploaded audio, so this only works with the server on the same host as `dubstudio`).
- **Slower.** Each cue synthesizes a "free" take, fits it, and ASR-verifies the result, retrying with a new seed on a mismatch — roughly 15-20s per cue on Apple Silicon (Metal), more on a retry.

## TTS model

By default, `dubstudio` generates speech with the [`eleven_multilingual_v2`](https://elevenlabs.io/docs/models) ElevenLabs model. You can choose a different model with the optional `tts_model` field, either globally or per speaker:

```yaml
auth_key: "sk_your_auth_key"
# Applies to every speaker unless overridden below
tts_model: "eleven_multilingual_v2"
default:
  model: "model_id"
  name: "Speaker name"
  speed: 1.1
models:
  Joe:
    model: "joe_model_id"
    name: "Joe"
    # Overrides the top-level tts_model just for Joe
    tts_model: "eleven_flash_v2_5"
```

The model is resolved per line in this order: the speaker's own `tts_model`, then the top-level `tts_model`, then the built-in default (`eleven_multilingual_v2`). Omitting the field entirely keeps the previous behaviour, so existing configs are unaffected.

> [!NOTE]
> `tts_model` is the ElevenLabs **model** (the synthesis engine). The `model` field under each speaker is the ElevenLabs **voice ID** and is unrelated.

> [!WARNING]
> `dubstudio` relies on the per-speaker `speed` setting and on [request stitching](https://elevenlabs.io/docs/eleven-api/guides/how-to/text-to-speech/request-stitching) for timing and cross-line continuity. Neither is supported by `eleven_v3`, so that model is not recommended here. `eleven_multilingual_v2` and the `eleven_flash_v2_5` / `eleven_flash_v2` models support both and are safe choices.

The selected model is included in the generated file's cache key, so switching models re-generates audio instead of reusing files produced by a different model. The run log also prints the model used for each line, e.g. `Speaking (as Joe via eleven_flash_v2_5)`.

## Merge lines

If you have multiple lines in a row spoken by the same speaker, you can merge them into line.
You can either set the `merge_lines_threshold_ms` in the config file or use the `-m` / `--merge-lines-threshold-ms` flag when running the program. The flag takes precedence over the config file.

The default is no merging.

Use `--merge-max-ms` to cap how long a merged cue's total window (its first line's start to its last line's end) is allowed to grow. Once folding the next line in would push the window past this cap, that cue is closed and a new one is started instead — so a long run of short, abutting same-speaker lines can't fold into one arbitrarily long take. The default, `0`, is unlimited (today's behaviour if you don't pass it). A cross-speaker boundary always breaks a merge regardless of this setting.

```sh
dubstudio run -m 120 --merge-max-ms 6000 data/130_EN.vtt
```

## Overlap detection

Because each speaker's lines share a single audio channel, two same-speaker cues whose audio overlaps in time will audibly play on top of each other. By default `dubstudio` treats any such overlap as fatal: it prints the offending cues under `Overlaps detected:` and exits with a non-zero status without writing the final WAV.

If a small amount of overlap is acceptable for your material, allow it with `-t` / `--overlap-tolerance-ms`: overlaps up to this many milliseconds are tolerated and the final audio is still written. The default is `0` (no tolerance, the original behaviour).

```sh
dubstudio run -t 150 data/130_EN.vtt
```

Overlaps between _different_ speakers are handled separately, since some cross-talk between speakers is often natural rather than a defect. Cross-speaker overlaps are always reported per-cue as `(CROSS-OVERLAP Nms)` in the run log, but by default they never fail the run. Pass `--cross-overlap-tolerance-ms` with a value `>= 0` to additionally gate on them past that many milliseconds, the same way `--overlap-tolerance-ms` gates same-speaker overlaps. The default is `-1` (report only, never fail).

```sh
dubstudio run --cross-overlap-tolerance-ms 300 data/130_EN.vtt
```

## Audio normalization

ElevenLabs' generated output level can vary noticeably between requests, even for the same voice — one line can end up audibly louder or quieter than its neighbours in the final mix. `dubstudio` corrects for this by gaining each cue's audio toward a consistent RMS level before mixing, controlled by `--normalize-target-db` (in dBFS). The default is `-20`, a typical target for spoken-word content; passing `0` or a positive value disables normalization entirely.

The gain applied to any single cue is capped at +24dB boost, so a near-silent or broken generation doesn't get amplified into audible noise — it just stays an obvious outlier to catch on review — and the result is clamped to a -1dBFS ceiling so normalization itself can never introduce clipping.

```sh
dubstudio run --normalize-target-db -18 data/130_EN.vtt
```
