package chplan

import "slices"

// HistogramFloatVectorJoin implements MUL (either operand order) and
// histogram-left DIV histogram-SCALING by a genuine per-series float-
// VECTOR operand (cerberus issue #2339), widened to on()/ignoring()
// reduced-key matching and the "histogram is the many side"
// group_left()/group_right() broadcast (cerberus issue #2342).
// [expHistogramScalarBinop]'s scaling machinery
// (internal/promql/histogram_native_scalar_binop.go, #2087) already
// folds a compile-time scalar LITERAL scale factor into every histogram
// bucket; this node supplies the row-by-row MATCH a genuine per-series
// float-VECTOR operand needs before that same fold can run — a real
// INNER JOIN keyed on Attributes, mirroring [VectorJoin]'s one-to-one /
// many-to-one shapes (vector_join.go) and [HistogramVectorJoin]'s
// histogram-side handling (histogram_vector_join.go, #2328), but neither
// of those directly: unlike VectorJoin, one side carries nine histogram
// fields instead of a single Value; unlike HistogramVectorJoin, the
// OTHER side carries a single plain Value instead of nine histogram
// fields.
//
// Left is the already-lowered histogram-valued operand (a
// *HistogramProjection); Right is the already-lowered PLAIN float-valued
// operand. The emitter (internal/chsql/histogram_float_vector_join.go)
// publishes ONE row per matched pair under the SAME canonical column
// names Left's own HistogramProjection cap publishes — MetricName,
// Attributes, TimeUnix, and the nine fixed Histogram*Column fields —
// plus Right's own ValueColumn carrying the per-series float scale
// factor. That is deliberately the exact shape [HistogramProjection]
// itself publishes, plus one extra Value column, so the calling
// lowering can feed this node straight into the SAME per-bucket
// scale-fold [scaleHistogramProjection] already applies for the
// literal-scalar case, reading the scale factor off Value instead of a
// constant.
//
// Card + Include mirror [HistogramVectorJoin]'s own fields: default
// CardOneToOne matches Match's key (full Attributes, or the on()/
// ignoring() reduced key) one row per side, with a runtime
// throwIf(uniqExact(...) > 1, ...) ambiguity guard on any side whose
// matching key is not already known-unique by construction.
// CardManyToOne keeps Left (the histogram side) at full per-series
// granularity — the "many" — while Right (the float side) collapses to
// one row per matching key — the "one" — with Include copying named
// labels from Right onto the output Attributes. Left is ALWAYS the
// histogram-valued operand regardless of which operand PromQL's
// group_left()/group_right() syntax names as "many", so a caller that
// recognises the mirror-image shape (float LHS, `group_right()`,
// histogram RHS) still sets Card to CardManyToOne — the emitter's Card
// vocabulary describes Left/Right roles, not PromQL's original operand
// order. CardOneToMany (the histogram side broadcasting as the "one"
// against many float rows) is not supported: the emitter rejects it
// outright rather than silently mis-joining.
type HistogramFloatVectorJoin struct {
	Left  Node // *HistogramProjection, the histogram-valued operand.
	Right Node // The plain float-valued operand.

	Match VectorMatch

	// Card is the cardinality modifier; default CardOneToOne. Only
	// CardOneToOne and CardManyToOne are supported — see the type doc.
	Card VectorCard
	// Include is the group_left(<labels>)/group_right(<labels>) extra-
	// label list, copied from Right (the float side, always the "one"
	// under the only supported CardManyToOne shape) onto the output
	// Attributes. Nil/empty when no Include was specified, or when
	// Card is CardOneToOne (Include is a group_left/group_right-only
	// modifier).
	Include []string

	// StepAligned mirrors VectorJoin.StepAligned / HistogramVectorJoin.
	// StepAligned: when true (range mode) the emitter additionally ANDs
	// `L.<ts> = R.<ts>` into the JOIN's ON clause so each per-step grid
	// anchor only joins its own per-anchor pair.
	StepAligned bool

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*HistogramFloatVectorJoin) planNode() {}

func (j *HistogramFloatVectorJoin) Children() []Node { return []Node{j.Left, j.Right} }

func (j *HistogramFloatVectorJoin) Equal(other Node) bool {
	o, ok := other.(*HistogramFloatVectorJoin)
	if !ok {
		return false
	}
	if !j.Match.Equal(o.Match) || j.StepAligned != o.StepAligned {
		return false
	}
	if j.Card != o.Card || !slices.Equal(j.Include, o.Include) {
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
