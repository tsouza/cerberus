//go:build integration

package nightly

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	promengine "github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/annotations"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

type histogramReportOracle map[string]map[int64]float64

type histogramReportSeriesSet struct {
	series []storage.Series
	index  int
}

func (s *histogramReportSeriesSet) Next() bool                      { s.index++; return s.index <= len(s.series) }
func (s *histogramReportSeriesSet) At() storage.Series              { return s.series[s.index-1] }
func (*histogramReportSeriesSet) Err() error                        { return nil }
func (*histogramReportSeriesSet) Warnings() annotations.Annotations { return nil }

// loadHistogramReportOracle evaluates the recorded cumulative bucket counters
// with the upstream Prometheus engine, independently of Cerberus's lowerer and
// ClickHouse's time-series functions. Quantiles use upstream BucketQuantile on
// the resulting rates. Original source identity is retained until after rate.
func loadHistogramReportOracle(ctx context.Context, t *testing.T, client *chclient.Client, expression string, start, end time.Time, step time.Duration) map[string]histogramReportOracle {
	t.Helper()
	const lookback = 5 * time.Minute
	rows, err := client.Conn().Query(ctx, `SELECT toUnixTimestamp64Milli(TimeUnix), Attributes, ResourceAttributes, BucketCounts, ExplicitBounds FROM otel_metrics_histogram WHERE MetricName = ? AND TimeUnix > ? AND TimeUnix <= ? ORDER BY TimeUnix`, histogramReportMetric, start.Add(-lookback), end)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type source struct {
		timestamps []int64
		values     [][]float64
		bounds     []float64
		deployment string
	}
	sources := make(map[string]*source)
	for rows.Next() {
		var timestamp int64
		var attributes, resources map[string]string
		var counts []uint64
		var bounds []float64
		if err := rows.Scan(&timestamp, &attributes, &resources, &counts, &bounds); err != nil {
			t.Fatal(err)
		}
		canonical := make(map[string]string, len(attributes)+len(resources))
		for key, value := range resources {
			if key != "service.name" && key != "service_name" {
				canonical[format.OTelToPromLabel(key)] = value
			}
		}
		for key, value := range attributes {
			canonical[format.OTelToPromLabel(key)] = value
		}
		identity := labels.FromMap(canonical).String()
		s := sources[identity]
		if s == nil {
			s = &source{values: make([][]float64, len(counts)), bounds: bounds, deployment: canonical["k8s_deployment_name"]}
			sources[identity] = s
		}
		if len(counts) != len(s.values) || len(bounds)+1 != len(counts) {
			t.Fatalf("capture changed bucket layout for %s", identity)
		}
		for i, bound := range bounds {
			if bound != s.bounds[i] {
				t.Fatalf("capture changed bound %d for %s", i, identity)
			}
		}
		duplicate := len(s.timestamps) > 0 && s.timestamps[len(s.timestamps)-1] == timestamp
		if !duplicate {
			s.timestamps = append(s.timestamps, timestamp)
		}
		var cumulative uint64
		for i, count := range counts {
			cumulative += count
			value := float64(cumulative)
			if duplicate {
				// Cerberus's sample contract resolves duplicate timestamps to
				// the greatest counter value before rate evaluates the window.
				last := len(s.values[i]) - 1
				s.values[i][last] = math.Max(s.values[i][last], value)
			} else {
				s.values[i] = append(s.values[i], value)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var series []storage.Series
	for identity, s := range sources {
		for i, values := range s.values {
			bound := math.Inf(1)
			if i < len(s.bounds) {
				bound = s.bounds[i]
			}
			chunk := chunkenc.NewXORChunk()
			appender, err := chunk.Appender()
			if err != nil {
				t.Fatal(err)
			}
			for j, value := range values {
				appender.Append(0, s.timestamps[j], value)
			}
			series = append(series, &storage.SeriesEntry{Lset: labels.FromStrings(
				"__name__", histogramReportMetric+"_bucket", "fixture_source", identity,
				"k8s_deployment_name", s.deployment, "deployment_environment_name", histogramReportEnvironment,
				"le", strconv.FormatFloat(bound, 'g', -1, 64),
			), SampleIteratorFn: chunk.Iterator})
		}
	}
	sort.Slice(series, func(i, j int) bool { return labels.Compare(series[i].Labels(), series[j].Labels()) < 0 })
	queryable := &storage.MockQueryable{MockQuerier: &storage.MockQuerier{
		SelectMockFunction: func(_ bool, _ *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
			var selected []storage.Series
			for _, s := range series {
				matches := true
				for _, matcher := range matchers {
					if !matcher.Matches(s.Labels().Get(matcher.Name)) {
						matches = false
						break
					}
				}
				if matches {
					selected = append(selected, s)
				}
			}
			return &histogramReportSeriesSet{series: selected}
		},
	}}
	const oracleMaxSamples = 20_000_000
	const oracleTimeout = 2 * time.Minute
	engine := promengine.NewEngine(promengine.EngineOpts{MaxSamples: oracleMaxSamples, Timeout: oracleTimeout, LookbackDelta: lookback})
	query, err := engine.NewRangeQuery(ctx, queryable, promengine.NewPrometheusQueryOpts(false, lookback), expression, start, end, step)
	if err != nil {
		t.Fatal(err)
	}
	defer query.Close()
	result := query.Exec(ctx)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	matrix, ok := result.Value.(promengine.Matrix)
	if !ok || len(matrix) == 0 {
		t.Fatalf("upstream returned empty or non-matrix result: %v", result)
	}
	oracles := map[string]histogramReportOracle{"buckets": {}, "shared": {}}
	byDeployment := make(map[string]map[int64]promengine.Buckets)
	for _, s := range matrix {
		key := s.Metric.String()
		oracles["buckets"][key] = make(map[int64]float64, len(s.Floats))
		deployment := s.Metric.Get("k8s_deployment_name")
		if byDeployment[deployment] == nil {
			byDeployment[deployment] = make(map[int64]promengine.Buckets)
		}
		bound, err := strconv.ParseFloat(s.Metric.Get("le"), 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, point := range s.Floats {
			oracles["buckets"][key][point.T] = point.F
			byDeployment[deployment][point.T] = append(byDeployment[deployment][point.T], promengine.Bucket{UpperBound: bound, Count: point.F})
		}
	}
	for name, phi := range map[string]float64{"p50": 0.5, "p95": 0.95, "p99": 0.99} {
		oracles[name] = make(histogramReportOracle)
		for deployment, grid := range byDeployment {
			key := labels.FromStrings("k8s_deployment_name", deployment).String()
			oracles[name][key] = make(map[int64]float64, len(grid))
			sharedKey := labels.FromStrings(
				"k8s_deployment_name", deployment,
				"quantile", labels.FormatOpenMetricsFloat(phi),
			).String()
			oracles["shared"][sharedKey] = make(map[int64]float64, len(grid))
			for timestamp, buckets := range grid {
				value, _, _, _, _, _ := promengine.BucketQuantile(phi, append(promengine.Buckets(nil), buckets...))
				oracles[name][key][timestamp] = value
				oracles["shared"][sharedKey][timestamp] = value
			}
		}
	}
	t.Logf("upstream Prometheus oracle: %d source histograms, %d bucket series, %d reduced bucket series", len(sources), len(series), len(matrix))
	return oracles
}

func assertHistogramReportOracle(t *testing.T, body []byte, expected histogramReportOracle, instant bool, end time.Time) {
	t.Helper()
	var response struct {
		Data struct {
			Result []struct {
				Metric map[string]string   `json:"metric"`
				Value  []json.RawMessage   `json:"value"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, result := range response.Data.Result {
		key := labels.FromMap(result.Metric).String()
		grid, ok := expected[key]
		if !ok || seen[key] {
			t.Fatalf("unexpected or duplicate series: %s", key)
		}
		seen[key] = true
		samples := result.Values
		wantSamples := len(grid)
		if instant {
			samples = [][]json.RawMessage{result.Value}
			wantSamples = 1
			if _, ok := grid[end.UnixMilli()]; !ok {
				t.Fatalf("unexpected instant series %s", key)
			}
		}
		if len(samples) != wantSamples {
			t.Fatalf("%s: %d samples, upstream has %d", key, len(samples), wantSamples)
		}
		seenTimes := make(map[int64]bool)
		for _, sample := range samples {
			var timestamp float64
			var encoded string
			if len(sample) != 2 {
				t.Fatalf("invalid sample: %s", sample)
			}
			if err := json.Unmarshal(sample[0], &timestamp); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(sample[1], &encoded); err != nil {
				t.Fatal(err)
			}
			millis := int64(math.Round(timestamp * float64(time.Second/time.Millisecond)))
			want, ok := grid[millis]
			if !ok || seenTimes[millis] || (instant && millis != end.UnixMilli()) {
				t.Fatalf("unexpected or duplicate timestamp %v for %s", timestamp, key)
			}
			seenTimes[millis] = true
			got, err := strconv.ParseFloat(encoded, 64)
			if err != nil {
				t.Fatal(err)
			}
			const relativeTolerance = 1e-9
			if (math.IsNaN(got) && math.IsNaN(want)) || got == want {
				continue
			}
			if math.IsNaN(got) || math.IsNaN(want) || math.IsInf(got, 0) || math.IsInf(want, 0) || math.Abs(got-want) > relativeTolerance*math.Max(1, math.Abs(want)) {
				t.Fatalf("%s at %v: got %.17g, upstream %.17g", key, timestamp, got, want)
			}
		}
	}
	for key, grid := range expected {
		if instant {
			if _, ok := grid[end.UnixMilli()]; !ok {
				continue
			}
		}
		if !seen[key] {
			t.Fatalf("missing upstream series %s", key)
		}
	}
}
