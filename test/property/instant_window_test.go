//go:build chdb

// Property test for the instant range-vector WINDOW ANCHOR.
//
// This is Build Family B: a pgregory.net/rapid property that sweeps the
// eval instant T across a continuous series and asserts cerberus's
// instant /api/v1/query result for `<fn>(series[range])` AGREES with a
// from-scratch oracle evaluating the SAME (T-range, T] window.
//
// # Why this test exists (the gap it closes)
//
// The chDB spec harness (test/spec/runner_chdb.go::substituteNow64)
// rewrites every now64(...) in emitted SQL to ONE fixed literal the
// seeds are aligned to, so the eval instant is never varied relative to
// the sample timestamps. The existing property lane
// (TestPromQL_Property_FromScratch) pins EvalTs to AnchorTime+200s — a
// single fixed offset just past the data. NEITHER sweeps T across the
// (T-range, T] window over real continuous data — which is exactly where
// the rc.8 "instant range-vector window anchored to now64(9) wall-clock
// instead of time=T" bug lived: the window silently became
// (serverNow-range, serverNow], ignoring time=T, so the result went
// EMPTY at eval instants ~60-90s old.
//
// # How it catches the bug
//
// Cerberus is driven through its REAL Prom HTTP handler with `time=T`
// (the same wire.RunInstant the existing lane uses). WITH the fix the
// emitted window bound is a literal toDateTime64(T...) so the window
// tracks T; WITHOUT the fix it is now64(9) — chDB wall-clock at execution
// (~weeks after the 2026-05-13 seed anchor), so the (serverNow-range,
// serverNow] window misses every seeded sample and the result is empty.
// The oracle evaluates the (T-range, T] window directly off the in-memory
// series and computes the exact PromQL result, including extrapolation and
// counter-reset arithmetic. The comparator pins row presence, labels,
// timestamp, and value, then rapid shrinks any drift to the minimal (interval,
// range, offset).
//
// # CI lane
//
// Build-tagged `chdb`; runs in the same `property` workflow as the other
// property tests under the full `chdb,agpl_oracle,chdb_agpl_oracle` tag set.
package property_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// TestPromQL_InstantWindowSweep_FromScratch sweeps the eval instant T
// across a continuous series and asserts cerberus's instant range-vector
// result agrees exactly with the from-scratch window oracle for the
// (T-range, T] sample set, row presence, labels, timestamp, and value.
//
// Without the modifiers.go fix the emitted window bound is now64(9)
// (wall-clock at chDB execution, weeks after the seed anchor), so the
// window misses every sample and cerberus returns empty while the oracle
// returns a value — a drift rapid reports and shrinks to the minimal
// (scrapeIntervalSec, rangeSec, evalOffsetSec).
func TestPromQL_InstantWindowSweep_FromScratch(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rapid.Check(t, func(rt *rapid.T) {
		c := gen.InstantWindowSweep().Draw(rt, "case")
		if invalid := property.ValidateMetricsDataset(c.Dataset); invalid != "" {
			rt.Fatalf("instant-window property: invalid generated dataset: %s", invalid)
		}
		if invalid := property.ValidateGeneratedQuery(c.Query); invalid != "" {
			rt.Fatalf("instant-window property: invalid generated query: %s", invalid)
		}
		cli.Seed(t, c.Dataset.DDL)

		want := oracleInstantWindow(c)
		got := wire.RunInstant(context.Background(), srv.URL, c.Query, wire.InstantOptions{})
		if diff := property.ValidateOutcomes(want, got); diff != "" {
			rt.Fatalf("instant-window sweep: drift\n"+
				"query=%s evalTs=%d offset=%ds range=%ds scrape=%ds fn=%s valueProfile=%d latestSample=%d\n"+
				"--- diff ---\n%s",
				c.Query.String, c.Query.EvalTs, c.EvalOffset, c.RangeSec, c.ScrapeSec,
				c.Fn, c.ValueProfile, c.LatestSample, diff)
		}
	})
}

// TestPromQL_InstantWindowShapeRoster executes one deterministic live value
// differential for every function/value-profile pair in the exact
// instant-window roster.
func TestPromQL_InstantWindowShapeRoster(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	type instantWindowShapeExample struct {
		generated gen.InstantWindowCase
		oracle    property.Outcome
	}

	property.RunShapeCases(
		t,
		gen.InstantWindowShapeIDs(),
		func(shapeID gen.ShapeID, _ int) (property.Dataset, instantWindowShapeExample, property.ShapeID, string) {
			propertyShapeID := property.ShapeID(shapeID)
			for attempt := range property.ShapeExampleAttemptLimit {
				seed := property.ShapeExampleAttemptSeed(propertyShapeID, attempt)
				generated := gen.InstantWindowSweepForShape(shapeID).Example(seed)
				if invalid := property.ValidateGeneratedDataset(generated.Dataset); invalid != "" {
					t.Fatalf("instant-window shape %q generated an invalid dataset: %s", shapeID, invalid)
				}
				if generated.Query.ShapeID != propertyShapeID {
					t.Fatalf("instant-window shape generator stamped %q, want %q",
						generated.Query.ShapeID, propertyShapeID)
				}
				if invalid := property.ValidateGeneratedQuery(generated.Query); invalid != "" {
					t.Fatalf("instant-window shape %q generated an invalid query: %s", shapeID, invalid)
				}

				oracleOut := oracleInstantWindow(generated)
				if oracleOut.Err != nil {
					t.Fatalf("instant-window shape %q oracle failed\nquery=%s\nerr=%v",
						shapeID, generated.Query.String, oracleOut.Err)
				}
				if len(oracleOut.Rows) == 0 {
					continue
				}
				return generated.Dataset, instantWindowShapeExample{
					generated: generated,
					oracle:    oracleOut,
				}, generated.Query.ShapeID, generated.Query.String
			}
			t.Fatalf("instant-window shape %q produced no oracle rows across %d stable attempts",
				propertyShapeID, property.ShapeExampleAttemptLimit)
			return property.Dataset{}, instantWindowShapeExample{}, "", ""
		},
		func(t *testing.T, _ property.Dataset, example instantWindowShapeExample) {
			generated := example.generated
			cli.Seed(t, generated.Dataset.DDL)
			systemOut := wire.RunInstant(t.Context(), srv.URL, generated.Query, wire.InstantOptions{})
			if diff := property.ValidateDeterministicOutcomes(example.oracle, systemOut); diff != "" {
				t.Fatalf("instant-window shape %q failed\nquery=%s\n--- diff ---\n%s",
					generated.Query.ShapeID, generated.Query.String, diff)
			}
		},
	)
}

const (
	prometheusExtrapolationThresholdFactor = 1.1
	millisecondsPerSecond                  = 1000
)

// oracleInstantWindow independently evaluates the single-series PromQL
// instant-window cases generated by gen.InstantWindowSweep. Every enrolled
// function produces a complete expected Outcome: labels, evaluation timestamp,
// presence, and value. Unsupported functions are harness errors, never an
// implicit empty result.
func oracleInstantWindow(c gen.InstantWindowCase) property.Outcome {
	if c.Dataset.Metrics == nil {
		return property.Outcome{Err: fmt.Errorf("instant-window oracle: missing metrics model")}
	}
	if len(c.Dataset.Metrics.Series) != 1 {
		return property.Outcome{Err: fmt.Errorf(
			"instant-window oracle: got %d series, want exactly one",
			len(c.Dataset.Metrics.Series),
		)}
	}
	if c.RangeSec <= 0 {
		return property.Outcome{Err: fmt.Errorf("instant-window oracle: non-positive range %d", c.RangeSec)}
	}
	switch c.ValueProfile {
	case gen.InstantWindowValueProfileWave,
		gen.InstantWindowValueProfilePositiveIncrements,
		gen.InstantWindowValueProfileMonotonicRunningTotal:
	default:
		return property.Outcome{Err: fmt.Errorf(
			"instant-window oracle: unsupported value profile %d",
			c.ValueProfile,
		)}
	}

	minimumSamples, err := instantWindowMinimumSamples(c.Fn)
	if err != nil {
		return property.Outcome{Err: err}
	}

	endMs := c.Query.EvalTs * millisecondsPerSecond
	startMs := endMs - c.RangeSec*millisecondsPerSecond
	points := c.Dataset.Metrics.Series[0].Points
	window := make([]property.Point, 0, len(points))
	for _, point := range points {
		if point.TimestampMs > startMs && point.TimestampMs <= endMs {
			window = append(window, point)
		}
	}
	sort.Slice(window, func(i, j int) bool {
		return window[i].TimestampMs < window[j].TimestampMs
	})
	for index := 1; index < len(window); index++ {
		if window[index].TimestampMs == window[index-1].TimestampMs {
			return property.Outcome{Err: fmt.Errorf(
				"instant-window oracle: duplicate timestamp %d",
				window[index].TimestampMs,
			)}
		}
	}
	if len(window) < minimumSamples {
		return property.Outcome{}
	}

	value, err := instantWindowValue(c.Fn, window, startMs, endMs, c.RangeSec)
	if err != nil {
		return property.Outcome{Err: err}
	}
	labels := make(map[string]string, len(c.Dataset.Metrics.Series[0].Labels))
	for name, value := range c.Dataset.Metrics.Series[0].Labels {
		labels[name] = value
	}
	return property.Outcome{Rows: []property.OutcomeRow{{
		Labels:      labels,
		TimestampMs: endMs,
		Value:       value,
	}}}
}

func instantWindowMinimumSamples(fn string) (int, error) {
	switch fn {
	case "sum_over_time", "count_over_time", "avg_over_time", "min_over_time", "max_over_time":
		return 1, nil
	case "rate", "increase", "delta":
		return 2, nil
	default:
		return 0, fmt.Errorf("instant-window oracle: unsupported function %q", fn)
	}
}

func instantWindowValue(
	fn string,
	window []property.Point,
	startMs int64,
	endMs int64,
	rangeSec int64,
) (float64, error) {
	switch fn {
	case "sum_over_time":
		var sum float64
		for _, point := range window {
			sum += point.Value
		}
		return sum, nil
	case "count_over_time":
		return float64(len(window)), nil
	case "avg_over_time":
		var sum float64
		for _, point := range window {
			sum += point.Value
		}
		return sum / float64(len(window)), nil
	case "min_over_time":
		minimum := window[0].Value
		for _, point := range window[1:] {
			if point.Value < minimum {
				minimum = point.Value
			}
		}
		return minimum, nil
	case "max_over_time":
		maximum := window[0].Value
		for _, point := range window[1:] {
			if point.Value > maximum {
				maximum = point.Value
			}
		}
		return maximum, nil
	case "rate", "increase", "delta":
		return instantWindowExtrapolatedValue(fn, window, startMs, endMs, rangeSec)
	default:
		return 0, fmt.Errorf("instant-window oracle: unsupported function %q", fn)
	}
}

// instantWindowExtrapolatedValue is a from-scratch transcription of the
// PromQL rate/increase/delta specification. The generator's ValueProfile field
// selects input value geometry only; all cases are gauge-table samples, so
// rate/increase apply Prometheus counter-reset arithmetic to every geometry and
// delta always applies a straight gauge difference.
func instantWindowExtrapolatedValue(
	fn string,
	window []property.Point,
	startMs int64,
	endMs int64,
	rangeSec int64,
) (float64, error) {
	if len(window) < 2 {
		return 0, fmt.Errorf("instant-window oracle: %s needs at least two samples", fn)
	}
	first := window[0]
	last := window[len(window)-1]
	sampledIntervalMs := float64(last.TimestampMs - first.TimestampMs)
	if sampledIntervalMs <= 0 {
		return 0, fmt.Errorf(
			"instant-window oracle: non-positive sampled interval %gms",
			sampledIntervalMs,
		)
	}

	isCounter := fn == "rate" || fn == "increase"
	result := last.Value - first.Value
	if isCounter {
		previous := first.Value
		for _, point := range window[1:] {
			if point.Value < previous {
				result += previous
			}
			previous = point.Value
		}
	}

	averageIntervalMs := sampledIntervalMs / float64(len(window)-1)
	thresholdMs := averageIntervalMs * prometheusExtrapolationThresholdFactor
	durationToStartMs := float64(first.TimestampMs - startMs)
	if durationToStartMs >= thresholdMs {
		durationToStartMs = averageIntervalMs / 2
	}
	if isCounter && result > 0 && first.Value >= 0 {
		durationToZeroMs := sampledIntervalMs * first.Value / result
		if durationToZeroMs < durationToStartMs {
			durationToStartMs = durationToZeroMs
		}
	}
	durationToEndMs := float64(endMs - last.TimestampMs)
	if durationToEndMs >= thresholdMs {
		durationToEndMs = averageIntervalMs / 2
	}

	factor := (sampledIntervalMs + durationToStartMs + durationToEndMs) / sampledIntervalMs
	if fn == "rate" {
		factor /= float64(rangeSec)
	}
	return result * factor, nil
}

func TestInstantWindowOracleComputesEveryFunctionValue(t *testing.T) {
	points := []property.Point{
		{TimestampMs: 15 * millisecondsPerSecond, Value: 10},
		{TimestampMs: 30 * millisecondsPerSecond, Value: 20},
		{TimestampMs: 45 * millisecondsPerSecond, Value: 30},
	}
	tests := []struct {
		fn   string
		want float64
	}{
		{fn: "sum_over_time", want: 60},
		{fn: "count_over_time", want: 3},
		{fn: "avg_over_time", want: 20},
		{fn: "min_over_time", want: 10},
		{fn: "max_over_time", want: 30},
		{fn: "rate", want: 2.0 / 3.0},
		{fn: "increase", want: 40},
		{fn: "delta", want: 40},
	}
	for _, tc := range tests {
		t.Run(tc.fn, func(t *testing.T) {
			outcome := oracleInstantWindow(instantWindowOracleCase(
				tc.fn,
				gen.InstantWindowValueProfileWave,
				points,
			))
			if outcome.Err != nil {
				t.Fatalf("oracleInstantWindow() error: %v", outcome.Err)
			}
			if len(outcome.Rows) != 1 {
				t.Fatalf("oracleInstantWindow() rows = %d, want 1", len(outcome.Rows))
			}
			want := property.Outcome{Rows: []property.OutcomeRow{{
				Labels:      map[string]string{"job": "api"},
				TimestampMs: 60 * millisecondsPerSecond,
				Value:       tc.want,
			}}}
			if diff := property.ValidateOutcomes(want, outcome); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestInstantWindowOracleExtrapolatedFunctionsCoverEveryValueGeometry(t *testing.T) {
	points := []property.Point{
		{TimestampMs: 15 * millisecondsPerSecond, Value: 10},
		{TimestampMs: 30 * millisecondsPerSecond, Value: 3},
		{TimestampMs: 45 * millisecondsPerSecond, Value: 8},
	}
	want := map[string]float64{
		"rate":     16.0 / 60.0,
		"increase": 16,
		"delta":    -4,
	}
	for _, profile := range []gen.InstantWindowValueProfile{
		gen.InstantWindowValueProfileWave,
		gen.InstantWindowValueProfilePositiveIncrements,
		gen.InstantWindowValueProfileMonotonicRunningTotal,
	} {
		for fn, wantValue := range want {
			name := fmt.Sprintf("value-profile-%d/%s", profile, fn)
			t.Run(name, func(t *testing.T) {
				outcome := oracleInstantWindow(instantWindowOracleCase(fn, profile, points))
				if outcome.Err != nil || len(outcome.Rows) != 1 {
					t.Fatalf("oracleInstantWindow() = %+v, want one successful row", outcome)
				}
				if diff := property.ValidateOutcomes(
					property.Outcome{Rows: []property.OutcomeRow{{
						Labels:      map[string]string{"job": "api"},
						TimestampMs: 60 * millisecondsPerSecond,
						Value:       wantValue,
					}}},
					outcome,
				); diff != "" {
					t.Fatal(diff)
				}
			})
		}
	}
}

func TestInstantWindowOracleUsesLeftExclusiveRightInclusiveWindow(t *testing.T) {
	points := []property.Point{
		{TimestampMs: 0, Value: 100},
		{TimestampMs: 1, Value: 1},
		{TimestampMs: 60 * millisecondsPerSecond, Value: 2},
		{TimestampMs: 60*millisecondsPerSecond + 1, Value: 1000},
	}
	outcome := oracleInstantWindow(instantWindowOracleCase(
		"sum_over_time",
		gen.InstantWindowValueProfileWave,
		points,
	))
	want := property.Outcome{Rows: []property.OutcomeRow{{
		Labels:      map[string]string{"job": "api"},
		TimestampMs: 60 * millisecondsPerSecond,
		Value:       3,
	}}}
	if diff := property.ValidateOutcomes(want, outcome); diff != "" {
		t.Fatal(diff)
	}
}

func TestInstantWindowOracleReturnsAbsentForInsufficientSamples(t *testing.T) {
	tests := []struct {
		name   string
		fn     string
		points []property.Point
	}{
		{name: "zero samples", fn: "sum_over_time"},
		{
			name:   "one sample",
			fn:     "rate",
			points: []property.Point{{TimestampMs: 30 * millisecondsPerSecond, Value: 1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := oracleInstantWindow(instantWindowOracleCase(
				tc.fn,
				gen.InstantWindowValueProfileWave,
				tc.points,
			))
			if outcome.Err != nil {
				t.Fatalf("oracleInstantWindow() error: %v", outcome.Err)
			}
			if len(outcome.Rows) != 0 {
				t.Fatalf("oracleInstantWindow() rows = %v, want absent result", outcome.Rows)
			}
		})
	}
}

func TestInstantWindowOracleExtrapolationThresholdIsInclusive(t *testing.T) {
	tests := []struct {
		name    string
		firstMs int64
		want    float64
	}{
		{
			name:    "just below threshold is not clamped",
			firstMs: 10*millisecondsPerSecond + 999,
			want:    4.1998,
		},
		{
			name:    "equal threshold is clamped",
			firstMs: 11 * millisecondsPerSecond,
			want:    3,
		},
		{
			name:    "above threshold is clamped",
			firstMs: 12 * millisecondsPerSecond,
			want:    3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			points := []property.Point{
				{TimestampMs: tc.firstMs, Value: 1},
				{TimestampMs: tc.firstMs + 10*millisecondsPerSecond, Value: 3},
			}
			got, err := instantWindowExtrapolatedValue(
				"delta",
				points,
				0,
				points[1].TimestampMs,
				points[1].TimestampMs/millisecondsPerSecond,
			)
			if err != nil {
				t.Fatalf("instantWindowExtrapolatedValue() error: %v", err)
			}
			assertInstantWindowFloat(t, got, tc.want)
		})
	}
}

func TestInstantWindowOracleCounterZeroClamp(t *testing.T) {
	tests := []struct {
		name   string
		values [2]float64
		want   float64
	}{
		{
			name:   "non-negative counter clamps to implied zero",
			values: [2]float64{1, 101},
			want:   101,
		},
		{
			name:   "negative first value disables zero clamp",
			values: [2]float64{-1, 99},
			want:   200,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := instantWindowExtrapolatedValue(
				"increase",
				[]property.Point{
					{TimestampMs: 10 * millisecondsPerSecond, Value: tc.values[0]},
					{TimestampMs: 20 * millisecondsPerSecond, Value: tc.values[1]},
				},
				0,
				20*millisecondsPerSecond,
				20,
			)
			if err != nil {
				t.Fatalf("instantWindowExtrapolatedValue() error: %v", err)
			}
			assertInstantWindowFloat(t, got, tc.want)
		})
	}
}

func TestInstantWindowOracleRejectsDuplicateTimestamp(t *testing.T) {
	duplicateTimestamp := int64(30 * millisecondsPerSecond)
	outcome := oracleInstantWindow(instantWindowOracleCase(
		"rate",
		gen.InstantWindowValueProfileWave,
		[]property.Point{
			{TimestampMs: duplicateTimestamp, Value: 1},
			{TimestampMs: duplicateTimestamp, Value: 2},
		},
	))
	if outcome.Err == nil {
		t.Fatal("oracleInstantWindow() accepted a zero-interval duplicate timestamp")
	}
}

func TestInstantWindowOracleRejectsUnknownFunction(t *testing.T) {
	caseWithUnknownFunction := instantWindowOracleCase(
		"unknown_over_time",
		gen.InstantWindowValueProfileWave,
		[]property.Point{{TimestampMs: 30 * millisecondsPerSecond, Value: 1}},
	)
	outcome := oracleInstantWindow(caseWithUnknownFunction)
	if outcome.Err == nil {
		t.Fatal("oracleInstantWindow() accepted an unsupported function")
	}
}

func assertInstantWindowFloat(t *testing.T, got, want float64) {
	t.Helper()
	wantOutcome := property.Outcome{Rows: []property.OutcomeRow{{Value: want}}}
	gotOutcome := property.Outcome{Rows: []property.OutcomeRow{{Value: got}}}
	if diff := property.ValidateOutcomes(wantOutcome, gotOutcome); diff != "" {
		t.Fatal(diff)
	}
}

func instantWindowOracleCase(
	fn string,
	profile gen.InstantWindowValueProfile,
	points []property.Point,
) gen.InstantWindowCase {
	labels := map[string]string{"job": "api"}
	return gen.InstantWindowCase{
		Dataset: property.Dataset{
			DDL: "seed",
			Metrics: &property.MetricsModel{Series: []property.SeriesData{{
				MetricName: "series",
				Labels:     labels,
				Points:     points,
			}}},
		},
		RangeSec:     60,
		ValueProfile: profile,
		Fn:           fn,
		MetricName:   "series",
		Labels:       labels,
		Query: property.Query{
			ShapeID: "test.instant-window",
			String:  fn + `(series{job="api"}[60s])`,
			EvalTs:  60,
		},
	}
}
