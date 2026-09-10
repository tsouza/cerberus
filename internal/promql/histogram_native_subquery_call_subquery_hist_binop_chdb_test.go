//go:build chdb

// chDB-backed coverage for the HistogramRowShape half of cerberus issue
// #2726's doubly-nested composition — the three
// `shape == chplan.HistogramRowShape` arms of
// [lowerHistogramOrMixedCallSubqueryInput], plus
// [lowerSelectFnOverCallSubqueryInput]'s last/first and resets/changes
// branches and [lowerExpHistogramFoldOverCallSubqueryInput]'s direct,
// non-Mixed use.
//
// histogram_native_subquery_call_subquery_chdb_test.go's own `HistAnd`
// cases were written to cover them and do not: a pure-histogram `and`/`or`
// set-op inner is claimed by [isExpHistogramValuedShape] (cerberus issue
// #2324) and answered by the single-level continuation, so
// [lowerSubqueryOverCallSubquery] is never reached for one. Cerberus issue
// #3253 measured that; this file is the coverage it was missing.
//
// The inner used here is a NON-default-matching exp-histogram binop,
// `(<a>) + on(series) (<b>)`. That is the shape that genuinely arrives:
// [isExpHistogramValuedShape] gates its `<hist> + <hist>` arm on default
// matching, so `on(...)` is withheld from every single-level recognizer,
// while [lowerExpHistogramHistogramBinop] lowers it to a HistogramRowShape
// relation all the same. The default-lane routing pins in
// histogram_native_subquery_call_subquery_routing_test.go assert exactly
// that split for all fifteen names.
package promql_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// callSubqHistBinopInner is `(A + on(series) B)` over the two metrics
// [callSubqHistAndSeed] already seeds under series "a": A's Count / Sum
// are minute+1 and its single positive bucket is 2*(minute+1); B's are
// the constant 1 and 2. The `+` merge is therefore exact arithmetic —
// Count = Sum = minute+2, bucket = 2*minute+4 — with no rescaling to
// reason about, because both operands carry Scale 0, PositiveOffset 0 and
// a one-element bucket array.
func callSubqHistBinopInner() string {
	return "((" + callSubqHistAndAMetric + ") + on(series) (" + callSubqHistAndBMetric + "))"
}

// callSubqHistBinopMerged is the merged (Count, Sum, bucket1) triple at
// the sample published `minute` minutes after [callSubqSeedBaseTS].
func callSubqHistBinopMerged(minute int) (count, sum, bucket1 float64) {
	return float64(minute + 2), float64(minute + 2), float64(2*minute + 4)
}

// TestSubqueryCallSubquery_HistBinop_LastOverTime_ChDB exercises
// [lowerSelectFnOverCallSubqueryInput]'s last_over_time branch on a
// HistogramRowShape wideInner: the newest merged histogram in each outer
// anchor's own 2m window, which for this seed is the merge published AT
// that anchor.
func TestSubqueryCallSubquery_HistBinop_LastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "last_over_time(" + callSubqHistBinopInner() + "[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	if got, want := len(rows), len(anchors); got != want {
		t.Fatalf("last_over_time(doubly-nested hist binop): got %d rows, want %d: %+v", got, want, rows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, rows, "a", anchor)
		minute := int(anchor.Sub(callSubqSeedBaseTS) / time.Minute)
		wantCount, wantSum, wantBucket := callSubqHistBinopMerged(minute)
		if row.cnt != wantCount || row.sum != wantSum || row.bucket1 != wantBucket {
			t.Errorf("anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (A's own sample at this anchor merged with B's constant one)",
				anchor, row.cnt, row.sum, row.bucket1, wantCount, wantSum, wantBucket)
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_FirstOverTime_ChDB is the same arm
// read from the other end: first_over_time must select the OLDEST merge
// in the window, which for a 2m/1m bracket at outer anchor T is the one
// published at T-1m. Without it, a last/first mix-up in the directional
// aggregate set would pass the test above.
func TestSubqueryCallSubquery_HistBinop_FirstOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "first_over_time(" + callSubqHistBinopInner() + "[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	if got, want := len(rows), len(anchors); got != want {
		t.Fatalf("first_over_time(doubly-nested hist binop): got %d rows, want %d: %+v", got, want, rows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, rows, "a", anchor)
		minute := int(anchor.Sub(callSubqSeedBaseTS)/time.Minute) - 1
		wantCount, wantSum, wantBucket := callSubqHistBinopMerged(minute)
		if row.cnt != wantCount || row.sum != wantSum || row.bucket1 != wantBucket {
			t.Errorf("anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (the T-1m merge, not the T one)",
				anchor, row.cnt, row.sum, row.bucket1, wantCount, wantSum, wantBucket)
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_SumOverTime_ChDB exercises
// [lowerExpHistogramFoldOverCallSubqueryInput] directly — the FOLD-family
// arm — with the one fold whose answer is exact arithmetic rather than a
// boundary extrapolation: sum_over_time adds the two merges the 2m/1m
// window holds, at (T-1m) and T.
func TestSubqueryCallSubquery_HistBinop_SumOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "sum_over_time(" + callSubqHistBinopInner() + "[2m:1m])[10m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	anchors := callSubqOuterAnchors()
	if got, want := len(rows), len(anchors); got != want {
		t.Fatalf("sum_over_time(doubly-nested hist binop): got %d rows, want %d: %+v", got, want, rows)
	}
	for _, anchor := range anchors {
		row := subqHistRowAt(t, rows, "a", anchor)
		minute := int(anchor.Sub(callSubqSeedBaseTS) / time.Minute)
		prevCount, prevSum, prevBucket := callSubqHistBinopMerged(minute - 1)
		lastCount, lastSum, lastBucket := callSubqHistBinopMerged(minute)
		wantCount, wantSum, wantBucket := prevCount+lastCount, prevSum+lastSum, prevBucket+lastBucket
		if row.cnt != wantCount || row.sum != wantSum || row.bucket1 != wantBucket {
			t.Errorf("anchor %v: got Count=%v Sum=%v Bucket1=%v, want %v/%v/%v (the two in-window merges added)",
				anchor, row.cnt, row.sum, row.bucket1, wantCount, wantSum, wantBucket)
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_Rate_ChDB exercises the same
// FOLD-family arm through rate, whose boundary extrapolation this file
// declines to hand-derive. What it does pin is that the fold ran on
// genuine per-inner-anchor merges rather than erroring or dropping to
// empty: the merged counter rises by exactly one Count per minute, so
// every outer anchor's rate must be strictly positive and — since the
// seed is perfectly linear — equal at every anchor.
func TestSubqueryCallSubquery_HistBinop_Rate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)

	query := "rate(" + callSubqHistBinopInner() + "[3m:1m])[8m:1m]"
	sqlStr, args := lowerAndEmit(t, query, s, evalTS)

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) == 0 {
		t.Fatalf("rate(doubly-nested hist binop): got 0 rows, want a per-outer-anchor rate matrix")
	}
	first := rows[0]
	for _, r := range rows {
		if r.cnt <= 0 {
			t.Errorf("rate anchor %v: Count = %v, want > 0 (a genuine boundary-corrected rate, not an empty/zero drop)",
				time.Unix(r.ts, 0).UTC(), r.cnt)
		}
		if r.cnt != first.cnt {
			t.Errorf("rate anchor %v: Count = %v, want %v — the seed rises by exactly one Count per minute at every anchor, so every window's rate is the same",
				time.Unix(r.ts, 0).UTC(), r.cnt, first.cnt)
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_ResetsChanges_ChDB exercises
// [lowerSelectFnOverCallSubqueryInput]'s resets/changes branch on a
// HistogramRowShape wideInner. The merged counter is monotonically
// increasing, so every 2m/1m window's two consecutive samples give
// resets = 0 and changes = 1.
func TestSubqueryCallSubquery_HistBinop_ResetsChanges_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)
	anchors := callSubqOuterAnchors()

	for _, tc := range []struct {
		fn   string
		want float64
		why  string
	}{
		{"resets", 0, "monotonically increasing merged Count, no counter reset"},
		{"changes", 1, "exactly one change between the window's two consecutive samples"},
	} {
		sqlStr, args := lowerAndEmit(t, tc.fn+"("+callSubqHistBinopInner()+"[2m:1m])[10m:1m]", s, evalTS)
		got := rangeSampleValueRows(t, fixture, sqlStr, args)
		for _, anchor := range anchors {
			v, ok := got["a"][anchor.Unix()]
			if !ok {
				t.Fatalf("%s: no row for anchor %v: %+v", tc.fn, anchor, got)
			}
			if v != tc.want {
				t.Errorf("%s anchor %v: got %v, want %v (%s)", tc.fn, anchor, v, tc.want, tc.why)
			}
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_CountPresentOverTime_ChDB exercises
// the four fully type-blind SELECT names on a HistogramRowShape
// wideInner. Every 2m/1m window holds exactly two of wideInner's own
// per-inner-anchor merges, so count_over_time reads 2 and
// present_over_time reads 1 at every outer anchor.
func TestSubqueryCallSubquery_HistBinop_CountPresentOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)
	anchors := callSubqOuterAnchors()

	for _, tc := range []struct {
		fn   string
		want float64
	}{
		{"count_over_time", 2},
		{"present_over_time", 1},
	} {
		sqlStr, args := lowerAndEmit(t, tc.fn+"("+callSubqHistBinopInner()+"[2m:1m])[10m:1m]", s, evalTS)
		got := rangeSampleValueRows(t, fixture, sqlStr, args)
		for _, anchor := range anchors {
			v, ok := got["a"][anchor.Unix()]
			if !ok {
				t.Fatalf("%s: no row for anchor %v: %+v", tc.fn, anchor, got)
			}
			if v != tc.want {
				t.Errorf("%s anchor %v: got %v, want %v", tc.fn, anchor, v, tc.want)
			}
		}
	}
}

// TestSubqueryCallSubquery_HistBinop_TsOfFirstLastOverTime_ChDB exercises
// the two ts_of_* names on a HistogramRowShape wideInner: each reports
// the UNIX-second timestamp of the earliest / latest merge in the outer
// anchor's window, which for a 2m/1m bracket is (T-1m) / T.
func TestSubqueryCallSubquery_HistBinop_TsOfFirstLastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, callSubqHistAndSeed())
	s := schema.DefaultOTelMetrics()
	evalTS := callSubqSeedBaseTS.Add(12 * time.Minute)
	anchors := callSubqOuterAnchors()

	for _, tc := range []struct {
		fn    string
		shift time.Duration
	}{
		{"ts_of_first_over_time", -time.Minute},
		{"ts_of_last_over_time", 0},
	} {
		sqlStr, args := lowerAndEmit(t, tc.fn+"("+callSubqHistBinopInner()+"[2m:1m])[10m:1m]", s, evalTS)
		got := rangeSampleValueRows(t, fixture, sqlStr, args)
		for _, anchor := range anchors {
			v, ok := got["a"][anchor.Unix()]
			if !ok {
				t.Fatalf("%s: no row for anchor %v: %+v", tc.fn, anchor, got)
			}
			want := float64(anchor.Add(tc.shift).Unix())
			if v != want {
				t.Errorf("%s anchor %v: got %v, want %v (%s)", tc.fn, anchor, v, want,
					strconv.FormatInt(anchor.Add(tc.shift).Unix(), 10))
			}
		}
	}
}
