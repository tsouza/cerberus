//go:build chdb

// chDB-backed proof that `sum`/`avg` [by/without] wrapping a FOLD-family
// range function over a subquery whose own inner is a bare mixed
// float/histogram `or` (cerberus issue #3265 Part 2,
// histogram_native_mixed_or_subquery_outer_aggregate_fold.go) answers
// reference's sum/avg drop-on-collision rule at real ClickHouse execution:
// a `by`/`without` group whose members disagree on value type is dropped
// entirely (including the query-wide default group a bare `sum()`/`avg()`
// with no clause reduces to), while a group whose only member is
// histogram-shaped publishes a REAL, properly-typed histogram reduction —
// never the float placeholder zero the pre-fix fallthrough produced.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	mofHistMetric  = "mof_wrapped_hist_side_exp_hist"
	mofFloatMetric = "mof_wrapped_float_side_gauge"
)

// mofSeed keys two histogram series ("h1" bucket="b1" — collides with f1
// under `by(bucket)`; "h3" bucket="b3" — the ONLY member of its bucket) and
// two float series ("f1" bucket="b1" — collides with h1; "f2" bucket="b2" —
// the ONLY member of its bucket). Each series carries two samples five
// minutes apart so rate()/increase() clear the two-point floor.
var mofSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + mofHistMetric + "', map('series', 'h1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, 0, [1], 0, []),\n" +
	"    ('" + mofHistMetric + "', map('series', 'h1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:02:00', 9), 5, 5.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + mofHistMetric + "', map('series', 'h3', 'bucket', 'b3'), toDateTime64('2026-01-01 00:00:00', 9), 2, 2.0, 0, 0, 0, [2], 0, []),\n" +
	"    ('" + mofHistMetric + "', map('series', 'h3', 'bucket', 'b3'), toDateTime64('2026-01-01 00:02:00', 9), 8, 8.0, 0, 0, 0, [8], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + mofFloatMetric + "', map('series', 'f1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),\n" +
	"    ('" + mofFloatMetric + "', map('series', 'f1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:02:00', 9), 40.0),\n" +
	"    ('" + mofFloatMetric + "', map('series', 'f2', 'bucket', 'b2'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),\n" +
	"    ('" + mofFloatMetric + "', map('series', 'f2', 'bucket', 'b2'), toDateTime64('2026-01-01 00:02:00', 9), 400.0);\n"

var mofEvalTS = time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

// mofRow is one output row read back from an emitted mixed-or-subquery-fold
// aggregate: its grouping label (bucket, or "" for a bare `sum()`), its
// plain Value, and the two histogram payload columns that are non-zero only
// for a genuinely histogram-typed row.
type mofRow struct {
	bucket      string
	val, hc, hs float64
}

// mofRun lowers + emits query and returns every output row, keyed by the
// `bucket` label (empty string when the query carries no `by`/`without`
// clause, since every row then shares Attributes = {}).
func mofRun(t *testing.T, fixture *chdbFixture, query string) map[string]mofRow {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mofEvalTS, mofEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.MixedRowShape)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Attributes`['bucket'] AS bucket, `Value` AS val, `HistogramCount` AS hc, `HistogramSum` AS hs", sqlStr, args)
	defer func() { _ = rows.Close() }()

	got := map[string]mofRow{}
	for rows.Next() {
		var r mofRow
		if err := rows.Scan(&r.bucket, &r.val, &r.hc, &r.hs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[r.bucket] = r
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestSumOverMixedOrSubqueryFoldFn_ChDB_NoByClauseCollisionDrops proves the
// bare `sum()`/`avg()` (no `by`/`without`) shape: every series in the seed
// — two histogram, two float — falls into ONE default group, which
// therefore mixes types and must be dropped entirely (reference's
// MixedFloatsHistogramsAggWarning), not answered with the float side's own
// rate alone (the pre-fix fallthrough's actual behavior).
func TestSumOverMixedOrSubqueryFoldFn_ChDB_NoByClauseCollisionDrops(t *testing.T) {
	fixture := newChDBFixture(t, mofSeed)
	orExpr := "(" + mofHistMetric + " or " + mofFloatMetric + ")"

	for _, query := range []string{
		"sum(rate(" + orExpr + "[5m:1m]))",
		"avg(rate(" + orExpr + "[5m:1m]))",
	} {
		t.Run(query, func(t *testing.T) {
			got := mofRun(t, fixture, query)
			if len(got) != 0 {
				t.Fatalf("query %q: got %d rows %+v, want 0 (the single default group mixes histogram and float samples, reference drops it)", query, len(got), got)
			}
		})
	}
}

// TestSumOverMixedOrSubqueryFoldFn_ChDB_ByBucketPerGroupOutcome proves the
// `by(bucket)` shape's THREE distinct per-group outcomes at once: "b1"
// (h1 + f1, a real collision) is dropped; "b2" (f2 alone) survives as its
// own correctly-computed float rate; "b3" (h3 alone) survives as a REAL,
// properly-typed histogram rate — not the float placeholder zero the
// pre-fix fallthrough rendered it as.
func TestSumOverMixedOrSubqueryFoldFn_ChDB_ByBucketPerGroupOutcome(t *testing.T) {
	fixture := newChDBFixture(t, mofSeed)
	query := "sum by (bucket) (rate((" + mofHistMetric + " or " + mofFloatMetric + ")[5m:1m]))"
	got := mofRun(t, fixture, query)

	if r, ok := got["b1"]; ok {
		t.Errorf("query %q: got a row for bucket %q (%+v), want none (h1 and f1 collide in this group, reference drops it)", query, "b1", r)
	}

	// f2: 100 -> 400 over 120s. The exact extrapolation constant is this
	// package's own pre-existing, separately-tested rate() machinery — not
	// what this test verifies — so the expected value is pinned to that
	// reduction's own real-ClickHouse output (300 / 240s = 1.25) rather than
	// re-derived here; what this test actually discriminates is the TYPE
	// and the per-bucket collision outcome, not the extrapolation formula.
	wantFloatRate := 1.25
	if r, ok := got["b2"]; !ok {
		t.Errorf("query %q: no row for bucket %q, want the float-only rate", query, "b2")
	} else if diff := r.val - wantFloatRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("query %q: bucket %q Value = %v, want %v (f2 alone, a plain float rate)", query, "b2", r.val, wantFloatRate)
	} else if r.hc != 0 || r.hs != 0 {
		t.Errorf("query %q: bucket %q HistogramCount/Sum = %v/%v, want 0/0 (a float-typed row)", query, "b2", r.hc, r.hs)
	}

	// h3: Count 2 -> 8 over 120s, the identical extrapolation shape as f2
	// above (6 / 240s = 0.025) — same reasoning: pinned to this reduction's
	// own real output, not re-derived.
	wantHistRate := 0.025
	if r, ok := got["b3"]; !ok {
		t.Errorf("query %q: no row for bucket %q, want the histogram-only rate", query, "b3")
	} else if diff := r.hc - wantHistRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("query %q: bucket %q HistogramCount = %v, want %v (h3 alone, a real histogram rate — a placeholder-zero bug would report 0 here)", query, "b3", r.hc, wantHistRate)
	} else if r.val != 0 {
		t.Errorf("query %q: bucket %q Value = %v, want 0 (a histogram-typed row's Value is a placeholder)", query, "b3", r.val)
	}

	if len(got) != 2 {
		t.Errorf("query %q: got %d buckets %+v, want exactly 2 (b2, b3)", query, len(got), got)
	}
}
