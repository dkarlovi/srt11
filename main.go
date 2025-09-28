package main

import (
	"os"

	"github.com/dkarlovi/srt11/commands"
	"github.com/symfony-cli/console"
)

var (
	// version is overridden at linking time
	version = "dev"
	// buildDate is overridden at linking time
	buildDate string
)

func main() {
	app := &console.Application{
		Name:        "srt11",
		Usage:       "Convert subtitle files to audio using ElevenLabs TTS",
		Description: "Uses ElevenLabs Text-to-Speech (TTS) to convert an .srt (or .vtt) subtitle file into a WAV audio track, matching the subtitle timings.",
		Version:     version,
		BuildDate:   buildDate,
		Channel:     "stable",
		Flags: []console.Flag{
			&console.StringFlag{
				Name:         "config",
				Aliases:      []string{"c"},
				DefaultValue: "config.yaml",
				Usage:        "Path to config YAML file",
			},
		},
		Commands: commands.All(),
	}

	app.Run(os.Args)
}
