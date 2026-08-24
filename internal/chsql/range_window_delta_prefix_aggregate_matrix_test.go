package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// deltaPrefixAggregateMatrixGateWindow builds the minimal matrix rate()
// RangeWindow useAggregateDeltaPrefix governs (see
// emitWindowedArrayExtrapolatedMatrix), identical across every gate case
// below except for DeltaPrefixAggregateInput — set by the caller.
func deltaPrefixAggregateMatrixGateWindow(aggInput chplan.Node) *chplan.RangeWindow {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input:                     &chplan.Scan{Table: "otel_metrics_sum"},
		Func:                      "rate",
		Range:                     5 * time.Minute,
		Start:                     start,
		End:                       start.Add(10 * time.Minute),
		Step:                      time.Minute,
		OuterRange:                10 * time.Minute,
		TimestampColumn:           "TimeUnix",
		ValueColumn:               "Value",
		TemporalityColumn:         "AggregationTemporality",
		GroupBy:                   []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		DeltaPrefixAggregateInput: aggInput,
	}
}

// TestExtrapolatedMatrixDeltaPrefixAggregateGate pins
// emitWindowedArrayExtrapolatedMatrix's own
//
//	useAggregateDeltaPrefix := needsDeltaFirstLevel &&
//		r.DeltaPrefixAggregateInput != nil && e.deltaPrefixReadEnabled
//
// three-term gate independently on each operand, not just the "both set"
// happy path: PR #2514's own review found that a mistake on ANY one of
// these three terms — the wrong boolean operator, or the nil-check
// direction — either silently takes the exact-aggregate path when the
// operator never opted in (case C below: the field is populated by
// lowering unconditionally once the schema names the table, per
// attachDeltaPrefixAggregateArm's own doc — CERBERUS_DELTA_PREFIX_READ_ENABLED
// is the ONLY thing that is supposed to gate consumption), or crashes /
// misbehaves when the flag is on but the schema hasn't named a
// DELTA-prefix table at all (case B). Every case's SQL is compared against
// case A's byte-for-byte, rather than probed with substring assertions,
// so a boolean-operator mutation that flips the gate in EITHER direction
// on ANY operand is guaranteed to change at least one case's expected
// outcome.
func TestExtrapolatedMatrixDeltaPrefixAggregateGate(t *testing.T) {
	t.Parallel()

	baseline, _, err := Emit(context.Background(), deltaPrefixAggregateMatrixGateWindow(nil))
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=false): %v", err)
	}

	// Case B: DeltaPrefixReadEnabled is on, but this deployment's schema
	// never named a DELTA-prefix table (DeltaPrefixAggregateInput stays
	// nil — see schema.Metrics.DeltaPrefixTable's doc: it is empty unless
	// CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED opts in, independently of
	// CERBERUS_DELTA_PREFIX_READ_ENABLED). Must emit the SAME SQL as the
	// baseline, without error — never attempt to scan a nil node.
	withReadEnabledNoInput, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		deltaPrefixAggregateMatrixGateWindow(nil),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=true): %v", err)
	}
	if withReadEnabledNoInput != baseline {
		t.Errorf("DeltaPrefixAggregateInput=nil, readEnabled=true must match the baseline SQL exactly "+
			"(no DELTA-prefix table named by this schema means nothing to read) — got a DIFFERENT query:\nbaseline: %s\ngot:      %s",
			baseline, withReadEnabledNoInput)
	}

	// Case C: DeltaPrefixAggregateInput IS populated (lowering does this
	// unconditionally once the schema names the table) but
	// DeltaPrefixReadEnabled is FALSE — the documented default for every
	// deployment that has not explicitly opted in. Must emit the SAME SQL
	// as the baseline: populating the FIELD alone must never change the
	// emitted query.
	aggInput := &chplan.Scan{Table: "otel_metrics_sum_delta_prefix"}
	withInputNoReadEnabled, _, err := Emit(context.Background(), deltaPrefixAggregateMatrixGateWindow(aggInput))
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=false): %v", err)
	}
	if withInputNoReadEnabled != baseline {
		t.Errorf("DeltaPrefixAggregateInput populated but readEnabled=false must match the baseline SQL exactly "+
			"(the flag, not the field, gates consumption) — got a DIFFERENT query:\nbaseline: %s\ngot:      %s",
			baseline, withInputNoReadEnabled)
	}

	// Case D: both set — the real exact-mechanism path. Must DIFFER from
	// the baseline and reference the aggregate table by name, proving the
	// gate actually opens when every operand is true (a table-flattening
	// bug that always leaves the gate closed would otherwise pass cases
	// B/C/D vacuously).
	withBoth, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		deltaPrefixAggregateMatrixGateWindow(aggInput),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=true): %v", err)
	}
	if withBoth == baseline {
		t.Error("DeltaPrefixAggregateInput populated AND readEnabled=true must emit a DIFFERENT query than the " +
			"baseline — the exact-aggregate mechanism never fired")
	}
	if !strings.Contains(withBoth, "otel_metrics_sum_delta_prefix") {
		t.Errorf("DeltaPrefixAggregateInput populated AND readEnabled=true must scan the aggregate table by name\nSQL: %s", withBoth)
	}
}
