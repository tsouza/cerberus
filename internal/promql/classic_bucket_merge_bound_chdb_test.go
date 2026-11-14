//go:build chdb

// chDB-backed proof that the classic-histogram cross-series bucket merge
// budget guard (#2408, classic_bucket_merge_bound.go) actually FIRES at
// real ClickHouse execution — not merely that the emitted SQL contains the
// right tokens — and that it stays silent on a legitimate,
// well-within-budget merge, at BOTH call sites that build a
// classicBucketMergeShaping-shaped Aggregate: the instant-mode
// `histogram_quantile(phi, sum by(le)(...))` (histogram_quantile.go) and
// the range-mode query_range equivalent (histogram_quantile_range.go's
// lowerHistogramQuantileClassicAggRange — the exact function issue #2408's
// own audit names).
//
// Every seeded series carries a DISJOINT ExplicitBounds layout (series i's
// bounds are `[i*width+1 .. i*width+width]`) — classic_bucket_merge_bound.go's
// own header doc calibration seed, the worst case for the merge, where the
// merged union width and the total bucket-element volume grow together.
package promql_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// classicBucketMergeBoundSeedDDL is the OTel-CH classic-histogram table
// shape the default schema reads, matching hqNativeSeedDDL
// (histogram_quantile_classic_native_chdb_test.go, internal/chsql) — this
// package needs its own copy since Go test binaries don't share consts
// across packages.
const classicBucketMergeBoundSeedDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

// classicBucketMergeBoundMetric is the metric name every case below
// queries — distinct from other fixtures sharing this package's chDB
// session (see fixture_chdb_test.go) so this test's own predicate can never
// read a sibling fixture's rows.
const classicBucketMergeBoundMetric = "classic_bucket_merge_bound_test_metric"

// classicBucketMergeBoundSampleTS is the newest sample timestamp every
// seeded series carries; classicBucketMergeBoundEvalTS (instant path) and
// the range-mode grid (below) both read it inside their lookback window.
var classicBucketMergeBoundSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// classicBucketMergeBoundEvalTS is the instant eval time — one second after
// the newest sample, well inside the sum_over_time(...[5m]) window.
var classicBucketMergeBoundEvalTS = classicBucketMergeBoundSampleTS.Add(time.Second)

// classicBucketMergeBoundSampleSpacingSeconds separates one series'
// consecutive samples. With at most classicBucketMergeBoundMaxSamples
// samples a series spans under two minutes, so every sample sits inside the
// [5m] window of the instant eval time and of every range-grid anchor.
const classicBucketMergeBoundSampleSpacingSeconds = 10

// classicBucketMergeBoundMaxSamples is the most samples per series a seed
// may carry and keep every sample inside every evaluated window.
const classicBucketMergeBoundMaxSamples = 10

// classicBucketMergeBoundQuery is the aggregated classic-histogram idiom
// that reaches classicBucketMergeShaping in both the instant and range
// lowerers: `sum by(le)(sum_over_time(...))`, matched against
// histogramAggShapeLowerable (sum_over_time's 1-sample floor, unlike
// rate's 2, lets a single seeded sample per series exercise it).
const classicBucketMergeBoundQuery = "histogram_quantile(0.5, sum by(le)(sum_over_time(" +
	classicBucketMergeBoundMetric + "_bucket[5m])))"

// classicBucketMergeBoundGroupedQuery is classicBucketMergeBoundQuery merged
// per `grp` label: one merge group per value the seed spreads its series
// over.
const classicBucketMergeBoundGroupedQuery = "histogram_quantile(0.5, sum by(le, grp)(sum_over_time(" +
	classicBucketMergeBoundMetric + "_bucket[5m])))"

// classicBucketMergeBoundRangeAnchors is the number of range-grid anchors
// runClassicBucketMergeBoundRangeQuery evaluates. Every seeded sample sits
// inside every anchor's [5m] window, so each series reaches the merge once
// per anchor.
const classicBucketMergeBoundRangeAnchors = 3

// classicBucketMergeBoundShape is one seed: series series, each with its own
// disjoint width-wide layout, each carrying samples in-window samples, spread
// round-robin over groups values of the `grp` label (one value when groups
// is zero).
type classicBucketMergeBoundShape struct {
	series, width, samples, groups int
}

// mergeEntries is the bucket-entry count the guard charges one evaluation of
// this shape: one row per series reaches the cross-series merge (the
// per-series window has already folded its samples), each carrying width
// finite bounds plus the +Inf rung, summed across every merge group.
func (sh classicBucketMergeBoundShape) mergeEntries() int64 {
	return int64(sh.series) * int64(sh.width+1)
}

// groupEntries is the bucket-entry count of the largest merge group under
// classicBucketMergeBoundGroupedQuery.
func (sh classicBucketMergeBoundShape) groupEntries() int64 {
	groups := max(sh.groups, 1)
	return int64((sh.series+groups-1)/groups) * int64(sh.width+1)
}

// seedClassicBucketMergeBoundRows renders an INSERT of shape: series i's
// bounds are `[i*width+1 .. i*width+width]` (see this file's header doc),
// one observation per bucket per sample.
func seedClassicBucketMergeBoundRows(t *testing.T, shape classicBucketMergeBoundShape) string {
	t.Helper()
	if shape.samples < 1 || shape.samples > classicBucketMergeBoundMaxSamples {
		t.Fatalf("seed %+v: samples must lie in [1, %d]", shape, classicBucketMergeBoundMaxSamples)
	}
	return fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d)), 'grp', toString(intDiv(number, %d) %% %d)),
  toDateTime64('%s', 9) - toIntervalSecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(1)),
  arrayMap(j -> toFloat64(intDiv(number, %d) * %d + j), range(1, %d))
FROM numbers(%d)`,
		classicBucketMergeBoundMetric, shape.samples, shape.samples, max(shape.groups, 1),
		classicBucketMergeBoundSampleTS.Format("2006-01-02 15:04:05"), shape.samples, classicBucketMergeBoundSampleSpacingSeconds,
		shape.width+1,
		shape.samples, shape.width, shape.width+1,
		shape.series*shape.samples)
}

// newClassicBucketMergeBoundFixture seeds shape into a fresh fixture.
func newClassicBucketMergeBoundFixture(t *testing.T, shape classicBucketMergeBoundShape) *chdbFixture {
	t.Helper()
	return newChDBFixture(t, classicBucketMergeBoundSeedDDL+seedClassicBucketMergeBoundRows(t, shape)+";\n")
}

// boundsWithMergeBudget resolves the resource bounds an operator gets by
// setting CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_ENTRIES to budget, and
// checks the override took.
func boundsWithMergeBudget(t *testing.T, budget int64) promql.ResourceBounds {
	t.Helper()
	t.Setenv(promql.EnvClassicBucketMergeMaxEntries, strconv.FormatInt(budget, 10))
	bounds, err := promql.ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv: %v", err)
	}
	if bounds.ClassicBucketMergeMaxEntries != budget {
		t.Fatalf("ResourceBoundsFromEnv().ClassicBucketMergeMaxEntries = %d, want %d (the %s override)",
			bounds.ClassicBucketMergeMaxEntries, budget, promql.EnvClassicBucketMergeMaxEntries)
	}
	return bounds
}

// runClassicBucketMergeBoundInstantQuery lowers + emits the instant-mode
// query and runs it against fixture, returning the query error (nil on
// success). Wraps in `SELECT count() FROM (...)`, mirroring
// runHistogramMergeBoundQuery's identical rationale: the merged output
// carries Map/Array histogram columns chdb-go's parquet driver cannot
// decode, and a throwIf inside a column expression still fires even though
// the outer SELECT never reads that column's value.
func runClassicBucketMergeBoundInstantQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	return runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, promql.LowerOpts{})
}

// runClassicBucketMergeBoundInstantQueryWithOpts is
// runClassicBucketMergeBoundInstantQuery's opts-carrying variant, letting a
// test thread a non-default promql.LowerOpts.ResourceBounds (cerberus issue
// #2667) through the exact same lowering + emit + execute path — see
// TestClassicBucketMergeBudget_ChDB_EnvOverrideRaisesBudget /
// TestClassicBucketMergeBudget_ChDB_EnvOverrideLowersBudget.
func runClassicBucketMergeBoundInstantQueryWithOpts(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts) error {
	t.Helper()
	return runClassicBucketMergeBoundInstant(t, fixture, classicBucketMergeBoundQuery, opts)
}

// runClassicBucketMergeBoundInstant lowers + emits query in instant mode and
// runs it against fixture — see runClassicBucketMergeBoundInstantQuery.
func runClassicBucketMergeBoundInstant(t *testing.T, fixture *chdbFixture, query string, opts promql.LowerOpts) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, classicBucketMergeBoundEvalTS, classicBucketMergeBoundEvalTS, 0, opts)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.Query(context.Background(), wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		t.Fatal("count() query returned no rows")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	return rows.Err()
}

// runClassicBucketMergeBoundRangeQuery is
// runClassicBucketMergeBoundInstantQuery's range-mode counterpart: lowers
// via LowerAtRangeOpts over a grid covering classicBucketMergeBoundSampleTS,
// reaching lowerHistogramQuantileClassicAggRange — the exact function
// issue #2408's own audit names as the shared, unguarded merge's caller in
// range mode.
func runClassicBucketMergeBoundRangeQuery(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(classicBucketMergeBoundQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", classicBucketMergeBoundQuery, err)
	}
	gridStart := classicBucketMergeBoundSampleTS
	const gridStep = time.Minute
	gridEnd := gridStart.Add((classicBucketMergeBoundRangeAnchors - 1) * gridStep)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, gridStart, gridEnd, gridStep, opts)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", classicBucketMergeBoundQuery, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", classicBucketMergeBoundQuery, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.Query(context.Background(), wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan count(): %v", err)
		}
	}
	return rows.Err()
}

// classicBucketMergeBoundaryShapes are the seeds the guard-boundary test
// pins, chosen so a cost model other than the merged entry count misplaces
// the boundary on at least one of them: a single-sample and a multi-sample
// seed with identical merge volume (a model that charges samples rejects the
// second at the first's ceiling), a one-bound-per-series seed (a model that
// drops the +Inf rung halves its count) and a few wide rows (a model that
// multiplies by the row width rejects it at the ceiling).
var classicBucketMergeBoundaryShapes = []struct {
	name  string
	shape classicBucketMergeBoundShape
}{
	{"single-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 1}},
	{"multi-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 6}},
	{"one-bound-rows", classicBucketMergeBoundShape{series: 200, width: 1, samples: 3}},
	{"wide-rows", classicBucketMergeBoundShape{series: 4, width: 300, samples: 2}},
}

// classicBucketMergeBoundaryModes runs one shape through each call site that
// wraps a merge Aggregate in the guard.
// evaluations is how many times one series reaches the merge: once per
// evaluated instant. The sumMap modes select
// NativeClassicBucketMergeLowerer, whose merge runs inside the Aggregate.
var classicBucketMergeBoundaryModes = []struct {
	name        string
	evaluations int64
	sumMap      bool
	run         func(*testing.T, *chdbFixture, promql.LowerOpts) error
}{
	{"instant", 1, false, runClassicBucketMergeBoundInstantQueryWithOpts},
	{"range", classicBucketMergeBoundRangeAnchors, false, runClassicBucketMergeBoundRangeQuery},
	{"instant-summap", 1, true, runClassicBucketMergeBoundInstantQueryWithOpts},
	{"range-summap", classicBucketMergeBoundRangeAnchors, true, runClassicBucketMergeBoundRangeQuery},
}

// classicBucketMergeBoundOpts is the lowering options for a merge budget,
// selecting the sumMap merge when sumMap is set.
func classicBucketMergeBoundOpts(t *testing.T, budget int64, sumMap bool) promql.LowerOpts {
	t.Helper()
	opts := promql.LowerOpts{ResourceBounds: boundsWithMergeBudget(t, budget)}
	if sumMap {
		opts.Lowerers = promql.RangeLowerers{
			ClassicBucketMerge: promql.NativeClassicBucketMergeLowerer{Fallback: promql.FanoutClassicBucketMergeLowerer{}},
		}
	}
	return opts
}

// TestClassicBucketMergeBudget_ChDB_BoundaryIsTheMergedEntryCount pins the
// guard's cost model at its exact boundary, through the operator override:
// with the budget set to the query's merged entry count (series x
// (width + 1), once per evaluated instant) the query is admitted in instant
// and range mode, through both merge constructions, and one entry lower it
// is refused with the guard's own message. The admitted runs go first:
// chDB's Parquet output after a throwIf error is malformed (cerberus #3761).
func TestClassicBucketMergeBudget_ChDB_BoundaryIsTheMergedEntryCount(t *testing.T) {
	for _, tc := range classicBucketMergeBoundaryShapes {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClassicBucketMergeBoundFixture(t, tc.shape)
			for _, mode := range classicBucketMergeBoundaryModes {
				entries := tc.shape.mergeEntries() * mode.evaluations
				if err := mode.run(t, fixture, classicBucketMergeBoundOpts(t, entries, mode.sumMap)); err != nil {
					t.Fatalf("%s %+v: a budget of exactly %d merged entries must admit the merge: %v",
						mode.name, tc.shape, entries, err)
				}
			}
			for _, mode := range classicBucketMergeBoundaryModes {
				entries := tc.shape.mergeEntries() * mode.evaluations
				err := mode.run(t, fixture, classicBucketMergeBoundOpts(t, entries-1, mode.sumMap))
				if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
					t.Fatalf("%s %+v: a budget of %d, one below the %d merged entries, must be refused by the guard: %v",
						mode.name, tc.shape, entries-1, entries, err)
				}
			}
		})
	}
}

// TestClassicBucketMergeBudget_ChDB_BoundCountsEveryGroup pins that the
// ceiling bounds the whole query, not one merge group: six `grp` groups,
// each of whose entries sits well under a budget their sum exceeds, are
// refused under `sum by (le, grp)`, through both merge constructions. The
// admitted run at the total goes first (cerberus #3761).
func TestClassicBucketMergeBudget_ChDB_BoundCountsEveryGroup(t *testing.T) {
	shape := classicBucketMergeBoundShape{series: 60, width: 20, samples: 1, groups: 6}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	total := shape.mergeEntries()
	budget := total - 1
	if group := shape.groupEntries(); group*2 > budget {
		t.Fatalf("seed %+v: largest group carries %d entries; want it well under the %d budget", shape, group, budget)
	}
	for _, sumMap := range []bool{false, true} {
		if err := runClassicBucketMergeBoundInstant(t, fixture, classicBucketMergeBoundGroupedQuery,
			classicBucketMergeBoundOpts(t, total, sumMap)); err != nil {
			t.Fatalf("sumMap=%v %+v: a budget of exactly the query's %d entries must admit it: %v", sumMap, shape, total, err)
		}
	}
	for _, sumMap := range []bool{false, true} {
		err := runClassicBucketMergeBoundInstant(t, fixture, classicBucketMergeBoundGroupedQuery,
			classicBucketMergeBoundOpts(t, budget, sumMap))
		if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
			t.Fatalf("sumMap=%v %+v: groups of at most %d entries summing to %d must be refused under a %d budget: %v",
				sumMap, shape, shape.groupEntries(), total, budget, err)
		}
	}
}

// TestClassicBucketMergeBudget_ChDB_DefaultRejectsPastCeiling seeds three
// series of one-million-bound disjoint layouts — 3,000,003 merged entries,
// past the shipped ceiling of 2,982,616 — and asserts the default budget refuses
// the instant query with the guard's own message. Few wide rows keep the
// seed cheap: the guard counts entries before the merge Aggregate reads a
// row, so it refuses before any merge array is built.
func TestClassicBucketMergeBudget_ChDB_DefaultRejectsPastCeiling(t *testing.T) {
	shape := classicBucketMergeBoundShape{series: 3, width: 1_000_000, samples: 1}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	if got, ceiling := shape.mergeEntries(), promql.ClassicBucketMergeEntriesForMemory(0); got <= ceiling {
		t.Fatalf("seed %+v carries %d merged entries; want past the default ceiling %d", shape, got, ceiling)
	}
	err := runClassicBucketMergeBoundInstantQuery(t, fixture)
	if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
		t.Fatalf("seed %+v: the default budget must refuse the merge with the guard's message: %v", shape, err)
	}
}

// TestClassicBucketMergeBudget_ChDB_HalfGiBCapRefusesPastDerivedCeiling
// seeds two series of one-million-bound disjoint layouts — 2,000,002 merged
// entries, within the ceiling the 1 GiB default cap derives and past the one a
// 512 MiB cap derives — and asserts that lowering under a 512 MiB
// CERBERUS_CH_QUERY_MAX_MEMORY, with no explicit ceiling, refuses the merge
// with the guard's own message.
func TestClassicBucketMergeBudget_ChDB_HalfGiBCapRefusesPastDerivedCeiling(t *testing.T) {
	const (
		defaultCap = config.DefaultCHQueryMaxMemory
		halfGiBCap = defaultCap / 2
	)
	shape := classicBucketMergeBoundShape{series: 2, width: 1_000_000, samples: 1}
	atDefault := promql.ClassicBucketMergeEntriesForMemory(defaultCap)
	atHalf := promql.ClassicBucketMergeEntriesForMemory(halfGiBCap)
	if got := shape.mergeEntries(); got > atDefault || got <= atHalf {
		t.Fatalf("seed %+v carries %d merged entries; want within the default cap's ceiling %d and past the 512 MiB cap's %d",
			shape, got, atDefault, atHalf)
	}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, promql.LowerOpts{
		ResourceBounds: promql.ResourceBounds{CHQueryMaxMemory: halfGiBCap},
	})
	if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
		t.Fatalf("seed %+v: a 512 MiB cap must refuse the merge with the guard's message: %v", shape, err)
	}
}

// TestClassicBucketMergeBudget_ChDB_DefaultAdmitsOrdinaryMerge is the
// negative control: a small merge (50 series, disjoint 20-wide layouts,
// four samples each) answers under the default budget in both modes.
func TestClassicBucketMergeBudget_ChDB_DefaultAdmitsOrdinaryMerge(t *testing.T) {
	shape := classicBucketMergeBoundShape{series: 50, width: 20, samples: 4}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	for _, mode := range classicBucketMergeBoundaryModes {
		if err := mode.run(t, fixture, promql.LowerOpts{}); err != nil {
			t.Fatalf("%s %+v: an ordinary merge must not trip the guard: %v", mode.name, shape, err)
		}
	}
}
