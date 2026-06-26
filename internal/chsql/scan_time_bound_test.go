package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// instantOverTimeArrayLeaf builds an instant (OuterRange == 0)
// sum_over_time RangeWindow over a raw Scan — an instant windowed-array leaf
// that routes through emitWindowedArray. With InstantScanBounded unset it is
// the pre-establishment shape the emit guard must refuse.
func instantOverTimeArrayLeaf() *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func:            "sum_over_time",
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// instantDirectLeaf builds an instant min_over_time RangeWindow — the
// OverTimeDirect fast path (overTimeDirectAggFrag), which bounds the scan via
// its own WHERE rather than instantWindowScanBoundsFrags but shares the same
// fail-closed contract.
func instantDirectLeaf() *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func:            "min_over_time",
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// TestEmit_FailsClosedOnUnboundedInstantLeaf proves the emit-time complement of
// the analyzer invariant: an instant windowed-array leaf that reaches an
// emitter without its IR scan-time bound established surfaces a loud
// ErrUnsupported instead of a silently unbounded full-retention groupArray.
// Both the array path (emitWindowedArray family) and the OverTimeDirect path
// are covered.
func TestEmit_FailsClosedOnUnboundedInstantLeaf(t *testing.T) {
	t.Parallel()

	t.Run("array-path", func(t *testing.T) {
		t.Parallel()
		e := &emitter{}
		err := e.emitRangeWindow(instantOverTimeArrayLeaf())
		if err == nil {
			t.Fatal("emit must fail closed on an unbounded instant windowed-array leaf")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
		if !strings.Contains(err.Error(), "scan time bound") {
			t.Errorf("error should name the missing scan time bound, got %q", err.Error())
		}
	})

	t.Run("direct-path", func(t *testing.T) {
		t.Parallel()
		e := &emitter{}
		err := e.emitRangeWindow(instantDirectLeaf())
		if err == nil {
			t.Fatal("OverTimeDirect emit must fail closed on an unbounded instant leaf")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	})
}

// TestEmit_BoundedInstantLeafSucceeds confirms the guard does not over-reject:
// once the IR bound is established (as the public Emit entry point does via
// chplan.AttachInstantScanTimeBounds), both leaf shapes emit cleanly.
func TestEmit_BoundedInstantLeafSucceeds(t *testing.T) {
	t.Parallel()

	for name, leaf := range map[string]*chplan.RangeWindow{
		"array-path":  instantOverTimeArrayLeaf(),
		"direct-path": instantDirectLeaf(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := Emit(context.Background(), leaf)
			if err != nil {
				t.Fatalf("Emit must accept a leaf once the bound is established: %v", err)
			}
			// The established bound renders an upper anchor (now64) and a
			// lower window edge on the timestamp column.
			if !strings.Contains(sql, "`TimeUnix` <= now64(9)") {
				t.Errorf("bounded leaf SQL missing the scan upper bound:\n%s", sql)
			}
			if !strings.Contains(sql, "`TimeUnix` > now64(9) - toIntervalNanosecond(300000000000)") {
				t.Errorf("bounded leaf SQL missing the scan lower bound:\n%s", sql)
			}
		})
	}
}
