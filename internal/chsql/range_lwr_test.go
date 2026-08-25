package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitRangeLWR_SinglePassShape pins the structural invariants of the
// RangeLWR emitter: a bounded sample-side fan-out (arrayJoin over a
// `range(greatest(0, ...), least(N, ...))` index set), a per-(series,
// anchor) argMax collapse, and the unshadowed re-alias — and crucially the
// ABSENCE of the old StepGrid CROSS JOIN shape. This is the cheap guard
// that the single-pass rewrite stays single-pass even if a future edit
// re-touches the emitter; the byte-exact SQL is pinned by the TXTAR
// goldens (test/spec/promql/*.txtar).
func TestEmitRangeLWR_SinglePassShape(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// MUST be single-pass: no CROSS JOIN, no StepGrid grid fan-out.
	if strings.Contains(sql, "CROSS JOIN") {
		t.Errorf("RangeLWR emit must NOT contain a CROSS JOIN (single-pass invariant); got:\n%s", sql)
	}
	// MUST carry the bounded sample-side fan-out: arrayJoin over a clamped
	// range, anchored on the grid base walking BACK by i*step.
	for _, want := range []string{
		"arrayJoin(arrayMap(i ->",
		"range(greatest(0,",
		"least(11,", // (5m / 30s) + 1 = 11 anchors
		"argMax(`Value`, `TimeUnix`)",
		"GROUP BY `MetricName`, `Attributes`, `anchor_ts`",
		"anchor_ts AS `TimeUnix`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("RangeLWR emit missing %q; got:\n%s", want, sql)
		}
	}
}

// TestEmitRangeLWR_OffsetShiftsWindowNotAnchor pins that a non-zero Offset
// folds onto the MEMBERSHIP base (the dateDiff target) but the emitted
// anchor stays on the unshifted grid base — `offset` shifts the staleness
// window, not the reported timestamp.
func TestEmitRangeLWR_OffsetShiftsWindowNotAnchor(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          time.Minute,
		Lookback:      5 * time.Minute,
		Offset:        -5 * time.Minute, // forward shift
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The membership base subtracts the (negative) offset → forward shift.
	if !strings.Contains(sql, "toIntervalNanosecond(-300000000000)") {
		t.Errorf("offset emit must fold the offset onto the membership base; got:\n%s", sql)
	}
	// The emitted anchor still walks back from the unshifted End grid.
	if !strings.Contains(sql, "toDateTime64('2026-01-01 00:05:00.000000000', 9) - toIntervalNanosecond(i * 60000000000)") {
		t.Errorf("offset emit must keep the anchor on the unshifted grid base; got:\n%s", sql)
	}
}

// TestEmitRangeLWR_RejectsZeroStep guards the Step > 0 precondition.
func TestEmitRangeLWR_RejectsZeroStep(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Step:          0,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	if _, _, err := chsql.Emit(context.Background(), plan); err == nil {
		t.Errorf("RangeLWR with Step=0 should error")
	}
}

// TestEmitRangeLWR_FanoutLimitIsMaxRowsPlusOne kills the two ARITHMETIC_BASE
// mutants at lwr_fanout_bound.go:213:24 and :224:22 (`maxRows + 1`, in
// lwrFanoutBoundedSourceFrag's bounded-read LIMIT and its independent
// truncation-probe LIMIT). The "+1" is the whole truncation-detection
// mechanism: a returned probe count landing exactly on maxRows+1 can only
// happen if the true fanned-out row count reached or exceeded that LIMIT. A
// `+`->`-` flip renders `maxRows - 1` instead, a directly observable
// literal change in the emitted SQL (maxRangeLWRFanoutRows = 40_000_000, so
// the correct literal is 40000001, not 39999999).
func TestEmitRangeLWR_FanoutLimitIsMaxRowsPlusOne(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	const wantLimit = "LIMIT 40000001"
	if got := strings.Count(sql, wantLimit); got != 2 {
		t.Errorf("expected %q exactly twice (bounded read + truncation probe), got %d occurrences; SQL:\n%s", wantLimit, got, sql)
	}
	for _, bad := range []string{"LIMIT 39999999", "LIMIT 40000000"} {
		if strings.Contains(sql, bad) {
			t.Errorf("unexpected %q in SQL (maxRows+1 arithmetic flipped?):\n%s", bad, sql)
		}
	}
}
