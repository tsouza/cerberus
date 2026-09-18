package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// fixedAccumulatorGateWindow is deltaPrefixAggregateMatrixGateWindow with
// the fixed-accumulator decomposition selected, so the same three-term
// DELTA-prefix gate is exercised on emitFixedAccumulatorExtrapolatedMatrix
// rather than on the array-fold emitter.
func fixedAccumulatorGateWindow(aggInput chplan.Node) *chplan.RangeWindow {
	r := deltaPrefixAggregateMatrixGateWindow(aggInput)
	r.FixedAccumulatorExtrapolated = true
	return r
}

// TestFixedAccumulatorDeltaPrefixAggregateGate is
// TestExtrapolatedMatrixDeltaPrefixAggregateGate's sibling for
// emitFixedAccumulatorExtrapolatedMatrix's own copy of the gate,
//
//	useAggregateDeltaPrefix := needsDeltaFirstLevel &&
//		r.DeltaPrefixAggregateInput != nil && e.deltaPrefixReadEnabled
//
// pinned independently on each operand and by whole-SQL equality, so a
// wrong boolean operator or a flipped nil-check on ANY term changes at
// least one case's expected outcome. The fixed-accumulator emitter is its
// own function with its own copy of this expression; the array-fold
// emitter's pin says nothing about it.
func TestFixedAccumulatorDeltaPrefixAggregateGate(t *testing.T) {
	t.Parallel()

	baseline, _, err := Emit(context.Background(), fixedAccumulatorGateWindow(nil))
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=false): %v", err)
	}
	if !strings.Contains(baseline, "`"+fixedAccumLastValAlias+"`") {
		t.Fatalf("the gate window did not take the fixed-accumulator strategy:\n%s", baseline)
	}

	withReadEnabledNoInput, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		fixedAccumulatorGateWindow(nil),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=true): %v", err)
	}
	if withReadEnabledNoInput != baseline {
		t.Errorf("DeltaPrefixAggregateInput=nil, readEnabled=true must match the baseline SQL exactly:\nbaseline: %s\ngot:      %s",
			baseline, withReadEnabledNoInput)
	}

	aggInput := &chplan.Scan{Table: "otel_metrics_sum_delta_prefix"}
	withInputNoReadEnabled, _, err := Emit(context.Background(), fixedAccumulatorGateWindow(aggInput))
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=false): %v", err)
	}
	if withInputNoReadEnabled != baseline {
		t.Errorf("DeltaPrefixAggregateInput populated but readEnabled=false must match the baseline SQL exactly:\nbaseline: %s\ngot:      %s",
			baseline, withInputNoReadEnabled)
	}

	withBoth, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		fixedAccumulatorGateWindow(aggInput),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=true): %v", err)
	}
	if withBoth == baseline {
		t.Error("DeltaPrefixAggregateInput populated AND readEnabled=true must emit a DIFFERENT query than the baseline")
	}
	if !strings.Contains(withBoth, "otel_metrics_sum_delta_prefix") {
		t.Errorf("DeltaPrefixAggregateInput populated AND readEnabled=true must scan the aggregate table by name\nSQL: %s", withBoth)
	}
}

// TestFixedAccumulatorMatrixShapeCheck pins fixedAccumulatorMatrixShapeCheck
// on each field it rejects, one at a time from an otherwise valid window,
// and on the zero boundary of the two durations: a matrix window needs
// OuterRange > 0 AND Step > 0, so a zero in EITHER is unsupported while
// the valid window passes.
func TestFixedAccumulatorMatrixShapeCheck(t *testing.T) {
	t.Parallel()

	valid := func() *chplan.RangeWindow { return fixedAccumulatorGateWindow(nil) }
	if err := fixedAccumulatorMatrixShapeCheck(valid()); err != nil {
		t.Fatalf("the valid matrix window must pass: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *chplan.RangeWindow)
	}{
		{name: "timestamp column unset", mutate: func(r *chplan.RangeWindow) { r.TimestampColumn = "" }},
		{name: "value column unset", mutate: func(r *chplan.RangeWindow) { r.ValueColumn = "" }},
		{name: "zero outer range", mutate: func(r *chplan.RangeWindow) { r.OuterRange = 0 }},
		{name: "zero step", mutate: func(r *chplan.RangeWindow) { r.Step = 0 }},
		{name: "negative outer range", mutate: func(r *chplan.RangeWindow) { r.OuterRange = -time.Minute }},
		{name: "negative step", mutate: func(r *chplan.RangeWindow) { r.Step = -time.Minute }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(r)
			err := fixedAccumulatorMatrixShapeCheck(r)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("want ErrUnsupported, got %v", err)
			}
		})
	}
}
