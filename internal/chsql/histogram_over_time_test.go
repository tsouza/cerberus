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

// TestEmitMetricsHistogramOverTimeBare exercises the bare emission
// path (no wrapping RangeWindow). Confirms the SQL has a synthesised
// bucket column, a count(1) reducer, the <attr> >= 2 filter, and a
// GROUP BY that covers (user group-by..., bucket).
func TestEmitMetricsHistogramOverTimeBare(t *testing.T) {
	t.Parallel()

	plan := &chplan.MetricsHistogramOverTime{
		Attr:        &chplan.ColumnRef{Name: "Duration"},
		IsDuration:  true,
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}

	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Bucket key: the next power of two (Tempo's Log2Bucketize), divided
	// by 1e9 for the duration intrinsic so the label reads in seconds.
	if !strings.Contains(sql, "pow(2, ceil(log2(toFloat64(`Duration`)))) / 1000000000 AS `__bucket`") {
		t.Errorf("expected `pow(2, ceil(log2(toFloat64(Duration)))) / 1e9 AS __bucket`, SQL=%s", sql)
	}
	// count(1) AS Value reducer (wrapped in toFloat64 so the column
	// scans into chclient.Sample.Value's *float64 — clickhouse-go/v2
	// refuses UInt64 → *float64; see chsql/emit_node.go::aggFuncFrag).
	if !strings.Contains(sql, "toFloat64(count(?)) AS `Value`") {
		t.Errorf("expected `toFloat64(count(?)) AS Value`, SQL=%s", sql)
	}
	// <attr> >= 2 drop filter.
	if !strings.Contains(sql, "`Duration` >= 2") {
		t.Errorf("expected `Duration >= 2` filter, SQL=%s", sql)
	}
	// GROUP BY includes the bucket column.
	if !strings.Contains(sql, "GROUP BY `__bucket`") {
		t.Errorf("expected `GROUP BY __bucket`, SQL=%s", sql)
	}

	// One bound arg: the LitInt{1} from count(1).
	if len(args) != 1 {
		t.Fatalf("expected 1 arg (count operand), got %d: %v", len(args), args)
	}
	if v, ok := args[0].(int64); !ok || v != 1 {
		t.Errorf("expected args[0] = int64(1), got %T(%v)", args[0], args[0])
	}
}

// TestEmitMetricsHistogramOverTimeNonDuration confirms the bucket key
// for non-duration attributes is the raw next-power-of-two
// `pow(2, ceil(log2(toFloat64(<attr>))))` (no / 1e9) — Tempo's
// bucketizeAttribute TypeInt arm applies Log2Bucketize without the
// seconds rebase.
func TestEmitMetricsHistogramOverTimeNonDuration(t *testing.T) {
	t.Parallel()

	plan := &chplan.MetricsHistogramOverTime{
		Attr:        &chplan.ColumnRef{Name: "BodySize"},
		IsDuration:  false,
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "pow(2, ceil(log2(toFloat64(`BodySize`)))) AS `__bucket`") {
		t.Errorf("expected `pow(2, ceil(log2(toFloat64(BodySize)))) AS __bucket` (no /1e9), SQL=%s", sql)
	}
	if strings.Contains(sql, "/ 1000000000") {
		t.Errorf("expected no /1e9 divisor for non-duration attr, SQL=%s", sql)
	}
}

// TestEmitMetricsHistogramOverTimeByLabel adds a user-supplied `by`
// group key and confirms it threads through SELECT and GROUP BY.
func TestEmitMetricsHistogramOverTimeByLabel(t *testing.T) {
	t.Parallel()

	plan := &chplan.MetricsHistogramOverTime{
		Attr:           &chplan.ColumnRef{Name: "Duration"},
		IsDuration:     true,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		GroupByAliases: []string{"service.name"},
		BucketAlias:    "__bucket",
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`ServiceName` AS `service.name`") {
		t.Errorf("expected group-by SELECT alias, SQL=%s", sql)
	}
	if !strings.Contains(sql, "GROUP BY `ServiceName`, `__bucket`") {
		t.Errorf("expected GROUP BY `ServiceName`, `__bucket`, SQL=%s", sql)
	}
}

// TestEmitRangeWindowHistogramMatrix exercises the matrix-shape path
// (RangeWindow wrapping MetricsHistogramOverTime). Confirms the
// sample-side arrayJoin anchor fanout, the bucket column threading
// into the outer GROUP BY, and the zero-fill UNION ALL +
// sum(in_window) reducer that pins a 0 sample at every grid anchor of
// every observed (group, __bucket) series — upstream histogram series
// ride NewCountOverTimeAggregator, whose counts reach the wire dense
// (SeriesSet.ToProto skips only NaN, never 0).
func TestEmitRangeWindowHistogramMatrix(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsHistogramOverTime{
			Attr:        &chplan.ColumnRef{Name: "Duration"},
			IsDuration:  true,
			BucketAlias: "__bucket",
			ValueAlias:  "Value",
			Inner:       &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// 5-minute span / 1-minute step = 6 anchors, clamped sample-side:
	// each row fans only across the (≤ range/step + 1) anchors whose
	// window contains it.
	if !strings.Contains(sql, "least(6, intDiv(dateDiff('nanosecond', `Timestamp`, ") {
		t.Errorf("expected sample-side anchor bound least(6, ...), SQL=%s", sql)
	}
	if !strings.Contains(sql, "pow(2, ceil(log2(toFloat64(`Duration`)))) / 1000000000 AS `__bucket`") {
		t.Errorf("expected bucket key in inner SELECT, SQL=%s", sql)
	}
	// Zero-fill: sum(in_window) reducer over a UNION ALL of the sample
	// arm (in_window = 1) and the per-(series, anchor) generator arm
	// (in_window = 0), so empty grid anchors surface as 0 samples
	// instead of dropping — matching Tempo's dense count-based
	// histogram series.
	if !strings.Contains(sql, "toFloat64(sum(in_window)) AS `Value`") {
		t.Errorf("expected toFloat64(sum(in_window)) reducer in outer SELECT, SQL=%s", sql)
	}
	if !strings.Contains(sql, "UNION ALL") {
		t.Errorf("expected zero-fill UNION ALL generator arm, SQL=%s", sql)
	}
	if !strings.Contains(sql, "1 AS `in_window`") || !strings.Contains(sql, "0 AS `in_window`") {
		t.Errorf("expected in_window markers on both arms, SQL=%s", sql)
	}
	// The generator arm discovers observed series GROUPed by bucket.
	if !strings.Contains(sql, "GROUP BY `__bucket`)") {
		t.Errorf("expected per-bucket series discovery in the generator arm, SQL=%s", sql)
	}
	// Outer GROUP BY includes both bucket and anchor.
	if !strings.Contains(sql, "GROUP BY `__bucket`, `anchor_ts`") {
		t.Errorf("expected outer GROUP BY __bucket, anchor_ts, SQL=%s", sql)
	}
}

// TestEmitRangeWindowHistogramLeftOpenWindow pins the per-anchor
// bucket boundary for the histogram matrix path: the sample-side
// fanout's lower index bound carries the `- <rangeNS>` shift + `+ 1`
// strict-edge bump while the upper bound floors the unshifted
// distance, so the per-anchor window is left-open / right-closed,
// matching Tempo's `IntervalMapperQueryRange.interval` semantics.
// Mirrors TestRangeWindowMetricsLeftOpenWindow for the non-histogram
// metrics emitter; same off-by-one bug class would otherwise surface
// as histogram-bucket counts drifting from Tempo by 1 at step
// boundaries.
func TestEmitRangeWindowHistogramLeftOpenWindow(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsHistogramOverTime{
			Attr:        &chplan.ColumnRef{Name: "Duration"},
			IsDuration:  true,
			BucketAlias: "__bucket",
			ValueAlias:  "Value",
			Inner:       &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Strict left-open edge: `- 60000000000` (the 1m range) shift +
	// `+ 1` bump on the lower index bound.
	if !strings.Contains(sql, " - 60000000000, toInt64(60000000000)) - (modulo(") {
		t.Errorf("expected range-shifted strict lower index bound; SQL=%s", sql)
	}
	// Right-closed edge: the upper bound floors the unshifted distance.
	if !strings.Contains(sql, "least(6, intDiv(dateDiff('nanosecond', `Timestamp`, ") {
		t.Errorf("expected unshifted inclusive upper index bound; SQL=%s", sql)
	}
	// The legacy per-(row, anchor) WHERE re-check must be gone.
	if strings.Contains(sql, "ts > anchor_ts") || strings.Contains(sql, "ts <= anchor_ts") {
		t.Errorf("window predicate must live in the fanout bounds, not a WHERE re-check; SQL=%s", sql)
	}
}

// TestEmitRangeWindowHistogramRejectsZeroStep guards the matrix path's
// Step > 0 invariant — without it the arrayJoin range would divide
// by zero.
func TestEmitRangeWindowHistogramRejectsZeroStep(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsHistogramOverTime{
			Attr:        &chplan.ColumnRef{Name: "Duration"},
			IsDuration:  true,
			BucketAlias: "__bucket",
			ValueAlias:  "Value",
			Inner:       &chplan.Scan{Table: "otel_traces"},
		},
		TimestampColumn: "Timestamp",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for Step=0, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestEmitMetricsHistogramOverTimeRejectsNilAttr surfaces the chplan
// invariant that Attr is required.
func TestEmitMetricsHistogramOverTimeRejectsNilAttr(t *testing.T) {
	t.Parallel()

	plan := &chplan.MetricsHistogramOverTime{
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for nil Attr, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}
