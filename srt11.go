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

func buildModelChannelMap(config *Config) map[string]int {
	channels := make(map[string]int)
	// Default model always goes to channel 0
	channels[config.Default.Name] = 0

	currentChannel := 1
	// Assign unique channels to each distinct model
	for _, model := range config.Models {
		if _, exists := channels[model.Name]; !exists {
			channels[model.Name] = currentChannel
			currentChannel++
		}
	}
	return channels
}

func parseSubtitleFile(config *Config, path string) []Item {
	subs, err := astisub.OpenFile(path)
	if err != nil {
		log.Fatalf("Error parsing VTT file: %v", err)
	}

	realDir, _ := filepath.Abs(filepath.Dir(path))
	modelChannels := buildModelChannelMap(config)
	items := make([]Item, 0)
	for i, sub := range subs.Items {
		var model Model
		sub.Index = i + 1
		if sub.Lines[0].VoiceName != "" {
			model = Model{name: config.Models[sub.Lines[0].VoiceName].Name, model: config.Models[sub.Lines[0].VoiceName].Model, offset: modelChannels[sub.Lines[0].VoiceName]}
		} else if len(sub.Comments) > 0 {
			model = Model{name: config.Models[sub.Comments[0]].Name, model: config.Models[sub.Comments[0]].Model, offset: modelChannels[sub.Comments[0]]}
		} else {
			re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
			match := re.FindStringSubmatch(sub.String())
			if len(match) > 1 {
				model = Model{name: config.Models[match[1]].Name, model: config.Models[match[1]].Model, offset: modelChannels[match[1]]}
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

	return items
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

func generateMissingVoiceLines(client *elevenlabs.Client, items []Item) []AudioFile {
	audioFiles := make([]AudioFile, 0)
	for _, item := range items {
		if _, err := os.Stat(item.Path); err == nil {
			log.Printf("Already spoke (as %s) \"%s\"\n", item.Model.name, item.Sub.String())
			audioFiles = append(audioFiles, AudioFile{Path: item.Path, Offset: item.Sub.StartAt, Channel: item.Model.offset})

			continue
		}

		log.Printf("Speaking (as %s) \"%s\"\n", item.Model.name, item.Sub.String())
		ttsReq := elevenlabs.TextToSpeechRequest{
			Text:    item.Sub.String(),
			ModelID: "eleven_multilingual_v2",
		}

		speech, err := client.TextToSpeech(item.Model.model, ttsReq)
		if err != nil {
			log.Fatal(err)
		}

		if err := os.WriteFile(item.Path, speech, 0644); err != nil {
			log.Fatal(err)
		}
		log.Printf("Wrote %s\n", item.Path)

		audioFiles = append(audioFiles, AudioFile{Path: item.Path, Offset: item.Sub.StartAt, Channel: item.Model.offset})
	}

	return audioFiles
}

func combineAudioFiles(files []AudioFile, outputPath string) error {
	const sampleRate = 44100
	const bitDepth = 16

	numChannels := 0
	for _, file := range files {
		numChannels = max(numChannels, file.Channel+1)
	}

	var maxEndTime time.Duration
	for _, file := range files {
		f, err := os.Open(file.Path)
		defer f.Close()
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", file.Path, err)
		}
		decoder, err := mp3.NewDecoder(f)
		if err != nil {
			return fmt.Errorf("failed to create decoder for %s: %w", file.Path, err)
		}
		duration := float64(decoder.Length()) / (2 * float64(decoder.SampleRate()))
		endTime := file.Offset + time.Duration(duration*float64(time.Second))
		if endTime > maxEndTime {
			maxEndTime = endTime
		}
	}

	totalFrames := int(maxEndTime.Seconds() * float64(sampleRate))
	totalSamples := totalFrames * numChannels
	mixBuffer := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: numChannels,
			SampleRate:  sampleRate,
		},
		Data:           make([]int, totalSamples),
		SourceBitDepth: bitDepth,
	}

	for _, file := range files {
		f, err := os.Open(file.Path)
		defer f.Close()
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", file.Path, err)
		}

		decoder, err := mp3.NewDecoder(f)
		if err != nil {
			return fmt.Errorf("failed to create decoder for %s: %w", file.Path, err)
		}

		offsetSamples := int(file.Offset.Seconds()*float64(sampleRate)) * numChannels
		tmpBuf := make([]byte, 4096)
		for {
			n, err := decoder.Read(tmpBuf)
			if n > 0 {
				for i := 0; i < n-1; i += 2 {
					if offsetSamples < len(mixBuffer.Data) {
						sample := int(int16(tmpBuf[i]) | int16(tmpBuf[i+1])<<8)
						mixBuffer.Data[offsetSamples] += sample // Mix by adding
						offsetSamples++
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("failed to read audio data: %w", err)
			}
		}
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer out.Close()

	enc := wav.NewEncoder(out, sampleRate, bitDepth, numChannels, 1)
	defer enc.Close()

	return enc.Write(mixBuffer)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path to subtitle file>", os.Args[0])
	}
	path := os.Args[1]

	config, err := readConfig("config.yaml")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	items := parseSubtitleFile(config, path)

	// TODO print items here in a readable format for debugging

	client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)
	audioFiles := generateMissingVoiceLines(client, items)

	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".wav"
	if err := combineAudioFiles(audioFiles, outputPath); err != nil {
		log.Fatalf("Error writing final audio track: %v", err)
	}
	log.Printf("Final audio track written to %s\n", outputPath)
}
