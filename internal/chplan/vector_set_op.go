package chplan

// MixedDiscriminatorColumn is the trailing per-row discriminator column
// name a Mixed [VectorSetOp]'s SELECT carries (cerberus issue #2330):
// 1 on a row that came from the histogram-shaped arm (its nine
// Histogram*Column outputs are real, Value is the [HistogramProjection]
// placeholder), 0 on a row from the float-shaped arm (the mirror image).
// internal/chsql/vector_set_op.go's emitter is the one place that writes
// it; [RowShapeOf]'s [*Project] case reads it back to recognise a
// PromQL wrapper's re-projection of a Mixed node as still MixedRowShape
// (internal/promql's `label_replace`/`label_join` composition, cerberus
// issue #2449) without walking into the Project's input.
//
// chclient may not import chplan (see .go-arch-lint.yml: chclient
// declares no internal dependencies), so internal/chclient/cursor.go's
// decode-side probe duplicates this exact string literal locally rather
// than importing this symbol — the same pairing the Histogram*Column
// constants already have with that file's histogramLastColumn.
const MixedDiscriminatorColumn = "_setop_is_histogram"

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

	// Mixed marks a VectorSetOp whose own output publishes the fourteen-
	// column Mixed contract -- the canonical quartet, the nine
	// Histogram*Column outputs, and a trailing per-row discriminator
	// ([MixedDiscriminatorColumn]) -- instead of the plain four-column
	// Sample contract or the thirteen-column Histogram one. Two distinct
	// lowerings build this:
	//
	//   - A VectorSetOr whose Left and Right disagree on value type --
	//     exactly one is a [HistogramProjection] (or a nested
	//     histogram-shaped VectorSetOp) and the other is a float vector
	//     (cerberus issue #2330), published as any of [SampleRowShape],
	//     [GridWindowRowShape] (a matrix range function), or
	//     [ReducedWindowRowShape] (an instant derived shape). Reference
	//     Prometheus's `or` never inspects value type, only labels, so a
	//     `Vector` it returns can freely hold both float and
	//     native-histogram `Sample`s at once -- cerberus answers that by
	//     widening BOTH arms to the shared fourteen-column projection,
	//     letting the non-native side publish placeholder values for the
	//     columns its own shape doesn't have: the float arm's nine
	//     histogram columns are placeholders (mirroring
	//     histogramSampleValuePlaceholder's Value placeholder on the
	//     histogram side, just the other four-vs-nine way around), and
	//     the histogram arm's Value stays the placeholder
	//     [HistogramProjection] already carries. See internal/promql's
	//     lowerMixedExpHistogramSetOp (cerberus issue #2333) for the
	//     shape guard.
	//
	//   - ANY VectorSetOp -- `and`/`unless`/`or` alike -- whose relevant
	//     operand is ITSELF already Mixed-shaped, because
	//     [mixedExpHistogramSetOp] resolved as a NESTED operand of this
	//     set op rather than only at the query root (cerberus issue
	//     #2555). `and`/`unless` always forward Left verbatim, so Mixed
	//     mirrors Left's own shape there; `or`'s union composes a Mixed
	//     arm with a Histogram-shaped, Sample-shaped, or another
	//     Mixed-shaped other arm the same way it composes the two-pure-
	//     shapes case above. An already-Mixed arm needs no placeholder
	//     synthesis -- its own real per-row discriminator is forwarded
	//     unchanged.
	//
	// Either way, internal/chsql/vector_set_op.go's mixedVectorSetOpArmFrag
	// (Or) / vectorSetOpCanonicalArmFrag (And/Unless) is the canonicalisation
	// and emitMixedVectorSetOp is the Or SQL shape;
	// internal/chclient/cursor.go's shapeSampleMixed is the decode side
	// that reads the discriminator to decide, per row, whether
	// Sample.Histogram or Sample.Value is the real answer.
	//
	// Mixed and Histogram are mutually exclusive: Histogram means BOTH
	// (or, for And/Unless, the forwarded) side(s) are already
	// histogram-shaped with no discriminator needed, Mixed means the
	// relevant side(s) need or already carry one.
	Mixed bool

	// MixedHistogramOnLeft is meaningful only for the construct-from-two-
	// pure-shapes case above (Mixed true, neither arm itself already
	// Mixed-shaped): true when Left is the histogram-shaped arm and Right
	// is the float arm, false when it's the other way around. `or`'s
	// union keeps every Left row unconditionally and only the Right rows
	// whose signature Left doesn't already have, so which side is which
	// changes which arm's rows win a signature collision -- this flag
	// preserves the query's own LHS/RHS order rather than normalising it
	// away. It is ignored (left false) on a node built by the
	// already-Mixed-operand case (cerberus issue #2555): there the
	// emitter classifies each arm directly from its own
	// [RowShapeOf] instead, since an already-Mixed arm carries no single
	// "is this the histogram side" answer to give.
	MixedHistogramOnLeft bool

	// MixedDropCollisions narrows a Mixed VectorSetOr from `or`'s
	// left-wins union to reference Prometheus's mixed-aggregation-group
	// rule: a match key carrying rows from BOTH arms is a group whose
	// members disagreed on value type, which reference drops entirely
	// (a MixedFloatsHistogramsAggWarning annotation, not an error), while
	// a key present on only one arm survives through that arm unchanged.
	// The union is therefore a SYMMETRIC DIFFERENCE on the match key
	// rather than a left-biased union, and MixedHistogramOnLeft has no
	// effect: a collision drops both sides, so neither arm wins one.
	//
	// It exists so internal/promql's combineMixedAggregateBranches can
	// express that rule as ONE node reading each branch ONCE, instead of
	// the equivalent three-node `(hist unless float) or (float unless
	// hist)` spelling — which names each branch TWICE, so every stacked
	// recombination squared the emitted SQL and doubled the reads
	// (cerberus issue #2728: a triple-nested subquery composition stacks
	// two of them and emitted 549KB of SQL, past ClickHouse's own
	// max_ast_elements ceiling, for a query the single-node spelling
	// emits in 142KB).
	//
	// Meaningful only alongside Mixed with Op == VectorSetOr; the emitter
	// rejects it anywhere else rather than silently ignoring it.
	MixedDropCollisions bool

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
	if s.MixedDropCollisions != o.MixedDropCollisions {
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
