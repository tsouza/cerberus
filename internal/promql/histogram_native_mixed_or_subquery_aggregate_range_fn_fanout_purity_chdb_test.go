//go:build chdb

// chDB-backed proof of cerberus issue #2715's own fix: the FOLD family's
// window-purity collision-DROP test, rescoped per OUTPUT ANCHOR for a true
// query_range fan-out (no `@` pin) — [lowerSumOrAvgMixedOrSubqueryFoldFnRange].
//
// This is the fan-out analogue of
// histogram_native_mixed_or_subquery_aggregate_range_fn_window_purity_chdb_test.go's
// single-window proof: series "x" seeds a histogram sample at 00:01 and TWO
// gauge samples, at 00:11 and 00:16, inside a thirteen-minute window
// evaluated at two DIFFERENT output anchors five minutes apart:
//
//	00:13's own window (00:00, 00:13] holds the 00:01 histogram sample AND
//	  the 00:11 gauge sample — a genuine collision — so series "x" must be
//	  DROPPED entirely at this anchor.
//	00:18's own window (00:05, 00:18] no longer reaches back to the 00:01
//	  histogram sample (it fell out of range five minutes ago) but holds
//	  BOTH gauge samples (00:11, 00:16) — a pure float window — so series
//	  "x" must fold NORMALLY (a positive value) at this anchor.
//
// A single window-WIDE purity test (this file's own bug before #2715) would
// see the histogram sample AND a gauge sample somewhere in the combined
// range at EVERY anchor and drop series "x" everywhere, including 00:18 —
// this file proves that does NOT happen: the two anchors disagree, which
// only a genuinely PER-ANCHOR purity test can produce.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	fanoutPurityExpHistMetric = "fanout_purity_exp_hist"
	fanoutPurityGaugeMetric   = "fanout_purity_gauge"
)

var fanoutPuritySeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + fanoutPurityExpHistMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 1, 1.0, 0, 0, 0, [1], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + fanoutPurityGaugeMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:11:00', 9), 10.0),\n" +
	"    ('" + fanoutPurityGaugeMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:16:00', 9), 20.0);\n"

func fanoutPurityQuery(fn string) string {
	inner := "sum by (series) ((" + fanoutPurityExpHistMetric + ") or (" + fanoutPurityGaugeMetric + "))"
	return fn + "((" + inner + ")[13m:1m])"
}

// TestSumOrAvgMixedOrSubqueryFanoutPurity_SumOverTime_ChDB proves
// sum_over_time — a window-purity-filtered FOLD-family member — drops
// series "x" at the COLLIDING anchor (00:13) while still folding it to a
// real, positive value at the PURE anchor (00:18), five minutes later. The
// PURE anchor's own exact value is deliberately not pinned here: a
// subquery ticks its inner expression once per its own step (1m) and
// ordinary instant-vector staleness carries each raw gauge sample forward
// across every tick it stays valid for (up to 5m), so sum_over_time's
// window fold is a multiple of 10 and 20 weighted by tick-visibility
// count, not a bare 10+20 — this is genuine, reference-matching subquery
// semantics (see the sibling window-purity file's own count_over_time test
// for the identical carry-forward multiplicity), just not the simplest
// number to hand-verify here. A positive value already proves the anchor
// folded NORMALLY rather than being dropped, which is this test's own
// claim.
func TestSumOrAvgMixedOrSubqueryFanoutPurity_SumOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fanoutPuritySeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 13, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 18, 0, 0, time.UTC)

	sqlStr, args := lowerAndEmitRange(t, fanoutPurityQuery("sum_over_time"), s, start, end, 5*time.Minute)

	// The Mixed OR union underlying combineMixedAggregateBranches widens
	// the SURVIVING side's row to the full fourteen-column contract, so the
	// float winner at the PURE anchor also carries placeholder
	// Histogram*Column fields — subqHistQueryRows reads those unconditionally,
	// with no discriminator filter, so checking it must be scoped to the
	// COLLISION anchor specifically (where NO row of either type survives at
	// all) rather than scanned across every anchor.
	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	if _, present := subqHistRowAtOptional(histRows, "x", start); present {
		t.Errorf("sum_over_time @ %v: series x has a row in the histogram-column projection, want it DROPPED entirely (this anchor's own window holds both a histogram and a float sample)", start)
	}

	got := rangeSampleValueRows(t, fixture, sqlStr, args)
	if _, present := got["x"][start.Unix()]; present {
		t.Errorf("sum_over_time @ %v: series x = %v, want DROPPED (this anchor's own window holds both a histogram and a float sample)", start, got["x"][start.Unix()])
	}
	if got := got["x"][end.Unix()]; got <= 0 {
		t.Errorf("sum_over_time @ %v: series x = %v, want a positive sum (this anchor's own window is float-pure, the histogram sample has aged out)", end, got)
	}
}

// TestSumOrAvgMixedOrSubqueryFanoutPurity_Rate_ChDB is the identical
// per-anchor collision-drop proof for rate, the issue's own second
// FOLD-family evidence shape.
func TestSumOrAvgMixedOrSubqueryFanoutPurity_Rate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fanoutPuritySeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 13, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 18, 0, 0, time.UTC)

	sqlStr, args := lowerAndEmitRange(t, fanoutPurityQuery("rate"), s, start, end, 5*time.Minute)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	if _, present := subqHistRowAtOptional(histRows, "x", start); present {
		t.Errorf("rate @ %v: series x has a row in the histogram-column projection, want it DROPPED entirely (this anchor's own window holds both a histogram and a float sample)", start)
	}

	got := rangeSampleValueRows(t, fixture, sqlStr, args)
	if _, present := got["x"][start.Unix()]; present {
		t.Errorf("rate @ %v: series x = %v, want DROPPED (this anchor's own window holds both a histogram and a float sample)", start, got["x"][start.Unix()])
	}
	if got := got["x"][end.Unix()]; got <= 0 {
		t.Errorf("rate @ %v: series x = %v, want a positive rate (this anchor's own window is float-pure: 10.0 then 20.0, an increase)", end, got)
	}
}
