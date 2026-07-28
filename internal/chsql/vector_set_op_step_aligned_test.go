package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// setOpPair builds a two-arm VectorSetOp over the canonical 4-column
// arm shape, so the emitted SQL differs between the step-aligned and
// label-only forms only in the match key itself.
func setOpPair(op chplan.VectorSetOpKind, stepAligned bool) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left:             narySetOpArm("a"),
		Right:            narySetOpArm("b"),
		Op:               op,
		StepAligned:      stepAligned,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

func emitSetOp(t *testing.T, s *chplan.VectorSetOp) string {
	t.Helper()
	sql, _, err := chsql.Emit(context.Background(), s)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sql
}

// TestEmitVectorSetOp_StepAlignedMatchKey pins the range-mode match key
// for all three set ops. PromQL matches set-op arms once per evaluation
// timestamp, so a range query's key is (signature, timestamp) — the
// membership tuple for `and` / `unless`, the window partition for `or`.
// An instant query has a single evaluation timestamp, so its key is the
// bare signature.
func TestEmitVectorSetOp_StepAlignedMatchKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   chplan.VectorSetOpKind
		// stepAligned is the (signature, timestamp) key the range
		// shape must emit; labelOnly is the instant-shape key.
		stepAligned string
		labelOnly   string
	}{
		{
			name:        "and",
			op:          chplan.VectorSetAnd,
			stepAligned: "WHERE (`Attributes`, `TimeUnix`) IN ((SELECT DISTINCT `Attributes`, `TimeUnix` FROM",
			labelOnly:   "WHERE `Attributes` IN ((SELECT DISTINCT `Attributes` FROM",
		},
		{
			name:        "unless",
			op:          chplan.VectorSetUnless,
			stepAligned: "WHERE (`Attributes`, `TimeUnix`) NOT IN ((SELECT DISTINCT `Attributes`, `TimeUnix` FROM",
			labelOnly:   "WHERE `Attributes` NOT IN ((SELECT DISTINCT `Attributes` FROM",
		},
		{
			name:        "or",
			op:          chplan.VectorSetOr,
			stepAligned: "OVER (PARTITION BY `Attributes`, `TimeUnix`)",
			labelOnly:   "OVER (PARTITION BY `Attributes`)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ranged := emitSetOp(t, setOpPair(tc.op, true))
			if !strings.Contains(ranged, tc.stepAligned) {
				t.Errorf("step-aligned %s: missing per-timestamp match key %q; sql=%s",
					tc.name, tc.stepAligned, ranged)
			}
			if strings.Contains(ranged, tc.labelOnly) {
				t.Errorf("step-aligned %s: emitted the label-only key %q, which drops the "+
					"per-timestamp dimension; sql=%s", tc.name, tc.labelOnly, ranged)
			}

			instant := emitSetOp(t, setOpPair(tc.op, false))
			if !strings.Contains(instant, tc.labelOnly) {
				t.Errorf("instant %s: missing label-only match key %q; sql=%s",
					tc.name, tc.labelOnly, instant)
			}
			if strings.Contains(instant, tc.stepAligned) {
				t.Errorf("instant %s: emitted the per-timestamp key %q; sql=%s",
					tc.name, tc.stepAligned, instant)
			}
		})
	}
}
