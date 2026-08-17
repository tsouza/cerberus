package chplan

// HistogramFloatVectorJoin implements MUL (either operand order) and
// histogram-left DIV histogram-SCALING by a genuine per-series float-
// VECTOR operand, under DEFAULT (full-Attributes) one-to-one vector
// matching (cerberus issue #2339). [expHistogramScalarBinop]'s scaling
// machinery (internal/promql/histogram_native_scalar_binop.go, #2087)
// already folds a compile-time scalar LITERAL scale factor into every
// histogram bucket; this node supplies the row-by-row MATCH a genuine
// per-series float-VECTOR operand needs before that same fold can run —
// a real INNER JOIN keyed on Attributes, mirroring [VectorJoin]'s
// default one-to-one shape (vector_join.go) and [HistogramVectorJoin]'s
// histogram-side handling (histogram_vector_join.go, #2328), but neither
// of those directly: unlike VectorJoin, one side carries nine histogram
// fields instead of a single Value; unlike HistogramVectorJoin, the
// OTHER side carries a single plain Value instead of nine histogram
// fields, and there is no group_left()/group_right() cardinality here to
// broadcast.
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
// Only default (full-Attributes) one-to-one matching is supported today:
// Match must carry no Labels and On == false. on()/ignoring() reduced-key
// matching and group_left()/group_right() many-to-one broadcast for this
// histogram/float-vector scaling shape are tracked as follow-up work —
// see the #2339 PR body for the filed issue. The emitter rejects any
// other Match shape outright rather than silently mis-joining.
type HistogramFloatVectorJoin struct {
	Left  Node // *HistogramProjection, the histogram-valued operand.
	Right Node // The plain float-valued operand.

	Match VectorMatch

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
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn ||
		j.ValueColumn != o.ValueColumn {
		return false
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
