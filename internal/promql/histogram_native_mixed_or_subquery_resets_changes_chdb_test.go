//go:build chdb

// chDB-backed proof that `resets`/`changes` over a sum/avg-wrapped mixed
// float/histogram `or` subquery — cerberus issue #2615's second gap,
// histogram_native_mixed_or_subquery_resets_changes.go — actually merges
// the window's float and histogram samples by timestamp and counts a type
// FLIP as an automatic reset/change, matching reference Prometheus's own
// funcResets/funcChanges, at real ClickHouse execution.
//
// Every case seeds ONE series ("m") with FOUR samples spaced 6 minutes
// apart — alternating histogram, histogram, float, float — so each
// sample's own 5-minute staleness lookback has expired by the time the
// NEXT sample appears (otherwise an EARLIER sample would still be
// "present" via ordinary instant-vector staleness carry-forward at a
// LATER subquery anchor, colliding with the newer sample of the other
// arm and triggering [combineMixedAggregateBranches]'s per-anchor
// collision drop instead of the clean type-flip this file means to
// prove):
//
//	00:00  H1  Count=2 Sum=4  Bucket1=6    (histogram)
//	00:06  H2  Count=3 Sum=9  Bucket1=12   (histogram)
//	00:12  G1  Value=5.0                   (float)
//	00:18  G2  Value=3.0                   (float)
//
// Hand-computed against reference's own three-way switch
// (histogram_native_mixed_or_subquery_resets_changes.go's own top-level
// doc transcribes it) over the full four-sample, three-pair window:
//
//	pair(H1,H2): hist/hist — Count 2→3, no bucket/schema regression, so
//	  DetectReset is FALSE (not a reset); Count differs, so Equals is
//	  FALSE (a change).
//	pair(H2,G1): a type FLIP — always a reset AND a change.
//	pair(G1,G2): float/float — 3.0 < 5.0, so a VALUE DECREASE (a reset);
//	  3.0 != 5.0 (a change).
//
//	resets = 0 + 1 + 1 = 2
//	changes = 1 + 1 + 1 = 3
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	mrcMixedExpHistMetric = "mrc_mixed_exp_hist"
	mrcMixedGaugeMetric   = "mrc_mixed_gauge"
)

// mrcMixedOrSeed is this file's own seed — see the file-level doc for the
// exact four samples and why they are spaced 6 minutes apart.
var mrcMixedOrSeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + mrcMixedExpHistMetric + "', map('series', 'm'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mrcMixedExpHistMetric + "', map('series', 'm'), toDateTime64('2026-01-01 00:06:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + mrcMixedGaugeMetric + "', map('series', 'm'), toDateTime64('2026-01-01 00:12:00', 9), 5.0),\n" +
	"    ('" + mrcMixedGaugeMetric + "', map('series', 'm'), toDateTime64('2026-01-01 00:18:00', 9), 3.0);\n"

// mrcMixedOrQuery builds `<fn>((sum by (series) ((h) or (g)))[19m:6m])` —
// the issue's own trigger shape, this file's own metric names and a
// 19-minute (not 18 — see subqueryGridCtx's own left-open window doc)
// window so the subquery's own inner anchor grid includes the 00:00
// sample despite the window's exclusive lower bound.
func mrcMixedOrQuery(fn string) string {
	inner := "sum by (series) ((" + mrcMixedExpHistMetric + ") or (" + mrcMixedGaugeMetric + "))"
	return fn + "((" + inner + ")[19m:6m])"
}

var mrcMixedOrEvalTS = time.Date(2026, 1, 1, 0, 18, 0, 0, time.UTC)

// TestMixedOrSubqueryResetsChanges_Instant_ChDB proves the instant-mode
// composition — this file's own top-level doc's hand-computed resets=2 /
// changes=3 over the full four-sample window.
func TestMixedOrSubqueryResetsChanges_Instant_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mrcMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, mrcMixedOrQuery("resets"), s, mrcMixedOrEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["m"] != 2 {
		t.Errorf("resets: series m = %v, want 2 (0 hist/hist + 1 flip + 1 float decrease)", got["m"])
	}

	sqlStr, args = lowerAndEmit(t, mrcMixedOrQuery("changes"), s, mrcMixedOrEvalTS)
	got = sampleValueRows(t, fixture, sqlStr, args)
	if got["m"] != 3 {
		t.Errorf("changes: series m = %v, want 3 (1 hist/hist + 1 flip + 1 float differ)", got["m"])
	}
}

// TestMixedOrSubqueryResetsChanges_PinnedBroadcast_ChDB proves the
// `@`-pinned query_range broadcast mode: the SAME single window (ending
// at 00:18:00, this file's own eval instant) is evaluated once and
// broadcast across every output step, so both output rows below report
// the identical instant-mode answer.
func TestMixedOrSubqueryResetsChanges_PinnedBroadcast_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mrcMixedOrSeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 12, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 18, 0, 0, time.UTC)

	pinnedQuery := func(fn string) string {
		inner := "sum by (series) ((" + mrcMixedExpHistMetric + ") or (" + mrcMixedGaugeMetric + "))"
		return fn + "((" + inner + ")[19m:6m] @ end())"
	}

	sqlStr, args := lowerAndEmitRange(t, pinnedQuery("resets"), s, start, end, 6*time.Minute)
	rows := rangeSampleValueRows(t, fixture, sqlStr, args)
	for _, ts := range []time.Time{start, end} {
		if got := rows["m"][ts.Unix()]; got != 2 {
			t.Errorf("resets @ %v: series m = %v, want 2 (broadcast of the instant answer)", ts, got)
		}
	}

	sqlStr, args = lowerAndEmitRange(t, pinnedQuery("changes"), s, start, end, 6*time.Minute)
	rows = rangeSampleValueRows(t, fixture, sqlStr, args)
	for _, ts := range []time.Time{start, end} {
		if got := rows["m"][ts.Unix()]; got != 3 {
			t.Errorf("changes @ %v: series m = %v, want 3 (broadcast of the instant answer)", ts, got)
		}
	}
}

// TestMixedOrSubqueryResetsChanges_TrueFanout_ChDB proves the true
// query_range fan-out mode — no `@` pin — evaluates each output step's
// own [19m] window independently, unlike this file's FOLD-family sibling
// (which still rejects that shape entirely, cerberus issue #2615's third
// gap): output step 00:18:00 sees the full four-sample window this file's
// own doc hand-computes (resets=2, changes=3), while output step
// 00:24:00's OWN independent window has slid past the 00:00 histogram
// sample entirely (it is more than 19 minutes stale by 00:24:00) and
// sees only H2/G1/G2 — a DIFFERENT answer (resets=2, changes=2) that a
// broadcast implementation could not produce, proving this is a genuine
// per-anchor fan-out and not a repeat of one shared value.
func TestMixedOrSubqueryResetsChanges_TrueFanout_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mrcMixedOrSeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 18, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 24, 0, 0, time.UTC)

	sqlStr, args := lowerAndEmitRange(t, mrcMixedOrQuery("resets"), s, start, end, 6*time.Minute)
	rows := rangeSampleValueRows(t, fixture, sqlStr, args)
	if got := rows["m"][start.Unix()]; got != 2 {
		t.Errorf("resets @ %v: series m = %v, want 2 (full four-sample window)", start, got)
	}
	if got := rows["m"][end.Unix()]; got != 2 {
		t.Errorf("resets @ %v: series m = %v, want 2 (H2,G1,G2 window: 1 flip + 1 float decrease)", end, got)
	}

	sqlStr, args = lowerAndEmitRange(t, mrcMixedOrQuery("changes"), s, start, end, 6*time.Minute)
	rows = rangeSampleValueRows(t, fixture, sqlStr, args)
	if got := rows["m"][start.Unix()]; got != 3 {
		t.Errorf("changes @ %v: series m = %v, want 3 (full four-sample window)", start, got)
	}
	if got := rows["m"][end.Unix()]; got != 2 {
		t.Errorf("changes @ %v: series m = %v, want 2 (H2,G1,G2 window: 1 flip + 1 float differ)", end, got)
	}
}

// lowerAndEmitRange / rangeSampleValueRows are this file's own
// query_range analogues of paren_range_vector_test.go's [lowerAndEmit]
// and subquery_select_histogram_chdb_test.go's [sampleValueRows]: the
// former lowers through [promql.LowerAtRange] instead of [promql.LowerAt],
// the latter reads back the per-step sample quartet keyed by (series,
// step timestamp) instead of collapsing every step onto one value per
// series.
func lowerAndEmitRange(t *testing.T, query string, s schema.Metrics, start, end time.Time, step time.Duration) (string, []any) {
	t.Helper()
	expr := parseExprExp(t, query)
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr, args
}

func rangeSampleValueRows(t *testing.T, fixture *chdbFixture, sqlStr string, args []any) map[string]map[int64]float64 {
	t.Helper()
	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, toUnixTimestamp(`TimeUnix`) AS ts, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	out := map[string]map[int64]float64{}
	for rows.Next() {
		var series string
		var ts int64
		var val float64
		if err := rows.Scan(&series, &ts, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if out[series] == nil {
			out[series] = map[int64]float64{}
		}
		out[series][ts] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
