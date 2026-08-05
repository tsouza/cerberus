package promql

import (
	"math"
	"testing"

	"github.com/tsouza/cerberus/test/property"
)

// This file covers oracle surface the property runs exercise only when
// the generator happens to draw the shape, and which the hand-written
// oracle_test.go never pinned: the set operators, the absent/clamp/sort
// family, the quantile aggregator, the counter-shape window functions,
// unary negation, and the out-of-domain histogram_quantile arms.
//
// Every assertion states the Prometheus semantics the oracle claims to
// mirror, so a divergence introduced in the oracle fails here rather
// than showing up as an unexplained property-test diff against chDB.

// =================================================================
// Set operators — and / or / unless
// =================================================================

func TestSetOp_And_KeepsOnlyMatchedLHSRows(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("a", map[string]string{"job": "db"}, sampleSpec{60, 2}),
		makeSeries("b", map[string]string{"job": "api"}, sampleSpec{60, 99}),
	)
	// `and` keeps the LHS row's own VALUE, not the RHS's.
	assertRows(t, eval(d, `a and b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
	})
}

func TestSetOp_Unless_KeepsOnlyUnmatchedLHSRows(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("a", map[string]string{"job": "db"}, sampleSpec{60, 2}),
		makeSeries("b", map[string]string{"job": "api"}, sampleSpec{60, 99}),
	)
	assertRows(t, eval(d, `a unless b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "db"}, 2),
	})
}

func TestSetOp_Or_LHSWinsOnCollision(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("b", map[string]string{"job": "api"}, sampleSpec{60, 99}),
		makeSeries("b", map[string]string{"job": "db"}, sampleSpec{60, 7}),
	)
	// `or` emits every LHS row, then only the RHS rows whose key is
	// absent from the LHS — the api row must keep the LHS value 1.
	assertRows(t, eval(d, `a or b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
		row(map[string]string{"job": "db"}, 7),
	})
}

func TestSetOp_And_OnClauseNarrowsTheMatchKey(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 1}),
		makeSeries("b", map[string]string{"job": "api", "instance": "y"}, sampleSpec{60, 2}),
	)
	// Full label sets differ, so a bare `and` matches nothing...
	assertRows(t, eval(d, `a and b`, 90), nil)
	// ...but on(job) makes them the same key, and the surviving row
	// keeps its FULL label set (set ops never reshape labels).
	assertRows(t, eval(d, `a and on(job) b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api", "instance": "x"}, 1),
	})
}

func TestSetOp_Unless_IgnoringClause(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 1}),
		makeSeries("b", map[string]string{"job": "api", "instance": "y"}, sampleSpec{60, 2}),
	)
	// ignoring(instance) collapses both to {job=api}, so the LHS row is
	// excluded; without it, nothing matches and the row survives.
	assertRows(t, eval(d, `a unless ignoring(instance) b`, 90), nil)
	assertRows(t, eval(d, `a unless b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api", "instance": "x"}, 1),
	})
}

func TestSetOp_EmptyRHS(t *testing.T) {
	d := build(makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 1}))
	assertRows(t, eval(d, `a and b`, 90), nil)
	assertRows(t, eval(d, `a unless b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
	})
	assertRows(t, eval(d, `a or b`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
	})
}

// =================================================================
// absent()
// =================================================================

func TestAbsent_EmptyInnerEmitsMatcherLabels(t *testing.T) {
	d := build(makeSeries("other", map[string]string{"job": "api"}, sampleSpec{60, 1}))
	// The equality matchers (minus __name__) become the output labels;
	// the value is 1.
	assertRows(t, eval(d, `absent(up{job="api"})`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
	})
}

func TestAbsent_NonEmptyInnerEmitsNothing(t *testing.T) {
	d := build(makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 5}))
	assertRows(t, eval(d, `absent(up{job="api"})`, 90), nil)
}

func TestAbsent_NonEqualityMatchersAreNotCarried(t *testing.T) {
	d := build()
	// Only MatchEqual matchers become labels — a regex matcher does not
	// name a single value, so Prom drops it.
	assertRows(t, eval(d, `absent(up{job=~"a.*",env="prod"})`, 90), []property.OutcomeRow{
		row(map[string]string{"env": "prod"}, 1),
	})
}

// =================================================================
// clamp / clamp_min / clamp_max
// =================================================================

func TestClamp_BoundsBothEnds(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "lo"}, sampleSpec{60, -5}),
		makeSeries("m", map[string]string{"s": "mid"}, sampleSpec{60, 3}),
		makeSeries("m", map[string]string{"s": "hi"}, sampleSpec{60, 11}),
	)
	assertRows(t, eval(d, `clamp(m, 0, 10)`, 90), []property.OutcomeRow{
		row(map[string]string{"s": "lo"}, 0),
		row(map[string]string{"s": "mid"}, 3),
		row(map[string]string{"s": "hi"}, 10),
	})
}

func TestClamp_MaxBelowMinIsEmpty(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 3}))
	// Prom's documented degenerate case: max < min yields no samples.
	assertRows(t, eval(d, `clamp(m, 10, 0)`, 90), nil)
}

func TestClampMinAndMax(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "lo"}, sampleSpec{60, -5}),
		makeSeries("m", map[string]string{"s": "hi"}, sampleSpec{60, 11}),
	)
	assertRows(t, eval(d, `clamp_min(m, 0)`, 90), []property.OutcomeRow{
		row(map[string]string{"s": "lo"}, 0),
		row(map[string]string{"s": "hi"}, 11),
	})
	assertRows(t, eval(d, `clamp_max(m, 10)`, 90), []property.OutcomeRow{
		row(map[string]string{"s": "lo"}, -5),
		row(map[string]string{"s": "hi"}, 10),
	})
}

func TestClamp_NonScalarBoundIsAnError(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 3}))
	// clamp's bounds are scalars; a vector there is a type error, not a
	// silently-ignored argument.
	o := eval(d, `clamp_min(m, m)`, 90)
	if o.Err == nil {
		t.Fatalf("clamp_min with a vector bound returned rows %v, want an error", o.Rows)
	}
}

// =================================================================
// sort / sort_desc
// =================================================================

func TestSort_PreservesTheRowSet(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 3}),
		makeSeries("m", map[string]string{"s": "b"}, sampleSpec{60, 1}),
	)
	want := []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 3),
		row(map[string]string{"s": "b"}, 1),
	}
	// The comparator groups by label set, so sort()/sort_desc() must not
	// add, drop, or alter any row — only the (unobserved) array order.
	assertRows(t, eval(d, `sort(m)`, 90), want)
	assertRows(t, eval(d, `sort_desc(m)`, 90), want)
}

// =================================================================
// quantile() aggregator
// =================================================================

func TestAgg_Quantile_InterpolatesAcrossSeries(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 1}),
		makeSeries("m", map[string]string{"s": "b"}, sampleSpec{60, 2}),
		makeSeries("m", map[string]string{"s": "c"}, sampleSpec{60, 3}),
		makeSeries("m", map[string]string{"s": "d"}, sampleSpec{60, 4}),
	)
	// rank = phi*(n-1) = 0.5*3 = 1.5 → halfway between 2 and 3.
	assertRows(t, eval(d, `quantile(0.5, m)`, 90), []property.OutcomeRow{
		row(map[string]string{}, 2.5),
	})
	assertRows(t, eval(d, `quantile(0, m)`, 90), []property.OutcomeRow{
		row(map[string]string{}, 1),
	})
	assertRows(t, eval(d, `quantile(1, m)`, 90), []property.OutcomeRow{
		row(map[string]string{}, 4),
	})
}

func TestAgg_Quantile_By(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"job": "api", "s": "a"}, sampleSpec{60, 1}),
		makeSeries("m", map[string]string{"job": "api", "s": "b"}, sampleSpec{60, 3}),
		makeSeries("m", map[string]string{"job": "db", "s": "c"}, sampleSpec{60, 10}),
	)
	assertRows(t, eval(d, `quantile(0.5, m) by (job)`, 90), []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 2),
		row(map[string]string{"job": "db"}, 10),
	})
}

func TestAgg_Quantile_OutOfDomainPhi(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 1}),
		makeSeries("m", map[string]string{"s": "b"}, sampleSpec{60, 4}),
	)
	// Prom's documented out-of-domain results: -Inf below 0, +Inf above 1.
	o := eval(d, `quantile(-0.5, m)`, 90)
	if len(o.Rows) != 1 || !math.IsInf(o.Rows[0].Value, -1) {
		t.Fatalf("quantile(-0.5) = %v, want a single -Inf row", o.Rows)
	}
	o = eval(d, `quantile(1.5, m)`, 90)
	if len(o.Rows) != 1 || !math.IsInf(o.Rows[0].Value, 1) {
		t.Fatalf("quantile(1.5) = %v, want a single +Inf row", o.Rows)
	}
	o = eval(d, `quantile(NaN, m)`, 90)
	if len(o.Rows) != 1 || !math.IsNaN(o.Rows[0].Value) {
		t.Fatalf("quantile(NaN) = %v, want a single NaN row", o.Rows)
	}
}

// =================================================================
// Window functions the generator draws rarely
// =================================================================

func TestFn_ChangesOverWindow(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"},
		sampleSpec{10, 1}, sampleSpec{20, 2}, sampleSpec{30, 2}, sampleSpec{40, 5}))
	// Three transitions are possible; the repeated 2 is not one of them, and
	// the very FIRST pair (1→2) counts — the walk starts at index 1, not 2.
	assertRows(t, eval(d, `changes(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 2),
	})
}

func TestFn_ChangesOverWindow_ConsecutiveNaNsAreUnchanged(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"},
		sampleSpec{10, 1}, sampleSpec{20, math.NaN()},
		sampleSpec{30, math.NaN()}, sampleSpec{40, 2}))
	// NaN != NaN in IEEE terms, but Prom explicitly guards that pair, so
	// only 1→NaN and NaN→2 count.
	assertRows(t, eval(d, `changes(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 2),
	})
}

func TestFn_ResetsOverWindow(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"},
		sampleSpec{10, 5}, sampleSpec{20, 5}, sampleSpec{30, 6},
		sampleSpec{40, 1}, sampleSpec{50, 2}, sampleSpec{55, 0}))
	// Two drops (6→1, 2→0). A FLAT step (5→5) is not a reset — the
	// comparison is strictly-less, not less-or-equal — and a rising step
	// never is.
	assertRows(t, eval(d, `resets(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 2),
	})
}

func TestFn_StddevAndStdvarOverTime(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"},
		sampleSpec{10, 2}, sampleSpec{20, 4}, sampleSpec{30, 4},
		sampleSpec{40, 4}, sampleSpec{50, 5}, sampleSpec{55, 5}))
	// Population variance of {2,4,4,4,5,5} is 1 (mean 4), so stddev is 1.
	assertRows(t, eval(d, `stdvar_over_time(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 1),
	})
	assertRows(t, eval(d, `stddev_over_time(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 1),
	})
}

func TestFn_StdvarOverTime_SinglePointIsZero(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"}, sampleSpec{10, 7}))
	assertRows(t, eval(d, `stdvar_over_time(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 0),
	})
}

func TestFn_LastOverTime_TakesTheNewestSample(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"},
		sampleSpec{10, 1}, sampleSpec{50, 9}))
	assertRows(t, eval(d, `last_over_time(m[5m])`, 60), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 9),
	})
}

// =================================================================
// Unary operators
// =================================================================

func TestUnary_NegateVectorFlipsEverySample(t *testing.T) {
	d := build(
		makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 3}),
		makeSeries("m", map[string]string{"s": "b"}, sampleSpec{60, -4}),
	)
	assertRows(t, eval(d, `-m`, 90), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, -3),
		row(map[string]string{"s": "b"}, 4),
	})
}

func TestUnary_PlusIsIdentity(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"s": "a"}, sampleSpec{60, 3}))
	// `+x` is a literal no-op — same row, same value, sign untouched.
	assertRows(t, eval(d, `+m`, 90), []property.OutcomeRow{
		row(map[string]string{"s": "a"}, 3),
	})
}

func TestUnary_NegateScalar(t *testing.T) {
	d := build()
	assertRows(t, eval(d, `-(2 + 3)`, 90), []property.OutcomeRow{
		row(map[string]string{}, -5),
	})
}

// =================================================================
// histogram_quantile out-of-domain phi
// =================================================================

func TestHistogram_Quantile_PhiOutOfDomainEmitsOneRowPerSeries(t *testing.T) {
	d := build(
		makeSeries("h_bucket", map[string]string{"job": "api", "le": "1"}, sampleSpec{60, 1}),
		makeSeries("h_bucket", map[string]string{"job": "api", "le": "+Inf"}, sampleSpec{60, 2}),
		makeSeries("h_bucket", map[string]string{"job": "db", "le": "1"}, sampleSpec{60, 3}),
		makeSeries("h_bucket", map[string]string{"job": "db", "le": "+Inf"}, sampleSpec{60, 4}),
	)
	// Out-of-domain phi collapses to a constant per histogram series —
	// grouped by the non-`le` labels, so two rows, not four.
	o := eval(d, `histogram_quantile(-1, h_bucket)`, 90)
	if len(o.Rows) != 2 {
		t.Fatalf("phi<0: want 2 rows (one per job), got %v", o.Rows)
	}
	for _, r := range o.Rows {
		if !math.IsInf(r.Value, -1) {
			t.Errorf("phi<0 row %v: want -Inf, got %g", r.Labels, r.Value)
		}
		if _, ok := r.Labels["le"]; ok {
			t.Errorf("le label survived into the output: %v", r.Labels)
		}
	}
	o = eval(d, `histogram_quantile(2, h_bucket)`, 90)
	if len(o.Rows) != 2 {
		t.Fatalf("phi>1: want 2 rows, got %v", o.Rows)
	}
	for _, r := range o.Rows {
		if !math.IsInf(r.Value, 1) {
			t.Errorf("phi>1 row %v: want +Inf, got %g", r.Labels, r.Value)
		}
	}
}

// =================================================================
// Series identity
// =================================================================

func TestSeries_NameAndSeriesKeyExcludeMetricName(t *testing.T) {
	d := build(makeSeries("m", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 1}))
	model := FromDataset(d)
	if len(model.Series) != 1 {
		t.Fatalf("want 1 series, got %d", len(model.Series))
	}
	s := model.Series[0]
	if s.Name() != "m" {
		t.Fatalf("Name() = %q, want %q", s.Name(), "m")
	}
	// SeriesKey is the join/group identity: it must not include the
	// metric name, or `a + b` would never match.
	key := s.SeriesKey()
	if key == "" {
		t.Fatal("SeriesKey() is empty for a labelled series")
	}
	if got := (&Series{Labels: map[string]string{"job": "api", "instance": "x"}}).SeriesKey(); got != key {
		t.Fatalf("SeriesKey() = %q with __name__, %q without — the metric name leaked into the identity", key, got)
	}
	if got := (&Series{Labels: map[string]string{}}).Name(); got != "" {
		t.Fatalf("Name() on an unnamed series = %q, want empty", got)
	}
}
