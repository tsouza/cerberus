package chplan

import "slices"

// HistogramVectorJoin implements group_left()/group_right() (many-to-one
// vector-matching cardinality) broadcast between two histogram-valued
// operands — the cerberus issue #2328 gap [VectorJoin] already closes for
// plain float-valued operands (see that type's own doc, in particular its
// CardManyToOne / CardOneToMany branches).
//
// One-to-one matching (default, on(), ignoring()) does NOT use this node:
// it stays on the UnionAll+Aggregate `count() = 2` shape
// (internal/promql's histogram_native_binop.go
// mergeTwoHistogramProjections / histogram_native_binop_eq.go
// compareTwoHistogramProjections), which has no way to express "N rows on
// one side, 1 row on the other, broadcast the one row's histogram onto
// each of the N" — see internal/promql/histogram_native_binop_match.go's
// header doc for why on()/ignoring() can reuse that shape unchanged while
// group_left()/group_right() cannot.
//
// Left and Right are each an already-lowered *HistogramProjection (the
// operator's syntactic LHS/RHS operand), unmodified — every field of both
// stays at full, per-series granularity going in. Card selects which one
// plays the "many" role (kept at per-series granularity in the emitted
// join) versus the "one" role (collapsed to at most one row per
// Match-reduced key, guarded by a runtime uniqueness check mirroring
// chsql's VectorJoin roleOne): CardManyToOne (`group_left`) → Left is
// many, Right is one. CardOneToMany (`group_right`) → Right is many,
// Left is one.
//
// The emitter (internal/chsql/histogram_vector_join.go) renders a real
// INNER JOIN and projects every relevant field from BOTH sides — under
// `_hq_L_<field>` / `_hq_R_<field>` aliases — deliberately leaving the
// merge/compare VALUE fold and the Include-label output-Attributes
// overlay to the calling lowering (internal/promql's
// histogram_native_binop_card.go), which builds them as ordinary
// chplan.Expr projections over those aliases, reusing the exact
// field-fold helpers the one-to-one path already has
// (histogramBinopMergeProjections, histogramCompareFieldsExpr, and their
// siblings) unchanged. This node is deliberately "dumb": it has no notion
// of merge vs. compare, no Op, no ReturnBool — the join shape is
// identical either way, only what the caller projects on top differs.
type HistogramVectorJoin struct {
	Left, Right Node

	Match   VectorMatch
	Card    VectorCard // CardManyToOne or CardOneToMany only.
	Include []string

	// StepAligned mirrors VectorJoin.StepAligned: when true (range mode)
	// the emitter additionally keeps TimestampColumn in the "one" side's
	// GROUP BY and ANDs `L.<ts> = R.<ts>` into the JOIN's ON clause, so
	// each per-step grid anchor only joins its own per-anchor pair.
	StepAligned bool

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
}

func (*HistogramVectorJoin) planNode() {}

func (j *HistogramVectorJoin) Children() []Node { return []Node{j.Left, j.Right} }

func (j *HistogramVectorJoin) Equal(other Node) bool {
	o, ok := other.(*HistogramVectorJoin)
	if !ok {
		return false
	}
	if !j.Match.Equal(o.Match) || j.Card != o.Card || !slices.Equal(j.Include, o.Include) {
		return false
	}
	if j.StepAligned != o.StepAligned {
		return false
	}
	if j.MetricNameColumn != o.MetricNameColumn ||
		j.AttributesColumn != o.AttributesColumn ||
		j.TimestampColumn != o.TimestampColumn {
		return false
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
