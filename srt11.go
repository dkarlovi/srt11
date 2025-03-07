package main

import (
	"context"
	"crypto/md5"
	"fmt"

	"github.com/asticode/go-astisub"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"github.com/haguro/elevenlabs-go"
	"github.com/hajimehoshi/go-mp3"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	AuthKey string `yaml:"auth_key"`
	Default struct {
		Model string `yaml:"model"`
		Name  string `yaml:"name"`
	} `yaml:"default"`
	Models map[string]struct {
		Model string `yaml:"model"`
		Name  string `yaml:"name"`
	} `yaml:"models"`
}

type Model struct {
	model  string
	name   string
	offset int
}

type Item struct {
	Sub   *astisub.Item
	Model Model
	Path  string
}

type AudioFile struct {
	Path    string
	Offset  time.Duration
	Channel int
}

func combineAudioFiles(files []AudioFile, outputPath string, numChannels int) error {
	const sampleRate = 44100
	const bytesPerSample = 2

	// Create output file
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Calculate total length
	var totalLength int
	for _, file := range files {
		f, err := os.Open(file.Path)
		if err != nil {
			return err
		}
		defer f.Close()

		decoder, err := mp3.NewDecoder(f)
		if err != nil {
			return err
		}
		offsetBytes := int(file.Offset.Seconds() * float64(sampleRate) * float64(numChannels) * float64(bytesPerSample))
		totalLength = max(totalLength, offsetBytes+int(decoder.Length()))
	}

	// Create WAV encoder
	enc := wav.NewEncoder(out, sampleRate, 16, numChannels, 1)
	defer enc.Close()

	// Process audio data
	samples := make([]int, totalLength/bytesPerSample)
	for _, file := range files {
		f, err := os.Open(file.Path)
		if err != nil {
			return err
		}
		defer f.Close()

		decoder, err := mp3.NewDecoder(f)
		if err != nil {
			return err
		}

		offsetSamples := int(file.Offset.Seconds() * float64(sampleRate) * float64(numChannels))
		buf := make([]byte, 4096)
		written := offsetSamples

		for {
			n, err := decoder.Read(buf)
			if n > 0 {
				// Convert bytes to samples directly
				for i := 0; i < n-1; i += 2 {
					if written < len(samples) {
						sample := int(int16(buf[i]) | int16(buf[i+1])<<8)
						samples[written] = sample
						written++
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
	}

	buf := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: numChannels,
			SampleRate:  sampleRate,
		},
		Data:           samples,
		SourceBitDepth: 16,
	}
	return enc.Write(buf)
}

func readConfig(filename string) (*Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func parseVTT(filename string) (*astisub.Subtitles, error) {
	return astisub.OpenFile(filename)
}

func generateFilename(item *astisub.Item, model Model) string {
	re := regexp.MustCompile(`[,.!?'<>:"/\\|?*\x00-\x1F]`)
	dialog := re.ReplaceAllString(item.String(), "")
	dialog = strings.ToLower(dialog)
	dialog = strings.Replace(dialog, " ", "_", -1)
	dialog = strings.TrimSpace(dialog)

	if len(dialog) > 50 {
		dialog = dialog[:50]
	}

	checksum := md5.Sum([]byte(model.name + dialog))

	return fmt.Sprintf("%04d-%s-%s.%X.mp3", item.Index, model.name, dialog, checksum[:2])
}

func generateVoiceLine(client *elevenlabs.Client, item *Item) {
	if _, err := os.Stat(item.Path); err == nil {
		log.Printf("Already spoke (as %s) \"%s\"\n", item.Model.name, item.Sub.String())
		return
	}

	log.Printf("Speaking (as %s) \"%s\"\n", item.Model.name, item.Sub.String())
	ttsReq := elevenlabs.TextToSpeechRequest{
		Text:    item.Sub.String(),
		ModelID: "eleven_multilingual_v2",
	}

	audio, err := client.TextToSpeech(item.Model.model, ttsReq)
	if err != nil {
		log.Fatal(err)
	}

	if err := os.WriteFile(item.Path, audio, 0644); err != nil {
		log.Fatal(err)
	}
	log.Printf("Wrote %s\n", item.Path)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path to VTT file>", os.Args[0])
	}
	vttPath := os.Args[1]

	config, err := readConfig("config.yaml")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	subs, err := parseVTT(vttPath)
	if err != nil {
		log.Fatalf("Error parsing VTT file: %v", err)
	}
	realDir, _ := filepath.Abs(filepath.Dir(vttPath))

	var items = make([]Item, 0)
	for i, sub := range subs.Items {
		var model Model
		sub.Index = i + 1
		if len(sub.Comments) > 0 {
			model = Model{name: config.Models[sub.Comments[0]].Name, model: config.Models[sub.Comments[0]].Model, offset: 1}
		} else {
			re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
			match := re.FindStringSubmatch(sub.String())
			if len(match) > 1 {
				model = Model{name: config.Models[match[1]].Name, model: config.Models[match[1]].Model, offset: 1}
				sub.Lines[0].Items[0].Text = match[2]
			} else {
				model = Model{name: config.Default.Name, model: config.Default.Model, offset: 0}
			}
		}

		path := filepath.Join(realDir, generateFilename(sub, model))
		item := Item{
			Sub:   sub,
			Model: model,
			Path:  path,
		}

		items = append(items, item)
	}

	// TODO print items here in a readable format for debugging

	// Generate voice lines for each item, skipping those that have already been generated
	client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)
	audioFiles := make([]AudioFile, 0)
	for _, item := range items {
		generateVoiceLine(client, &item)
		audioFiles = append(audioFiles, AudioFile{Path: item.Path, Offset: item.Sub.StartAt, Channel: 0})
	}

	// Write the final MP3 file
	outputPath := strings.TrimSuffix(vttPath, filepath.Ext(vttPath)) + ".wav"

	// TODO: 2 is hardcoded here, but it should be the number of channels in the final audio track
	// calculate it from the number of models
	if err := combineAudioFiles(audioFiles, outputPath, 2); err != nil {
		log.Fatalf("Error writing final audio track: %v", err)
	}
	log.Printf("Final audio track written to %s\n", outputPath)
}
