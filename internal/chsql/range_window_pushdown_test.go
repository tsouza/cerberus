package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// The matrix-shape MetricsAggregate emitters
// (emitRangeWindowMetrics, emitRangeWindowMetricsQuantileBuckets,
// emitMetricsExemplars) pin the inner otel_traces scan to the
// (Start - range, End] window via maybePushInnerScanTimeBounds so
// ClickHouse can prune partitions / granules by the Timestamp key.
// The pushdown is gated on BOTH Start AND End being set — the PromQL
// subquery-internal RangeWindow shapes (only Range / Step / OuterRange,
// no explicit grid) rely on the bounds being absent to keep the
// emitter output byte-stable against pinned snapshots.
//
// The tests below pin both halves of the gate:
//
//   - Start AND End set → WHERE clause MUST carry the `<tsCol> >
//     toDateTime64(...)` / `<tsCol> <= toDateTime64(...)` pair.
//   - Either Start or End zero → WHERE clause MUST NOT carry that pair
//     (so OuterRange-only PromQL shapes stay byte-stable).
//
// These coverage shapes kill the INVERT_LOGICAL mutant on the
// `Start.IsZero() || End.IsZero()` short-circuit (flip the `||` to
// `&&` and Start=zero+End=set emits the WHERE clause that the original
// suppressed).

// pushdownLowerSubstr is the load-bearing substring the matrix-shape
// emitters render for the lower half of the inner-scan time pushdown.
// It is structurally distinct from the outer per-anchor WHERE
// (`ts > anchor_ts - ...`) because the inner pushdown references the
// scan's actual timestamp column (`Timestamp`) rather than the inner
// `ts` alias — so a substring hit uniquely identifies the pushdown.
const (
	pushdownLowerSubstr = "`Timestamp` > toDateTime64("
	pushdownUpperSubstr = "`Timestamp` <= toDateTime64("
)

func TestRangeWindowMetricsInnerScanPushdown_BothSet(t *testing.T) {
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

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected inner-scan lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected inner-scan upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestRangeWindowMetricsInnerScanPushdown_OnlyOneSet pins the gate's
// short-circuit: with only one of Start/End set the inner-scan WHERE
// pushdown MUST be suppressed (so the PromQL subquery-internal shapes
// — OuterRange-only — stay byte-stable). Kills the INVERT_LOGICAL
// mutant on `Start.IsZero() || End.IsZero()` (flip to `&&` and the
// pushdown leaks into the SQL with a `now64(9)` half-bound).
func TestRangeWindowMetricsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         chplan.MetricsOpRate,
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("inner-scan lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("inner-scan upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_BothSet pins
// the matrix-quantile path's pushdown: with Start AND End set the
// inner SELECT carries the (Start - range, End] WHERE.
func TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
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
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_OnlyOneSet
// mirrors the rate path's gate-short-circuit test for the
// quantile-bucket emitter.
func TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         chplan.MetricsOpQuantileOverTime,
					Attr:       &chplan.ColumnRef{Name: "Duration"},
					Quantiles:  []float64{0.95},
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestEmitMetricsExemplarsInnerScanPushdown_BothSet pins the same
// (Start - range, End] pushdown contract for the exemplar emitter.
func TestEmitMetricsExemplarsInnerScanPushdown_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected exemplar lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected exemplar upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestEmitMetricsExemplarsInnerScanPushdown_OnlyOneSet mirrors the
// matrix-path gate test for the exemplar emitter. With only one of
// Start/End set the WHERE pushdown MUST be suppressed — kills the
// INVERT_LOGICAL mutant on the gate.
//
// Note: EmitMetricsExemplars rejects a missing Range upfront via
// `rw.Range == 0 → defaults to Step`, so the gate is reached even when
// one bound is zero (numAnchors falls through to 1 in that branch).
func TestEmitMetricsExemplarsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:         chplan.MetricsOpRate,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input:           m,
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("EmitMetricsExemplars: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("exemplar lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("exemplar upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}
