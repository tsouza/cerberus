//go:build chdb

// chDB-backed proof that cerberus issue #2728's TRIPLE-nested composition
// — `<outer-fn2>(<fn>(<inner-sub>)[<outer-range>:<step>])`, an outer
// range-vector function wrapping #2726's doubly-nested subquery directly
// with no bracket of its own — executes correctly against real
// ClickHouse, for both the HistogramRowShape and MixedRowShape MID
// relation and across all three grid modes (instant, `@`-pinned under
// query_range, true query_range fan-out).
//
// # The oracle
//
// Every case here reads the MID relation — the doubly-nested subquery
// `rate(<inner>[3m:1m])[4m:1m]` on its OWN, which cerberus issue #2726
// already proved correct and which
// histogram_native_subquery_call_subquery_chdb_test.go already covers —
// straight out of chDB, and then asserts that the triple-nested query's
// answer is exactly the enclosing function's reduction OF THOSE ROWS,
// summed / counted in Go. That is what "<outer-fn2> reduces the range
// vector sub denotes" means operationally, and it needs no independent
// re-derivation of rate's own extrapolation arithmetic to be a real
// assertion: if the reduction silently re-folded an already-folded
// value, or read a mis-widened anchor grid, the two sides disagree.
package promql_test

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// outerFn2BaseTS is 2026-01-01T00:00:00Z; every seeded timestamp and
// every eval anchor in this file is expressed relative to it.
var outerFn2BaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	outerFn2HistMetric  = "outer_fn2_hist_exp_hist"
	outerFn2GaugeMetric = "outer_fn2_gauge"
	outerFn2GateMetric  = "outer_fn2_gate"

	// outerFn2SeedSamples is the number of one-minute-apart samples each
	// seeded series carries, at 00:00 … 00:12 inclusive.
	outerFn2SeedSamples = 13

	// outerFn2MidBracket is the doubly-nested composition cerberus issue
	// #2726 answers, and outerFn2OuterAnchors is how many anchors its
	// `[4m:1m]` bracket materialises at a given eval instant: the grid is
	// epoch-aligned and left-OPEN, so a 4-minute window at 1-minute
	// spacing holds exactly four.
	outerFn2MidBracket   = "[3m:1m])[4m:1m]"
	outerFn2OuterAnchors = 4
)

// outerFn2HistInner is the `and`-forwarded shape whose MID relation
// resolves HistogramRowShape: the gate metric shares the histogram's own
// series key, so the `and` keeps every histogram row and forwards its
// thirteen-column shape (cerberus issue #2589's and/unless forwarding).
// A PURE `(hist) and (hist)` inner would not reach the triple-nesting arm
// at all — an older root-level recognizer
// ([isExpHistogramValuedShape]'s expHistogramSetOp case) statically
// recognises a two-histogram set op and intercepts the whole query first.
func outerFn2HistInner() string {
	return "((" + outerFn2HistMetric + ") and (" + outerFn2GateMetric + "))"
}

// outerFn2MixedInner is the mixed `or` whose MID relation resolves
// MixedRowShape: series "h" is histogram-typed at every anchor, series
// "g" is float-typed at every anchor, and the two never collide.
func outerFn2MixedInner() string {
	return "((" + outerFn2HistMetric + ") or (" + outerFn2GaugeMetric + "))"
}

// outerFn2Seed seeds three metrics, all thirteen samples one minute
// apart at 00:00 … 00:12, all rising by one per minute so `rate` over any
// window is a well-defined positive number:
//
//   - outerFn2HistMetric   — exp-histogram, series "h", Count = Sum = minute+1.
//   - outerFn2GaugeMetric  — gauge, series "g", Value = minute+1.
//   - outerFn2GateMetric   — gauge, series "h" (the histogram's OWN key),
//     present purely so `(hist) and (gate)` keeps every histogram row.
func outerFn2Seed() string {
	histRows, gaugeRows, gateRows := "", "", ""
	for i := 0; i < outerFn2SeedSamples; i++ {
		ts := outerFn2BaseTS.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		c := strconv.Itoa(i + 1)
		sep := ",\n"
		if i == outerFn2SeedSamples-1 {
			sep = ";\n"
		}
		histRows += "    ('" + outerFn2HistMetric + "', map('series', 'h'), toDateTime64('" + ts + "', 9), " +
			c + ", " + c + ".0, 0, 0, 0, [" + strconv.Itoa((i+1)*2) + "], 0, [])" + sep
		gaugeRows += "    ('" + outerFn2GaugeMetric + "', map('series', 'g'), toDateTime64('" + ts + "', 9), " + c + ".0)" + sep
		gateRows += "    ('" + outerFn2GateMetric + "', map('series', 'h'), toDateTime64('" + ts + "', 9), " + c + ".0)" + sep
	}
	return subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		histRows +
		swapGaugeSeedDDL +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" + gaugeRows +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" + gateRows
}

// outerFn2Row is one row of a Histogram- or Mixed-shaped relation, read
// through a single projection so a case that needs both the float Value
// and the histogram payload costs ONE chDB execution rather than two.
type outerFn2Row struct {
	series string
	ts     int64
	value  float64
	cnt    float64
	sum    float64
}

const outerFn2Projection = "`Attributes`['series'] AS series, toUnixTimestamp(`TimeUnix`) AS ts, " +
	"`Value` AS value, `HistogramCount` AS cnt, `HistogramSum` AS sum"

func outerFn2Rows(t *testing.T, fixture *chdbFixture, sqlStr string, args []any) map[string][]outerFn2Row {
	t.Helper()
	rows := fixture.queryOverEmitted(t, outerFn2Projection, sqlStr, args)
	defer func() { _ = rows.Close() }()
	out := map[string][]outerFn2Row{}
	for rows.Next() {
		var r outerFn2Row
		if err := rows.Scan(&r.series, &r.ts, &r.value, &r.cnt, &r.sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[r.series] = append(out[r.series], r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// outerFn2Mid reads the MID relation's own per-(series, anchor) rows —
// the oracle every assertion below reduces. Its shape is the
// doubly-nested subquery cerberus issue #2726 already proved correct.
func outerFn2Mid(t *testing.T, fixture *chdbFixture, inner string, evalTS time.Time) map[string][]outerFn2Row {
	t.Helper()
	sqlStr, args := lowerAndEmit(t, "rate("+inner+outerFn2MidBracket, schema.DefaultOTelMetrics(), evalTS)
	return outerFn2Rows(t, fixture, sqlStr, args)
}

// outerFn2CloseEnough compares two Float64 sums that ClickHouse and Go
// accumulated in a different order.
func outerFn2CloseEnough(got, want float64) bool {
	const rel = 1e-9
	return math.Abs(got-want) <= rel*math.Max(1, math.Abs(want))
}

// TestOuterFn2_Histogram_ChDB proves the triple nesting over a
// HistogramRowShape MID: count_over_time counts exactly the MID's own
// outer-subquery anchors, and sum_over_time (the FOLD family, which
// re-folds the already-folded per-anchor histograms) sums exactly those
// anchors' Count/Sum.
func TestOuterFn2_Histogram_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, outerFn2Seed())
	s := schema.DefaultOTelMetrics()
	evalTS := outerFn2BaseTS.Add(12 * time.Minute)
	inner := outerFn2HistInner()
	mid := outerFn2Mid(t, fixture, inner, evalTS)
	if got := len(mid["h"]); got != outerFn2OuterAnchors {
		t.Fatalf("MID relation: series h has %d outer anchors, want %d — the oracle itself is wrong", got, outerFn2OuterAnchors)
	}

	countSQL, countArgs := lowerAndEmit(t, "count_over_time(rate("+inner+outerFn2MidBracket+")", s, evalTS)
	counts := rangeSampleValueRows(t, fixture, countSQL, countArgs)
	if len(counts["h"]) != 1 {
		t.Fatalf("count_over_time: series h returned %d rows, want exactly 1 (instant eval): %+v", len(counts["h"]), counts)
	}
	for _, got := range counts["h"] {
		if want := float64(len(mid["h"])); got != want {
			t.Errorf("count_over_time = %v, want %v (one per MID outer anchor)", got, want)
		}
	}

	sumSQL, sumArgs := lowerAndEmit(t, "sum_over_time(rate("+inner+outerFn2MidBracket+")", s, evalTS)
	sumRows := outerFn2Rows(t, fixture, sumSQL, sumArgs)
	if len(sumRows["h"]) != 1 {
		t.Fatalf("sum_over_time: series h returned %d rows, want exactly 1 (instant eval): %+v", len(sumRows["h"]), sumRows)
	}
	var wantCount, wantSum float64
	for _, r := range mid["h"] {
		wantCount += r.cnt
		wantSum += r.sum
	}
	got := sumRows["h"][0]
	if !outerFn2CloseEnough(got.cnt, wantCount) || !outerFn2CloseEnough(got.sum, wantSum) {
		t.Errorf("sum_over_time: got Count=%v Sum=%v, want %v/%v (the sum of the MID's own %d per-anchor rates)",
			got.cnt, got.sum, wantCount, wantSum, len(mid["h"]))
	}
}

// TestOuterFn2_Mixed_ChDB is the MixedRowShape sibling: the FOLD family
// splits the MID by its discriminator, folds each half through the
// unchanged single-type continuation and recombines — the composition
// that used to emit a 549KB, past-max_ast_elements query before
// [combineMixedAggregateBranches] became a single-reference node.
func TestOuterFn2_Mixed_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, outerFn2Seed())
	s := schema.DefaultOTelMetrics()
	evalTS := outerFn2BaseTS.Add(12 * time.Minute)
	inner := outerFn2MixedInner()
	mid := outerFn2Mid(t, fixture, inner, evalTS)
	for _, series := range []string{"h", "g"} {
		if got := len(mid[series]); got != outerFn2OuterAnchors {
			t.Fatalf("MID relation: series %s has %d outer anchors, want %d — the oracle itself is wrong", series, got, outerFn2OuterAnchors)
		}
	}

	countSQL, countArgs := lowerAndEmit(t, "count_over_time(rate("+inner+outerFn2MidBracket+")", s, evalTS)
	counts := rangeSampleValueRows(t, fixture, countSQL, countArgs)
	for _, series := range []string{"h", "g"} {
		if len(counts[series]) != 1 {
			t.Fatalf("count_over_time: series %s returned %d rows, want exactly 1: %+v", series, len(counts[series]), counts)
		}
		for _, got := range counts[series] {
			if want := float64(len(mid[series])); got != want {
				t.Errorf("count_over_time series %s = %v, want %v (one per MID outer anchor)", series, got, want)
			}
		}
	}

	// The histogram half re-folds through the histogram window-fold
	// machinery; the float half through an ordinary RangeWindow. Both
	// halves' answers, and the recombination that keeps them apart, are
	// asserted against the same MID oracle.
	var wantCount, wantSum, wantFloat float64
	for _, r := range mid["h"] {
		wantCount += r.cnt
		wantSum += r.sum
	}
	for _, r := range mid["g"] {
		wantFloat += r.value
	}
	sumSQL, sumArgs := lowerAndEmit(t, "sum_over_time(rate("+inner+outerFn2MidBracket+")", s, evalTS)
	sums := outerFn2Rows(t, fixture, sumSQL, sumArgs)
	if len(sums["h"]) != 1 || len(sums["g"]) != 1 {
		t.Fatalf("sum_over_time: got %d rows for series h and %d for series g, want exactly 1 each: %+v", len(sums["h"]), len(sums["g"]), sums)
	}
	if gotH := sums["h"][0]; !outerFn2CloseEnough(gotH.cnt, wantCount) || !outerFn2CloseEnough(gotH.sum, wantSum) {
		t.Errorf("sum_over_time series h: got Count=%v Sum=%v, want %v/%v (the sum of the MID's own %d per-anchor rates)",
			gotH.cnt, gotH.sum, wantCount, wantSum, len(mid["h"]))
	}
	if gotG := sums["g"][0]; !outerFn2CloseEnough(gotG.value, wantFloat) {
		t.Errorf("sum_over_time series g: got Value=%v, want %v (the sum of the MID's own %d per-anchor rates)",
			gotG.value, wantFloat, len(mid["g"]))
	}
}

// TestOuterFn2_Pinned_ChDB proves the `@`-pinned grid mode: lowered
// through query_range over a window that holds NO seeded data at all,
// with the real eval instant pinned via `@`, every output step must
// carry the instant answer — the pin has to reach both the MID's own
// outer-subquery grid AND wideInner's grid underneath it, and the
// ambient query_range window must never be consulted.
func TestOuterFn2_Pinned_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, outerFn2Seed())
	s := schema.DefaultOTelMetrics()
	evalTS := outerFn2BaseTS.Add(12 * time.Minute)
	inner := outerFn2MixedInner()
	mid := outerFn2Mid(t, fixture, inner, evalTS)

	var wantCount float64
	for _, r := range mid["h"] {
		wantCount += r.cnt
	}
	if wantCount <= 0 {
		t.Fatalf("MID relation: series h summed Count = %v, want > 0 — the oracle itself is wrong", wantCount)
	}

	// A query_range window 999 hours past every seeded sample: without
	// the pin winning, every step reduces an empty window.
	wrongStart := evalTS.Add(999 * time.Hour)
	wrongEnd := wrongStart.Add(3 * time.Minute)
	query := "sum_over_time(rate(" + inner + "[3m:1m])[4m:1m] @ " + strconv.FormatInt(evalTS.Unix(), 10) + ")"
	sqlStr, args := lowerAndEmitRange(t, query, s, wrongStart, wrongEnd, time.Minute)

	seen := map[int64]bool{}
	for _, r := range outerFn2Rows(t, fixture, sqlStr, args)["h"] {
		seen[r.ts] = true
		if !outerFn2CloseEnough(r.cnt, wantCount) {
			t.Errorf("pinned step %v: Count = %v, want %v (the pinned instant answer, broadcast unchanged)",
				time.Unix(r.ts, 0).UTC(), r.cnt, wantCount)
		}
	}
	// [wrongStart, wrongEnd] at 1m spacing, end-inclusive.
	if want := int(wrongEnd.Sub(wrongStart)/time.Minute) + 1; len(seen) != want {
		t.Errorf("pinned: series h landed on %d distinct steps, want %d (one per query_range step)", len(seen), want)
	}
}

// TestOuterFn2_RangeFanout_ChDB proves the true query_range fan-out: each
// output step reduces its OWN `[4m:1m]` window of MID anchors, which is
// only possible if [widenNestedCallSubqueryInner] widened the MID's
// outer-subquery grid across `[ctx.start - 4m, ctx.end]` first. Without
// that widening the MID stays anchored on `ctx.end` alone and every step
// but the last reduces an empty window.
func TestOuterFn2_RangeFanout_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, outerFn2Seed())
	s := schema.DefaultOTelMetrics()
	inner := outerFn2MixedInner()
	// Every step's own `(t-4m, t]` window, and every MID anchor's own
	// `(t-3m, t]` inner window under it, lies inside the seeded
	// 00:00…00:12 span, so every step sees a FULL four-anchor window.
	start := outerFn2BaseTS.Add(9 * time.Minute)
	end := outerFn2BaseTS.Add(12 * time.Minute)
	steps := int(end.Sub(start)/time.Minute) + 1

	countSQL, countArgs := lowerAndEmitRange(t, "count_over_time(rate("+inner+outerFn2MidBracket+")", s, start, end, time.Minute)
	counts := rangeSampleValueRows(t, fixture, countSQL, countArgs)
	for _, series := range []string{"h", "g"} {
		if len(counts[series]) != steps {
			t.Fatalf("count_over_time series %s: %d steps, want %d: %+v", series, len(counts[series]), steps, counts[series])
		}
		for ts, got := range counts[series] {
			if got != outerFn2OuterAnchors {
				t.Errorf("count_over_time series %s step %v = %v, want %v (a full `[4m:1m]` window of MID anchors)",
					series, time.Unix(ts, 0).UTC(), got, outerFn2OuterAnchors)
			}
		}
	}

	// The gauge rises by exactly one per minute, so every MID anchor's
	// rate is the same constant and every step's four-anchor sum is the
	// same positive number. A step whose window lost anchors to a
	// mis-widened grid would break the equality, not merely the sign.
	sumSQL, sumArgs := lowerAndEmitRange(t, "sum_over_time(rate("+inner+outerFn2MidBracket+")", s, start, end, time.Minute)
	sums := rangeSampleValueRows(t, fixture, sumSQL, sumArgs)
	if len(sums["g"]) != steps {
		t.Fatalf("sum_over_time series g: %d steps, want %d: %+v", len(sums["g"]), steps, sums["g"])
	}
	var first float64
	var firstTS int64
	for ts, v := range sums["g"] {
		if firstTS == 0 || ts < firstTS {
			firstTS, first = ts, v
		}
	}
	if first <= 0 {
		t.Fatalf("sum_over_time series g step %v = %v, want > 0 (a rising gauge has a positive rate)", time.Unix(firstTS, 0).UTC(), first)
	}
	for ts, got := range sums["g"] {
		if !outerFn2CloseEnough(got, first) {
			t.Errorf("sum_over_time series g step %v = %v, want %v (identical at every step over a constant-slope gauge)",
				time.Unix(ts, 0).UTC(), got, first)
		}
	}
}
