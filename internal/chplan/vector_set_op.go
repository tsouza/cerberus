package chplan

// VectorSetOpKind identifies a PromQL vector set-operator: `and` (semi-
// join over label signatures), `or` (union with anti-right), and
// `unless` (anti-join). Parameterising the node by kind keeps the
// three lowerings on a single typed path so optimizer rules and the
// emitter can dispatch on the same shape.
type VectorSetOpKind string

const (
	// VectorSetAnd is PromQL's `A and B`: keep samples from A whose
	// match-key signature appears at least once in B.
	VectorSetAnd VectorSetOpKind = "and"
	// VectorSetOr is PromQL's `A or B`: keep all samples from A, plus
	// samples from B whose match-key signature does not appear in A.
	VectorSetOr VectorSetOpKind = "or"
	// VectorSetUnless is PromQL's `A unless B`: keep samples from A
	// whose match-key signature does NOT appear in B.
	VectorSetUnless VectorSetOpKind = "unless"
)

// VectorSetOp models a PromQL vector set-operator binary expression.
//
// The set ops are inherently many-to-many on labels: each sample on the
// LHS / RHS is matched against the opposite side's match-key signature
// — defined by the full Attributes map (default), or by the listed
// labels (`on(...)` / `ignoring(...)`). PromQL's parser rejects
// `group_left` / `group_right` on set ops ("set operations must always
// be many-to-many"), so the node deliberately omits a Card slot — the
// chsql emitter assumes many-to-many.
//
// The result carries the LHS sample values verbatim for `and` / `unless`;
// for `or` the LHS rows plus the LHS-anti-matched RHS rows are unioned.
// Output Attributes preserves each surviving row's full Attributes —
// set ops never derive a new sample, they filter / union existing ones,
// so `__name__` flows through unchanged (unlike arithmetic / comparison
// V-V binops which always drop the metric name).
type VectorSetOp struct {
	Left  Node
	Right Node
	Op    VectorSetOpKind
	Match VectorMatch

	// StepAligned marks a range-mode set op, whose arms all carry
	// per-step rows under a shared grid anchor. PromQL evaluates set
	// operators once per evaluation timestamp, so the match key is
	// (label signature, timestamp) there; instant mode matches on the
	// signature alone because each arm carries one row per series with
	// an arm-local timestamp. Mirrors VectorJoin.StepAligned.
	StepAligned bool

	// Histogram marks a set op whose Left AND Right are both a
	// [HistogramProjection] — the shape internal/promql's
	// lowerExpHistogramSetOp builds for `and`/`or`/`unless` between two
	// exponential-histogram-valued operands (cerberus issue #2324).
	// Reference Prometheus's set operators never inspect a matched
	// sample's value, only its label set, so the join/union machinery
	// below is identical to the float case — this flag only widens the
	// emitted projection to carry the nine
	// [HistogramCountColumn]…[HistogramNegativeBucketCountsColumn]
	// outputs alongside the canonical quartet, instead of forwarding
	// ValueColumn's meaningless placeholder and silently dropping the
	// histogram. Both arms publish those nine columns under their FIXED
	// alias names regardless of the schema's physical column names (see
	// HistogramProjection's own emitter), so no additional column-name
	// plumbing is needed here.
	Histogram bool

	// Mixed marks a VectorSetOr whose Left and Right disagree on value
	// type -- exactly one is a [HistogramProjection] (or a nested
	// histogram-shaped VectorSetOp) and the other is a plain
	// [SampleRowShape] float vector (cerberus issue #2330, the
	// mixed-type sibling of the Histogram flag above). Reference
	// Prometheus's `or` never inspects value type, only labels, so a
	// `Vector` it returns can freely hold both float and native-histogram
	// `Sample`s at once -- cerberus answers that by widening BOTH arms to
	// the same fourteen-column projection (the canonical quartet, the
	// nine Histogram*Column outputs, and a trailing per-row
	// discriminator) and letting the non-native side publish placeholder
	// values for the columns its own shape doesn't have: the float arm's
	// nine histogram columns are placeholders (mirroring
	// histogramSampleValuePlaceholder's Value placeholder on the
	// histogram side, just the other four-vs-nine way around), and the
	// histogram arm's Value stays the placeholder [HistogramProjection]
	// already carries. See internal/chsql/vector_set_op.go's
	// emitMixedVectorSetOp for the SQL shape and
	// internal/chclient/cursor.go's shapeSampleMixed for the decode side
	// that reads the discriminator to decide, per row, whether
	// Sample.Histogram or Sample.Value is the real answer.
	//
	// Mixed and Histogram are mutually exclusive: Histogram means BOTH
	// arms are already histogram-shaped (no discriminator needed, no
	// placeholders), Mixed means exactly one is. Only VectorSetOr
	// supports Mixed today -- `and`/`unless` between a float-valued and a
	// histogram-valued operand remains cerberus issue #2325, which this
	// field does not attempt.
	Mixed bool

	// MixedHistogramOnLeft is meaningful only when Mixed is true: true
	// when Left is the histogram-shaped arm and Right is the float arm,
	// false when it's the other way around. `or`'s union keeps every
	// Left row unconditionally and only the Right rows whose signature
	// Left doesn't already have, so which side is which changes which
	// arm's rows win a signature collision -- this flag preserves the
	// query's own LHS/RHS order rather than normalising it away.
	MixedHistogramOnLeft bool

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*VectorSetOp) planNode() {}

func (s *VectorSetOp) Children() []Node { return []Node{s.Left, s.Right} }

func (s *VectorSetOp) Equal(other Node) bool {
	o, ok := other.(*VectorSetOp)
	if !ok {
		return false
	}
	if s.Op != o.Op || !s.Match.Equal(o.Match) || s.StepAligned != o.StepAligned || s.Histogram != o.Histogram {
		return false
	}
	if s.Mixed != o.Mixed || s.MixedHistogramOnLeft != o.MixedHistogramOnLeft {
		return false
	}
	if s.MetricNameColumn != o.MetricNameColumn ||
		s.AttributesColumn != o.AttributesColumn ||
		s.TimestampColumn != o.TimestampColumn ||
		s.ValueColumn != o.ValueColumn {
		return false
	}
	return s.Left.Equal(o.Left) && s.Right.Equal(o.Right)
}
