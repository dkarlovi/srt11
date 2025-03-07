package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/haguro/elevenlabs-go"
	"gopkg.in/yaml.v2"
)

type Config struct {
	AuthKey string            `yaml:"auth_key"`
	Models  map[string]string `yaml:"models"`
	Default string            `yaml:"default"`
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

func parseSRT(filename string) (*astisub.Subtitles, error) {
	return astisub.OpenFile(filename)
}

func generateVoiceLine(client *elevenlabs.Client, text, model, previousID string) (string, error) {
	// Implement the API call to ElevenLabs TTS here
	// This is a placeholder function
	return "path/to/generated/audio/file", nil
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <path to SRT file>", os.Args[0])
	}
	srtPath := os.Args[1]

	config, err := readConfig("config.yaml")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	subs, err := parseSRT(srtPath)
	if err != nil {
		log.Fatalf("Error parsing SRT file: %v", err)
	}

	client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)

	var previousID string
	for _, item := range subs.Items {
		text := item.String()
		model := config.Default
		if strings.HasPrefix(text, "@") {
			parts := strings.SplitN(text, " ", 2)
			if len(parts) > 1 {
				modelKey := strings.TrimPrefix(parts[0], "@")
				if m, ok := config.Models[modelKey]; ok {
					model = m
				} else {
					log.Fatalf("Unknown model: %s", modelKey)
				}
				text = parts[1]
			}
		}

		audioPath, err := generateVoiceLine(client, text, model, previousID)
		if err != nil {
			log.Fatalf("Error generating voice line: %v", err)
		}

		// Store the audioPath and handle timings here
		previousID = audioPath // Update previousID for the next line
	}

	// Combine audio files into a final file with separate tracks
	// Implement the audio combining logic here

	fmt.Println("Processing complete. Final file written to disk.")
}
