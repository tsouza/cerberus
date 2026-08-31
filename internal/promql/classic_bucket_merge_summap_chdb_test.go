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
//   - TestClassicBucketMergeSumMapDifferential_ZeroBucket pins the same
//     identity for a group whose merged per-bucket count is exactly zero at
//     an interior bound — the sumMap key-drop quirk
//     classic_bucket_merge_summap.go's header documents and works around.
//   - TestClassicBucketMergeSumMapDifferential_Heterogeneous pins the
//     DOCUMENTED, ACCEPTED divergence (issue #2817) for a heterogeneous
//     bucket layout: the two strategies answer DIFFERENTLY, with both
//     values hand-derived from Prometheus's own bucketQuantile algorithm
//     (prometheus/promql/quantile.go) against each strategy's own merged
//     ladder — not a "whatever the code currently does" tolerance.
//   - TestClassicBucketMergeSumMap_ArrayCumSumNaNPropagation and
//     TestClassicBucketMergeSumMap_ZeroKeyIdentity pin the underlying
//     ClickHouse primitives (arrayCumSum, sumMap, arrayDistinct, indexOf)
//     classic_bucket_merge_summap.go's design relies on directly, since a
//     NaN or a -0.0/0.0 identity mismatch cannot be forced through a
//     legitimate seeded query (Array(UInt64) BucketCounts has no NaN
//     representation).
package promql_test

import (
	"context"
	"math"
	"strconv"
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
// sumMap + arrayCumSum merge), both with the OTHER family's fields left at
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
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(summapDiffQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", summapDiffQuery, err)
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

// TestClassicBucketMergeSumMapDifferential_ZeroBucket seeds a SINGLE series
// whose middle bucket's count is exactly zero — the sumMap zero-summed-key
// drop this file's header documents (sumMap([1,2,3],[10,0,5]) drops key 2
// entirely). Both strategies must still answer identically: the fanout
// path never drops the bound, and classicBucketSumMapLookupExpr's
// indexOf-based reconstruction restores the dropped zero explicitly.
func TestClassicBucketMergeSumMapDifferential_ZeroBucket(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [10, 0, 5], [1.0, 2.0, 3.0]);
`)

	fanout := runSummapDiffQuery(t, fixture, false)
	native := runSummapDiffQuery(t, fixture, true)
	if fanout != native {
		t.Fatalf("zero interior bucket: fanout = %v, native (sumMap) = %v; want equal", fanout, native)
	}
}

// TestClassicBucketMergeSumMapDifferential_Heterogeneous seeds two series
// with DISJOINT bucket layouts — the shape classic_bucket_merge_summap.go's
// header and cerberus issue #2817 document as a real, accepted divergence
// (why chopt.FeatureClassicBucketMergeSumMap ships AutoSelect: false).
//
// Both expected values are hand-derived from Prometheus's own
// bucketQuantile algorithm applied to EACH strategy's own merged ladder —
// not a "whatever the code answers today" tolerance:
//
//	Series A: bounds [1,2,3], counts [10,5,0] (equal-length, no overflow).
//	Series B: bounds [1,5],   counts [7,0].
//
//	Fanout (has-filter fold, then monotonic prefix-max repair):
//	  merged ladder over union bounds [1,2,3,5]: rung@1 = 10+7 = 17 (both
//	  series report le=1); rung@2 = 15 (only A reports le=2); rung@3 = 15
//	  (only A); rung@5 = 7 (only B) — raw ladder [17,15,15,7], NOT
//	  monotonic (a real, unrepaired dip: a la carte per-rung has-filtered
//	  sums). +Inf rung = SUM of every row's own total (15 + 7 = 22).
//	  Prefix-max repair over [17,15,15,7,22] = [17,17,17,17,22].
//	  observations = 22, rank = 0.95*22 = 20.9. arrayFirstIndex(c>=20.9) = 5
//	  (length(cum) — the OVERFLOW-rung tie case, since no finite rung
//	  reaches 20.9), so the interpolation clamps to the HIGHEST finite
//	  bound: value = 5.0.
//
//	sumMap + arrayCumSum: per-bucket values keyed by bound, summed
//	  key-wise: key 1 = 10+7 = 17; key 2 = 5 (only A; sumMap keeps it —
//	  nonzero); key 3 = 0 (only A; DROPPED by sumMap's zero-key quirk,
//	  reconstructed back to 0 by the indexOf lookup); key 5 = 0 (only B;
//	  also dropped/reconstructed). Reconstructed per-union-bound values
//	  [17,5,0,0], arrayCumSum = [17,22,22,22] — monotonic BY CONSTRUCTION,
//	  no repair needed. +Inf rung = SUM of every row's own total (15+7=22,
//	  same computation as fanout's). Full ladder [17,22,22,22,22].
//	  observations = 22, rank = 20.9. arrayFirstIndex(c>=20.9) = 2 (cum[1]
//	  = 17 < 20.9, cum[2] = 22 >= 20.9) — NOT the overflow-tie case (idx !=
//	  length(cum) = 5), NOT idx==1. General interpolation: prevBound =
//	  bounds[1] = 1, prevCum = cum[1] = 17, bound = bounds[2] = 2, thisCum
//	  = cum[2] = 22. value = 1 + (2-1) * ((20.9-17)/(22-17)) = 1 + 3.9/5 =
//	  1.78.
func TestClassicBucketMergeSumMapDifferential_Heterogeneous(t *testing.T) {
	fixture := newChDBFixture(t, summapDiffSeedDDL+`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES
    ('`+summapDiffMetric+`', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [10, 5, 0], [1.0, 2.0, 3.0]),
    ('`+summapDiffMetric+`', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [7, 0], [1.0, 5.0]);
`)

	const wantFanout = 5.0
	const wantNative = 1.78
	// floatCompareTolerance absorbs ordinary float64 representation error
	// (1 + 3.9/5 rounds to 1.7799999999999998, not 1.78 exactly) — not a
	// "whatever the code answers" fudge: both wantFanout and wantNative are
	// hand-derived from Prometheus's own bucketQuantile formula (this
	// test's own doc), and 1e-9 is many orders below the ~1e-15 float64
	// rounding this specific arithmetic produces.
	const floatCompareTolerance = 1e-9

	fanout := runSummapDiffQuery(t, fixture, false)
	native := runSummapDiffQuery(t, fixture, true)

	if math.Abs(fanout-wantFanout) > floatCompareTolerance {
		t.Errorf("heterogeneous layout: fanout = %v, want %v (the overflow-rung clamp — see this test's own doc)", fanout, wantFanout)
	}
	if math.Abs(native-wantNative) > floatCompareTolerance {
		t.Errorf("heterogeneous layout: native (sumMap) = %v, want %v (general interpolation — see this test's own doc)", native, wantNative)
	}
	if math.Abs(fanout-native) <= floatCompareTolerance {
		t.Fatal("heterogeneous layout: fanout and native (sumMap) answered IDENTICALLY — " +
			"this test exists to pin their documented divergence (issue #2817); " +
			"if this now passes, the two constructions may have become equivalent and " +
			"chopt.FeatureClassicBucketMergeSumMap's AutoSelect: false posture should be revisited")
	}
}

// TestClassicBucketMergeSumMap_ArrayCumSumNaNPropagation pins, directly
// against real ClickHouse, the NaN-propagation difference
// classic_bucket_merge_summap.go's header documents as an accepted,
// pinned risk rather than a glossed-over one: arrayCumSum propagates a NaN
// forward to EVERY higher rung once it appears, unlike the fanout path's
// has-filter fold, which only poisons the rungs a NaN row's own layout
// carries. A NaN cannot reach this stage through a legitimate seeded
// BucketCounts (Array(UInt64) has no NaN representation) — it can only
// arise from window-stage floating-point arithmetic — so this pins the
// underlying ClickHouse primitive classicBucketSumMapLadderExpr's
// arrayCumSum call relies on directly, independent of cerberus's own SQL
// generation.
func TestClassicBucketMergeSumMap_ArrayCumSumNaNPropagation(t *testing.T) {
	fixture := newChDBFixture(t, `SELECT 1`)
	rows := fixture.queryOverEmitted(t, "toString(arrayCumSum([1., nan, 2., 3.]))", "SELECT 1", nil)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("no rows")
	}
	var got string
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	const want = "[1,nan,nan,nan]"
	if got != want {
		t.Fatalf("arrayCumSum([1, nan, 2, 3]) = %s, want %s — the NaN-propagation guarantee "+
			"classic_bucket_merge_summap.go's header documents no longer holds on this "+
			"ClickHouse version", got, want)
	}
}

// TestClassicBucketMergeSumMap_ZeroKeyIdentity pins, directly against real
// ClickHouse, the two -0.0/0.0 primitives classic_bucket_merge_summap.go's
// header and classicBucketZeroCanonicalExpr's own doc rely on:
//
//   - sumMap merges -0.0 and 0.0 as ONE aggregate key (so canonicalising
//     every row's bound to +0.0 before sumMap sees it cannot ITSELF split
//     one logical bound into two sumMap keys).
//   - arrayDistinct — the union-bounds dedup classicBucketUnionBoundsExpr
//     applies — does NOT (so skipping the canonicalisation would let a
//     -0.0-reporting row and a 0.0-reporting row surface as two distinct
//     union rungs, double-counting whichever sumMap key indexOf resolves
//     both to).
func TestClassicBucketMergeSumMap_ZeroKeyIdentity(t *testing.T) {
	fixture := newChDBFixture(t, `
CREATE OR REPLACE TABLE zero_key_identity_probe (bounds Array(Float64), counts Array(Float64)) ENGINE = Memory;
INSERT INTO zero_key_identity_probe VALUES ([-0.0, 1.0], [3.0, 4.0]), ([0.0, 1.0], [1.0, 2.0]);
`)

	// TWO rows — one reporting a -0.0-keyed bucket, the other a 0.0-keyed
	// one — is the load-bearing cross-row case
	// classicBucketZeroCanonicalExpr's own rationale depends on: if sumMap
	// treated -0.0 and 0.0 as DISTINCT keys, this would answer
	// keys=[-0,0,1] vals=[3,1,6] (two separate zero-ish keys); instead it
	// merges them into ONE key.
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
			"treats -0.0 and 0.0 as one key; classicBucketZeroCanonicalExpr's own rationale for "+
			"canonicalising BEFORE sumMap sees it should be re-verified", keys, vals)
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
		t.Fatalf("arrayDistinct([-0.0, 0.0, 1.0]) = %s, want %s — if arrayDistinct now merges "+
			"-0.0/0.0 too, classicBucketZeroCanonicalExpr's normalisation is no longer load-bearing "+
			"(harmless either way, but its rationale should be re-verified)", distinct, wantDistinct)
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
}
