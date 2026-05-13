package chsql_test

import (
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

	sql, args, err := chsql.Emit(plan)
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
	// Rate reducer normalises by range_seconds (60s).
	if !strings.Contains(sql, "count(?) / 60") {
		t.Errorf("expected `count(?) / 60`, SQL=%s", sql)
	}
	// args has the LitInt{1} bound by count(1).
	if len(args) != 1 {
		t.Fatalf("expected 1 arg (count operand), got %d: %v", len(args), args)
	}
	if v, ok := args[0].(int64); !ok || v != 1 {
		t.Errorf("expected args[0] = int64(1), got %T(%v)", args[0], args[0])
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
	_, _, err := chsql.Emit(plan)
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
	_, _, err := chsql.Emit(plan)
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
			_, _, err := chsql.Emit(plan)
			if err == nil {
				t.Fatalf("expected error for %s without Attr", op)
			}
		})
	}
}
