package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_vector_join.go is the one place a vector-vector
// binary operator over at least one MIXED float/histogram operand is joined
// and folded. The four root recognisers that lower `<mixed or> OP <mixed
// or>` / `<mixed or> OP <plain vector>` for arithmetic and comparison
// (histogram_native_mixed_or_vector_{arithmetic,plain_arithmetic,comparison,
// plain_comparison}.go) and the generic vector-vector consumer that meets an
// already-lowered mixed plan one level down ([lowerVectorVector], cerberus
// issue #3562 — `sort_by_label(h or f, l) + up`, `limitk(k, h or f) == up`,
// `last_over_time((h or f)[r:s]) * v`) all route through
// [lowerMixedVectorJoinBinary], so the per-operator fold is selected in
// exactly one switch and no consumer can join a mixed relation through the
// plain [chplan.VectorJoin], which reads a histogram row's placeholder
// Value as a float sample.

// mixedJoinOperand widens one already-lowered operand to the fourteen-column
// contract [chplan.MixedVectorJoin] reads from BOTH sides:
//
//   - a LIVE mixed relation is republished under the canonical column
//     names by role ([projectSampleRoles] with the payload preserved), so a
//     wrapper that renamed an identity column still joins;
//   - a histogram-valued relation (one lowerRoot's own histogram
//     recognisers declined — a histogram forwarded through `and`/`unless`,
//     say) keeps its payload and gains a discriminator of 1
//     ([widenHistogramVectorToMixedShape]);
//   - a float relation — including a physically mixed one whose rows are
//     already PROVEN float ([mixedFloatRowsProven]) — is narrowed to its
//     canonical quartet where it still carries the payload columns, then
//     widened with placeholder payload and a discriminator of 0 by
//     [widenPlainVectorToMixedShape];
//   - anything else (an opaque relation) is refused by that widening.
//
// Each per-row discriminator is then a fact about THAT row, so the folds
// behind [lowerMixedVectorJoinBinary] answer a mixed-vs-histogram pair
// exactly as they answer a mixed-vs-mixed one whose other row happens to
// be histogram-shaped.
func mixedJoinOperand(node chplan.Node, s schema.Metrics) (chplan.Node, error) {
	switch liveSampleKind(node) {
	case chplan.SampleKindMixed:
		return projectSampleRoles(node, s,
			sampleProjectionPolicy{name: preserveSampleName, payload: preserveMixedSamplePayload},
			derivedSampleProjectionLayout(node),
			func(sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{} }), nil
	case chplan.SampleKindHistogram:
		return widenHistogramVectorToMixedShape(node, s), nil
	}
	if node.RowType().Has(chplan.RoleDiscriminator) {
		// Physically mixed, proven float: drop the payload columns the
		// widening below would otherwise refuse as a non-float schema.
		node = projectSampleRoles(node, s,
			sampleProjectionPolicy{name: preserveSampleName, payload: floatSamplePayload},
			derivedSampleProjectionLayout(node),
			func(sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{} })
	}
	return widenPlainVectorToMixedShape(node, s)
}

// widenHistogramVectorToMixedShape is [widenPlainVectorToMixedShape]'s
// histogram-valued twin: the relation's own canonical quartet (its Value is
// already the [histogramSampleValuePlaceholder]) and nine Histogram*Column
// fields are forwarded by role and name, and every row is stamped
// discriminator=1 — the same fourteen-column contract chsql's
// mixedVectorSetOpArmFrag synthesises for a Histogram-shaped `or` arm. A
// name or timestamp the relation does not publish is supplied the way the
// float twin supplies it.
func widenHistogramVectorToMixedShape(node chplan.Node, s schema.Metrics) chplan.Node {
	row := node.RowType()
	roles := append([]chplan.Column(nil), metricRoles(s)...)
	roles = append(roles, chplan.HistogramPayloadColumns()...)
	roles = append(roles, chplan.Column{Name: mixedDiscriminatorColumn, Role: chplan.RoleDiscriminator})

	// A histogram schema has already validated every public role as named
	// and unique ([chplan.Schema.SampleKind]), so the optional lookups
	// below cannot fail; only presence is in question.
	metricName := chplan.Projection{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn}
	if ref, ok, _ := resolveOptionalSampleRole(row, chplan.RoleMetricName); ok {
		metricName = sampleForwardColumn(ref, s.MetricNameColumn, true)
	}
	timestamp := chplan.Projection{Expr: chplan.NowNanoMinusStaleness(), Alias: s.TimestampColumn}
	if ref, ok, _ := resolveOptionalSampleRole(row, chplan.RoleTimestamp); ok {
		timestamp = sampleForwardColumn(ref, s.TimestampColumn, true)
	}
	projections := []chplan.Projection{
		metricName,
		sampleForwardColumn(requireSampleRole(row, chplan.RoleAttributes), s.AttributesColumn, true),
		timestamp,
		sampleForwardColumn(requireSampleRole(row, chplan.RoleValue), s.ValueColumn, true),
	}
	for _, field := range chplan.HistogramPayloadColumns() {
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: field.Name}, Alias: field.Name})
	}
	projections = append(projections, chplan.Projection{Expr: &chplan.LitInt{V: mixedDiscriminatorHistogram}, Alias: mixedDiscriminatorColumn})
	return &chplan.Project{Roles: roles, Input: node, Projections: projections}
}

// newMixedVectorJoin builds the join both operands of a mixed vector-vector
// binary operator meet on; `left`/`right` are the operator's own syntactic
// LHS/RHS, already in the fourteen-column contract.
func newMixedVectorJoin(left, right chplan.Node, match chplan.VectorMatch, card chplan.VectorCard, include []string, s schema.Metrics, ctx lowerCtx) *chplan.MixedVectorJoin {
	return &chplan.MixedVectorJoin{
		Left:             left,
		Right:            right,
		Match:            match,
		Card:             card,
		Include:          include,
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
}

// lowerMixedVectorJoinBinary folds a [chplan.MixedVectorJoin] under `op`,
// selecting the one per-operator fold reference Prometheus's
// `vectorElemBinop` prescribes for a per-row float/histogram pair:
//
//   - `+`/`-`: [lowerMixedVVAdditiveArithmetic] — float,float computes,
//     histogram,histogram merges, a mismatched pair drops;
//   - `*`/`/`: [lowerMixedVVScaledArithmetic] — a histogram side is scaled
//     by a float side, histogram,histogram drops;
//   - `^`/`%`/`atan2`: [lowerMixedVVFloatOnlyArithmetic] — only float,float
//     survives;
//   - a comparison with `bool`: [lowerMixedVVCompareBool] — a float 1/0 per
//     compatible pair; without: [lowerMixedVVCompareFilter] — the LHS row
//     where the comparison holds, histogram payload preserved.
//
// `returnBool` is read only for a comparison: every caller has already
// refused the modifier on an arithmetic operator ([lowerVectorVector]'s
// own guard; the root recognisers pass false). An operator outside the
// set [promBinaryOp] produces is refused rather than folded through the
// closest-looking branch.
func lowerMixedVectorJoinBinary(join *chplan.MixedVectorJoin, op chplan.BinaryOp, returnBool bool, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if isComparison(op) {
		if returnBool {
			return lowerMixedVVCompareBool(join, op, s), nil
		}
		return lowerMixedVVCompareFilter(join, op, s), nil
	}
	switch op {
	case chplan.OpAdd, chplan.OpSub:
		return lowerMixedVVAdditiveArithmetic(join, op, s, ctx.resourceBounds.HistogramMergeMaxCostUnits), nil
	case chplan.OpMul, chplan.OpDiv:
		return lowerMixedVVScaledArithmetic(join, op, s), nil
	case chplan.OpPow, chplan.OpMod, chplan.OpAtan2:
		return lowerMixedVVFloatOnlyArithmetic(join, op, s), nil
	default:
		return nil, fmt.Errorf("promql: binary op %s over a mixed float/histogram operand is unsupported", op)
	}
}
