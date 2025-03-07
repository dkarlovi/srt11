package main

import (
	"crypto/md5"
	"fmt"
	"github.com/asticode/go-astisub"
	"github.com/haguro/elevenlabs-go"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	model string
	name  string
}

type Item struct {
	Sub   *astisub.Item
	Model Model
	Path  string
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

	return fmt.Sprintf("%04d-%s-%s.%X.wav", item.Index, model.name, dialog, checksum[:2])
}

func generateVoiceLine(client *elevenlabs.Client, item *Item) {

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
			model = Model{name: config.Models[sub.Comments[0]].Name, model: config.Models[sub.Comments[0]].Model}
		} else {
			re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
			match := re.FindStringSubmatch(sub.String())
			if len(match) > 1 {
				model = Model{name: config.Models[match[1]].Name, model: config.Models[match[1]].Model}
				sub.Lines[0].Items[0].Text = match[2]
			} else {
				model = Model{name: config.Default.Name, model: config.Default.Model}
			}
		}

		path := filepath.Join(realDir, generateFilename(sub, model))
		item := Item{
			Sub:   sub,
			Model: model,
			Path:  path,
		}
		log.Print(item)

		items = append(items, item)
	}

	// TODO print items here in a readable format for debugging

	// client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)

	fmt.Println("Processing complete. Final file written to disk.")
}
