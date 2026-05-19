package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestRangeWindowMetricsExplicitTimeGrid exercises the matrix-shape
// emission with an explicit Start / End grid (the shape the
// /api/metrics/query_range handler will produce). Confirms that:
//
//   - the anchor count is computed from (End-Start)/Step + 1, not
//     OuterRange (which is zero here);
//   - the anchor base is a DateTime64 literal, not now64();
//   - the rate reducer divides through range_seconds.
func TestRangeWindowMetricsExplicitTimeGrid(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Anchor base: explicit DateTime64 literal at end.
	if !strings.Contains(sql, "toDateTime64('2026-05-13 12:05:00.000000000', 9)") {
		t.Errorf("expected DateTime64 anchor base, SQL=%s", sql)
	}
	// 5-minute span / 1-minute step = 6 anchors (end-inclusive).
	if !strings.Contains(sql, "range(0, 6)") {
		t.Errorf("expected range(0, 6), SQL=%s", sql)
	}
	// Rate reducer normalises by range_seconds (60s); the count(?)
	// aggregate is wrapped in toFloat64 so the Value column has the
	// uniform Float64 wire type chclient.Sample.Value expects (UInt64
	// → *float64 ScanRow conversion is unsupported by the CH Go
	// driver). The substring is "toFloat64(count(?)) / 60".
	if !strings.Contains(sql, "toFloat64(count(?)) / 60") {
		t.Errorf("expected `toFloat64(count(?)) / 60`, SQL=%s", sql)
	}
	// args has the LitInt{1} bound by count(1).
	if len(args) != 1 {
		t.Fatalf("expected 1 arg (count operand), got %d: %v", len(args), args)
	}
	if v, ok := args[0].(int64); !ok || v != 1 {
		t.Errorf("expected args[0] = int64(1), got %T(%v)", args[0], args[0])
	}
}

// TestRangeWindowMetricsLeftOpenWindow pins the per-anchor bucket
// boundary: cerberus emits `ts > anchor_ts - toIntervalNanosecond(...)`
// (not `ts >=`) for the matrix-path WHERE clause, so the per-anchor
// window is left-open / right-closed — matching Tempo upstream's
// `IntervalMapperQueryRange.interval` semantics
// (`(start, start+step], (start+step, start+2*step], …`).
//
// Without the strict `>` a sample landing exactly on a step boundary
// gets counted in TWO adjacent anchors, surfacing as a per-anchor
// off-by-one against Tempo's reference counts (`metrics_count_over_time_*`
// + `metrics_rate_*` cases in the Tempo compat suite — fixed by the
// commit this test guards).
func TestRangeWindowMetricsLeftOpenWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   chplan.MetricsOp
		attr chplan.Expr
		q    []float64
	}{
		{name: "rate", op: chplan.MetricsOpRate},
		{name: "count_over_time", op: chplan.MetricsOpCountOverTime},
		{name: "sum_over_time", op: chplan.MetricsOpSumOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "avg_over_time", op: chplan.MetricsOpAvgOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "quantile_over_time", op: chplan.MetricsOpQuantileOverTime, attr: &chplan.ColumnRef{Name: "Duration"}, q: []float64{0.95}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         c.op,
					Attr:       c.attr,
					Quantiles:  c.q,
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit %s: %v", c.name, err)
			}
			// Lower bound must be strict (`>`, not `>=`) so a sample at
			// exactly anchor_ts - range belongs only to the previous
			// anchor, not also to this one.
			if !strings.Contains(sql, "ts > anchor_ts - toIntervalNanosecond(") {
				t.Errorf("%s: expected strict lower bound `ts > anchor_ts - toIntervalNanosecond(...)`; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, "ts >= anchor_ts - toIntervalNanosecond(") {
				t.Errorf("%s: lower bound must be strict (`>`), not inclusive (`>=`); SQL=%s", c.name, sql)
			}
			// Upper bound stays right-closed (anchor_ts is included).
			if !strings.Contains(sql, "ts <= anchor_ts") {
				t.Errorf("%s: expected right-closed upper bound `ts <= anchor_ts`; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestRangeWindowMetricsRejectsZeroStep guards the matrix path's
// Step > 0 invariant — without it the inner arrayJoin range would
// divide by zero.
func TestRangeWindowMetricsRejectsZeroStep(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		TimestampColumn: "Timestamp",
		// Step zero — should error.
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for Step=0, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestRangeWindowMetricsRejectsBadStartEnd guards against End < Start
// in the explicit-grid path; the resulting anchor count would be
// negative.
func TestRangeWindowMetricsRejectsBadStartEnd(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for End < Start, got nil")
	}
}

// TestMetricsAggregateRequiresAttr surfaces the chplan-level invariant
// that the *_over_time / quantile_over_time ops carry an Attr operand.
func TestMetricsAggregateRequiresAttr(t *testing.T) {
	t.Parallel()

	cases := []chplan.MetricsOp{
		chplan.MetricsOpSumOverTime,
		chplan.MetricsOpAvgOverTime,
		chplan.MetricsOpMinOverTime,
		chplan.MetricsOpMaxOverTime,
		chplan.MetricsOpQuantileOverTime,
	}
	for _, op := range cases {
		op := op
		t.Run(op.String(), func(t *testing.T) {
			t.Parallel()
			plan := &chplan.MetricsAggregate{
				Op:         op,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			if op == chplan.MetricsOpQuantileOverTime {
				plan.Quantiles = []float64{0.95}
			}
			_, _, err := chsql.Emit(context.Background(), plan)
			if err == nil {
				t.Fatalf("expected error for %s without Attr", op)
			}
		})
	}
}

// TestMetricsAggregateMultiQuantileBare exercises the bare-emit fan-out
// for `quantile_over_time(attr, p1, p2, ...)` with N > 1: the inner
// SELECT aggregates `quantiles(p1, p2, ...)(Attr)` (returning
// Array(Float64)), and the wrapping SELECT layers fan the array out
// into one row per phi tagged with the synthetic `__phi__` label.
//
// The test pins the structural markers (`quantiles(?, ?, ?)`, the
// `__phi__` label projection, the inline phi-string array) so the
// emit shape stays stable under future refactors.
func TestMetricsAggregateMultiQuantileBare(t *testing.T) {
	t.Parallel()

	plan := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpQuantileOverTime,
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		Quantiles:  []float64{0.5, 0.9, 0.99},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantSubstrings := []string{
		"quantiles(?, ?, ?)(`Duration`)",
		"AS qs_array",
		"['0.5', '0.9', '0.99']",
		"AS phi_val",
		"tupleElement(phi_val, 1) AS `__phi__`",
		"tupleElement(phi_val, 2) AS `Value`",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(sql, s) {
			t.Errorf("expected SQL to contain %q; got %s", s, sql)
		}
	}
	wantArgs := []any{0.5, 0.9, 0.99}
	if len(args) != len(wantArgs) {
		t.Fatalf("len(args) = %d, want %d: %v", len(args), len(wantArgs), args)
	}
	for i, a := range wantArgs {
		if args[i] != a {
			t.Errorf("args[%d] = %v, want %v", i, args[i], a)
		}
	}
}

// TestRangeWindowMetricsQuantileBuckets exercises the matrix-path
// emit for `quantile_over_time(...)`: the SQL projects one row per
// (group, anchor, bucket) with `pow(2, ceil(log2(toFloat64(metric_arg))))`
// as the synthetic `__bucket` column and `toFloat64(count(1))` as the
// per-bucket count. The Tempo handler post-processes those rows via
// `pkg/traceql.Log2QuantileWithBucket` to produce the per-(group,
// anchor, phi) wire value — so the SQL no longer carries the phi
// constants and no longer calls CH's `quantile` / `quantiles` aggregate
// (whose interpolation diverges from Tempo's HistogramAggregator).
func TestRangeWindowMetricsQuantileBuckets(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.5, 0.9, 0.99},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "Timestamp",
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantSubstrings := []string{
		"pow(2, ceil(log2(toFloat64(metric_arg))))",
		"AS `__bucket`",
		"toFloat64(count(1)) AS `Value`",
		"metric_arg >= 2",
		"GROUP BY `anchor_ts`, `__bucket`",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(sql, s) {
			t.Errorf("expected SQL to contain %q; got %s", s, sql)
		}
	}
	// No CH-side quantile call — the phi values are consumed by the
	// post-processor in internal/api/tempo/metrics_query_range.go.
	for _, banned := range []string{"quantile(?)", "quantiles(?", "qs_array", "phi_val", "__phi__"} {
		if strings.Contains(sql, banned) {
			t.Errorf("matrix quantile SQL must not contain %q (post-processor handles phi): got %s", banned, sql)
		}
	}
	// The phi values do not bind to the SQL anymore — the matrix
	// quantile emitter drives only the bucket projection.
	if len(args) != 0 {
		t.Fatalf("len(args) = %d, want 0 (post-processor consumes phi): %v", len(args), args)
	}
}

// TestRangeWindowMetricsQuantileBucketsDuration pins the duration-aware
// branch of `quantileBucketFrag`: when MetricsAggregate.IsDuration is
// true the bucket key carries the `* 1e9` / `/ 1e9` rebase so the
// upstream `Log2Bucketize(d) / time.Second` formula reads bucket edges
// in fractional seconds. The min-value filter expands to
// `metric_arg * 1e9 >= 2` so the original-nanosecond `>= 2` guard from
// `bucketizeDuration` survives the seconds rebase.
func TestRangeWindowMetricsQuantileBucketsDuration(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			IsDuration: true,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantSubstrings := []string{
		"pow(2, ceil(log2(toFloat64(metric_arg) * 1000000000))) / 1000000000",
		"AS `__bucket`",
		"toFloat64(count(1)) AS `Value`",
		"metric_arg * 1000000000 >= 2",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(sql, s) {
			t.Errorf("expected SQL to contain %q; got %s", s, sql)
		}
	}
}

// TestRangeWindowMetricsReducerIsFloat64 pins the Value-column type
// invariant: every metrics-pipeline op in the matrix path must wrap the
// per-bucket reducer in `toFloat64(...)` so chclient.Sample.Value
// (a `float64`) can be Scan'd directly out of the row stream. Without
// the wrap, `| count_over_time()` (CH `count()` → UInt64) and
// `| {sum,min,max}_over_time(duration)` (CH aggregate over Int64 →
// Int64) both surface as `engine: execute: chclient: scan: (Value)
// converting UInt64 to *float64 is unsupported` against a real
// ClickHouse — the bug fixed in #(this PR).
//
// The stubQuerier-backed handler test (metrics_query_range_test.go)
// can't catch this because it returns pre-typed Go []chclient.Sample
// values; the ScanRow conversion path is bypassed.
func TestRangeWindowMetricsReducerIsFloat64(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   chplan.MetricsOp
		attr chplan.Expr
		q    []float64
	}{
		{name: "rate", op: chplan.MetricsOpRate},
		{name: "count_over_time", op: chplan.MetricsOpCountOverTime},
		{name: "sum_over_time", op: chplan.MetricsOpSumOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "avg_over_time", op: chplan.MetricsOpAvgOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "min_over_time", op: chplan.MetricsOpMinOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "max_over_time", op: chplan.MetricsOpMaxOverTime, attr: &chplan.ColumnRef{Name: "Duration"}},
		{name: "quantile_over_time", op: chplan.MetricsOpQuantileOverTime, attr: &chplan.ColumnRef{Name: "Duration"}, q: []float64{0.95}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         c.op,
					Attr:       c.attr,
					Quantiles:  c.q,
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit %s: %v", c.name, err)
			}
			if !strings.Contains(sql, "toFloat64(") {
				t.Errorf("%s reducer must wrap in toFloat64(...) so the Value column scans into chclient.Sample.Value (float64); SQL=%s", c.name, sql)
			}
		})
	}
}
