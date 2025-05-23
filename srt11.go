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
	"github.com/jessevdk/go-flags"
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

// --- Colorizing output ---

var (
	colorReset  = "\033[0m"
	colorGreen  = "\033[92m" // light green
	colorYellow = "\033[93m" // bright yellow
	colorRed    = "\033[91m"
	colorBlue   = "\033[94m" // bright blue
	colorCyan   = "\033[96m"
	colorGray   = "\033[90m"
)

var tagColors = map[string]string{
	"info":  colorGreen,
	"time":  colorYellow,
	"error": colorRed,
	"warn":  colorBlue,
	"note":  colorCyan,
	"debug": colorGray,
}

// Go's regexp doesn't support backreferences, so match <tag>...</tag> and check tag equality in code.
var tagRegex = regexp.MustCompile(`<([a-zA-Z]+)>(.*?)</([a-zA-Z]+)>`)

func ColorizeTags(s string) string {
	return tagRegex.ReplaceAllStringFunc(s, func(m string) string {
		matches := tagRegex.FindStringSubmatch(m)
		if len(matches) == 4 && matches[1] == matches[3] {
			color, ok := tagColors[matches[1]]
			if ok {
				return color + matches[2] + colorReset
			}
		}
		return m
	})
}

// --- end colorizing output ---

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
	MergeLinesThresholdMs int `yaml:"merge_lines_threshold_ms"` // optional
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
	Sub        *astisub.Item
	Model      Model
	Path       Path
	MergedFrom []string // timings of merged-from lines
}

type AudioFile struct {
	Item     Item
	Duration time.Duration
	Offset   time.Duration
	Channel  int
	Overlap  time.Duration
}

type Options struct {
	MergeLinesThresholdMs int    `short:"m" long:"merge-lines-threshold-ms" description:"Merge lines if same speaker and gap is below this threshold (ms)"`
	ConfigPath            string `short:"c" long:"config" description:"Path to config YAML file" default:"config.yaml"`
	Help                  bool   `short:"h" long:"help" description:"Show this help message"`
	Positional            struct {
		Path string `description:"Path to subtitle file" positional-arg-name:"path"`
	} `positional-args:"yes"`
}

var opts Options

func printHelp(parser *flags.Parser) {
	parser.WriteHelp(os.Stdout)
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

	checksum := md5.Sum([]byte(model.model + fmt.Sprintf("%f", model.speed) + item.String()))
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

func parseSubtitleFile(config *Config, path string, mergeLinesThresholdMs int) []Item {
	subs, err := astisub.OpenFile(path)
	if err != nil {
		log.Fatalf("Error parsing VTT file: %v", err)
	}

	modelChannels := generateModelChannelMap(config)
	items := make([]Item, 0)
	root, _ := filepath.Abs(filepath.Dir(path))

	// Merge logic
	type mergedResult struct {
		item       *astisub.Item
		mergedFrom []string
	}
	mergedSubs := make([]mergedResult, 0)
	i := 0
	for i < len(subs.Items) {
		cur := subs.Items[i]
		// Determine speaker for current line
		var curSpeaker string
		if cur.Lines[0].VoiceName != "" {
			curSpeaker = cur.Lines[0].VoiceName
		} else if len(cur.Comments) > 0 {
			curSpeaker = cur.Comments[0]
		} else {
			re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
			match := re.FindStringSubmatch(cur.String())
			if len(match) > 1 {
				curSpeaker = match[1]
			}
		}
		// Prepare to merge into a single line
		mergedText := cur.String()
		mergedStart := cur.StartAt
		mergedEnd := cur.EndAt
		mergedVoiceName := cur.Lines[0].VoiceName
		mergedComments := cur.Comments
		mergedFrom := []string{
			ColorizeTags(fmt.Sprintf("<time>%s</time> --> <time>%s</time> (duration <time>%s</time>) | <info>%s</info>",
				cur.StartAt.Round(time.Millisecond),
				cur.EndAt.Round(time.Millisecond),
				(cur.EndAt - cur.StartAt).Round(time.Millisecond),
				strings.TrimSpace(cur.String()),
			)),
		}
		for {
			// Try to merge with next lines if threshold is set
			if mergeLinesThresholdMs > 0 && i+1 < len(subs.Items) {
				next := subs.Items[i+1]
				var nextSpeaker string
				if next.Lines[0].VoiceName != "" {
					nextSpeaker = next.Lines[0].VoiceName
				} else if len(next.Comments) > 0 {
					nextSpeaker = next.Comments[0]
				} else {
					re := regexp.MustCompile(`\[(.*?)\]\s*(.+)`)
					match := re.FindStringSubmatch(next.String())
					if len(match) > 1 {
						nextSpeaker = match[1]
					}
				}
				gap := next.StartAt - mergedEnd
				if curSpeaker == nextSpeaker && gap.Milliseconds() >= 0 && gap.Milliseconds() <= int64(mergeLinesThresholdMs) {
					// Merge: extend end time, concat text
					mergedEnd = next.EndAt
					mergedText = strings.TrimSpace(mergedText) + " " + strings.TrimSpace(next.String())
					mergedFrom = append(mergedFrom, ColorizeTags(fmt.Sprintf("<time>%s</time> --> <time>%s</time> (duration <time>%s</time>) | <info>%s</info>",
						next.StartAt.Round(time.Millisecond),
						next.EndAt.Round(time.Millisecond),
						(next.EndAt-next.StartAt).Round(time.Millisecond),
						strings.TrimSpace(next.String()),
					)))
					i++
					continue
				}
			}
			break
		}
		// Create a new astisub.Item with the merged text as a single line
		mergedItem := &astisub.Item{
			StartAt: mergedStart,
			EndAt:   mergedEnd,
			Lines: []astisub.Line{
				{
					VoiceName: mergedVoiceName,
					Items: []astisub.LineItem{
						{Text: mergedText},
					},
				},
			},
			Comments: mergedComments,
		}
		mergedSubs = append(mergedSubs, mergedResult{item: mergedItem, mergedFrom: mergedFrom})
		i++
	}

	for i, res := range mergedSubs {
		sub := res.item
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
			Sub:        sub,
			Model:      model,
			Path:       generatePathTemplate(root, sub, model),
			MergedFrom: res.mergedFrom,
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

		log.Printf("Speaking (as %s) \"%s\"\n", item.Model.name, ColorizeTags("<info>"+item.Sub.String()+"</info>"))
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

	duration := float64(decoder.Length()) / (4 * float64(decoder.SampleRate()))
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

		startFrame := int(file.Offset.Seconds() * float64(sampleRate))
		tmpBuf := make([]byte, 4096)
		currentFrame := 0
		for {
			n, err := decoder.Read(tmpBuf)
			if n > 0 {
				// Process samples in pairs of bytes (16-bit samples, 2 channels)
				for i := 0; i < n-1; i += 4 {
					frame := startFrame + (currentFrame / 4)
					if frame < totalFrames {
						sample := int(int16(tmpBuf[i]) | int16(tmpBuf[i+1])<<8)
						pos := (frame * numChannels) + file.Channel
						if pos < len(mixBuffer.Data) {
							mixBuffer.Data[pos] += sample
						}
					}
					currentFrame += 4
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
	parser := flags.NewParser(&opts, flags.Default)
	parser.Usage = "[OPTIONS] <path to subtitle file>"
	_, err := parser.Parse()
	if err != nil {
		os.Exit(1)
	}

	if opts.Help || opts.Positional.Path == "" {
		printHelp(parser)
		os.Exit(1)
	}
	path := opts.Positional.Path

	config, err := readConfig(opts.ConfigPath)
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	threshold := config.MergeLinesThresholdMs
	if opts.MergeLinesThresholdMs > 0 {
		threshold = opts.MergeLinesThresholdMs
	}
	if threshold > 0 {
		log.Printf("Using merge threshold: %d", threshold)
	} else {
		log.Printf("No merge threshold set, not merging lines")
	}

	items := parseSubtitleFile(config, path, threshold)

	client := elevenlabs.NewClient(context.Background(), config.AuthKey, 30*time.Second)
	audioFiles := generateMissingVoiceLines(client, items)

	overlaps := make([]AudioFile, 0)
	for i, file := range audioFiles {
		fileEndAt := file.Offset + file.Duration
		var overlap bool
		var overlapText string
		if i < len(audioFiles)-1 {
			nextFile := audioFiles[i+1]
			overlap = fileEndAt > nextFile.Offset && file.Item.Model.model == nextFile.Item.Model.model
			file.Overlap = fileEndAt - nextFile.Offset
			if overlap {
				overlapText = ColorizeTags(fmt.Sprintf(" (<warn>OVERLAP %s</warn>)", file.Overlap.Round(time.Millisecond)))
				overlaps = append(overlaps, file)
			}
		}

		fmt.Printf(
			ColorizeTags(
				"#%03d\n<info>%s</info>\nSpeaker:  <note>%s</note>, speed: %.2f\nSubtitle: <time>%s</time> --> <time>%s</time> (duration <time>%s</time>)\nAudio:    <time>%s</time> --> <time>%s</time> (duration <time>%s</time>)%s\nPath:     <debug>%s</debug>\n",
			),
			file.Item.Sub.Index+1,
			file.Item.Sub.String(),
			file.Item.Model.name,
			file.Item.Model.speed,
			file.Item.Sub.StartAt.Round(time.Millisecond),
			file.Item.Sub.EndAt.Round(time.Millisecond),
			(file.Item.Sub.EndAt - file.Item.Sub.StartAt).Round(time.Millisecond),
			file.Offset.Round(time.Millisecond),
			fileEndAt.Round(time.Millisecond),
			file.Duration.Round(time.Millisecond),
			overlapText,
			file.Item.Path.Path,
		)
		// Print merged-from info if present
		if len(file.Item.MergedFrom) > 1 {
			fmt.Printf("Merged from:\n")
			for _, line := range file.Item.MergedFrom {
				parts := strings.SplitN(line, " | ", 2)
				if len(parts) == 2 {
					fmt.Printf(
						ColorizeTags("    %s\n    %s\n"),
						parts[1], parts[0],
					)
				} else {
					fmt.Printf(ColorizeTags("    %s\n"), line)
				}
			}
		}
		fmt.Printf("\n")
	}

	if len(overlaps) > 0 {
		fmt.Println(ColorizeTags("<warn>Overlaps detected:</warn>"))
		for _, overlap := range overlaps {
			fmt.Printf(
				ColorizeTags("#%03d <warn>%s</warn>\n<info>%s</info>\n\n"),
				overlap.Item.Sub.Index+1,
				overlap.Overlap.Round(time.Millisecond),
				overlap.Item.Sub.String(),
			)
		}
		fmt.Println("Fix and rerun the script to generate the final audio file.")
		os.Exit(1)
	}

	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + "_" + time.Now().Format("2006-01-02-15-04-05") + ".wav"
	if err := generateFinalAudioFile(audioFiles, outputPath); err != nil {
		log.Fatalf("Error writing final audio track: %v", err)
	}
	log.Printf("Final audio track written to %s\n", outputPath)
}
