package loki

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

func TestMinePatternsVolumeFloor(t *testing.T) {
	t.Parallel()

	const (
		belowFloorCount = minimumPatternVolume - 1
		atFloorCount    = minimumPatternVolume
	)
	base := time.Unix(0, 0).UTC()
	lines := repeatedPatternLines(base, "rare alpha quiet path", belowFloorCount)
	lines = append(lines, repeatedPatternLines(base, "common beta loud route", atFloorCount)...)

	got := minePatterns(lines, minimumPatternSampleResolution)
	if len(got) != 1 {
		t.Fatalf("patterns=%d want 1: %+v", len(got), got)
	}
	if got[0].Pattern != "common beta loud route" {
		t.Fatalf("retained pattern=%q want the cluster at the volume floor", got[0].Pattern)
	}
	if volume := patternTestVolume(got[0]); volume != atFloorCount {
		t.Fatalf("retained volume=%d want %d", volume, atFloorCount)
	}
}

func TestMinePatternsVolumeSortAndSeriesCap(t *testing.T) {
	t.Parallel()

	const candidateSeries = maximumPatternSeries + 1
	base := time.Unix(0, 0).UTC()
	lines := make([]chclient.TimestampedLine, 0, candidateSeries*minimumPatternVolume+1)
	var highestVolumePattern string
	for i := 0; i < candidateSeries; i++ {
		body := distinctPatternBody(i)
		lines = append(lines, repeatedPatternLines(base, body, minimumPatternVolume)...)
		if i == candidateSeries-1 {
			highestVolumePattern = body
			lines = append(lines, chclient.TimestampedLine{Timestamp: base, Body: body, Severity: "INFO"})
		}
	}

	got := minePatterns(lines, minimumPatternSampleResolution)
	if len(got) != maximumPatternSeries {
		t.Fatalf("patterns=%d want hard cap %d", len(got), maximumPatternSeries)
	}
	if got[0].Pattern != highestVolumePattern {
		t.Fatalf("first pattern=%q want highest-volume %q", got[0].Pattern, highestVolumePattern)
	}
	if volume := patternTestVolume(got[0]); volume != minimumPatternVolume+1 {
		t.Fatalf("first volume=%d want %d", volume, minimumPatternVolume+1)
	}
	for i := 1; i < len(got); i++ {
		if patternTestVolume(got[i-1]) < patternTestVolume(got[i]) {
			t.Fatalf("patterns not volume-descending at %d: %d < %d", i,
				patternTestVolume(got[i-1]), patternTestVolume(got[i]))
		}
	}
}

func TestMinePatternsUsesRequestedStep(t *testing.T) {
	t.Parallel()

	const requestedStep = 15 * time.Second
	base := time.Unix(0, 0).UTC()
	lines := make([]chclient.TimestampedLine, 0, minimumPatternVolume)
	for i := 0; i < minimumPatternVolume; i++ {
		lines = append(lines, chclient.TimestampedLine{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Body:      "common beta loud route",
			Severity:  "INFO",
		})
	}

	got := minePatterns(lines, requestedStep)
	if len(got) != 1 {
		t.Fatalf("patterns=%d want 1: %+v", len(got), got)
	}
	want := [][2]int64{{0, 15}, {15, 15}}
	if len(got[0].Samples) != len(want) {
		t.Fatalf("samples=%v want %v", got[0].Samples, want)
	}
	for i := range want {
		if got[0].Samples[i] != want[i] {
			t.Fatalf("sample[%d]=%v want %v", i, got[0].Samples[i], want[i])
		}
	}
}

func TestParsePatternsStep(t *testing.T) {
	t.Parallel()

	start := time.Unix(0, 0).UTC()
	end := start.Add(time.Hour)
	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "absent defaults to drain resolution", want: minimumPatternSampleResolution},
		{name: "sub-floor step uses execution floor", raw: "5s", want: minimumPatternSampleResolution},
		{name: "plain seconds", raw: "15", want: 15 * time.Second},
		{name: "duration", raw: "1m", want: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePatternsStep(tc.raw, start, end)
			if err != nil {
				t.Fatalf("parsePatternsStep(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("step=%s want %s", got, tc.want)
			}
		})
	}

	for _, raw := range []string{"0", "-1", "invalid"} {
		if _, err := parsePatternsStep(raw, start, end); err == nil {
			t.Errorf("parsePatternsStep(%q) succeeded; want error", raw)
		}
	}
	overCapEnd := start.Add((format.MaxResolutionPoints + 1) * time.Second)
	if _, err := parsePatternsStep("1s", start, overCapEnd); err == nil {
		t.Error("step exceeding the resolution cap succeeded; want error")
	}
}

func repeatedPatternLines(base time.Time, body string, count int) []chclient.TimestampedLine {
	lines := make([]chclient.TimestampedLine, count)
	for i := range lines {
		lines[i] = chclient.TimestampedLine{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Body:      body,
			Severity:  "INFO",
		}
	}
	return lines
}

func distinctPatternBody(n int) string {
	id := alphabeticPatternID(n)
	return strings.Join([]string{"alpha" + id, "beta" + id, "gamma" + id, "delta" + id}, " ")
}

func alphabeticPatternID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	var reversed [3]byte
	pos := len(reversed)
	for {
		pos--
		reversed[pos] = alphabet[n%len(alphabet)]
		n = n/len(alphabet) - 1
		if n < 0 {
			return string(reversed[pos:])
		}
	}
}

func patternTestVolume(pattern Pattern) int64 {
	var total int64
	for _, sample := range pattern.Samples {
		total += sample[1]
	}
	return total
}
