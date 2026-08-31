package chsql_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// vectorJoinArgAndMaxFusionPlan builds a minimal VectorJoin over two plain
// Scans, varying exactly the axes that decide whether
// chplan.VectorJoin.ArgAndMaxFusion (cerberus issue #2764) fires:
// StepAligned (range mode), the match key (default vs on(), which selects
// roleMany vs roleOne), and the fusion flag itself.
func vectorJoinArgAndMaxFusionPlan(match chplan.VectorMatch, stepAligned, fused bool) *chplan.VectorJoin {
	return &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Match:            match,
		StepAligned:      stepAligned,
		ArgAndMaxFusion:  fused,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

// TestEmitVectorJoin_ArgAndMaxFusion_RoleMany pins the roleMany (default,
// full-Attributes matching) instant-mode arm: with the flag on, the
// argMax(Value, TimeUnix) + max(TimeUnix) pair collapses into one
// argAndMax(Value, TimeUnix) tuple, destructured back via tupleElement;
// with the flag off (the pre-#2764 shape), the two-aggregate pair and
// plain column renames survive unchanged.
func TestEmitVectorJoin_ArgAndMaxFusion_RoleMany(t *testing.T) {
	t.Parallel()
	match := chplan.VectorMatch{} // default: full-Attributes, roleMany/roleMany

	fusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, false, true))
	for _, want := range []string{
		"argAndMax(`Value`, `TimeUnix`)",
		"tupleElement(",
	} {
		if !strings.Contains(fusedSQL, want) {
			t.Errorf("fused roleMany emit missing %q; got:\n%s", want, fusedSQL)
		}
	}
	if strings.Contains(fusedSQL, "max(`TimeUnix`)") {
		t.Errorf("fused roleMany emit must NOT contain a separate max(TimeUnix); got:\n%s", fusedSQL)
	}

	unfusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, false, false))
	for _, want := range []string{
		"argMax(`Value`, `TimeUnix`)",
		"max(`TimeUnix`)",
	} {
		if !strings.Contains(unfusedSQL, want) {
			t.Errorf("unfused roleMany emit missing %q; got:\n%s", want, unfusedSQL)
		}
	}
	if strings.Contains(unfusedSQL, "argAndMax") {
		t.Errorf("unfused roleMany emit must NOT contain argAndMax; got:\n%s", unfusedSQL)
	}
}

// TestEmitVectorJoin_ArgAndMaxFusion_RoleOne pins the roleOne (on()-reduced
// match) non-StepAligned arm's identical fusion behaviour.
func TestEmitVectorJoin_ArgAndMaxFusion_RoleOne(t *testing.T) {
	t.Parallel()
	match := chplan.VectorMatch{On: true, Labels: []string{"job"}} // roleOne/roleOne

	fusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, false, true))
	for _, want := range []string{
		"argAndMax(`Value`, `TimeUnix`)",
		"tupleElement(",
		// The Attributes-picking argMax (a DIFFERENT pair — argMax by
		// TimeUnix, no companion max) is untouched by the fusion.
		"argMax(mapSort(`Attributes`), `TimeUnix`)",
	} {
		if !strings.Contains(fusedSQL, want) {
			t.Errorf("fused roleOne emit missing %q; got:\n%s", want, fusedSQL)
		}
	}

	unfusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, false, false))
	if strings.Contains(unfusedSQL, "argAndMax") {
		t.Errorf("unfused roleOne emit must NOT contain argAndMax; got:\n%s", unfusedSQL)
	}
}

// TestEmitVectorJoin_ArgAndMaxFusion_StepAlignedUnaffected pins that
// StepAligned=true, ArgAndMaxFusion=true renders BYTE-IDENTICAL SQL to
// StepAligned=true, ArgAndMaxFusion=false — the range-mode roleMany arm
// never pairs argMax with a separate max(TimeUnix) (TimestampColumn is a
// plain GROUP BY key there), so the flag has nothing to act on. This is
// the scope boundary chplan.VectorJoin.ArgAndMaxFusion's own doc names.
func TestEmitVectorJoin_ArgAndMaxFusion_StepAlignedUnaffected(t *testing.T) {
	t.Parallel()
	match := chplan.VectorMatch{}

	fusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, true, true))
	unfusedSQL := emitJoin(t, vectorJoinArgAndMaxFusionPlan(match, true, false))
	if fusedSQL != unfusedSQL {
		t.Errorf("StepAligned emit must be byte-identical regardless of ArgAndMaxFusion:\nfused:\n%s\nunfused:\n%s", fusedSQL, unfusedSQL)
	}
	if strings.Contains(fusedSQL, "argAndMax") {
		t.Errorf("StepAligned emit must never contain argAndMax; got:\n%s", fusedSQL)
	}
}

// TestEmitVectorJoin_ArgAndMaxFusion_DerivedUnaffected pins the same
// byte-identity guarantee for the derived (range-vector-operand) arm,
// which has no real TimestampColumn to argMax by at all.
func TestEmitVectorJoin_ArgAndMaxFusion_DerivedUnaffected(t *testing.T) {
	t.Parallel()
	match := chplan.VectorMatch{}
	derivedOperand := func() chplan.Node {
		return &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
	}
	planWith := func(fused bool) *chplan.VectorJoin {
		return &chplan.VectorJoin{
			Left:             derivedOperand(),
			Right:            derivedOperand(),
			Op:               chplan.OpAdd,
			Match:            match,
			ArgAndMaxFusion:  fused,
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}

	fusedSQL := emitJoin(t, planWith(true))
	unfusedSQL := emitJoin(t, planWith(false))
	if fusedSQL != unfusedSQL {
		t.Errorf("derived-arm emit must be byte-identical regardless of ArgAndMaxFusion:\nfused:\n%s\nunfused:\n%s", fusedSQL, unfusedSQL)
	}
	if strings.Contains(fusedSQL, "argAndMax") {
		t.Errorf("derived-arm emit must never contain argAndMax; got:\n%s", fusedSQL)
	}
}
