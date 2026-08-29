package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_VectorSetOpOutputCols_MixedOrVsMixedAnd pins
// vectorSetOpOutputCols's own guard at vector_set_op.go:710
// (`s.Mixed && s.Op != chplan.VectorSetOr`): a Mixed VectorSetOr never
// widens the column list here — real callers always render that shape
// through emitMixedVectorSetOp instead, whose own
// mixedVectorSetOpOutputCols does the widening — while a Mixed
// VectorSetAnd (the forwarded-Left shape cerberus issue #2555 added)
// DOES widen here, since it never reaches emitMixedVectorSetOp.
//
// Kills vector_set_op.go:710:21 (CONDITIONALS_NEGATION `s.Op !=
// chplan.VectorSetOr` -> `s.Op == chplan.VectorSetOr`): under that
// mutation a Mixed VectorSetOr would ALSO widen (the guard flips to
// true), which this test's Or case catches, and a Mixed VectorSetAnd
// would stop widening (the guard flips to false), which this test's
// And case catches.
func TestMutation_VectorSetOpOutputCols_MixedOrVsMixedAnd(t *testing.T) {
	t.Parallel()

	base := func(op chplan.VectorSetOpKind) *chplan.VectorSetOp {
		return &chplan.VectorSetOp{
			Op:               op,
			Mixed:            true,
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}

	const canonicalQuartetCols = 4

	orCols := vectorSetOpOutputCols(base(chplan.VectorSetOr))
	if len(orCols) != canonicalQuartetCols {
		t.Errorf("Mixed VectorSetOr: got %d output columns, want %d (never widened here — emitMixedVectorSetOp's own mixedVectorSetOpOutputCols owns that)", len(orCols), canonicalQuartetCols)
	}

	andCols := vectorSetOpOutputCols(base(chplan.VectorSetAnd))
	if len(andCols) <= canonicalQuartetCols {
		t.Errorf("Mixed VectorSetAnd: got %d output columns, want more than %d (a forwarded-Left Mixed And never reaches emitMixedVectorSetOp, so this function must widen it)", len(andCols), canonicalQuartetCols)
	}
}
