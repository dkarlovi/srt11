package commands

import (
	"time"

	"github.com/asticode/go-astisub"
)

// TTSProvider is the abstraction dkarlovi asked for: "the key part is to
// abstract the ElevenLabs integration so it's pluggable... you end up with
// something that supports either ElevenLabs or the audio.cpp API, and you
// can request one or the other through the API." ElevenLabs (elevenlabs.go)
// and audio.cpp (audiocpp.go) are the two implementations; nothing else in
// this package should import github.com/haguro/elevenlabs-go or know an
// audio.cpp request shape directly.
//
// One TTSProvider is used for an entire parseSubtitleFile/
// generateMissingVoiceLines call -- provider selection is per Generate/
// Export request (dkarlovi's requirement #2), not per speaker within a
// video, so every cue in one call goes through the same provider.
type TTSProvider interface {
	// Name identifies the provider for logging and error messages
	// ("elevenlabs" | "audiocpp").
	Name() string

	// CachePath returns where this cue's audio is, or would be, cached --
	// same role as generatePathTemplate before this refactor. Each
	// provider's cache key formula differs (see elevenlabs.go/audiocpp.go),
	// so this is provider-owned, not shared logic.
	CachePath(root string, sub *astisub.Item, model Model) Path

	// Synthesize generates audio for items[idx], which is not already
	// cached (Path.Path == ""), writes it to disk, and returns where it
	// wrote it and the audio's measured duration. items/idx give access to
	// neighbors for a provider that wants request-stitching context
	// (ElevenLabs); audio.cpp ignores them -- each cue is synthesized
	// independently.
	Synthesize(items []Item, idx int) (path string, duration time.Duration, err error)

	// ReadDuration measures an already-cached file at path, in this
	// provider's own audio format.
	ReadDuration(path string) (time.Duration, error)

	// DecodeSamples reads every sample from path's left channel (mono, or
	// the left channel of stereo) plus its native sample rate, for mixing
	// into the final track. generateFinalAudioFile resamples if this isn't
	// the mix's own rate.
	DecodeSamples(path string) (samples []int, sampleRate int, err error)
}
