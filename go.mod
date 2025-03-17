module github.com/dkarlovi/srt11

go 1.23.6

require (
	github.com/asticode/go-astisub v0.34.0
	github.com/go-audio/audio v1.0.0
	github.com/go-audio/wav v1.1.0
	github.com/haguro/elevenlabs-go v0.2.4
	github.com/hajimehoshi/go-mp3 v0.3.4
	gopkg.in/yaml.v2 v2.4.0
)

require (
	github.com/asticode/go-astikit v0.20.0 // indirect
	github.com/asticode/go-astits v1.8.0 // indirect
	github.com/go-audio/riff v1.0.0 // indirect
	golang.org/x/net v0.0.0-20200904194848-62affa334b73 // indirect
	golang.org/x/text v0.3.2 // indirect
)

replace github.com/haguro/elevenlabs-go => ../elevenlabs-go
