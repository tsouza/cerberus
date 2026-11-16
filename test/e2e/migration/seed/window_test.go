package seed

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestNewWindowKeepsTheLastRecordOffReferenceLokiSplits walks a whole day of
// wall-clock instants. Reference Loki cuts a range query at every whole
// multiple of ReferenceLokiSplitInterval, and the sub-query starting at a
// cut does not return a flushed chunk's last entry stamped exactly there;
// AnchorEnd is the last entry of every fixture stream, so no instant may
// put it on a cut.
func TestNewWindowKeepsTheLastRecordOffReferenceLokiSplits(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for now := day; now.Before(day.Add(24 * time.Hour)); now = now.Add(SampleStep) {
		w := NewWindow(now)
		if w.AnchorEnd.Truncate(ReferenceLokiSplitInterval).Equal(w.AnchorEnd) {
			t.Fatalf("NewWindow(%s) anchors at %s, a reference Loki query split", now, w.AnchorEnd)
		}
		if w.AnchorEnd.UnixNano()%int64(ReferenceLokiSplitInterval) == 0 {
			t.Fatalf("NewWindow(%s) anchors at %s, a whole multiple of the split interval since the epoch",
				now, w.AnchorEnd)
		}
		if w.AnchorEnd.UnixNano()%int64(VerifyStep) != 0 {
			t.Fatalf("NewWindow(%s) anchors at %s, off the %s step grid", now, w.AnchorEnd, VerifyStep)
		}
		if w.AnchorEnd.After(now) || now.Sub(w.AnchorEnd) >= 2*VerifyStep {
			t.Fatalf("NewWindow(%s) anchors at %s, want the step-grid instant at or one step before now",
				now, w.AnchorEnd)
		}
		if lead := now.Sub(w.SeedStart); lead >= MaxSeedLead {
			t.Fatalf("NewWindow(%s) starts the seed %s before now, want under MaxSeedLead %s", now, lead, MaxSeedLead)
		}
		if !w.VerifyEnd.Equal(w.AnchorEnd) || !w.SeedStart.Equal(w.AnchorEnd.Add(-SeedWindow)) ||
			!w.VerifyStart.Equal(w.SeedStart.Add(RangeLookbackMargin)) {
			t.Fatalf("NewWindow(%s) = %+v: the window no longer hangs off its anchor", now, w)
		}
	}
}

// TestNewWindowMovesAnHourAnchorOneStepEarlier pins the instant that
// failed the parity lane: a seed run in the first minute of an hour.
func TestNewWindowMovesAnHourAnchorOneStepEarlier(t *testing.T) {
	t.Parallel()
	w := NewWindow(time.Date(2026, 11, 15, 12, 0, 59, 0, time.UTC))
	want := time.Date(2026, 11, 15, 11, 59, 0, 0, time.UTC)
	if !w.AnchorEnd.Equal(want) {
		t.Fatalf("AnchorEnd = %s, want %s", w.AnchorEnd, want)
	}
}

// TestLogQueryLimitHoldsAWholeStream pins that a whole-fixture log read
// cannot be truncated by its own entry limit.
func TestLogQueryLimitHoldsAWholeStream(t *testing.T) {
	t.Parallel()
	if n := len(NewWindow(time.Date(2026, 11, 15, 12, 30, 20, 0, time.UTC)).SampleTimes()); n >= LogQueryLimit {
		t.Fatalf("a fixture stream holds %d entries, at or above LogQueryLimit %d", n, LogQueryLimit)
	}
}

// referenceLokiConfig is the reference Loki config every lane tier mounts,
// relative to this package.
var referenceLokiConfig = filepath.Join("..", "..", "..", "..", "compatibility", "loki", "loki-config.yaml")

// TestReferenceLokiSplitIntervalMatchesTheConfig pins ReferenceLokiSplitInterval
// to the split interval reference Loki actually runs with, so neither a config
// edit nor an upstream default change can move the cuts back onto the anchor.
func TestReferenceLokiSplitIntervalMatchesTheConfig(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(referenceLokiConfig)
	if err != nil {
		t.Fatalf("read %s: %v", referenceLokiConfig, err)
	}
	var cfg struct {
		Limits struct {
			Split string `yaml:"split_queries_by_interval"`
		} `yaml:"limits_config"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", referenceLokiConfig, err)
	}
	if cfg.Limits.Split == "" {
		t.Fatalf("%s sets no limits_config.split_queries_by_interval; it must state the interval "+
			"ReferenceLokiSplitInterval assumes, %s", referenceLokiConfig, ReferenceLokiSplitInterval)
	}
	got, err := time.ParseDuration(cfg.Limits.Split)
	if err != nil {
		t.Fatalf("%s split_queries_by_interval %q: %v", referenceLokiConfig, cfg.Limits.Split, err)
	}
	if got != ReferenceLokiSplitInterval {
		t.Fatalf("%s splits queries every %s, but ReferenceLokiSplitInterval is %s",
			referenceLokiConfig, got, ReferenceLokiSplitInterval)
	}
}
