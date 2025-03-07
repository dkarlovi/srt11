package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"github.com/asticode/go-astisub"
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

func writeWavHeader(w io.Writer, dataSize int, numChannels int) error {
	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	// Total file size - 8 bytes
	if err := binary.Write(w, binary.LittleEndian, uint32(dataSize+36)); err != nil {
		return err
	}
	// WAVE header
	if _, err := w.Write([]byte("WAVE")); err != nil {
		return err
	}
	// fmt chunk
	if _, err := w.Write([]byte("fmt ")); err != nil {
		return err
	}
	// fmt chunk size (16 bytes)
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	// Audio format (1 = PCM)
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	// Number of channels
	if err := binary.Write(w, binary.LittleEndian, uint16(numChannels)); err != nil {
		return err
	}
	// Sample rate
	if err := binary.Write(w, binary.LittleEndian, uint32(44100)); err != nil {
		return err
	}
	// Byte rate
	if err := binary.Write(w, binary.LittleEndian, uint32(44100*numChannels*2)); err != nil {
		return err
	}
	// Block align
	if err := binary.Write(w, binary.LittleEndian, uint16(numChannels*2)); err != nil {
		return err
	}
	// Bits per sample
	if err := binary.Write(w, binary.LittleEndian, uint16(16)); err != nil {
		return err
	}
	// data chunk
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	// data size
	if err := binary.Write(w, binary.LittleEndian, uint32(dataSize)); err != nil {
		return err
	}
	return nil
}

func combineAudioFiles(files []AudioFile, outputPath string, numChannels int) error {
	const sampleRate = 44100
	const bytesPerSample = 2 // 16-bit audio

	var totalLength int
	// First pass: calculate total length
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

	// Create the final audio buffer
	audioBuffer := make([]byte, totalLength)

	// Read and place each file's audio data
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
		buf := make([]byte, 4096)
		written := offsetBytes
		for {
			n, err := decoder.Read(buf)
			if n > 0 {
				copy(audioBuffer[written:], buf[:n])
				written += n
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
	}

	// Write WAV file
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := writeWavHeader(f, totalLength, numChannels); err != nil {
		return err
	}

	_, err = f.Write(audioBuffer)
	return err
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
