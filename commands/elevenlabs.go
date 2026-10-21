package commands

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/haguro/elevenlabs-go"
	"github.com/hajimehoshi/go-mp3"
)

// elevenLabsProvider is the original TTSProvider implementation: everything
// this package did before the audio.cpp provider existed, unchanged in
// behavior, just moved behind the interface (see ttsprovider.go).
type elevenLabsProvider struct {
	client *elevenlabs.Client
}

func newElevenLabsProvider(authKey string) *elevenLabsProvider {
	return &elevenLabsProvider{client: elevenlabs.NewClient(context.Background(), authKey, 30*time.Second)}
}

func (p *elevenLabsProvider) Name() string { return "elevenlabs" }

func (p *elevenLabsProvider) CachePath(root string, sub *astisub.Item, model Model) Path {
	re := regexp.MustCompile(`[,.!?'<>:"/\\|?*\x00-\x1F]`)
	dialog := re.ReplaceAllString(sub.String(), "")
	dialog = strings.ToLower(dialog)
	dialog = strings.Replace(dialog, " ", "_", -1)
	dialog = strings.TrimSpace(dialog)
	if runes := []rune(dialog); len(runes) > 50 {
		// Truncate by rune, not byte: a byte slice can land mid-character on
		// multi-byte UTF-8 text (em dashes, curly quotes, accented letters),
		// producing an invalid UTF-8 filename that breaks any consumer
		// decoding this program's output as UTF-8.
		dialog = string(runes[:50])
	}

	// Everything that changes the produced audio goes into the checksum:
	// voice ID, TTS model, effective speed (per-speaker or per-line) and the
	// text. A cache hit is therefore just this hash plus a lookup on disk.
	checksum := md5.Sum([]byte(model.model + model.ttsModel + fmt.Sprintf("%f", model.speed) + sub.String()))
	template := filepath.Join(root, fmt.Sprintf("%X-%s-%s.%%s.mp3", checksum[:4], model.name, dialog))

	glob := fmt.Sprintf(template, "*")
	if files, err := filepath.Glob(glob); err == nil && len(files) > 0 {
		chosen := files[0]
		if len(files) > 1 {
			if newest, err := newestFile(files); err != nil {
				log.Printf("Warning: %d cache files match %s, but could not compare mod times (%v); using %s", len(files), glob, err, filepath.Base(chosen))
			} else {
				log.Printf("Warning: %d cache files match %s, using the newest: %s", len(files), glob, filepath.Base(newest))
				chosen = newest
			}
		}
		// found the previously generated file, extract the ID out of it
		re := regexp.MustCompile(`([^.]+).mp3$`)
		match := re.FindStringSubmatch(filepath.Base(chosen))
		if len(match) > 1 {
			return Path{Path: chosen, Template: template, Id: match[1]}
		}
	}

	return Path{Template: template}
}

// previousIdsFor picks the request IDs this line should stitch onto: the up
// to `want` nearest preceding cues (within `lookback` positions) that have
// already been generated, in timeline order regardless of speaker or speed.
// Request stitching is what makes a multi-speaker dialogue sound continuous
// rather than a set of disconnected monologues, so the chain always follows
// the actual sequence of lines -- it never skips a line because it's a
// different voice or carries a per-line @speed override. audio.cpp has no
// equivalent (each cue is synthesized independently), so this is
// ElevenLabs-specific and lives here, not in generateMissingVoiceLines.
func previousIdsFor(items []Item, item Item, want, lookback int) []string {
	ids := make([]string, 0, want)
	for i := item.Sub.Index - 1; i >= 0 && item.Sub.Index-i <= lookback && len(ids) < want; i-- {
		if id := items[i].Path.Id; id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Synthesize sends one cue to ElevenLabs with request-stitching context from
// its neighbors, retrying transient failures (see retry.go), and writes the
// mp3 to items[idx].Path.Template.
func (p *elevenLabsProvider) Synthesize(items []Item, idx int) (string, time.Duration, error) {
	item := items[idx]
	previousRequestIds := previousIdsFor(items, item, 3, 10)

	nextRequestIds := make([]string, 0)
	nextText := ""
	for i := item.Sub.Index + 1; i <= item.Sub.Index+3; i++ {
		if i >= len(items) {
			continue
		}
		if items[i].Path.Id == "" {
			nextText = items[i].Sub.String()
			break
		}
		nextRequestIds = append(nextRequestIds, items[i].Path.Id)
	}

	log.Printf("Speaking (as %s via %s) \"%s\"\n", item.Model.name, item.Model.ttsModel, item.Sub.String())
	ttsReq := elevenlabs.TextToSpeechRequest{
		VoiceSettings: &elevenlabs.VoiceSettings{
			SpeakerBoost: true,
			Speed:        item.Model.speed,
		},
		Text:               item.Sub.String(),
		ModelID:            item.Model.ttsModel,
		PreviousRequestIds: previousRequestIds,
		NextRequestIds:     nextRequestIds,
		NextText:           nextText,
	}

	var speech []byte
	var id string
	err := withRetry(defaultTTSRetryPolicy(), retryableTTSError, time.Sleep, func() error {
		var callErr error
		speech, id, callErr = p.client.TextToSpeechWithRequestID(item.Model.model, ttsReq)
		return callErr
	})
	if err != nil {
		return "", 0, fmt.Errorf("synthesizing cue #%d as %s: %w", item.Sub.Index+1, item.Model.name, err)
	}

	path := fmt.Sprintf(item.Path.Template, id)
	if err := os.WriteFile(path, speech, 0644); err != nil {
		return "", 0, fmt.Errorf("writing %s: %w", path, err)
	}
	log.Printf("Wrote %s\n", path)

	duration, err := p.ReadDuration(path)
	if err != nil {
		return "", 0, fmt.Errorf("reading audio file %s duration: %w", path, err)
	}
	return path, duration, nil
}

func (p *elevenLabsProvider) ReadDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	decoder, err := mp3.NewDecoder(f)
	if err != nil {
		return 0, err
	}

	duration := float64(decoder.Length()) / (4 * float64(decoder.SampleRate()))
	return time.Duration(duration * float64(time.Second)), nil
}

// mp3SampleRate is what generateFinalAudioFile's mix buffer has always
// assumed ElevenLabs output already is -- go-mp3's decoder reports the
// real rate from the stream itself, but the pre-refactor mixing loop never
// checked it or resampled, so this preserves that exact (implicit) prior
// behavior instead of silently changing it.
const mp3SampleRate = 44100

// DecodeSamples reads every sample from an mp3 decoder's left channel (the
// mixing loop has only ever used the left channel of the stereo PCM go-mp3
// decodes to, even for a mono voice source).
func (p *elevenLabsProvider) DecodeSamples(path string) ([]int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	decoder, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create decoder for %s: %w", path, err)
	}

	samples := make([]int, 0, decoder.Length()/4)
	tmpBuf := make([]byte, 4096)
	for {
		n, err := decoder.Read(tmpBuf)
		if n > 0 {
			for i := 0; i < n-1; i += 4 {
				samples = append(samples, int(int16(tmpBuf[i])|int16(tmpBuf[i+1])<<8))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return samples, mp3SampleRate, nil
}
