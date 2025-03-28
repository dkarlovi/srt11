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
		Model string  `yaml:"model"`
		Name  string  `yaml:"name"`
		Speed float32 `yaml:"speed"`
	} `yaml:"default"`
	Models map[string]struct {
		Model string  `yaml:"model"`
		Name  string  `yaml:"name"`
		Speed float32 `yaml:"speed"`
	} `yaml:"models"`
}

type Model struct {
	model  string
	name   string
	offset int
	speed  float32
}

type Path struct {
	Path     string
	Template string
	Id       string
}

type Item struct {
	Sub   *astisub.Item
	Model Model
	Path  Path
}

type AudioFile struct {
	Item     Item
	Duration time.Duration
	Offset   time.Duration
	Channel  int
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

func generateModelChannelMap(config *Config) map[string]int {
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

func generatePathTemplate(root string, item *astisub.Item, model Model) Path {
	re := regexp.MustCompile(`[,.!?'<>:"/\\|?*\x00-\x1F]`)
	dialog := re.ReplaceAllString(item.String(), "")
	dialog = strings.ToLower(dialog)
	dialog = strings.Replace(dialog, " ", "_", -1)
	dialog = strings.TrimSpace(dialog)
	if len(dialog) > 50 {
		dialog = dialog[:50]
	}

	checksum := md5.Sum([]byte(model.model + fmt.Sprintf("%f", model.speed) + dialog))
	template := filepath.Join(root, fmt.Sprintf("%X-%s-%s.%%s.mp3", checksum[:4], model.name, dialog))

	glob := fmt.Sprintf(template, "*")
	if files, err := filepath.Glob(glob); err == nil && len(files) > 0 {
		// found the previously generated file, extract the ID out of it
		re := regexp.MustCompile(`([^.]+).mp3$`)
		match := re.FindStringSubmatch(filepath.Base(files[0]))
		if len(match) > 1 {
			return Path{Path: files[0], Template: template, Id: match[1]}
		}
	}

	return Path{Template: template}
}

func parseSubtitleFile(config *Config, path string) []Item {
	subs, err := astisub.OpenFile(path)
	if err != nil {
		log.Fatalf("Error parsing VTT file: %v", err)
	}

	modelChannels := generateModelChannelMap(config)
	items := make([]Item, 0)
	root, _ := filepath.Abs(filepath.Dir(path))
	for i, sub := range subs.Items {
		sub.Index = i
		var modelName string
		if sub.Lines[0].VoiceName != "" {
			modelName = sub.Lines[0].VoiceName
		} else if len(sub.Comments) > 0 {
			modelName = sub.Comments[0]
		} else {
			re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
			match := re.FindStringSubmatch(sub.String())
			if len(match) > 1 {
				modelName = match[1]
				sub.Lines[0].Items[0].Text = match[2]
			}
		}

		var model Model
		if modelName != "" {
			modelConfig := config.Models[modelName]
			model = Model{name: modelConfig.Name, model: modelConfig.Model, offset: modelChannels[modelName], speed: modelConfig.Speed}
		} else {
			model = Model{name: config.Default.Name, model: config.Default.Model, offset: 0, speed: config.Default.Speed}
		}

		item := Item{
			Sub:   sub,
			Model: model,
			Path:  generatePathTemplate(root, sub, model),
		}

		items = append(items, item)
	}

	return items
}

func generateMissingVoiceLines(client *elevenlabs.Client, items []Item) []AudioFile {
	audioFiles := make([]AudioFile, 0)
	for _, item := range items {
		if item.Path.Path != "" {
			duration, err := readAudioFileDuration(item.Path.Path)
			if err != nil {
				log.Fatalf("Error reading audio file %s duration: %v\n", item.Path.Path, err)
			}
			audioFiles = append(audioFiles, AudioFile{
				Item:     item,
				Offset:   item.Sub.StartAt,
				Channel:  item.Model.offset,
				Duration: duration,
			})
			continue
		}

		previousRequestIds := make([]string, 0)
		for i := item.Sub.Index - 1; i >= item.Sub.Index-3; i-- {
			if i < 0 || items[i].Path.Id == "" {
				continue
			}
			previousRequestIds = append(previousRequestIds, items[i].Path.Id)
		}

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

		log.Printf("Speaking (as %s) \"%s\"\n", item.Model.name, item.Sub.String())
		ttsReq := elevenlabs.TextToSpeechRequest{
			VoiceSettings: &elevenlabs.VoiceSettings{
				SpeakerBoost: true,
				Speed:        item.Model.speed,
			},
			Text:               item.Sub.String(),
			ModelID:            "eleven_multilingual_v2",
			PreviousRequestIds: previousRequestIds,
			NextRequestIds:     nextRequestIds,
			NextText:           nextText,
		}

		speech, id, err := client.TextToSpeechWithRequestID(item.Model.model, ttsReq)
		if err != nil {
			log.Fatal(err)
		}

		path := fmt.Sprintf(item.Path.Template, id)
		if err := os.WriteFile(path, speech, 0644); err != nil {
			log.Fatal(err)
		}
		log.Printf("Wrote %s\n", path)
		item.Path.Path = path

		duration, err := readAudioFileDuration(path)
		if err != nil {
			log.Fatalf("Error reading audio file %s duration: %v\n", item.Path.Path, err)
		}

		audioFiles = append(audioFiles, AudioFile{
			Item:     item,
			Offset:   item.Sub.StartAt,
			Channel:  item.Model.offset,
			Duration: duration,
		})
	}

	return audioFiles
}

func readAudioFileDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	decoder, err := mp3.NewDecoder(f)
	if err != nil {
		return 0, err
	}

	duration := float64(decoder.Length()) / (2 * float64(decoder.SampleRate()))
	return time.Duration(duration * float64(time.Second)), nil
}

func generateFinalAudioFile(files []AudioFile, outputPath string) error {
	const sampleRate = 44100
	const bitDepth = 16

	numChannels := 0
	for _, file := range files {
		numChannels = max(numChannels, file.Channel+1)
	}

	var maxEndTime time.Duration
	for _, file := range files {
		maxEndTime = max(maxEndTime, file.Offset+file.Duration)
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
		path := file.Item.Path.Path
		f, err := os.Open(path)
		defer f.Close()
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", path, err)
		}

		decoder, err := mp3.NewDecoder(f)
		if err != nil {
			return fmt.Errorf("failed to create decoder for %s: %w", path, err)
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

	client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)
	audioFiles := generateMissingVoiceLines(client, items)

	for _, file := range audioFiles {
		fmt.Printf(
			"#%03d\n%s\nSpeaker:  %s, speed: %.2f\nSubtitle: %s --> %s (duration %s)\nAudio:    %s --> %s (duration %s)\nPath:     %s\n\n",
			file.Item.Sub.Index+1,
			file.Item.Sub.String(),
			file.Item.Model.name,
			file.Item.Model.speed,
			file.Item.Sub.StartAt,
			file.Item.Sub.EndAt,
			file.Item.Sub.EndAt-file.Item.Sub.StartAt,
			file.Offset,
			file.Offset+file.Duration,
			file.Duration,
			file.Item.Path.Path,
		)
	}

	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_" + time.Now().Format("2006-01-02-15-04-05") + ".wav"
	if err := generateFinalAudioFile(audioFiles, outputPath); err != nil {
		log.Fatalf("Error writing final audio track: %v", err)
	}
	log.Printf("Final audio track written to %s\n", outputPath)
}
