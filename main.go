package main

import (
	"os"

	"github.com/dkarlovi/srt11/commands"
	"github.com/symfony-cli/console"
)

func main() {
	app := &console.Application{
		Name:        "srt11",
		Usage:       "Convert subtitle files to audio using ElevenLabs TTS",
		Description: "Uses ElevenLabs Text-to-Speech (TTS) to convert an .srt (or .vtt) subtitle file into a WAV audio track, matching the subtitle timings.",
		Commands: []*console.Command{
			{
				Name:        "run",
				Usage:       "Convert subtitle files to audio using ElevenLabs TTS",
				Description: "Convert subtitle files to audio",
				Flags: []console.Flag{
					&console.IntFlag{
						Name:    "merge-lines-threshold-ms",
						Aliases: []string{"m"},
						Usage:   "Merge lines if same speaker and gap is below this threshold (ms)",
					},
					&console.StringFlag{
						Name:         "config",
						Aliases:      []string{"c"},
						DefaultValue: "config.yaml",
						Usage:        "Path to config YAML file",
					},
				},
				Action: func(c *console.Context) error {
					return commands.RunSrt11(c)
				},
			},
		},
	}

	app.Run(os.Args)
}
