package loki

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMinePatternsVolumeFloor(t *testing.T) {
	t.Parallel()

	const (
		belowFloorCount = defaultPatternsMinVolume - 1
		atFloorCount    = defaultPatternsMinVolume
	)
	base := time.Unix(0, 0).UTC()
	lines := repeatedPatternLines(base, "rare alpha quiet path", belowFloorCount)
	lines = append(lines, repeatedPatternLines(base, "common beta loud route", atFloorCount)...)

	got := minePatterns(lines, base, base.Add(time.Minute), minimumPatternSampleResolution, defaultPatternsMinVolume)
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
	lines := make([]chclient.TimestampedLine, 0, candidateSeries*defaultPatternsMinVolume+1)
	var highestVolumePattern string
	for i := 0; i < candidateSeries; i++ {
		body := distinctPatternBody(i)
		lines = append(lines, repeatedPatternLines(base, body, defaultPatternsMinVolume)...)
		if i == candidateSeries-1 {
			highestVolumePattern = body
			lines = append(lines, chclient.TimestampedLine{Timestamp: base, Body: body, Severity: "INFO"})
		}
	}

	got := minePatterns(lines, base, base.Add(time.Minute), minimumPatternSampleResolution, defaultPatternsMinVolume)
	if len(got) != maximumPatternSeries {
		t.Fatalf("patterns=%d want hard cap %d", len(got), maximumPatternSeries)
	}
	if got[0].Pattern != highestVolumePattern {
		t.Fatalf("first pattern=%q want highest-volume %q", got[0].Pattern, highestVolumePattern)
	}
	if volume := patternTestVolume(got[0]); volume != defaultPatternsMinVolume+1 {
		t.Fatalf("first volume=%d want %d", volume, defaultPatternsMinVolume+1)
	}
	for i := 1; i < len(got); i++ {
		if patternTestVolume(got[i-1]) < patternTestVolume(got[i]) {
			t.Fatalf("patterns not volume-descending at %d: %d < %d", i,
				patternTestVolume(got[i-1]), patternTestVolume(got[i]))
		}
	}
}

// TestNewDefaultsPatternsMinVolume confirms Handler.New sets
// PatternsMinVolume to the package default (30, cerberus issue #2081's
// upstream-matching floor) so a Handler built without going through
// cmd/cerberus's config wiring keeps the historical behaviour.
func TestNewDefaultsPatternsMinVolume(t *testing.T) {
	t.Parallel()

	h := New(nil, schema.DefaultOTelLogs(), nil)
	if h.PatternsMinVolume != defaultPatternsMinVolume {
		t.Fatalf("PatternsMinVolume = %d; want %d", h.PatternsMinVolume, defaultPatternsMinVolume)
	}
}

// TestMinePatternsConfigurableMinVolume confirms the volume floor
// (CERBERUS_LOKI_PATTERNS_MIN_VOLUME / Handler.PatternsMinVolume) is honored
// as a parameter rather than hardcoded: a lower threshold retains a cluster
// the default would drop, and 0 disables the floor entirely (every detected
// template returned, cerberus's pre-#2205 behaviour).
func TestMinePatternsConfigurableMinVolume(t *testing.T) {
	t.Parallel()

	const belowDefaultFloor = defaultPatternsMinVolume - 1
	base := time.Unix(0, 0).UTC()
	lines := repeatedPatternLines(base, "rare alpha quiet path", belowDefaultFloor)

	if got := minePatterns(lines, base, base.Add(time.Minute), minimumPatternSampleResolution, defaultPatternsMinVolume); len(got) != 0 {
		t.Fatalf("default floor: patterns=%d want 0 (below the %d-line default floor): %+v", len(got), defaultPatternsMinVolume, got)
	}
	if got := minePatterns(lines, base, base.Add(time.Minute), minimumPatternSampleResolution, belowDefaultFloor); len(got) != 1 {
		t.Fatalf("lowered floor=%d: patterns=%d want 1", belowDefaultFloor, len(got))
	}
	if got := minePatterns(lines, base, base.Add(time.Minute), minimumPatternSampleResolution, 0); len(got) != 1 {
		t.Fatalf("floor=0 (disabled): patterns=%d want 1", len(got))
	}
}

func TestMinePatternsUsesRequestedStep(t *testing.T) {
	t.Parallel()

	const requestedStep = 15 * time.Second
	base := time.Unix(0, 0).UTC()
	lines := make([]chclient.TimestampedLine, 0, defaultPatternsMinVolume)
	for i := 0; i < defaultPatternsMinVolume; i++ {
		lines = append(lines, chclient.TimestampedLine{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Body:      "common beta loud route",
			Severity:  "INFO",
		})
	}

	got := minePatterns(lines, base, base.Add(30*time.Second), requestedStep, defaultPatternsMinVolume)
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
