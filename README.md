# srt11

`srt11` uses ElevenLabs Text-to-Speech (TTS) to convert an `.srt` (or `.vtt`) subtitle file into a WAV audio track, matching the subtitle timings. This enables easy replacement of audio tracks in videos using only subtitles.

## Download

Pre-built binaries for Linux, Windows, and Mac are available at:  
[https://github.com/dkarlovi/srt11/releases/latest](https://github.com/dkarlovi/srt11/releases/latest)

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
        # https://github.com/dkarlovi/srt11?tab=readme-ov-file#speakers
        Joe:
            model: "joe_model_id"
            name: "Joe"
    ```
2. create [the ElevenLabs API key](https://elevenlabs.io/app/settings/api-keys) and paste as `auth_key` into the config file
3. download the [latest release](https://github.com/dkarlovi/srt11/releases/latest) appropriate for your system and unpack somewhere
4. from the folder where you keep your `config.yaml`, run the binary like so:
    ```sh
    srt11 data/130_EN.vtt
    ```
5. this will process the VTT/SRT file and produce output similar to this:
    ```
    2025/05/23 16:07:17 No merge threshold set, not merging lines
    #001
    ...so you shouldn't eat any teeth half an hour before...
    Speaker:  Hana, speed: 1.00
    Subtitle: 0s --> 2.69s (duration 2.69s)
    Audio:    0s --> 3.657s (duration 3.657s)
    Path:     /home/dkarlovi/Development/OSS/srt11/data/96EB5561-Hana-so_you_shouldnt_eat_any_teeth_half_an_hour_before.jajumpwyk0xFZQn4P41i.mp3
   
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

The speed of the audio can be adjusted in the config file. The default is 1.0, but you can set it to any value between 0.7 and 1.3. The speed is per speaker.

## Merge lines

If you have multiple lines in a row spoken by the same speaker, you can merge them into line.
You can either set the `merge_lines_threshold_ms` in the config file or use the `-m` / `--merge-lines-threshold` flag when running the program.

The default is no merging.
