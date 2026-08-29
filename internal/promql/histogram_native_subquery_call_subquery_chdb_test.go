//go:build chdb

// chDB-backed proof that cerberus issue #2726's doubly-nested subquery
// composition — `<fn>(<inner-sub>)[<outer-range>:<step>]` — actually
// executes correctly against real ClickHouse for a HistogramRowShape /
// MixedRowShape `wideInner`, not merely that the emitted plan's Go shape
// looks right.
//
// # Why innerSub.Expr is a set-op, not a bare selector or rate(...)
//
// [lowerSubquery]'s own dispatch tries [lowerHistogramNativeSubqueryInner]
// FIRST: it treats sub.Expr — here `<fn>(<inner-sub>)` — as if IT were a
// query's own root, and [selectFnOverExpHistogramSubquery] /
// [rangeFnOverExpHistogramSubquery] already recognise EXACTLY that AST
// shape (an outer call directly wrapping a SubqueryExpr) whenever
// call.Func.Name is one of their own names AND innerSub.Expr is
// STATICALLY histogram-valued ([isExpHistogramValuedShape]) — which a
// bare selector or `rate(<selector>[range])` both are. So for any of the
// same 15 SELECT/FOLD names [lowerSubqueryOverCallSubquery] answers, a
// bare-selector or rate(...) innerSub.Expr is intercepted by the
// PRE-EXISTING #2545/#2569 continuations before [lowerSubqueryOverCallSubquery]
// is ever reached — that composition was already correct.
//
// [isExpHistogramValuedShape] does NOT recognise a bare `and`/`or`/`unless`
// set-op (that recognition lives only in [lowerVectorSetOpOperand], reached
// exclusively through the generic lowerBinary → lowerVectorSetOp path —
// see histogram_native_mixed_or_subquery_further_setop_range_fn.go's own
// doc for the identical reasoning one nesting level up), so a
// `(<a>) and/or (<b>)` innerSub.Expr is the shape that genuinely reaches
// [lowerSubqueryOverCallSubquery] with a Histogram/Mixed-shaped wideInner —
// exercising cerberus issue #2726's new code, not its #2545/#2569
// predecessor.
//
// Every case here lowers the doubly-nested SubqueryExpr as the BARE plan
// root (via promql.LowerAt, mirroring TestLowerSubquery_CallNested's own
// convention in subquery_nested_test.go) — this is
// [lowerSubqueryOverCallSubquery]'s own, in-scope, directly-testable
// output: the matrix `sub` denotes, BEFORE any FURTHER outer range-vector
// function reduces it again. A further outer wrap directly consuming that
// matrix without its OWN subquery bracket (`<outerFn2>(<doubly-nested>)`)
// is a separate, harder gap [lowerHistogramOrMixedSubqueryOuterFnInput]'s
// own nestedCallSubqueryShape guard explicitly defers — filed as a
// follow-up rather than fixed here.
package promql_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// callSubqSeedBaseTS is 2026-01-01T00:00:00Z. Every seeded timestamp and
// every test's own eval anchor in this file is expressed relative to it.
var callSubqSeedBaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	callSubqHistAndAMetric   = "call_subq_and_a_exp_hist"
	callSubqHistAndBMetric   = "call_subq_and_b_exp_hist"
	callSubqMixedHistMetric  = "call_subq_mixed_hist_exp_hist"
	callSubqMixedGaugeMetric = "call_subq_mixed_gauge"
)

// callSubqHistAndSeed seeds two exp-histogram metrics sharing series "a",
// each with thirteen samples one minute apart (00:00..00:12). Metric A's
// Count/Sum/Bucket1 grow with time (Count = minute + 1) so "the sample
// published at anchor T" is identifiable by its own Count; metric B exists
// at the SAME instants purely so `(A) and (B)` never drops a row — its own
// values are never read.
func callSubqHistAndSeed() string {
	rows := func(metric string, countOf func(i int) int) string {
		out := ""
		for i := 0; i <= 12; i++ {
			ts := callSubqSeedBaseTS.Add(time.Duration(i) * time.Minute)
			c := countOf(i)
			out += "    ('" + metric + "', map('series', 'a'), toDateTime64('" +
				ts.Format("2006-01-02 15:04:05") + "', 9), " +
				strconv.Itoa(c) + ", " + strconv.Itoa(c) + ".0, 0, 0, 0, [" + strconv.Itoa(c*2) + "], 0, [])"
			if i < 12 {
				out += ",\n"
			} else {
				out += ";\n"
			}
		}
		return out
	}
	return subqHistDDL +
		swapGaugeSeedDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		rows(callSubqHistAndAMetric, func(i int) int { return i + 1 }) +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		rows(callSubqHistAndBMetric, func(int) int { return 1 })
}

// callSubqMixedOrSeed seeds a histogram metric under series "h" and a
// gauge metric under series "g" — disjoint series, so `(hist) or (gauge)`
// never has to resolve a shadow conflict: series "h" is histogram-typed at
// every anchor, series "g" is float-typed at every anchor. Both use the
// same thirteen-samples-one-minute-apart, Count/Value = minute+1 pattern
// as callSubqHistAndSeed.
func callSubqMixedOrSeed() string {
	histRows := ""
	gaugeRows := ""
	for i := 0; i <= 12; i++ {
		ts := callSubqSeedBaseTS.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		c := i + 1
		histRows += "    ('" + callSubqMixedHistMetric + "', map('series', 'h'), toDateTime64('" + ts + "', 9), " +
			strconv.Itoa(c) + ", " + strconv.Itoa(c) + ".0, 0, 0, 0, [" + strconv.Itoa(c*2) + "], 0, [])"
		gaugeRows += "    ('" + callSubqMixedGaugeMetric + "', map('series', 'g'), toDateTime64('" + ts + "', 9), " + strconv.Itoa(c) + ".0)"
		if i < 12 {
			histRows += ",\n"
			gaugeRows += ",\n"
		} else {
			histRows += ";\n"
			gaugeRows += ";\n"
		}
	}
	return subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		histRows +
		swapGaugeSeedDDL +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		gaugeRows
}

// callSubqOuterAnchors are the ten outer-subquery anchors a `[10m:1m]`
// bracket ending at callSubqSeedBaseTS+12m produces: the grid is left-open
// and epoch-aligned, so [evalTS-10m, evalTS] holds 00:03 through 00:12
// (00:02 is the excluded left endpoint) — see
// TestSubqueryCallSubquery_HistAnd_LastOverTime_ChDB's own derivation,
// reused by every case in this file that shares the same [2m:1m]/[10m:1m]
// bracket pair.
func callSubqOuterAnchors() []time.Time {
	out := make([]time.Time, 0, 10)
	for i := 3; i <= 12; i++ {
		out = append(out, callSubqSeedBaseTS.Add(time.Duration(i)*time.Minute))
	}
	return out
}

// TestSubqueryCallSubquery_HistAnd_LastOverTime_ChDB proves the
// SELECT-family dispatch (lowerSelectFnOverCallSubqueryInput) for a
// HistogramRowShape wideInner produced by a bare `and` set-op inner.
func TestSubqueryCallSubquery_HistAnd_LastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "last_over_time(((" + callSubqHistAndAMetric + ") and (" + callSubqHistAndBMetric + "))[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	if got, want := len(rows), len(anchors); got != want {
		t.Fatalf("last_over_time(doubly-nested and): got %d rows, want %d: %+v", got, want, rows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, rows, "a", anchor)
		wantCount := float64(anchor.Sub(callSubqSeedBaseTS)/time.Minute) + 1
		if row.cnt != wantCount || row.sum != wantCount || row.bucket1 != wantCount*2 {
			t.Errorf("anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (metric A's own sample at this anchor, metric B only gates presence)",
				anchor, row.cnt, row.sum, row.bucket1, wantCount, wantCount, wantCount*2)
		}
	}
}

// TestSubqueryCallSubquery_HistAnd_Rate_ChDB proves the FOLD-family
// dispatch (lowerExpHistogramFoldOverCallSubqueryInput) for the same
// HistogramRowShape wideInner: rate folds wideInner's own per-inner-anchor
// histograms into a boundary-corrected rate at each outer anchor instead
// of erroring or dropping to empty.
func TestSubqueryCallSubquery_HistAnd_Rate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "rate(((" + callSubqHistAndAMetric + ") and (" + callSubqHistAndBMetric + "))[3m:1m])[8m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) == 0 {
		t.Fatalf("rate(doubly-nested and): got 0 rows, want a per-outer-anchor rate matrix")
	}
	for _, r := range rows {
		if r.cnt <= 0 {
			t.Errorf("rate(doubly-nested and) anchor %v: Count = %v, want > 0 (a genuine boundary-corrected rate, not an empty/zero drop)", time.Unix(r.ts, 0).UTC(), r.cnt)
		}
	}
}

// TestSubqueryCallSubquery_MixedOr_LastOverTime_ChDB proves the
// MixedRowShape SELECT-family dispatch (lowerMixedLastFirstOverCallSubqueryInput):
// series "h" (histogram-typed at every anchor) keeps publishing its full
// histogram, series "g" (float-typed at every anchor) keeps publishing its
// plain Value — the argMax/argMin selection stays type-coherent per row.
func TestSubqueryCallSubquery_MixedOr_LastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqMixedOrSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "last_over_time(((" + callSubqMixedHistMetric + ") or (" + callSubqMixedGaugeMetric + "))[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	for _, anchor := range anchors {
		row := subqHistRowAt(t, histRows, "h", anchor)
		wantCount := float64(anchor.Sub(callSubqSeedBaseTS)/time.Minute) + 1
		if row.cnt != wantCount || row.sum != wantCount || row.bucket1 != wantCount*2 {
			t.Errorf("series h anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v", anchor, row.cnt, row.sum, row.bucket1, wantCount, wantCount, wantCount*2)
		}
	}

	floatRows := rangeSampleValueRows(t, fixture, sqlStr, args)
	for _, anchor := range anchors {
		want := float64(anchor.Sub(callSubqSeedBaseTS)/time.Minute) + 1
		got, ok := floatRows["g"][anchor.Unix()]
		if !ok {
			t.Errorf("series g anchor %v: no float row found", anchor)
			continue
		}
		if got != want {
			t.Errorf("series g anchor %v: Value = %v, want %v", anchor, got, want)
		}
	}
}

// TestSubqueryCallSubquery_MixedOr_Rate_ChDB proves the MixedRowShape
// FOLD-family dispatch (lowerMixedFoldOverCallSubqueryInput): the
// histogram arm folds through the same window-fold machinery
// lowerExpHistogramFoldOverCallSubqueryInput uses, the float arm folds
// through an ordinary OuterRange-mode RangeWindow, and
// combineMixedAggregateBranches recombines them with stepAligned=true
// (this test is what an incorrect stepAligned=ctx.step>0 — this
// composition's branches are ALWAYS multi-row per outer anchor regardless
// of the ambient query's own instant/range mode — would have broken: a
// false StepAligned drops the anchor timestamp from the recombination's
// own match key, colliding every outer anchor of a series onto one row).
func TestSubqueryCallSubquery_MixedOr_Rate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqMixedOrSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	// A narrower outer window than this file's other cases (4m vs 8m):
	// the Mixed fold recombination folds BOTH arms and unions them, which
	// costs noticeably more chDB time per outer anchor than a single-arm
	// fold — two anchors are enough to prove the recombination doesn't
	// collapse them together.
	query := "rate(((" + callSubqMixedHistMetric + ") or (" + callSubqMixedGaugeMetric + "))[3m:1m])[4m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	// combineMixedAggregateBranches publishes one relation carrying BOTH
	// arms' rows (chplan.VectorSetOp.Mixed's own convention): a float-typed
	// row still carries the histogram columns, as an all-zero placeholder.
	// Reading it through subqHistQueryRows therefore also surfaces series
	// "g"'s own placeholder rows — skip them here; they're asserted for
	// real via rangeSampleValueRows below.
	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	hSeen := map[int64]bool{}
	for _, r := range histRows {
		if r.series != "h" {
			continue
		}
		if r.cnt <= 0 {
			t.Errorf("series h anchor %v: Count = %v, want > 0", time.Unix(r.ts, 0).UTC(), r.cnt)
		}
		if hSeen[r.ts] {
			t.Errorf("series h anchor %v: appears more than once — the recombination collapsed distinct outer anchors onto one row", time.Unix(r.ts, 0).UTC())
		}
		hSeen[r.ts] = true
	}
	if len(hSeen) < 2 {
		t.Errorf("series h: only %d distinct outer anchors, want multiple (one per outer-subquery anchor, not collapsed to one)", len(hSeen))
	}

	floatRows := rangeSampleValueRows(t, fixture, sqlStr, args)
	if len(floatRows["g"]) < 2 {
		t.Errorf("series g: only %d distinct outer anchors, want multiple", len(floatRows["g"]))
	}
	for ts, val := range floatRows["g"] {
		if val <= 0 {
			t.Errorf("series g anchor %v: rate = %v, want > 0", time.Unix(ts, 0).UTC(), val)
		}
	}
}

// TestSubqueryCallSubquery_MixedOr_ResetsChanges_ChDB proves the
// MixedRowShape resets/changes dispatch
// (lowerMixedResetsOrChangesOverCallSubqueryInput): both series are
// monotonically increasing (histogram Count, gauge Value), so resets()
// must find no counter reset while changes() must count every consecutive
// pair as a genuine value change, for either series' own type.
func TestSubqueryCallSubquery_MixedOr_ResetsChanges_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqMixedOrSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	resetsSQL, resetsArgs := lowerAndEmit(t, "resets((("+callSubqMixedHistMetric+") or ("+callSubqMixedGaugeMetric+"))[3m:1m])[8m:1m]", s, evalTS)
	got := rangeSampleValueRows(t, fixture, resetsSQL, resetsArgs)
	for series, byTS := range got {
		for ts, val := range byTS {
			if val != 0 {
				t.Errorf("resets: series %s anchor %v = %v, want 0 (monotonically increasing, no counter reset)", series, time.Unix(ts, 0).UTC(), val)
			}
		}
	}
	if len(got["h"]) == 0 || len(got["g"]) == 0 {
		t.Fatalf("resets: missing rows for one or both series: %+v", got)
	}

	changesSQL, changesArgs := lowerAndEmit(t, "changes((("+callSubqMixedHistMetric+") or ("+callSubqMixedGaugeMetric+"))[3m:1m])[8m:1m]", s, evalTS)
	got = rangeSampleValueRows(t, fixture, changesSQL, changesArgs)
	for series, byTS := range got {
		for ts, val := range byTS {
			if val <= 0 {
				t.Errorf("changes: series %s anchor %v = %v, want > 0 (every consecutive pair differs)", series, time.Unix(ts, 0).UTC(), val)
			}
		}
	}
	if len(got["h"]) == 0 || len(got["g"]) == 0 {
		t.Fatalf("changes: missing rows for one or both series: %+v", got)
	}
}

// TestSubqueryCallSubquery_HistAnd_FirstOverTime_ChDB proves the
// SELECT-family dispatch for first_over_time — the EARLIEST, not latest,
// in-window sample — over a HistogramRowShape wideInner. Every 2m/1m
// window at outer anchor T holds exactly wideInner's two samples at
// (T-1m) and T, so first_over_time must read the (T-1m) one.
func TestSubqueryCallSubquery_HistAnd_FirstOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "first_over_time(((" + callSubqHistAndAMetric + ") and (" + callSubqHistAndBMetric + "))[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	if got, want := len(rows), len(anchors); got != want {
		t.Fatalf("first_over_time(doubly-nested and): got %d rows, want %d: %+v", got, want, rows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, rows, "a", anchor)
		j := float64(anchor.Sub(callSubqSeedBaseTS) / time.Minute)
		if row.cnt != j || row.sum != j || row.bucket1 != j*2 {
			t.Errorf("anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (the earliest sample in the 2m window, one minute before this anchor — last_over_time's own sibling test pins the OTHER endpoint)",
				anchor, row.cnt, row.sum, row.bucket1, j, j, j*2)
		}
	}
}

// TestSubqueryCallSubquery_HistAnd_CountPresentOverTime_ChDB proves the
// SELECT-family dispatch for count_over_time / present_over_time over a
// HistogramRowShape wideInner: every one of this file's 2m/1m windows
// contains exactly two of wideInner's own per-inner-anchor rows (spaced
// one minute apart, epoch-aligned), so count_over_time must read exactly
// 2 and present_over_time exactly 1 at every one of the ten outer
// anchors — not the empty/dropped answer this composition produced
// before cerberus issue #2726.
func TestSubqueryCallSubquery_HistAnd_CountPresentOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)
	anchors := callSubqOuterAnchors()

	countSQL, countArgs := lowerAndEmit(t, "count_over_time((("+callSubqHistAndAMetric+") and ("+callSubqHistAndBMetric+"))[2m:1m])[10m:1m]", s, evalTS)
	countRows := rangeSampleValueRows(t, fixture, countSQL, countArgs)
	for _, anchor := range anchors {
		got, ok := countRows["a"][anchor.Unix()]
		if !ok {
			t.Fatalf("count_over_time: no row for anchor %v: %+v", anchor, countRows)
		}
		if got != 2 {
			t.Errorf("count_over_time anchor %v: got %v, want 2 (exactly two 1-minute-spaced samples in every 2m window)", anchor, got)
		}
	}

	presentSQL, presentArgs := lowerAndEmit(t, "present_over_time((("+callSubqHistAndAMetric+") and ("+callSubqHistAndBMetric+"))[2m:1m])[10m:1m]", s, evalTS)
	presentRows := rangeSampleValueRows(t, fixture, presentSQL, presentArgs)
	for _, anchor := range anchors {
		got, ok := presentRows["a"][anchor.Unix()]
		if !ok {
			t.Fatalf("present_over_time: no row for anchor %v: %+v", anchor, presentRows)
		}
		if got != 1 {
			t.Errorf("present_over_time anchor %v: got %v, want 1", anchor, got)
		}
	}
}

// TestSubqueryCallSubquery_HistAnd_SumAvgOverTime_ChDB proves the
// FOLD-family dispatch for sum_over_time / avg_over_time — pure additive
// folds with no boundary/reset correction, unlike rate's — over a
// HistogramRowShape wideInner. Every 2m/1m window at outer anchor T
// holds exactly wideInner's two samples at (T-1m) and T (Count = T and
// T+1 respectively, in the seed's own minute-index-plus-one pattern), so
// both functions' results are exact arithmetic, not merely non-empty.
func TestSubqueryCallSubquery_HistAnd_SumAvgOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)
	anchors := callSubqOuterAnchors()

	sumSQL, sumArgs := lowerAndEmit(t, "sum_over_time((("+callSubqHistAndAMetric+") and ("+callSubqHistAndBMetric+"))[2m:1m])[10m:1m]", s, evalTS)
	sumRows := subqHistQueryRows(t, fixture, sumSQL, sumArgs)
	if got, want := len(sumRows), len(anchors); got != want {
		t.Fatalf("sum_over_time(doubly-nested and): got %d rows, want %d: %+v", got, want, sumRows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, sumRows, "a", anchor)
		j := float64(anchor.Sub(callSubqSeedBaseTS) / time.Minute)
		want := 2*j + 1
		if row.cnt != want || row.sum != want || row.bucket1 != want*2 {
			t.Errorf("sum_over_time anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (sum of the window's two samples)",
				anchor, row.cnt, row.sum, row.bucket1, want, want, want*2)
		}
	}

	avgSQL, avgArgs := lowerAndEmit(t, "avg_over_time((("+callSubqHistAndAMetric+") and ("+callSubqHistAndBMetric+"))[2m:1m])[10m:1m]", s, evalTS)
	avgRows := subqHistQueryRows(t, fixture, avgSQL, avgArgs)
	if got, want := len(avgRows), len(anchors); got != want {
		t.Fatalf("avg_over_time(doubly-nested and): got %d rows, want %d: %+v", got, want, avgRows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, avgRows, "a", anchor)
		j := float64(anchor.Sub(callSubqSeedBaseTS) / time.Minute)
		want := (2*j + 1) / 2
		if row.cnt != want || row.sum != want || row.bucket1 != want*2 {
			t.Errorf("avg_over_time anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (average of the window's two samples)",
				anchor, row.cnt, row.sum, row.bucket1, want, want, want*2)
		}
	}
}
