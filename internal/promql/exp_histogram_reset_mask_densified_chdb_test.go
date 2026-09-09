//go:build chdb

// chDB-backed differential proof that the DENSIFIED counter-reset mask
// (histogram_native_reset.go, via expHistogramDenseContribsExpr) condemns
// exactly the sample pairs the per-target-bucket picker + Kahan fold it
// replaced condemns.
//
// # Why a differential rather than a hand-written reference
//
// The two renderings fold the IDENTICAL stored-count slices — the same
// (offset, length) pairs expHistogramBucketSliceBoundsExpr computes for
// both — and differ only in where the fold happens relative to the
// per-target-bucket loop. That is the kind of equality that is easy to
// state and easy to get off by one at exactly the clamped edges (an empty
// intersection, a slice starting one past the end, a downscale ratio
// above one). Running the SAME query through BOTH lowerings cannot make
// that mistake, because every part outside the pair comparison is
// literally the same code on both sides.
//
// This is the shape exp_histogram_increase_closed_form_chdb_test.go
// already uses to prove the closed-form fold against the telescoping one.
//
// # Why resets() is the observable
//
// `resets(<metric>[<range>])` publishes `arraySum(<mask>)` as its sample
// value (expHistogramPairCountExpr), so the mask's own verdict per pair
// is read back directly rather than inferred from a fold that consumes
// it. `increase()` is compared as well, because the mask reaches the
// bucket ladders through a different consumer — the coefficient vector —
// and a mask that agreed only where the fold happened to cancel would
// pass the first comparison alone.
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// resetMaskMetric routes onto the exponential-histogram table.
const resetMaskMetric = "reset_mask_exp_hist"

// resetMaskStep spaces the seeded samples and is the query_range step.
const resetMaskStep = 60 * time.Second

// resetMaskBaseline anchors the seed's final sample. Every series carries
// resetMaskSamples samples ending here, so one `[5m]` window holds them
// all.
var resetMaskBaseline = time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

// resetMaskSamples is the per-series sample count, hence
// resetMaskSamples-1 pairs each.
const resetMaskSamples = 4

// resetMaskDensifiedMarker and resetMaskPerTargetMarker are
// rendering-specific tokens of the two arms' emitted SQL: the pair
// comparison's own lambda parameters, `(rdc, rdp) -> rdc < rdp` for the
// densified arm and `rk` for the per-target one. The test asserts each
// call carries its OWN marker and not the other's — without that a
// lowering change routing both calls onto the same arm would leave this
// comparing a rendering against itself.
//
// Deliberately NOT the bare `arrayReduceInRanges` this used to key on.
// That function is the densified reading's mechanism, not its address:
// once the closed-form bucket ladders started folding per row through the
// same call (cerberus issue #3234), every arm's SQL carried the token and
// the per-target arm failed a check it had not changed. A marker has to
// name the rendering under test, not a primitive any sibling layer may
// also reach for.
const (
	resetMaskDensifiedMarker = "(rdc, rdp) ->"
	resetMaskPerTargetMarker = "arrayExists(rk ->"
)

const resetMaskDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64), " +
	"`AggregationTemporality` Int32" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// resetMaskSample is one seeded row of one series, spelled field by field
// so a case can move exactly the one field whose DetectReset condition it
// is there to exercise.
type resetMaskSample struct {
	count     uint64
	sum       float64
	scale     int32
	zeroCount uint64
	posOffset int32
	pos       []uint64
	negOffset int32
	neg       []uint64
}

// resetMaskCases are the seeded series, one per DetectReset condition and
// per edge of the slice arithmetic the two renderings share.
//
// They are hand-written rather than generated because the point is
// coverage of the CONDITIONS, and a generator over bucket counts would
// reach the Count condition on almost every draw while never producing a
// scale coarsening with an offset that straddles a merged bucket
// boundary.
var resetMaskCases = map[string][]resetMaskSample{
	// Monotonic in every field: no pair is a reset, which is what pins
	// that neither rendering condemns a clean counter.
	"steady": {
		{count: 10, sum: 5, posOffset: 0, pos: []uint64{6, 4}},
		{count: 20, sum: 10, posOffset: 0, pos: []uint64{12, 8}},
		{count: 30, sum: 15, posOffset: 0, pos: []uint64{18, 12}},
		{count: 40, sum: 20, posOffset: 0, pos: []uint64{24, 16}},
	},
	// Count alone regresses; every bucket keeps climbing.
	"count_regressed": {
		{count: 40, sum: 20, posOffset: 0, pos: []uint64{24, 16}},
		{count: 50, sum: 25, posOffset: 0, pos: []uint64{30, 20}},
		{count: 9, sum: 4, posOffset: 0, pos: []uint64{31, 21}},
		{count: 19, sum: 9, posOffset: 0, pos: []uint64{37, 25}},
	},
	// Count and ZeroCount both climb; ONE positive bucket drops. This is
	// the condition the two renderings actually differ in how they
	// compute, so it is the case the whole test exists for.
	"bucket_regressed": {
		{count: 12, sum: 6, posOffset: 0, pos: []uint64{10, 2}},
		{count: 28, sum: 14, posOffset: 0, pos: []uint64{8, 20}},
		{count: 40, sum: 20, posOffset: 0, pos: []uint64{14, 26}},
		{count: 52, sum: 26, posOffset: 0, pos: []uint64{20, 32}},
	},
	// The zero bucket alone regresses.
	"zero_regressed": {
		{count: 20, sum: 10, zeroCount: 8, posOffset: 0, pos: []uint64{6, 6}},
		{count: 30, sum: 15, zeroCount: 12, posOffset: 0, pos: []uint64{9, 9}},
		{count: 40, sum: 20, zeroCount: 3, posOffset: 0, pos: []uint64{18, 19}},
		{count: 50, sum: 25, zeroCount: 5, posOffset: 0, pos: []uint64{22, 23}},
	},
	// A resolution INCREASE, which reference condemns unconditionally —
	// the one condition decided before any bucket is read.
	"scale_increased": {
		{count: 10, sum: 5, scale: 0, posOffset: 0, pos: []uint64{6, 4}},
		{count: 20, sum: 10, scale: 0, posOffset: 0, pos: []uint64{12, 8}},
		{count: 30, sum: 15, scale: 1, posOffset: 0, pos: []uint64{9, 9, 6, 6}},
		{count: 40, sum: 20, scale: 1, posOffset: 0, pos: []uint64{12, 12, 8, 8}},
	},
	// A resolution DECREASE: the pair's previous row is FINER than the
	// current one, so the comparison downscales it by a ratio of two.
	// Every merged bucket still climbs, so no pair is a reset — which is
	// what pins that the downscale itself does not manufacture one.
	"scale_coarsened_clean": {
		{count: 20, sum: 10, scale: 1, posOffset: 0, pos: []uint64{5, 5, 5, 5}},
		{count: 30, sum: 15, scale: 1, posOffset: 0, pos: []uint64{8, 8, 7, 7}},
		{count: 44, sum: 22, scale: 0, posOffset: 0, pos: []uint64{22, 22}},
		{count: 60, sum: 30, scale: 0, posOffset: 0, pos: []uint64{30, 30}},
	},
	// The same coarsening, but one merged bucket falls: the regression is
	// visible ONLY after the pair's two finer buckets are summed, which
	// is precisely what the densified rendering moved outside the
	// per-target loop.
	"scale_coarsened_regressed": {
		{count: 40, sum: 20, scale: 1, posOffset: 0, pos: []uint64{12, 13, 7, 8}},
		{count: 60, sum: 30, scale: 1, posOffset: 0, pos: []uint64{18, 19, 11, 12}},
		{count: 70, sum: 35, scale: 0, posOffset: 0, pos: []uint64{30, 40}},
		{count: 90, sum: 45, scale: 0, posOffset: 0, pos: []uint64{40, 50}},
	},
	// Offsets move between samples, so each pair's merged range is wider
	// than either row and both slices clamp at an edge.
	"offset_shifted": {
		{count: 10, sum: 5, posOffset: -3, pos: []uint64{4, 6}},
		{count: 24, sum: 12, posOffset: 0, pos: []uint64{10, 14}},
		{count: 36, sum: 18, posOffset: -5, pos: []uint64{16, 20}},
		{count: 50, sum: 25, posOffset: 2, pos: []uint64{22, 28}},
	},
	// Offsets far from zero, matching the shape cerberus's own telemetry
	// carries (issue #3178 measured positive offsets of -1786..-1656).
	"offset_far": {
		{count: 10, sum: 5, scale: 6, posOffset: -1786, pos: []uint64{4, 6}},
		{count: 22, sum: 11, scale: 6, posOffset: -1786, pos: []uint64{10, 12}},
		{count: 34, sum: 17, scale: 6, posOffset: -1785, pos: []uint64{15, 19}},
		{count: 48, sum: 24, scale: 6, posOffset: -1785, pos: []uint64{21, 27}},
	},
	// No positive buckets at all: the merged range is empty and both
	// renderings must read that as "nothing regressed" rather than as an
	// out-of-range slice.
	"positive_empty": {
		{count: 10, sum: -5, posOffset: 0, pos: nil, negOffset: 0, neg: []uint64{6, 4}},
		{count: 20, sum: -10, posOffset: 0, pos: nil, negOffset: 0, neg: []uint64{12, 8}},
		{count: 30, sum: -15, posOffset: 0, pos: nil, negOffset: 0, neg: []uint64{18, 12}},
		{count: 40, sum: -20, posOffset: 0, pos: nil, negOffset: 0, neg: []uint64{24, 16}},
	},
	// The NEGATIVE ladder regresses while the positive one climbs, so the
	// verdict can only come from the second of the two ladder terms.
	"negative_regressed": {
		{count: 20, sum: 0, posOffset: 0, pos: []uint64{5, 5}, negOffset: 0, neg: []uint64{6, 4}},
		{count: 34, sum: 0, posOffset: 0, pos: []uint64{9, 9}, negOffset: 0, neg: []uint64{9, 7}},
		{count: 46, sum: 0, posOffset: 0, pos: []uint64{15, 15}, negOffset: 0, neg: []uint64{4, 12}},
		{count: 60, sum: 0, posOffset: 0, pos: []uint64{20, 20}, negOffset: 0, neg: []uint64{8, 12}},
	},
	// One sample carries a bucket the next does not carry at all: a
	// populated previous bucket absent from the current histogram is a
	// regression in reference, and it is the case whose slice is empty on
	// the current side and non-empty on the previous one.
	"bucket_disappeared": {
		{count: 20, sum: 10, posOffset: 0, pos: []uint64{8, 6, 6}},
		{count: 30, sum: 15, posOffset: 0, pos: []uint64{12, 9, 9}},
		{count: 40, sum: 20, posOffset: 0, pos: []uint64{22, 18}},
		{count: 52, sum: 26, posOffset: 0, pos: []uint64{28, 24}},
	},
}

// resetMaskSeed renders the DDL plus one INSERT covering every case.
func resetMaskSeed() string {
	cells := func(vs []uint64) string {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = fmt.Sprintf("%d", v)
		}
		return strings.Join(parts, ",")
	}
	start := resetMaskBaseline.Add(-time.Duration(resetMaskSamples-1) * resetMaskStep)
	tuples := make([]string, 0, len(resetMaskCases)*resetMaskSamples)
	for name, samples := range resetMaskCases {
		for i, smp := range samples {
			tuples = append(tuples, fmt.Sprintf(
				"('%s', map('series', '%s'), toDateTime64('%s', 9), %d, %f, %d, %d, %d, [%s], %d, [%s], %d)",
				resetMaskMetric, name,
				start.Add(time.Duration(i)*resetMaskStep).Format("2006-01-02 15:04:05"),
				smp.count, smp.sum, smp.scale, smp.zeroCount,
				smp.posOffset, cells(smp.pos), smp.negOffset, cells(smp.neg),
				schema.AggregationTemporalityCumulative,
			))
		}
	}
	return resetMaskDDL +
		"INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, " +
		"PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts, AggregationTemporality) VALUES\n    " +
		strings.Join(tuples, ",\n    ") + ";\n"
}

// TestExpHistogramResetMaskDensified_ChDB_MatchesPerTargetOverEveryCondition
// asserts the densified and per-target renderings condemn the same pairs,
// read back through resets() — the mask's own count — and through
// increase()'s bucket ladders, its other consumer.
func TestExpHistogramResetMaskDensified_ChDB_MatchesPerTargetOverEveryCondition(t *testing.T) {
	fixture := newChDBFixture(t, resetMaskSeed())

	perTarget := promql.LowerOpts{
		Lowerers: promql.RangeLowerers{
			ExpHistogramResetMask: promql.PerTargetExpHistogramResetMaskLowerer{},
		},
	}

	t.Run("resets", func(t *testing.T) {
		densified := resetMaskResetsRun(t, fixture, promql.LowerOpts{}, true)
		oracle := resetMaskResetsRun(t, fixture, perTarget, false)
		resetMaskCompare(t, densified, oracle, func(v []float64) string { return fmt.Sprint(v) })
		// A run in which NO series records a reset would compare two
		// all-zero maps and prove nothing, so the corpus must actually
		// exercise the condemning branch.
		var condemned int
		for _, v := range densified {
			if v[0] > 0 {
				condemned++
			}
		}
		if condemned == 0 {
			t.Fatal("no seeded series recorded a reset — the comparison is vacuous")
		}
	})

	t.Run("increase", func(t *testing.T) {
		densified := resetMaskIncreaseRun(t, fixture, promql.LowerOpts{}, true)
		oracle := resetMaskIncreaseRun(t, fixture, perTarget, false)
		resetMaskCompare(t, densified, oracle, func(v []float64) string { return fmt.Sprint(v) })
	})
}

// resetMaskCompare fails unless the two result maps carry the same series
// with the same values.
func resetMaskCompare(t *testing.T, got, want map[string][]float64, render func([]float64) string) {
	t.Helper()
	if len(got) != len(resetMaskCases) {
		t.Fatalf("densified emitted %d series, want %d", len(got), len(resetMaskCases))
	}
	for name := range resetMaskCases {
		g, ok := got[name]
		if !ok {
			t.Fatalf("series %s missing from the densified result", name)
		}
		w, ok := want[name]
		if !ok {
			t.Fatalf("series %s missing from the per-target result", name)
		}
		if len(g) != len(w) {
			t.Fatalf("series %s: densified %d values, per-target %d", name, len(g), len(w))
		}
		for i := range w {
			if g[i] != w[i] {
				t.Fatalf("series %s: densified = %s, per-target = %s",
					name, render(g), render(w))
			}
		}
	}
}

// resetMaskLower lowers expr over a two-anchor query_range under opts and
// asserts which mask rendering the emitted SQL actually is.
func resetMaskLower(t *testing.T, expr string, opts promql.LowerOpts, wantDensified bool) (string, []any) {
	t.Helper()
	parsed, err := promparser.NewParser(promparser.Options{}).ParseExpr(expr)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", expr, err)
	}
	plan, err := promql.LowerAtRangeOpts(
		context.Background(), parsed, schema.DefaultOTelMetrics(),
		resetMaskBaseline.Add(-resetMaskStep), resetMaskBaseline, resetMaskStep, opts,
	)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", expr, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", expr, err)
	}
	for _, marker := range []struct {
		token string
		want  bool
	}{
		{resetMaskDensifiedMarker, wantDensified},
		{resetMaskPerTargetMarker, !wantDensified},
	} {
		if got := strings.Contains(sqlStr, marker.token); got != marker.want {
			t.Fatalf("emitted SQL for %q carries %q = %v, want %v — the two arms are not the two "+
				"renderings this test believes it is comparing",
				expr, marker.token, got, marker.want)
		}
	}
	return sqlStr, args
}

// resetMaskResetsRun returns each seeded series' reset COUNT at the last
// anchor — the anchor whose `[5m]` window holds every seeded sample.
func resetMaskResetsRun(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts, wantDensified bool) map[string][]float64 {
	t.Helper()
	sqlStr, args := resetMaskLower(t, "resets("+resetMaskMetric+"[5m])", opts, wantDensified)
	rows, err := fixture.db.Query(
		"SELECT `Attributes`['series'], toString(`Value`) FROM ("+sqlStr+
			") WHERE `TimeUnix` = toDateTime64('"+resetMaskBaseline.Format("2006-01-02 15:04:05")+"', 9)",
		args...,
	)
	if err != nil {
		t.Fatalf("resets query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]float64{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var v float64
		if _, err := fmt.Sscanf(value, "%g", &v); err != nil {
			t.Fatalf("series %s: parse %q: %v", name, value, err)
		}
		out[name] = []float64{v}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// resetMaskIncreaseRun returns each seeded series' window-folded positive
// and negative bucket ladders at the last anchor, concatenated — the
// mask's other consumer.
func resetMaskIncreaseRun(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts, wantDensified bool) map[string][]float64 {
	t.Helper()
	sqlStr, args := resetMaskLower(t, "increase("+resetMaskMetric+"[5m])", opts, wantDensified)
	rows, err := fixture.db.Query(
		"SELECT `Attributes`['series'], arrayStringConcat(arrayMap(x -> toString(x), "+
			"arrayConcat(`HistogramPositiveBucketCounts`, `HistogramNegativeBucketCounts`)), ',') FROM ("+sqlStr+
			") WHERE `TimeUnix` = toDateTime64('"+resetMaskBaseline.Format("2006-01-02 15:04:05")+"', 9)",
		args...,
	)
	if err != nil {
		t.Fatalf("increase query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]float64{}
	for rows.Next() {
		var name, joined string
		if err := rows.Scan(&name, &joined); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parts := strings.Split(joined, ",")
		vals := make([]float64, 0, len(parts))
		for _, p := range parts {
			if p == "" {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(p, "%g", &v); err != nil {
				t.Fatalf("series %s: parse %q: %v", name, p, err)
			}
			vals = append(vals, v)
		}
		out[name] = vals
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
