//go:build chdb

// chDB-backed differential proof for issue #2756's sumMap-based classic-
// bucket cross-series merge (classic_bucket_merge_summap.go): the SAME
// query and seed, lowered once through the default groupArray-fold
// strategy and once through NativeClassicBucketMergeLowerer (chopt
// classic_bucket_merge_summap), executed against real ClickHouse (chDB)
// and compared.
//
//   - TestClassicBucketMergeSumMapDifferential_Homogeneous pins that the two
//     strategies answer IDENTICALLY for a homogeneous bucket layout (every
//     contributing series shares one ExplicitBounds) — the shape this
//     issue's own ~50x win estimate is calibrated on.
//   - TestClassicBucketMergeSumMapDifferential_DroppedZeroKey pins the same
//     identity for a group whose merged CUMULATIVE count is exactly zero at
//     a bound — the sumMap key-drop quirk classic_bucket_merge_summap.go's
//     header documents and reconstructs around.
//   - TestClassicBucketMergeSumMapDifferential_Heterogeneous pins that the
//     two strategies answer identically for DISJOINT bucket layouts too,
//     with the shared expected value hand-derived from Prometheus's own
//     bucketQuantile algorithm (prometheus/promql/quantile.go) over the
//     `sum by(le)` ladder — not a "whatever the code currently does"
//     tolerance. This case USED to pin a divergence (cerberus issue #2817);
//     see the test's own doc.
//   - TestClassicBucketMergeSumMapDifferential_RepeatedAndUnorderedBounds
//     pins the same identity for a PARTIALLY overlapping pair of layouts,
//     seeded from stored bounds that repeat a bound / are not ascending. The
//     per-row normalisations those stored shapes would need are pinned a
//     layer down, by classic_bucket_merge_summap_row_chdb_test.go — see that
//     file's header for why no query shape reaches them.
//   - TestClassicBucketMergeSumMap_NaNPrimitives and
//     TestClassicBucketMergeSumMap_ZeroKeyIdentity pin the underlying
//     ClickHouse primitives (arrayCumSum, arrayMax, sumMap, arrayDistinct,
//     indexOf) classic_bucket_merge_summap.go's design relies on directly,
//     since a NaN or a -0.0/0.0 identity mismatch cannot be forced through a
//     legitimate seeded query (Array(UInt64) BucketCounts has no NaN
//     representation).
package promql_test

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// summapDiffSeedDDL mirrors classicBucketMergeBoundSeedDDL — this file's
// own copy since Go test binaries don't share consts across packages.
const summapDiffSeedDDL = `
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

// summapDiffMetric is the metric name every case below queries — distinct
// from other fixtures sharing this package's chDB session (fixture_chdb_test.go).
const summapDiffMetric = "classic_bucket_merge_summap_diff_test_metric"

// summapDiffQuery is `sum by(le)(sum_over_time(...))`: sum_over_time's
// 1-sample floor (unlike rate's 2) lets one seeded sample per series
// exercise the merge with its raw per-bucket counts surviving the window
// stage's cumsum-then-diff round trip unchanged — mirroring
// classic_bucket_merge_bound_chdb_test.go's identical choice.
const summapDiffQuery = "histogram_quantile(0.95, sum by(le)(sum_over_time(" +
	summapDiffMetric + "_bucket[5m])))"

var summapDiffSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

var summapDiffEvalTS = summapDiffSampleTS.Add(time.Second)

// summapDiffLowerers is the two strategies under differential test:
// fanout (the default groupArray-fold merge) and native (this issue's
// sumMap merge over per-row cumulative counts), both with the OTHER
// family's fields left at
// their own zero-value default (resolved by withDefaults at the lowering
// entry — see RangeLowerers' own doc).
func summapDiffLowerers(native bool) promql.RangeLowerers {
	if !native {
		return promql.RangeLowerers{}
	}
	return promql.RangeLowerers{
		ClassicBucketMerge: promql.NativeClassicBucketMergeLowerer{
			Fallback: promql.FanoutClassicBucketMergeLowerer{},
		},
	}
}

// runSummapDiffQuery lowers summapDiffQuery under the given strategy and
// returns the single resulting Value cell.
func runSummapDiffQuery(t *testing.T, fixture *chdbFixture, native bool) float64 {
	t.Helper()
	return runSummapDiffQueryFor(t, fixture, native, summapDiffQuery)
}

// runSummapDiffQueryFor is runSummapDiffQuery over an explicit query — the
// bare-selector shape (summapDiffBareQuery) reaches the merge with each row's
// RAW ExplicitBounds, which the range-function shape's own per-series window
// stage would have sorted and de-duplicated first.
func runSummapDiffQueryFor(t *testing.T, fixture *chdbFixture, native bool, query string) float64 {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, summapDiffEvalTS, summapDiffEvalTS, 0,
		promql.LowerOpts{Lowerers: summapDiffLowerers(native)})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(native=%v): %v", native, err)
	}
	// Guard against a vacuous differential: every case below compares the
	// two strategies' ANSWERS, which agree trivially if the native lowering
	// silently fell back to the fan-out shaping (a query shape the sumMap
	// path does not claim, say). The aggregate name is the discriminator —
	// only classicBucketMergeShapingSumMap emits one.
	if strings.Contains(sqlStr, "sumMap(") != native {
		t.Fatalf("native=%v but emitted SQL %s sumMap(): the strategy under test is not the one running\n%s",
			native, map[bool]string{true: "carries", false: "lacks"}[strings.Contains(sqlStr, "sumMap(")], sqlStr)
	}
	// Projected to a plain string (Value's own toString), not scanned
	// alongside the raw row — chdb-go's parquet driver cannot decode this
	// query's Map(String,String) Attributes cell into a Go destination; see
	// chdbFixture.queryOverEmitted's own doc.
	rows := fixture.queryOverEmitted(t, "toString(Value)", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query (native=%v) returned no rows", native)
	}
	var valueStr string
	if err := rows.Scan(&valueStr); err != nil {
		t.Fatalf("scan (native=%v): %v", native, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		t.Fatalf("parse Value %q (native=%v): %v", valueStr, native, err)
	}
	return value
}

// TestClassicBucketMergeSumMapDifferential_Homogeneous seeds two series
// sharing ONE ExplicitBounds layout ([1, 2, 3]) — the overwhelmingly common
// real shape. classic_bucket_merge_summap.go's header proves the two
// constructions are mathematically identical here; this pins that they
// answer IDENTICALLY at real ClickHouse execution too.
func TestClassicBucketMergeSumMapDifferential_Homogeneous(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [10, 5, 0], [1.0, 2.0, 3.0]),
    ('`+summapDiffMetric+`', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [3, 4, 3], [1.0, 2.0, 3.0]);
`)

	fanout := runSummapDiffQuery(t, fixture, false)
	native := runSummapDiffQuery(t, fixture, true)
	if fanout != native {
		t.Fatalf("homogeneous layout: fanout = %v, native (sumMap) = %v; want equal", fanout, native)
	}
}

// summapDiffFloatTolerance absorbs ordinary float64 representation error in
// the hand-derived expected values below (Prometheus's own linear
// interpolation is a divide, so 2 + 3*(10.2/11) does not round to a decimal
// literal). It is NOT a "whatever the code answers" fudge: every expectation
// it guards is derived from Prometheus's bucketQuantile formula, and 1e-9 is
// many orders below the ~1e-15 rounding this arithmetic produces.
const summapDiffFloatTolerance = 1e-9

// TestClassicBucketMergeSumMapDifferential_DroppedZeroKey seeds a SINGLE
// series whose two lowest buckets are empty, so its CUMULATIVE count at
// bounds 1 and 2 is exactly zero — the keys sumMap drops outright
// (sumMap([1,2,3],[0,0,5]) returns keys=[3]), the quirk
// classic_bucket_merge_summap.go's header documents. Both strategies must
// still answer identically: the fan-out path never drops the bound, and
// classicBucketSumMapLookupExpr's indexOf-based reconstruction restores the
// dropped rung to the 0.0 the has-filter fold sums to.
//
// A leading empty run is what a dropped key looks like once the values are
// per-row CUMULATIVE counts; an interior empty bucket (this case's shape
// before that reformulation) no longer drops anything, because the running
// count carries over it.
func TestClassicBucketMergeSumMapDifferential_DroppedZeroKey(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 5], [1.0, 2.0, 3.0]);
`)

	// Ladder [0,0,5] + Inf rung 5; observations = 5, rank = 0.95*5 = 4.75.
	// arrayFirstIndex(c >= 4.75) = 3, not the overflow-rung tie, so the
	// general interpolation runs between bounds[2]=2 and bounds[3]=3 over
	// cum 0 -> 5: 2 + (3-2) * ((4.75-0)/(5-0)) = 2.95.
	const wantMerged = 2.95

	fanout := runSummapDiffQuery(t, fixture, false)
	native := runSummapDiffQuery(t, fixture, true)
	if math.Abs(fanout-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("dropped zero key: fanout = %v, want %v", fanout, wantMerged)
	}
	if math.Abs(native-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("dropped zero key: native (sumMap) = %v, want %v", native, wantMerged)
	}
	if fanout != native {
		t.Fatalf("dropped zero key: fanout = %v, native (sumMap) = %v; want equal", fanout, native)
	}
}

// TestClassicBucketMergeSumMapDifferential_Heterogeneous seeds two series
// with DISJOINT bucket layouts — the shape cerberus issue #2817 was filed
// against — and pins that both strategies now answer the SAME value, and
// that the value is the one reference Prometheus answers.
//
// # Reading a failure here
//
// This test USED to assert the opposite: it pinned fanout = 5.0 against
// native = 1.78 and FAILED if the two ever converged, on the theory that the
// sumMap merge's divergence for mismatched layouts was documented and
// accepted. It was not acceptable — `sum by(le)` has one right answer and
// 1.78 was not it — so the divergence was fixed at the source (each row now
// cumulates over its OWN buckets before the merge; see
// classic_bucket_merge_summap.go's header) and the assertion inverted. A red
// here is therefore a REGRESSION, never the intended outcome: it means the
// sumMap path has gone back to summing rows at union bounds their own layout
// does not carry.
//
// # Deriving the expected value
//
//	Series A: bounds [1,2,3], counts [10,5,0] (equal-length, no overflow).
//	Series B: bounds [1,5],   counts [7,0].
//
// Prometheus sees six float series: A's {le=1}=10, {le=2}=15, {le=3}=15,
// {le=+Inf}=15 and B's {le=1}=7, {le=5}=7, {le=+Inf}=7 (each already
// cumulative). `sum by(le)` sums each `le` group over whichever series
// reports it: rung@1 = 10+7 = 17; rung@2 = 15 (A only); rung@3 = 15 (A
// only); rung@5 = 7 (B only); rung@+Inf = 15+7 = 22. That ladder DIPS, and
// ensureMonotonicAndIgnoreSmallDeltas lifts it to [17,17,17,17,22].
//
// bucketQuantile then takes observations = 22, rank = 0.95*22 = 20.9. No
// finite rung reaches 20.9, so the answer is the OVERFLOW-rung clamp: the
// highest finite bound, 5.0.
func TestClassicBucketMergeSumMapDifferential_Heterogeneous(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [10, 5, 0], [1.0, 2.0, 3.0]),
    ('`+summapDiffMetric+`', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [7, 0], [1.0, 5.0]);
`)

	const wantMerged = 5.0

	fanout := runSummapDiffQuery(t, fixture, false)
	native := runSummapDiffQuery(t, fixture, true)

	if math.Abs(fanout-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("heterogeneous layout: fanout = %v, want %v (the overflow-rung clamp — see this test's own doc)", fanout, wantMerged)
	}
	if math.Abs(native-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("heterogeneous layout: native (sumMap) = %v, want %v (the overflow-rung clamp — see this test's own doc)", native, wantMerged)
	}
	if math.Abs(fanout-native) > summapDiffFloatTolerance {
		t.Fatalf("heterogeneous layout: fanout = %v, native (sumMap) = %v — the two constructions "+
			"answered DIFFERENTLY. This assertion is the inverted form of the one that pinned "+
			"cerberus issue #2817's divergence; a failure here means the sumMap merge has regressed "+
			"to summing rows at union bounds their own layout does not carry", fanout, native)
	}
}

// summapDiffBareQuery is `sum by(le)(<metric>_bucket)` — no range-vector
// function, so the merge stage reads each row's RAW ExplicitBounds off the
// table. summapDiffQuery's own sum_over_time wrapper cannot exercise that:
// its per-series window stage projects an arraySort + arrayDistinct union as
// each series' output layout, so a stored layout that repeats a bound or is
// not ascending is already normalised before the merge ever sees it.
const summapDiffBareQuery = "histogram_quantile(0.95, sum by(le)(" + summapDiffMetric + "_bucket))"

// TestClassicBucketMergeSumMapDifferential_RepeatedAndUnorderedBounds seeds
// two series whose stored layouts are individually malformed in the two ways
// a prefix sum is sensitive to and the has-filter fold is not — series A
// repeats the bound 1.0, series B stores its bounds descending — and which
// PARTIALLY overlap each other once normalised ([1,5] against [2,5], sharing
// only their top bound). That is a different merge topology from
// _Heterogeneous's near-disjoint pair, and it is the shape
// `histogram_quantile_classic_duplicate_bounds.txtar` covers for a single
// series.
//
// What this pins is the END-TO-END answer for such an input. It does NOT
// reach classicBucketSumMapRowArgs's own per-row bound ordering and
// equal-bound run collapse: the per-series stage feeding the merge projects
// classicBucketUnionBoundsExpr as each row's ExplicitBounds, so both stored
// layouts arrive at the merge already sorted and de-duplicated. Those two
// normalisations are pinned directly, against rows the merge cannot be
// handed through any query shape, by
// TestClassicBucketSumMapRowArgs_MatchesFoldReading
// (classic_bucket_merge_summap_row_chdb_test.go).
//
// # Deriving the expected value
//
//	Series A: bounds [1,1,5], counts [2,3,4] -> {le=1}=5, {le=5}=9, +Inf 9.
//	Series B: bounds [5,2],   counts [6,1]   -> {le=2}=1, {le=5}=7, +Inf 7.
//
// `sum by(le)` over union bounds [1,2,5]: rung@1 = 5 (A only); rung@2 = 1 (B
// only); rung@5 = 9+7 = 16; rung@+Inf = 9+7 = 16. The monotonic repair lifts
// the dip to [5,5,16,16]. observations = 16, rank = 0.95*16 = 15.2;
// arrayFirstIndex(c >= 15.2) = 3, not the overflow-rung tie, so the general
// interpolation runs between bounds[2]=2 and bounds[3]=5 over cum 5 -> 16:
// 2 + (5-2) * ((15.2-5)/(16-5)) = 2 + 3*(10.2/11).
func TestClassicBucketMergeSumMapDifferential_RepeatedAndUnorderedBounds(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [2, 3, 4], [1.0, 1.0, 5.0]),
    ('`+summapDiffMetric+`', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [6, 1], [5.0, 2.0]);
`)

	wantMerged := 2 + 3*(10.2/11)

	fanout := runSummapDiffQueryFor(t, fixture, false, summapDiffBareQuery)
	native := runSummapDiffQueryFor(t, fixture, true, summapDiffBareQuery)

	if math.Abs(fanout-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("repeated/unordered bounds: fanout = %v, want %v (see this test's own doc)", fanout, wantMerged)
	}
	if math.Abs(native-wantMerged) > summapDiffFloatTolerance {
		t.Errorf("repeated/unordered bounds: native (sumMap) = %v, want %v (see this test's own doc)", native, wantMerged)
	}
	if math.Abs(fanout-native) > summapDiffFloatTolerance {
		t.Fatalf("repeated/unordered bounds: fanout = %v, native (sumMap) = %v; want equal — the "+
			"sumMap merge is not reproducing the has-filter fold for partially overlapping "+
			"bucket layouts", fanout, native)
	}
}

// TestClassicBucketMergeSumMap_NaNPrimitives pins, directly against real
// ClickHouse, the two primitives classic_bucket_merge_summap.go's header
// relies on for its claim that a NaN reaches the same rungs on both merge
// strategies.
//
// A NaN cannot reach this stage through a legitimate seeded BucketCounts
// (Array(UInt64) has no NaN representation) — it can only arise from
// window-stage floating-point arithmetic — so this pins the ClickHouse
// behaviour independent of cerberus's own SQL generation:
//
//   - arrayCumSum propagates a NaN forward to every LATER element of the
//     array it is summing. That array is now ONE ROW's own buckets, so the
//     NaN poisons that row's rungs at and above the NaN bucket and no
//     others — the same reach classicBucketRowCumulativeExpr's arraySum
//     gives it on the fan-out path.
//   - arrayMax IGNORES a NaN, so the monotonic repair both strategies share
//     answers a poisoned rung with the running maximum either way.
func TestClassicBucketMergeSumMap_NaNPrimitives(t *testing.T) {
	fixture := newChDBFixture(t, `SELECT 1`)

	cumRows := fixture.queryOverEmitted(t, "toString(arrayCumSum([1., nan, 2., 3.]))", "SELECT 1", nil)
	defer func() { _ = cumRows.Close() }()
	if !cumRows.Next() {
		t.Fatal("no rows")
	}
	var gotCum string
	if err := cumRows.Scan(&gotCum); err != nil {
		t.Fatalf("scan: %v", err)
	}
	const wantCum = "[1,nan,nan,nan]"
	if gotCum != wantCum {
		t.Fatalf("arrayCumSum([1, nan, 2, 3]) = %s, want %s — the within-row NaN reach "+
			"classic_bucket_merge_summap.go's header documents no longer holds on this "+
			"ClickHouse version", gotCum, wantCum)
	}

	maxRows := fixture.queryOverEmitted(t, "toString(arrayMax([1., nan, 2.]))", "SELECT 1", nil)
	defer func() { _ = maxRows.Close() }()
	if !maxRows.Next() {
		t.Fatal("no rows")
	}
	var gotMax string
	if err := maxRows.Scan(&gotMax); err != nil {
		t.Fatalf("scan: %v", err)
	}
	const wantMax = "2"
	if gotMax != wantMax {
		t.Fatalf("arrayMax([1, nan, 2]) = %s, want %s — classicBucketMonotonicExpr's prefix "+
			"maximum no longer ignores a NaN, so the two merge strategies' shared repair layer "+
			"needs re-verifying against a poisoned rung", gotMax, wantMax)
	}
}

// TestClassicBucketMergeSumMap_ZeroKeyIdentity pins, directly against real
// ClickHouse, the four -0.0/0.0 primitives that together make the two merge
// strategies agree on a group where one row reports a -0.0 bound and another
// reports 0.0 — the case classic_bucket_merge_summap.go's header covers:
//
//   - sumMap merges -0.0 and 0.0 as ONE aggregate key, so the group holds a
//     single entry for the logical zero bound.
//   - arrayDistinct — the union-bounds dedup classicBucketUnionBoundsExpr
//     applies — does NOT, so the merged LAYOUT carries both rungs. That is
//     the same layout the fan-out path's own union produces from the same
//     rows: the two strategies share this construction verbatim.
//   - indexOf treats -0.0 and 0.0 as equal, so both of those rungs read the
//     ONE sumMap entry — each getting that cumulative count once.
//   - has treats them as equal too, so classicBucketMergedLadderExpr's own
//     filter admits the same rows at both rungs and folds the same value
//     into each.
//
// Reading the same cumulative count at two rungs of one bound is not a
// double count in the cumulative domain — it is what BOTH constructions
// produce, and emitHistogramQuantile's adjacent-duplicate-bound dedup
// collapses it downstream. It WAS a double count for the per-bucket-counts
// shape this feature first shipped, where the shared entry was ADDED once
// per duplicate rung by the union-wide arrayCumSum; the zero-canonicalising
// step that guarded that went away with the arrayCumSum itself.
func TestClassicBucketMergeSumMap_ZeroKeyIdentity(t *testing.T) {
	fixture := newChDBFixture(t, `
CREATE OR REPLACE TABLE zero_key_identity_probe (bounds Array(Float64), counts Array(Float64)) ENGINE = Memory;
INSERT INTO zero_key_identity_probe VALUES ([-0.0, 1.0], [3.0, 4.0]), ([0.0, 1.0], [1.0, 2.0]);
`)

	// TWO rows — one reporting a -0.0-keyed bucket, the other a 0.0-keyed
	// one — is the load-bearing cross-row case: if sumMap treated -0.0 and
	// 0.0 as DISTINCT keys, this would answer keys=[-0,0,1] vals=[3,1,6]
	// (two separate zero-ish keys) and each union rung would resolve to its
	// own partial sum instead of the merged one.
	sumMapRows := fixture.queryOverEmitted(t,
		"toString(sm.1), toString(sm.2)", "SELECT sumMap(bounds, counts) AS sm FROM zero_key_identity_probe", nil)
	defer func() { _ = sumMapRows.Close() }()
	if !sumMapRows.Next() {
		t.Fatal("no rows")
	}
	var keys, vals string
	if err := sumMapRows.Scan(&keys, &vals); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if keys != "[-0,1]" || vals != "[4,6]" {
		t.Fatalf("sumMap over a -0.0-keyed row and a 0.0-keyed row = keys=%s vals=%s, "+
			"want keys=[-0,1] vals=[4,6] (merged into ONE key, summed 3+1=4) — sumMap no longer "+
			"treats -0.0 and 0.0 as one key, so the two union rungs a -0.0/0.0 layout produces no "+
			"longer read one merged entry; classic_bucket_merge_summap.go's header needs revisiting", keys, vals)
	}

	distinctRows := fixture.queryOverEmitted(t, "toString(arrayDistinct([-0.0, 0.0, 1.0]))", "SELECT 1", nil)
	defer func() { _ = distinctRows.Close() }()
	if !distinctRows.Next() {
		t.Fatal("no rows")
	}
	var distinct string
	if err := distinctRows.Scan(&distinct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	const wantDistinct = "[-0,0,1]"
	if distinct != wantDistinct {
		t.Fatalf("arrayDistinct([-0.0, 0.0, 1.0]) = %s, want %s — the union layout a -0.0/0.0 group "+
			"produces has changed shape; both merge strategies build it from this one construction, "+
			"so they still agree, but classic_bucket_merge_summap.go's header needs revisiting",
			distinct, wantDistinct)
	}

	indexOfRows := fixture.queryOverEmitted(t,
		"indexOf([-0.0, 1.0, 2.0], 0.0), indexOf([0.0, 1.0, 2.0], -0.0)", "SELECT 1", nil)
	defer func() { _ = indexOfRows.Close() }()
	if !indexOfRows.Next() {
		t.Fatal("no rows")
	}
	var idxA, idxB int
	if err := indexOfRows.Scan(&idxA, &idxB); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if idxA != 1 || idxB != 1 {
		t.Fatalf("indexOf(-0.0/0.0 cross-lookup) = (%d, %d), want (1, 1) — "+
			"classicBucketSumMapLookupExpr's indexOf-based reconstruction assumes indexOf treats "+
			"-0.0 and 0.0 as equal for lookup purposes", idxA, idxB)
	}

	// has is the fan-out path's own rung filter (classicBucketMergedLadderExpr).
	// It has to agree with indexOf about -0.0/0.0 for the two strategies to
	// admit the same rows at the same rungs.
	hasRows := fixture.queryOverEmitted(t,
		"has([-0.0, 1.0, 2.0], 0.0), has([0.0, 1.0, 2.0], -0.0)", "SELECT 1", nil)
	defer func() { _ = hasRows.Close() }()
	if !hasRows.Next() {
		t.Fatal("no rows")
	}
	var hasA, hasB int
	if err := hasRows.Scan(&hasA, &hasB); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if hasA != 1 || hasB != 1 {
		t.Fatalf("has(-0.0/0.0 cross-lookup) = (%d, %d), want (1, 1) — the fan-out fold's own "+
			"rung filter no longer agrees with indexOf about -0.0/0.0, so the two merge "+
			"strategies would admit different rows at a zero bound", hasA, hasB)
	}
}
