package chplan

import "slices"

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
// caller (internal/promql's histogram_native_mixed_or_vector_arithmetic.go
// / histogram_native_mixed_or_vector_comparison.go) builds the
// per-combination Value/Histogram*/discriminator fold as ordinary
// chplan.Expr projections over those aliases — a discriminator-keyed
// `if`/`multiIf` (chplan.FnIf / chplan.FnMultiIf), the mechanism issue
// #2449 itself named — reusing the SAME per-field scale-fold helpers
// (histogram_native_scalar_binop.go's scaleHistogramScalarExpr /
// scaleHistogramLadderExpr) the MUL/DIV scalar scaling lowering
// (histogram_native_mixed_or_scale.go) already uses, rather than
// duplicating them.
//
// Card + Include support group_left()/group_right() over two
// independently-mixed operands (cerberus issue #2449's ninth wrapper
// family) the identical way [HistogramFloatVectorJoin] widened past its
// own original CardOneToOne-only shape for #2342: CardManyToOne
// (`group_left`) keeps Left at full per-series granularity (the "many")
// while Right collapses to one row per matching key (the "one");
// CardOneToMany (`group_right`) mirrors with roles swapped. The per-row
// four-combination fold this node stays deliberately blind to (see above)
// is UNCHANGED by cardinality — broadcasting the "one" side's collapsed
// row against each of the "many" side's own rows still hands the caller
// one L/R pair per output row, each still carrying its own independent
// discriminator pair, so the SAME Value/Histogram*/discriminator
// projections this node's callers already build for CardOneToOne compose
// unmodified; only the caller's OWN output-Attributes fold needs to learn
// the manySide+Include overlay (mirroring
// internal/promql/histogram_native_binop_card.go's
// histogramCardOutputAttributesExpr for [HistogramVectorJoin]'s identical
// shape). Default CardOneToOne.
//
// One side may also be a WIDENED plain (non-mixed) vector rather than a
// genuine Mixed [VectorSetOp] — cerberus issue #2449's tenth and final
// wrapper family, `(a or b) <op> plain_vector`
// (internal/promql/histogram_native_mixed_or_vector_plain_arithmetic.go /
// _plain_comparison.go). internal/promql's widenPlainVectorToMixedShape
// wraps the plain operand's own lowered Node in a Project publishing the
// identical fourteen-column contract — the real canonical quartet plus
// nine typed-zero/empty-array Histogram*Column placeholders and a
// discriminator that is always 0 — so this node cannot tell the
// difference from either side, and every existing per-op fold reads it
// correctly with no change: a statically-0 discriminator is simply the
// degenerate case of a per-row one that always happens to read 0. See
// that file's own header for the full argument.
type MixedVectorJoin struct {
	// Left, Right are each an already-lowered Mixed VectorSetOp, OR a
	// plain vector Node widened to the same fourteen-column contract via
	// internal/promql's widenPlainVectorToMixedShape (see this type's own
	// doc comment above).
	Left, Right Node

	Match VectorMatch

	// Card is the cardinality modifier; default CardOneToOne. All three
	// VectorCard values are supported (CardManyToMany can never reach
	// this node — the parser only ever sets it for the `and`/`or`/
	// `unless` set operators, which internal/promql's mixed-or
	// vector-vector recognizers never claim in the first place).
	Card VectorCard
	// Include is the group_left(<labels>)/group_right(<labels>)
	// extra-label list, copied from the "one" side onto the "many"
	// side's output Attributes. Nil/empty when no Include was specified,
	// or when Card is CardOneToOne.
	Include []string

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
