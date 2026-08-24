package chplan

// MixedVectorJoin joins two already-lowered Mixed [VectorSetOp] results
// (each the fourteen-column float/histogram shape [MixedDiscriminatorColumn]
// discriminates — see that node's own Mixed-field doc) on labels, for a
// vector-vector PromQL binary expression whose BOTH operands are themselves
// a mixed float/histogram `or` (internal/promql's
// histogram_native_mixed_or.go, cerberus issue #2330) — the vector-vector
// case cerberus issue #2449 named as its own remaining piece after every
// scalar-wrapped family (sum/avg #2346, label_replace/label_join,
// single-arg math functions, drop-family arithmetic binops, scalar
// comparisons, and MUL/histogram-left-DIV scaling) had already landed.
//
// Deliberately "dumb", mirroring [HistogramVectorJoin] and
// [HistogramFloatVectorJoin]: it performs the INNER JOIN and exposes every
// one of the fourteen Mixed columns from BOTH sides under
// `_mvj_L_<field>` / `_mvj_R_<field>` aliases (internal/chsql/
// mixed_vector_join.go's emitter), with no notion of which arithmetic op
// is being computed or which of the four float/histogram payload
// combinations a given matched pair turns out to carry at runtime. The
// caller (internal/promql's histogram_native_mixed_or_vector_arithmetic.go)
// builds the per-combination Value/Histogram*/discriminator fold as
// ordinary chplan.Expr projections over those aliases — a
// discriminator-keyed `if`/`multiIf` (chplan.FnIf / chplan.FnMultiIf), the
// mechanism issue #2449 itself named — reusing the SAME per-field
// scale-fold helpers (histogram_native_scalar_binop.go's
// scaleHistogramScalarExpr / scaleHistogramLadderExpr) the MUL/DIV scalar
// scaling lowering (histogram_native_mixed_or_scale.go) already uses,
// rather than duplicating them.
//
// Deliberately CardOneToOne only: unlike HistogramVectorJoin/
// HistogramFloatVectorJoin (which exist BECAUSE the one-to-one path
// couldn't express group_left()/group_right()), this node's one-to-one
// join is the ONLY cardinality it supports — group_left()/group_right()
// over two independently-mixed operands would need the "many" side kept
// at full per-series granularity while STILL discriminating each row's
// own payload, compounding the four-combination fold with the Include-
// label broadcast; internal/promql's recognizer rejects that shape
// outright rather than mis-widening it, tracked as remaining scope under
// #2449 rather than attempted here. There is accordingly no Card/Include
// field on this type at all — unlike its two siblings, which default to
// (and in HistogramVectorJoin's case, require) a many-to-one cardinality.
type MixedVectorJoin struct {
	Left, Right Node // each an already-lowered Mixed VectorSetOp.

	Match VectorMatch

	// StepAligned mirrors VectorJoin.StepAligned / HistogramVectorJoin.
	// StepAligned: true in range mode, so the emitter keeps TimestampColumn
	// in each side's per-match-key grouping and ANDs `L.<ts> = R.<ts>`
	// into the JOIN's ON clause, pairing each grid anchor with its own
	// per-anchor match.
	StepAligned bool

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*MixedVectorJoin) planNode() {}

func (j *MixedVectorJoin) Children() []Node { return []Node{j.Left, j.Right} }

func (j *MixedVectorJoin) Equal(other Node) bool {
	o, ok := other.(*MixedVectorJoin)
	if !ok {
		return false
	}
	if !j.Match.Equal(o.Match) || j.StepAligned != o.StepAligned {
		return false
	}
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn ||
		j.ValueColumn != o.ValueColumn {
		return false
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
