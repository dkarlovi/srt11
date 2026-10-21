package commands

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"
)

func TestCanMergeCue(t *testing.T) {
	cases := []struct {
		name                                       string
		curSpeaker, nextSpeaker                    string
		mergedStart, mergedEnd, nextStart, nextEnd time.Duration
		mergeThresholdMs, mergeMaxMs               int
		want                                       bool
	}{
		{
			"same voice, within gap, within cap",
			"hana", "hana", 0, 3 * time.Second, 3 * time.Second, 6 * time.Second,
			120, 7000, true,
		},
		{
			"same voice, within gap, cap 0 means unlimited",
			"hana", "hana", 0, 3 * time.Second, 3 * time.Second, 10 * time.Second,
			120, 0, true,
		},
		{
			"same voice, within gap, but candidate window exceeds cap",
			"hana", "hana", 0, 6 * time.Second, 6 * time.Second, 9 * time.Second,
			120, 7000, false,
		},
		{
			"different voice never merges, even with room under the cap",
			"hana", "matko", 0, 3 * time.Second, 3 * time.Second, 4 * time.Second,
			120, 7000, false,
		},
		{
			"gap past threshold still blocks merging",
			"hana", "hana", 0, 3 * time.Second, 3*time.Second + 200*time.Millisecond, 6 * time.Second,
			120, 7000, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canMergeCue(tc.curSpeaker, tc.nextSpeaker, tc.mergedStart, tc.mergedEnd, tc.nextStart, tc.nextEnd, tc.mergeThresholdMs, tc.mergeMaxMs)
			if got != tc.want {
				t.Errorf("canMergeCue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func writeTempVTT(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.vtt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp VTT: %v", err)
	}
	return path
}

// threeAbuttingSameVoiceCues are three 3s Matko cues, back to back with no
// gap. Each line is explicitly tagged: an untagged line resolves to the
// default speaker (Hana in testConfig), not "whichever speaker tagged the
// previous line".
const threeAbuttingSameVoiceCues = `WEBVTT

00:00:00.000 --> 00:00:03.000
[Matko] one

00:00:03.000 --> 00:00:06.000
[Matko] two

00:00:06.000 --> 00:00:09.000
[Matko] three
`

func TestParseSubtitleFile_MergeMaxMs(t *testing.T) {
	cfg := testConfig()

	t.Run("cap 7000ms splits into [1+2],[3]", func(t *testing.T) {
		path := writeTempVTT(t, threeAbuttingSameVoiceCues)
		items := parseSubtitleFile(cfg, path, 120, 7000, "", newElevenLabsProvider(""))

		if len(items) != 2 {
			t.Fatalf("want 2 merged cues, got %d: %+v", len(items), items)
		}
		if len(items[0].MergedFrom) != 2 {
			t.Errorf("want first cue merged from 2 lines, got %d", len(items[0].MergedFrom))
		}
		if len(items[1].MergedFrom) != 1 {
			t.Errorf("want second cue merged from 1 line, got %d", len(items[1].MergedFrom))
		}
	})

	t.Run("cap 0 is unlimited: all three merge", func(t *testing.T) {
		path := writeTempVTT(t, threeAbuttingSameVoiceCues)
		items := parseSubtitleFile(cfg, path, 120, 0, "", newElevenLabsProvider(""))

		if len(items) != 1 {
			t.Fatalf("want 1 merged cue, got %d: %+v", len(items), items)
		}
		if len(items[0].MergedFrom) != 3 {
			t.Errorf("want cue merged from 3 lines, got %d", len(items[0].MergedFrom))
		}
	})

	t.Run("a different-voice cue in between always breaks the merge, cap or no cap", func(t *testing.T) {
		// Hana is the default speaker in testConfig, so her lines are
		// untagged; Matko's line in between carries the explicit tag.
		vtt := `WEBVTT

00:00:00.000 --> 00:00:03.000
one

00:00:03.000 --> 00:00:06.000
[Matko] two

00:00:06.000 --> 00:00:09.000
three
`
		path := writeTempVTT(t, vtt)
		items := parseSubtitleFile(cfg, path, 120, 0, "", newElevenLabsProvider(""))

		if len(items) != 3 {
			t.Fatalf("want 3 separate cues (Hana/Matko/Hana never merges across Matko), got %d: %+v", len(items), items)
		}
		for _, item := range items {
			if len(item.MergedFrom) != 1 {
				t.Errorf("want no merging across voices, got MergedFrom=%d for %q", len(item.MergedFrom), item.Sub.String())
			}
		}
	})
}

func writeFileWithModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestNewestFile(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.mp3")
	newer := filepath.Join(dir, "newer.mp3")
	now := time.Now()
	writeFileWithModTime(t, older, now.Add(-1*time.Hour))
	writeFileWithModTime(t, newer, now)

	t.Run("older listed first", func(t *testing.T) {
		got, err := newestFile([]string{older, newer})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != newer {
			t.Errorf("want %s, got %s", newer, got)
		}
	})

	t.Run("newer listed first -- order must not matter", func(t *testing.T) {
		got, err := newestFile([]string{newer, older})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != newer {
			t.Errorf("want %s, got %s", newer, got)
		}
	})

	t.Run("single file", func(t *testing.T) {
		got, err := newestFile([]string{newer})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != newer {
			t.Errorf("want %s, got %s", newer, got)
		}
	})

	t.Run("missing file errors instead of picking blindly", func(t *testing.T) {
		if _, err := newestFile([]string{filepath.Join(dir, "does-not-exist.mp3")}); err == nil {
			t.Fatal("want an error for a missing file, got nil")
		}
	})
}

func TestGeneratePathTemplate_PrefersNewestOnDuplicateCacheMatch(t *testing.T) {
	dir := t.TempDir()
	item := &astisub.Item{
		Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "hello there"}}}},
	}
	model := Model{model: "voice-id", name: "Matko", ttsModel: "eleven_multilingual_v2", speed: 1.0}

	// No cache files yet -- confirms the template and lets the test build
	// matching filenames without hand-computing the checksum.
	first := newElevenLabsProvider("").CachePath(dir, item, model)
	if first.Path != "" {
		t.Fatalf("want no cache hit yet, got %+v", first)
	}

	olderPath := fmt.Sprintf(first.Template, "older-id")
	newerPath := fmt.Sprintf(first.Template, "newer-id")
	now := time.Now()
	writeFileWithModTime(t, olderPath, now.Add(-1*time.Hour))
	writeFileWithModTime(t, newerPath, now)

	got := newElevenLabsProvider("").CachePath(dir, item, model)
	if got.Id != "newer-id" {
		t.Errorf("want the newest cache file's id %q, got %q (path %s)", "newer-id", got.Id, got.Path)
	}
}

func TestGeneratePathTemplate_SingleCacheMatchUnaffected(t *testing.T) {
	dir := t.TempDir()
	item := &astisub.Item{
		Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: "only one take"}}}},
	}
	model := Model{model: "voice-id", name: "Hana", ttsModel: "eleven_multilingual_v2", speed: 1.2}

	first := newElevenLabsProvider("").CachePath(dir, item, model)
	onlyPath := fmt.Sprintf(first.Template, "only-id")
	writeFileWithModTime(t, onlyPath, time.Now())

	got := newElevenLabsProvider("").CachePath(dir, item, model)
	if got.Id != "only-id" {
		t.Errorf("want %q, got %q", "only-id", got.Id)
	}
}

// squareWaveAtDb builds n samples of a square wave at a given dBFS level.
// A square wave's RMS equals its peak, which makes the expected level of
// the fixture exact rather than approximate (unlike e.g. a sine wave).
func squareWaveAtDb(db float64, n int) []int {
	amplitude := int(math.Round(32768 * math.Pow(10, db/20)))
	samples := make([]int, n)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = amplitude
		} else {
			samples[i] = -amplitude
		}
	}
	return samples
}

func peakDb(samples []int) float64 {
	peak := 0
	for _, s := range samples {
		abs := s
		if abs < 0 {
			abs = -abs
		}
		if abs > peak {
			peak = abs
		}
	}
	if peak == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(float64(peak)/32768.0)
}

func TestNormalizationGain(t *testing.T) {
	const target = -20.0
	const tolerance = 0.5 // dB

	t.Run("quiet clip is boosted toward target", func(t *testing.T) {
		samples := squareWaveAtDb(-35, 2000)
		gain := normalizationGain(samples, target)
		gainDb := 20 * math.Log10(gain)
		gotLevel := -35 + gainDb
		if math.Abs(gotLevel-target) > tolerance {
			t.Errorf("resulting level = %.2fdB, want ~%.2fdB", gotLevel, target)
		}
	})

	t.Run("loud clip is attenuated toward target", func(t *testing.T) {
		samples := squareWaveAtDb(-3, 2000)
		gain := normalizationGain(samples, target)
		if gain >= 1 {
			t.Fatalf("want attenuation (gain < 1), got %v", gain)
		}
		gainDb := 20 * math.Log10(gain)
		gotLevel := -3 + gainDb
		if math.Abs(gotLevel-target) > tolerance {
			t.Errorf("resulting level = %.2fdB, want ~%.2fdB", gotLevel, target)
		}
	})

	t.Run("near-silent broken clip is boost-capped, not fully corrected", func(t *testing.T) {
		// Mirrors the real near-silent cache files found in production
		// (measured around -85 to -89dB RMS with ffmpeg astats).
		samples := squareWaveAtDb(-85, 2000)
		gain := normalizationGain(samples, target)
		gainDb := 20 * math.Log10(gain)
		if math.Abs(gainDb-normalizeMaxBoostDb) > 0.01 {
			t.Fatalf("want gain capped at %.1fdB, got %.2fdB", normalizeMaxBoostDb, gainDb)
		}
		gotLevel := -85 + gainDb
		if gotLevel >= target-1 {
			t.Errorf("capped boost should leave the clip well short of target, got %.2fdB", gotLevel)
		}
	})

	t.Run("ceiling prevents clipping on a peaky clip despite a low overall RMS", func(t *testing.T) {
		// One near-full-scale sample among 999 silent ones: low RMS (which
		// alone would ask for a big boost) but a peak already close to 0dBFS.
		samples := make([]int, 1000)
		samples[0] = 32000
		gain := normalizationGain(samples, target)
		gotPeakDb := peakDb(samples) + 20*math.Log10(gain)
		if gotPeakDb > normalizeCeilingDb+0.01 {
			t.Fatalf("resulting peak %.2fdB exceeds ceiling %.2fdB", gotPeakDb, normalizeCeilingDb)
		}
	})

	t.Run("clip already at target gets ~unity gain", func(t *testing.T) {
		samples := squareWaveAtDb(target, 2000)
		gain := normalizationGain(samples, target)
		if math.Abs(gain-1) > 0.1 {
			t.Errorf("want gain close to 1, got %v", gain)
		}
	})

	t.Run("true silence returns unity gain", func(t *testing.T) {
		samples := make([]int, 1000)
		if gain := normalizationGain(samples, target); gain != 1 {
			t.Errorf("want gain 1 for silence, got %v", gain)
		}
	})

	t.Run("empty input returns unity gain", func(t *testing.T) {
		if gain := normalizationGain(nil, target); gain != 1 {
			t.Errorf("want gain 1 for empty input, got %v", gain)
		}
	})
}

func TestGeneratePathTemplate_TruncatesLongTextByRuneNotByte(t *testing.T) {
	dir := t.TempDir()
	// 48 ASCII bytes followed by a 3-byte em dash lands the byte offset 50
	// squarely inside the em dash's UTF-8 encoding -- a byte-slice there
	// produces an invalid UTF-8 filename that breaks any consumer decoding
	// this program's output as UTF-8 (e.g. Python's subprocess.run with
	// text=True).
	longText := strings.Repeat("a", 48) + "—" + "more text after the dash to push well past the limit"
	item := &astisub.Item{
		Lines: []astisub.Line{{Items: []astisub.LineItem{{Text: longText}}}},
	}
	model := Model{model: "voice-id", name: "Hana", ttsModel: "eleven_multilingual_v2", speed: 1.0}

	path := newElevenLabsProvider("").CachePath(dir, item, model)

	if !utf8.ValidString(path.Template) {
		t.Fatalf("template contains invalid UTF-8: %q", path.Template)
	}
}

func TestParseSubtitleFile_MergeStripsEachLinesOwnSpeakerTag(t *testing.T) {
	// Three abutting Matko-tagged lines that should merge into one cue.
	// resolveSpeaker only strips a tag anchored at the very start of a
	// string, so only the first line's own [Matko] sits where that reaches
	// it -- a naive concat leaves the other two embedded mid-string, where
	// they'd be spoken literally instead of stripped.
	vtt := `WEBVTT

00:00:00.000 --> 00:00:03.000
[Matko] one

00:00:03.000 --> 00:00:06.000
[Matko] two

00:00:06.000 --> 00:00:09.000
[Matko] three
`
	path := writeTempVTT(t, vtt)
	items := parseSubtitleFile(testConfig(), path, 120, 0, "", newElevenLabsProvider(""))

	if len(items) != 1 {
		t.Fatalf("want 1 merged cue, got %d: %+v", len(items), items)
	}
	text := items[0].Sub.String()
	if strings.Contains(text, "[Matko]") {
		t.Fatalf("merged text still contains a literal speaker tag: %q", text)
	}
	if text != "one two three" {
		t.Errorf("want %q, got %q", "one two three", text)
	}
}
