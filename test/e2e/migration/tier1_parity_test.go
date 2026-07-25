//go:build migration_tier1

package migration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// The parity assertions for the Tier-1 substrate: proof that the reference
// backends and cerberus hold the SAME logical telemetry over the SAME window.
//
// Nothing here compares one side's length against the other's. Every assertion
// runs against an ORACLE — the fixture rebuilt in-process from the archetype's
// data declaration and the seeder's manifest window — so each side is checked
// independently against the data that was declared. `len(a) == len(b)` would
// pass on two empty results; `a == oracle && b == oracle` cannot.
//
// Rebuilding the fixture is itself an assertion. The seeder built it from the
// same declaration, one RNG seeded with a fixed value, and hash-derived
// identifiers; if any of those determinism devices broke, the rebuilt oracle
// would not match what either backend returned.

const (
	fixtureDeclPath = "archetypes/three-signal/seed/fixture.json"
	expectedPath    = "archetypes/three-signal/expected/tier1.json"
	manifestEnvKey  = "TIER1_MANIFEST"
	defaultManifest = ".out/manifest.json"
	lokiEntryLimit  = "5000"
)

// expectedTier1 is the archetype's declared cardinality. It is the anti-hollow
// device for the oracle itself: the oracle is derived from the fixture, so a
// declaration change that silently shrank the fixture would move the oracle
// too — these pinned values do not move, and the run fails.
type expectedTier1 struct {
	LogStreams       int `json:"log_streams"`
	LogRecords       int `json:"log_records"`
	PromSeries       int `json:"prom_series"`
	Traces           int `json:"traces"`
	TracesPerService int `json:"traces_per_service"`
	Spans            int `json:"spans"`
	SamplesPerSeries int `json:"samples_per_series"`
	VerifySteps      int `json:"verify_steps"`
	Series           struct {
		Gauge     int `json:"gauge"`
		Sum       int `json:"sum"`
		Histogram int `json:"histogram"`
	} `json:"series"`
	MetricQueries []struct {
		Name   string `json:"name"`
		Query  string `json:"query"`
		Series int    `json:"series"`
	} `json:"metric_queries"`
}

// tier1Context is everything the parity assertions read: the declaration, the
// manifest the seeder published, the rebuilt fixture oracle and the pinned
// expectations.
type tier1Context struct {
	declaration seed.Declaration
	manifest    seed.Manifest
	window      seed.Window
	fixture     seed.Fixture
	expected    expectedTier1
	steps       []time.Time
}

func loadTier1Context(t *testing.T) tier1Context {
	t.Helper()

	declaration, err := seed.LoadDeclaration(fixtureDeclPath)
	if err != nil {
		t.Fatalf("load fixture declaration: %v", err)
	}
	manifestPath := envOr(manifestEnvKey, defaultManifest)
	manifest, err := seed.ReadManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest %s (run `just migration-tier1-seed` first): %v", manifestPath, err)
	}
	window, err := manifest.Window()
	if err != nil {
		t.Fatalf("manifest window: %v", err)
	}
	fixture, err := seed.Build(declaration, window)
	if err != nil {
		t.Fatalf("rebuild fixture oracle: %v", err)
	}

	var expected expectedTier1
	readJSONFile(t, expectedPath, &expected)

	return tier1Context{
		declaration: declaration,
		manifest:    manifest,
		window:      window,
		fixture:     fixture,
		expected:    expected,
		steps:       window.VerifySteps(),
	}
}

// TestTier1Parity asserts that both sides of the migration diff hold the same
// telemetry. Sub-tests share the seeded stack and run in order; the negative
// control runs last because it deliberately perturbs one side.
func TestTier1Parity(t *testing.T) {
	ctx := context.Background()
	tc := loadTier1Context(t)

	t.Run("substrate_is_pristine", func(t *testing.T) {
		assertNoLeftoverDrift(t, ctx)
	})

	t.Run("fixture_matches_the_declared_cardinality", func(t *testing.T) {
		assertTier1Cardinality(t, tc)
	})

	t.Run("metrics_agree_with_the_declared_fixture", func(t *testing.T) {
		for _, c := range metricParityCases(tc) {
			t.Run(c.name, func(t *testing.T) {
				oracle := c.oracle(tc)
				assertDeclaredQuery(t, tc, c, len(oracle))
				for _, side := range promSides() {
					got := queryRangeMatrix(t, ctx, side.baseURL, c.query, tc)
					assertMatrixEqualsOracle(t, side.name, c.query, got, oracle, tc.steps)
				}
			})
		}
	})

	t.Run("logs_agree_with_the_declared_fixture", func(t *testing.T) {
		for _, stream := range tc.fixture.LogStreams {
			name := stream.ServiceName + "_" + stream.Format
			t.Run(name, func(t *testing.T) {
				wantEntries, wantLabelSets := logOracle(stream, tc)
				if len(wantEntries) == 0 {
					t.Fatalf("stream %s has no records inside the verify window; the oracle would be vacuous", name)
				}
				for _, side := range logSides() {
					entries, labelSets := queryLogStream(t, ctx, side.baseURL, stream, tc)
					assertLogEntriesEqual(t, side.name, name, entries, wantEntries)
					assertLabelSetsEqual(t, side.name, name, labelSets, wantLabelSets)
				}
			})
		}
	})

	t.Run("traces_agree_with_the_declared_fixture", func(t *testing.T) {
		for _, service := range tc.declaration.Services {
			t.Run(service, func(t *testing.T) {
				want := tc.fixture.SortedTraceIDs(service)
				if len(want) != tc.expected.TracesPerService {
					t.Fatalf("fixture holds %d traces for %s, want the declared %d",
						len(want), service, tc.expected.TracesPerService)
				}
				// The two wire renderings compared below are both derived from
				// the canonical id, so the canonical form has to hold first.
				for _, id := range want {
					assertCanonicalTraceID(t, id)
				}
				assertTraceSearch(t, ctx, service, want, tc)
			})
		}
	})

	// Runs last: it perturbs the ClickHouse side on purpose.
	t.Run("negative_control_the_lane_can_see_disagreement", func(t *testing.T) {
		assertDriftIsObserved(t, ctx, tc)
	})
}

// assertNoLeftoverDrift requires the substrate to be free of a previous run's
// negative-control injection. The injection is real corruption by design — it
// is what proves the lane can see a disagreement — so a second parity run
// against the same seeded stack would compare against a perturbed side. Naming
// that as a precondition failure is the honest reading; silently tolerating
// the extra series would not be.
func assertNoLeftoverDrift(t *testing.T, ctx context.Context) {
	t.Helper()
	conn, err := seed.DialCH(ctx, tier1CHConfig())
	if err != nil {
		t.Fatalf("dial clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := seed.DriftRowCount(ctx, conn)
	if err != nil {
		t.Fatalf("count injected drift rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("clickhouse holds %d rows from a previous negative-control injection. The parity lane "+
			"runs once per seed: re-run `just migration-tier1-down && just migration-tier1-up && "+
			"just migration-tier1-seed` before asserting again.", rows)
	}
}

func tier1CHConfig() seed.CHConfig {
	return seed.CHConfig{
		Addr:     envOr(chAddrEnvKey, defaultCHAddr),
		Database: envOr(chDatabaseEnvKey, defaultCHDatabase),
		Username: envOr(chUsernameEnvKey, defaultCHUsername),
		Password: envOr(chPasswordEnvKey, defaultCHPassword),
	}
}

// assertTier1Cardinality pins the fixture the oracle is derived from against
// the archetype's declared expectations, and pins the seeder's manifest
// against the same numbers. A fixture that quietly shrank would otherwise
// shrink the oracle with it and every later comparison would still pass.
func assertTier1Cardinality(t *testing.T, tc tier1Context) {
	t.Helper()
	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"log streams (fixture)", len(tc.fixture.LogStreams), tc.expected.LogStreams},
		{"log streams (manifest)", tc.manifest.Streams, tc.expected.LogStreams},
		{"log records (fixture)", tc.fixture.LogRecordCount(), tc.expected.LogRecords},
		{"log records (manifest)", tc.manifest.LogRecords, tc.expected.LogRecords},
		{"gauge series (fixture)", len(tc.fixture.Gauge), tc.expected.Series.Gauge},
		{"gauge series (manifest)", tc.manifest.Series.Gauge, tc.expected.Series.Gauge},
		{"sum series (fixture)", len(tc.fixture.Counter), tc.expected.Series.Sum},
		{"sum series (manifest)", tc.manifest.Series.Sum, tc.expected.Series.Sum},
		{"histogram series (fixture)", len(tc.fixture.Histogram), tc.expected.Series.Histogram},
		{"histogram series (manifest)", tc.manifest.Series.Histogram, tc.expected.Series.Histogram},
		{"prometheus series (fixture)", tc.fixture.PromSeriesCount(), tc.expected.PromSeries},
		{"prometheus series (manifest)", tc.manifest.PromSeries, tc.expected.PromSeries},
		{"traces (fixture)", len(tc.fixture.Traces), tc.expected.Traces},
		{"traces (manifest)", tc.manifest.Traces, tc.expected.Traces},
		{"spans (fixture)", tc.fixture.SpanCount(), tc.expected.Spans},
		{"spans (manifest)", tc.manifest.Spans, tc.expected.Spans},
		{"samples per series (manifest)", tc.manifest.SamplesPerSeries, tc.expected.SamplesPerSeries},
		{"verify steps (manifest)", tc.manifest.VerifySteps, tc.expected.VerifySteps},
		{"verify steps (window)", len(tc.steps), tc.expected.VerifySteps},
	} {
		if c.got != c.want {
			t.Fatalf("%s = %d, want the declared %d", c.what, c.got, c.want)
		}
	}
}

// assertDeclaredQuery cross-checks a parity case against the archetype's pinned
// expectation for it: the PromQL that is about to be replayed, and the number
// of groups the oracle produced.
//
// Pinning the query text is what makes the archetype's `query` field load-
// bearing rather than a decorative copy. The corpus-shape rule that keeps the
// collector's ClickHouse-only warm-up rows unreachable from a comparison —
// every query names a fixture metric explicitly — is enforced against the JSON
// by test/regression/migration_tier1_test.go; unless the JSON is the query
// that actually runs, that gate guards a string nothing executes.
//
// The series count is the anti-hollow half: an oracle that collapsed its
// grouping cannot quietly agree with a backend that did the same.
func assertDeclaredQuery(t *testing.T, tc tier1Context, c metricParityCase, got int) {
	t.Helper()
	for _, q := range tc.expected.MetricQueries {
		if q.Name != c.name {
			continue
		}
		if q.Query != c.query {
			t.Fatalf("%s declares query %q as %q, but the lane replays %q",
				expectedPath, c.name, q.Query, c.query)
		}
		if got != q.Series {
			t.Fatalf("oracle for %q produced %d series, want the declared %d", c.name, got, q.Series)
		}
		return
	}
	t.Fatalf("%s declares no metric query named %q", expectedPath, c.name)
}

// --- metric parity -------------------------------------------------------

// metricParityCase is one query replayed against both sides, plus the oracle
// that says what each side must return.
type metricParityCase struct {
	name    string
	query   string
	groupBy []string
	// source selects the fixture series the query aggregates. Returning the
	// series rather than a precomputed result keeps the oracle derived from
	// the declaration.
	source func(seed.Fixture) []seed.MetricSeries
	// carriesName is set for a raw (unaggregated) query, whose result label
	// sets include __name__.
	carriesName bool
}

func metricParityCases(tc tier1Context) []metricParityCase {
	counter := func(f seed.Fixture) []seed.MetricSeries { return f.Counter }
	gauge := func(f seed.Fixture) []seed.MetricSeries { return f.Gauge }
	histogramCount := func(f seed.Fixture) []seed.MetricSeries {
		out := make([]seed.MetricSeries, 0, len(f.Histogram))
		for _, h := range f.Histogram {
			out = append(out, h.CountSeries())
		}
		return out
	}
	histogramSum := func(f seed.Fixture) []seed.MetricSeries {
		out := make([]seed.MetricSeries, 0, len(f.Histogram))
		for _, h := range f.Histogram {
			out = append(out, h.SumSeries())
		}
		return out
	}
	d := tc.declaration
	return []metricParityCase{
		{
			// Raw series: proves the two sides carry the same SERIES IDENTITY,
			// not merely the same totals. An aggregate can agree while the
			// underlying label sets differ.
			name:        "counter_raw_series",
			query:       d.CounterMetric,
			source:      counter,
			carriesName: true,
		},
		{
			// The full CROSS JOIN survives on both sides. A fixture that
			// derived status from service would produce 3 groups here, not 6.
			name:    "gauge_by_service_and_status",
			query:   fmt.Sprintf("sum by (service_name, http_status_code) (%s)", d.GaugeMetric),
			groupBy: []string{"service_name", "http_status_code"},
			source:  gauge,
		},
		{
			name:    "counter_by_service",
			query:   fmt.Sprintf("sum by (service_name) (%s)", d.CounterMetric),
			groupBy: []string{"service_name"},
			source:  counter,
		},
		{
			// The classic-histogram companion route: `<base>_count` and
			// `<base>_sum` are lowered onto otel_metrics_histogram under the
			// bare base name, so these two cases are the only ones that
			// exercise that table at all.
			name:    "histogram_count_by_status",
			query:   fmt.Sprintf("sum by (http_status_code) (%s_count)", d.HistogramMetric),
			groupBy: []string{"http_status_code"},
			source:  histogramCount,
		},
		{
			name:    "histogram_sum_by_status",
			query:   fmt.Sprintf("sum by (http_status_code) (%s_sum)", d.HistogramMetric),
			groupBy: []string{"http_status_code"},
			source:  histogramSum,
		},
	}
}

// seriesKey is a label set rendered as a stable string, so oracle and result
// groups can be matched without depending on map iteration order.
type seriesKey string

func makeSeriesKey(labels map[string]string) seriesKey {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return seriesKey(b.String())
}

// oracle computes, for every group the query produces, the exact value at
// every verify step.
//
// The arithmetic is a plain per-instant sum, and that is sound rather than a
// simplification: the seed window starts on a step boundary and every sample
// sits a whole multiple of the sample step from it, so each verify step
// coincides with a sample instant on every series. A missing sample at a step
// is therefore a fixture bug, and the oracle fails on it rather than
// interpolating.
func (c metricParityCase) oracle(tc tier1Context) map[seriesKey][]float64 {
	byKey := map[seriesKey]map[int64]float64{}
	labelsFor := map[seriesKey]map[string]string{}

	for _, s := range c.source(tc.fixture) {
		labels := map[string]string{}
		if c.groupBy == nil {
			for k, v := range s.Attributes {
				labels[k] = v
			}
			if c.carriesName {
				labels["__name__"] = s.MetricName
			}
		} else {
			for _, g := range c.groupBy {
				labels[g] = s.Attributes[g]
			}
		}
		key := makeSeriesKey(labels)
		labelsFor[key] = labels
		if byKey[key] == nil {
			byKey[key] = map[int64]float64{}
		}
		for _, sample := range s.Samples {
			byKey[key][sample.Time.UnixMilli()] += sample.Value
		}
	}

	out := make(map[seriesKey][]float64, len(byKey))
	for key, samples := range byKey {
		values := make([]float64, 0, len(tc.steps))
		for _, step := range tc.steps {
			values = append(values, samples[step.UnixMilli()])
		}
		out[key] = values
	}
	return out
}

// promSide is one queryable Prometheus-API endpoint.
type promSide struct {
	name    string
	baseURL string
}

func promSides() []promSide {
	return []promSide{
		{name: "reference-prometheus", baseURL: envOr(promURLEnvKey, defaultPromURL)},
		{name: "cerberus", baseURL: envOr(cerberusURLEnvKey, defaultCerberusURL)},
	}
}

func logSides() []promSide {
	return []promSide{
		{name: "reference-loki", baseURL: envOr(lokiURLEnvKey, defaultLokiURL)},
		{name: "cerberus", baseURL: envOr(cerberusURLEnvKey, defaultCerberusURL)},
	}
}

// promMatrix is a decoded range-query response keyed by label set.
type promMatrix map[seriesKey]promMatrixSeries

type promMatrixSeries struct {
	labels  map[string]string
	samples []promSample
}

type promSample struct {
	unixMilli int64
	value     float64
}

func queryRangeMatrix(t *testing.T, ctx context.Context, baseURL, query string, tc tier1Context) promMatrix {
	t.Helper()
	step, err := tc.manifest.StepDuration()
	if err != nil {
		t.Fatalf("manifest step: %v", err)
	}
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query_range?" + url.Values{
		"query": {query},
		"start": {strconv.FormatInt(tc.manifest.VerifyStart.Unix(), 10)},
		"end":   {strconv.FormatInt(tc.manifest.VerifyEnd.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	}.Encode()

	var got struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]json.RawMessage
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, target, &got); err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	if got.Status != "success" {
		t.Fatalf("%s returned status %q, want %q", target, got.Status, "success")
	}
	if got.Data.ResultType != "matrix" {
		t.Fatalf("%s returned resultType %q, want %q", target, got.Data.ResultType, "matrix")
	}

	out := promMatrix{}
	for _, r := range got.Data.Result {
		samples := make([]promSample, 0, len(r.Values))
		for _, v := range r.Values {
			if len(v) != 2 {
				t.Fatalf("%s: sample has %d fields, want 2", target, len(v))
			}
			var ts float64
			if err := json.Unmarshal(v[0], &ts); err != nil {
				t.Fatalf("%s: decode sample timestamp: %v", target, err)
			}
			var raw string
			if err := json.Unmarshal(v[1], &raw); err != nil {
				t.Fatalf("%s: decode sample value: %v", target, err)
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("%s: parse sample value %q: %v", target, raw, err)
			}
			samples = append(samples, promSample{unixMilli: int64(ts) * int64(time.Second/time.Millisecond), value: value})
		}
		key := makeSeriesKey(r.Metric)
		if _, clash := out[key]; clash {
			t.Fatalf("%s returned two series with the identical label set %v", target, r.Metric)
		}
		out[key] = promMatrixSeries{labels: r.Metric, samples: samples}
	}
	return out
}

// assertMatrixEqualsOracle requires one side's response to equal the oracle
// exactly: the same groups, the same step grid, the same values. No tolerance
// — every fixture value is a whole number or a negative power of two, so the
// arithmetic on both sides is exact in IEEE-754.
func assertMatrixEqualsOracle(t *testing.T, side, query string, got promMatrix, oracle map[seriesKey][]float64, steps []time.Time) {
	t.Helper()
	if len(got) != len(oracle) {
		t.Fatalf("%s: %q returned %d series, want the declared %d (got %v)",
			side, query, len(got), len(oracle), sortedKeys(got))
	}
	for key, want := range oracle {
		series, ok := got[key]
		if !ok {
			t.Fatalf("%s: %q is missing the declared series %q (returned %v)", side, query, key, sortedKeys(got))
		}
		if len(series.samples) != len(steps) {
			t.Fatalf("%s: %q series %q returned %d samples, want one per verify step (%d)",
				side, query, key, len(series.samples), len(steps))
		}
		for i, step := range steps {
			if series.samples[i].unixMilli != step.UnixMilli() {
				t.Fatalf("%s: %q series %q sample %d at %d, want the step instant %d",
					side, query, key, i, series.samples[i].unixMilli, step.UnixMilli())
			}
			if series.samples[i].value != want[i] {
				t.Fatalf("%s: %q series %q sample %d = %v, want the declared %v",
					side, query, key, i, series.samples[i].value, want[i])
			}
		}
	}
}

func sortedKeys(m promMatrix) []seriesKey {
	out := make([]seriesKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- log parity ----------------------------------------------------------

// logEntry is one returned log line, reduced to the two fields both backends
// must agree on byte-for-byte.
type logEntry struct {
	unixNano int64
	line     string
}

// logOracle returns the entries a stream must yield over the verify window,
// and the label sets those entries must be grouped under.
//
// Both backends surface a record's structured metadata alongside its stream
// labels, so a stream whose records rotate through three levels comes back as
// three label sets. That is a property of the data, not of either backend, so
// the oracle states it rather than normalising it away.
func logOracle(stream seed.LogStream, tc tier1Context) ([]logEntry, map[seriesKey]map[string]string) {
	var entries []logEntry
	labelSets := map[seriesKey]map[string]string{}
	for _, r := range stream.Records {
		if r.Time.Before(tc.manifest.VerifyStart) || r.Time.After(tc.manifest.VerifyEnd) {
			continue
		}
		entries = append(entries, logEntry{unixNano: r.Time.UnixNano(), line: r.Line})
		labels := map[string]string{"detected_level": r.Level}
		for k, v := range stream.Labels {
			labels[k] = v
		}
		labelSets[makeSeriesKey(labels)] = labels
	}
	sortLogEntries(entries)
	return entries, labelSets
}

func sortLogEntries(entries []logEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].unixNano != entries[j].unixNano {
			return entries[i].unixNano < entries[j].unixNano
		}
		return entries[i].line < entries[j].line
	})
}

// queryLogStream reads one fixture stream back from a side, returning its
// entries and the label sets they arrived under.
func queryLogStream(t *testing.T, ctx context.Context, baseURL string, stream seed.LogStream, tc tier1Context) ([]logEntry, map[seriesKey]map[string]string) {
	t.Helper()

	selector := logSelector(stream)
	// The end bound is exclusive on the Loki query API, so it is pushed one
	// nanosecond past the last fixture record; without that the closing
	// record of the window would be missing from both sides and the oracle
	// would have to be weakened to match.
	target := strings.TrimRight(baseURL, "/") + "/loki/api/v1/query_range?" + url.Values{
		"query":     {selector},
		"start":     {strconv.FormatInt(tc.manifest.VerifyStart.UnixNano(), 10)},
		"end":       {strconv.FormatInt(tc.manifest.VerifyEnd.UnixNano()+1, 10)},
		"limit":     {lokiEntryLimit},
		"direction": {"forward"},
	}.Encode()

	var got struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := fetchJSON(ctx, target, &got); err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	if got.Status != "success" {
		t.Fatalf("%s returned status %q, want %q", target, got.Status, "success")
	}

	var entries []logEntry
	labelSets := map[seriesKey]map[string]string{}
	for _, r := range got.Data.Result {
		labelSets[makeSeriesKey(r.Stream)] = r.Stream
		for _, v := range r.Values {
			if len(v) < 2 {
				t.Fatalf("%s: entry has %d fields, want at least 2", target, len(v))
			}
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				t.Fatalf("%s: parse entry timestamp %q: %v", target, v[0], err)
			}
			entries = append(entries, logEntry{unixNano: ns, line: v[1]})
		}
	}
	sortLogEntries(entries)
	return entries, labelSets
}

func logSelector(stream seed.LogStream) string {
	keys := make([]string, 0, len(stream.Labels))
	for k := range stream.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, stream.Labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func assertLogEntriesEqual(t *testing.T, side, stream string, got, want []logEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: stream %s returned %d entries, want the declared %d", side, stream, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: stream %s entry %d = (%d, %q), want the declared (%d, %q)",
				side, stream, i, got[i].unixNano, got[i].line, want[i].unixNano, want[i].line)
		}
	}
}

func assertLabelSetsEqual(t *testing.T, side, stream string, got, want map[seriesKey]map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: stream %s returned %d label sets, want the declared %d (got %v, want %v)",
			side, stream, len(got), len(want), labelSetKeys(got), labelSetKeys(want))
	}
	for key, wantLabels := range want {
		gotLabels, ok := got[key]
		if !ok {
			t.Fatalf("%s: stream %s is missing the declared label set %v (returned %v)",
				side, stream, wantLabels, labelSetKeys(got))
		}
		if len(gotLabels) != len(wantLabels) {
			t.Fatalf("%s: stream %s returned label set %v, want exactly %v", side, stream, gotLabels, wantLabels)
		}
	}
}

func labelSetKeys(m map[seriesKey]map[string]string) []seriesKey {
	out := make([]seriesKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- trace parity --------------------------------------------------------

// assertTraceSearch requires each side to return exactly the fixture's traces
// for a service, compared as 16-byte trace IDENTITIES.
//
// The two sides render that identity differently, and the difference is
// asserted rather than normalised away. Reference Tempo strips leading zeros
// from trace-id hex on the wire; cerberus emits the canonical fixed-width
// 32-char form the OTel and Tempo specs require, deliberately declining to
// propagate upstream's wire-format defect (see internal/api/tempo/handler.go,
// stripLeadingHexZeros). Both renderings are checked here, so a change to
// either backend's rendering fails this test instead of passing under a
// normalising comparison.
func assertTraceSearch(t *testing.T, ctx context.Context, service string, wantIDs []string, tc tier1Context) {
	t.Helper()

	query := seed.TraceServiceQuery(service)
	searchStart, searchEnd := tc.window.TraceSearchRange()
	for _, side := range []struct {
		name    string
		baseURL string
		render  func(string) string
	}{
		{
			name:    "reference-tempo",
			baseURL: envOr(tempoURLEnvKey, defaultTempoURL),
			render:  seed.RenderTempoTraceID,
		},
		{
			name:    "cerberus",
			baseURL: envOr(cerberusURLEnvKey, defaultCerberusURL),
			render:  func(id string) string { return id },
		},
	} {
		target := strings.TrimRight(side.baseURL, "/") + "/api/search?" + url.Values{
			"q":     {query},
			"start": {strconv.FormatInt(searchStart.Unix(), 10)},
			"end":   {strconv.FormatInt(searchEnd.Unix(), 10)},
			"limit": {strconv.Itoa(seed.TraceSearchLimit)},
		}.Encode()

		var got struct {
			Traces []struct {
				TraceID string `json:"traceID"`
			} `json:"traces"`
		}
		if err := fetchJSON(ctx, target, &got); err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}

		gotIDs := make([]string, 0, len(got.Traces))
		for _, tr := range got.Traces {
			gotIDs = append(gotIDs, tr.TraceID)
		}
		sort.Strings(gotIDs)

		want := make([]string, 0, len(wantIDs))
		for _, id := range wantIDs {
			want = append(want, side.render(id))
		}
		sort.Strings(want)

		if len(gotIDs) != len(want) {
			t.Fatalf("%s: search for %s returned %d traces, want the declared %d",
				side.name, service, len(gotIDs), len(want))
		}
		for i := range want {
			if gotIDs[i] != want[i] {
				t.Fatalf("%s: search for %s returned trace id %q at position %d, want the declared %q",
					side.name, service, gotIDs[i], i, want[i])
			}
		}
	}
}

// --- negative control ----------------------------------------------------

// assertDriftIsObserved proves the lane can see a disagreement.
//
// Every assertion above shows the two sides AGREE; none of them shows the
// comparison would notice if they did not. Without this, a harness that had
// silently stopped comparing anything would still read green. The injected
// margin (DriftFactor) is far outside any float tolerance, so a comparison
// that misses it is not comparing values.
func assertDriftIsObserved(t *testing.T, ctx context.Context, tc tier1Context) {
	t.Helper()

	service := tc.declaration.Services[0]
	query := fmt.Sprintf("sum by (service_name) (%s)", tc.declaration.GaugeMetric)

	conn, err := seed.DialCH(ctx, tier1CHConfig())
	if err != nil {
		t.Fatalf("dial clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The pre-injection reading of the perturbed group, from the oracle, so
	// the post-injection expectation is a declared value rather than whatever
	// the backend happened to return a moment ago.
	base := gaugeOracleForService(tc, service)
	injected, err := seed.InjectGaugeDrift(ctx, conn, tc.fixture, service)
	if err != nil {
		t.Fatalf("inject reference drift: %v", err)
	}

	reference := queryRangeMatrix(t, ctx, envOr(promURLEnvKey, defaultPromURL), query, tc)
	perturbed := queryRangeMatrix(t, ctx, envOr(cerberusURLEnvKey, defaultCerberusURL), query, tc)

	key := makeSeriesKey(map[string]string{"service_name": service})
	referenceSeries, ok := reference[key]
	if !ok {
		t.Fatalf("reference prometheus lost the %s series after the injection", service)
	}
	perturbedSeries, ok := perturbed[key]
	if !ok {
		t.Fatalf("cerberus lost the %s series after the injection", service)
	}
	assertStepGridComplete(t, "reference prometheus", service, referenceSeries, tc.steps)
	assertStepGridComplete(t, "cerberus", service, perturbedSeries, tc.steps)

	var observed int
	for i, step := range tc.steps {
		want := base[step.UnixMilli()] + injected[step.UnixMilli()]
		if perturbedSeries.samples[i].value != want {
			t.Fatalf("cerberus sample %d = %v after the injection, want the declared %v",
				i, perturbedSeries.samples[i].value, want)
		}
		if referenceSeries.samples[i].value != base[step.UnixMilli()] {
			t.Fatalf("reference sample %d = %v, want the un-perturbed %v",
				i, referenceSeries.samples[i].value, base[step.UnixMilli()])
		}
		if perturbedSeries.samples[i].value != referenceSeries.samples[i].value {
			observed++
		}
	}
	if observed != len(tc.steps) {
		t.Fatalf("the injected drift was visible at %d of %d steps; a comparison that cannot see a "+
			"%vx perturbation is not comparing values", observed, len(tc.steps), seed.DriftFactor)
	}

	// The other services must be untouched: an injection that moved every
	// group would prove nothing about the comparison's resolution.
	for _, other := range tc.declaration.Services[1:] {
		otherKey := makeSeriesKey(map[string]string{"service_name": other})
		refOther, refOK := reference[otherKey]
		cerbOther, cerbOK := perturbed[otherKey]
		if !refOK || !cerbOK {
			t.Fatalf("service %s disappeared from one side after the injection", other)
		}
		assertStepGridComplete(t, "reference prometheus", other, refOther, tc.steps)
		assertStepGridComplete(t, "cerberus", other, cerbOther, tc.steps)
		for i := range tc.steps {
			if refOther.samples[i].value != cerbOther.samples[i].value {
				t.Fatalf("service %s diverged at step %d (%v vs %v); the injection was supposed to touch "+
					"only %s", other, i, refOther.samples[i].value, cerbOther.samples[i].value, service)
			}
		}
	}
}

// assertStepGridComplete requires a series to carry one sample per verify step
// before the negative control indexes it. A short response is precisely the
// regression this control exists to surface, so it has to be NAMED — an
// unguarded index would kill the test binary with `index out of range` from a
// helper instead, losing which side returned how many samples.
func assertStepGridComplete(t *testing.T, side, service string, series promMatrixSeries, steps []time.Time) {
	t.Helper()
	if len(series.samples) != len(steps) {
		t.Fatalf("%s: service %s returned %d samples after the injection, want one per verify step (%d)",
			side, service, len(series.samples), len(steps))
	}
}

// gaugeOracleForService is the declared `sum by (service_name)` value of the
// gauge for one service at every sample instant.
func gaugeOracleForService(tc tier1Context, service string) map[int64]float64 {
	out := map[int64]float64{}
	for _, s := range tc.fixture.Gauge {
		if s.Attributes["service_name"] != service {
			continue
		}
		for _, sample := range s.Samples {
			out[sample.Time.UnixMilli()] += sample.Value
		}
	}
	return out
}

// readJSONFile decodes a JSON file, rejecting unknown fields so a renamed
// expectation surfaces as a parse failure rather than a zero value that every
// comparison would then trivially satisfy.
func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// hexDecodedLength is the byte length of a canonical trace id; the constant
// exists so the check below reads as an identity-width assertion rather than a
// bare 16.
const hexDecodedLength = 16

// assertCanonicalTraceID is used by the trace oracle to guarantee the fixture
// itself only ever produces canonical, fixed-width identifiers — the property
// the two renderings above are derived from.
func assertCanonicalTraceID(t *testing.T, id string) {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil {
		t.Fatalf("fixture trace id %q is not hex: %v", id, err)
	}
	if len(raw) != hexDecodedLength {
		t.Fatalf("fixture trace id %q decodes to %d bytes, want %d", id, len(raw), hexDecodedLength)
	}
}
