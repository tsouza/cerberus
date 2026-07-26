package seed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Manifest is the seeder's account of what it wrote and where. It is the
// contract between the seeder and everything downstream: the parity lane reads
// the window from here rather than defaulting to a live one, so both sides are
// always read over an interval that has stopped moving.
//
// `cerberus migrate verify`'s own defaults (--start -1h --end now) are NOT
// changed to suit the harness. A real operator legitimately wants a live
// window against their own Prometheus; the harness pins its own instead.
type Manifest struct {
	// The window geometry, in RFC 3339 with nanosecond precision.
	AnchorEnd   time.Time `json:"anchor_end"`
	SeedStart   time.Time `json:"seed_start"`
	VerifyStart time.Time `json:"verify_start"`
	VerifyEnd   time.Time `json:"verify_end"`
	// Step is the parity lane's query step, as a Go duration string.
	Step string `json:"step"`

	// Cardinalities, all derived from the fixture that was actually written
	// rather than restated by hand. A parity assertion that compares a side
	// against one of these is comparing against a declared value, not against
	// the other side's length.
	Streams          int         `json:"streams"`
	LogRecords       int         `json:"log_records"`
	Series           SeriesCount `json:"series"`
	PromSeries       int         `json:"prom_series"`
	Traces           int         `json:"traces"`
	Spans            int         `json:"spans"`
	SamplesPerSeries int         `json:"samples_per_series"`
	VerifySteps      int         `json:"verify_steps"`

	// The incumbent-only history: what reference Prometheus holds STRICTLY
	// before SeedStart, which is where ClickHouse's own ingest starts. These
	// are the oracle a split-read assertion checks the incumbent's
	// pre-boundary answer against — "the incumbent answered with the series
	// the seeder left it there", not merely "the incumbent answered".
	PreIngestStart            time.Time `json:"pre_ingest_start"`
	PreIngestSeries           int       `json:"pre_ingest_series"`
	PreIngestSamplesPerSeries int       `json:"pre_ingest_samples_per_series"`

	// The metric, stream and span handles the fixture occupies. A corpus query
	// names one of these explicitly; nothing in the fixture is selectable by a
	// bare aggregate, which is what keeps the ClickHouse-only warm-up rows
	// unreachable from a comparison.
	GaugeMetric     string `json:"gauge_metric"`
	CounterMetric   string `json:"counter_metric"`
	HistogramMetric string `json:"histogram_metric"`
	LogJob          string `json:"log_job"`
}

// SeriesCount is the per-table metric-series cardinality.
type SeriesCount struct {
	Gauge     int `json:"gauge"`
	Sum       int `json:"sum"`
	Histogram int `json:"histogram"`
}

// NewManifest derives the manifest from the fixture that was written. Every
// count is read off the fixture, so a manifest can never claim a cardinality
// the substrate does not hold.
func NewManifest(d Declaration, f Fixture) Manifest {
	return Manifest{
		AnchorEnd:        f.Window.AnchorEnd,
		SeedStart:        f.Window.SeedStart,
		VerifyStart:      f.Window.VerifyStart,
		VerifyEnd:        f.Window.VerifyEnd,
		Step:             f.Window.Step.String(),
		Streams:          len(f.LogStreams),
		LogRecords:       f.LogRecordCount(),
		Series:           SeriesCount{Gauge: len(f.Gauge), Sum: len(f.Counter), Histogram: len(f.Histogram)},
		PromSeries:       f.PromSeriesCount(),
		Traces:           len(f.Traces),
		Spans:            f.SpanCount(),
		SamplesPerSeries: len(f.Window.SampleTimes()),
		VerifySteps:      len(f.Window.VerifySteps()),

		PreIngestStart:            f.Window.PreIngestStart(),
		PreIngestSeries:           len(f.PromPreIngestSeries()),
		PreIngestSamplesPerSeries: len(f.Window.PreIngestSampleTimes()),

		GaugeMetric:     d.GaugeMetric,
		CounterMetric:   d.CounterMetric,
		HistogramMetric: d.HistogramMetric,
		LogJob:          d.LogJob,
	}
}

// manifestFileMode is the mode the manifest is written with: readable by the
// harness, not executable.
const manifestFileMode = 0o644

// WriteManifest serialises the manifest to path, creating the parent
// directory. The file is indented so a failing CI run can be read straight out
// of the artifact bundle.
func WriteManifest(path string, m Manifest) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // harness output directory
			return fmt.Errorf("create manifest directory %s: %w", dir, err)
		}
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, append(buf, '\n'), manifestFileMode); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	return nil
}

// ReadManifest loads a manifest written by the seeder.
func ReadManifest(path string) (Manifest, error) {
	buf, err := os.ReadFile(path) //nolint:gosec // harness-supplied manifest path
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(buf, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, nil
}

// Window reconstructs the fixture window the seeder used. Feeding it back into
// Build reproduces the fixture byte-for-byte — which is what lets a reader
// compare each side of the diff against the declared data rather than against
// the other side.
func (m Manifest) Window() (Window, error) {
	step, err := m.StepDuration()
	if err != nil {
		return Window{}, err
	}
	return Window{
		SeedStart:   m.SeedStart.UTC(),
		VerifyStart: m.VerifyStart.UTC(),
		VerifyEnd:   m.VerifyEnd.UTC(),
		AnchorEnd:   m.AnchorEnd.UTC(),
		Step:        step,
	}, nil
}

// StepDuration parses the manifest's step back into a duration.
func (m Manifest) StepDuration() (time.Duration, error) {
	d, err := time.ParseDuration(m.Step)
	if err != nil {
		return 0, fmt.Errorf("parse manifest step %q: %w", m.Step, err)
	}
	return d, nil
}
