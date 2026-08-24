package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_vector_comparison.go lowers a vector-vector
// COMPARISON binop (`==`, `!=`, `<`, `<=`, `>`, `>=`, with or without the
// `bool` modifier) whose BOTH operands are themselves a mixed
// float/histogram `or` (histogram_native_mixed_or.go's own #2330/#2335
// shape) — cerberus issue #2449's eighth wrapper family, and the piece PR
// #2521's own "what remains" section (vector-vector arithmetic, the
// seventh family) named as the very next one: "Reference's bool-modifier
// semantics for a vector-vector comparison keep every matched pair
// (emitting 1.0/0.0) regardless of type compatibility".
//
// That characterisation turns out to be WRONG for the general case, and
// this file exists because the wrong version was assumed rather than
// checked. Reference Prometheus's `vectorElemBinop` (promql/engine.go,
// verified directly against the vendored `tsouza/prometheus:cerberus-
// parser` fork, not assumed from any issue text) is the SAME function
// [vectorVectorArithmeticOverMixedExpHistogramSetOp]'s own header already
// analysed for arithmetic — its `hlhs`/`hrhs` nil-ness switch answers
// comparisons too, and the crucial detail is WHERE the `bool`-modifier
// override happens relative to that switch: `VectorBinop`'s `doBinOp`
// closure checks `vectorElemBinop`'s returned `err` FIRST
// (`if err != nil { lastErr = err; return }`) and only applies the
// `bool`-modifier override (`if returnBool { histogramValue = nil;
// floatValue = keep ? 1.0 : 0.0 }`) AFTER that check. Every combination
// `vectorElemBinop` answers with `NewIncompatibleTypesInBinOpInfo` (an
// `err`, not merely a `keep=false`) is therefore dropped OUTRIGHT — the
// `bool`-modifier override code path is never reached for it, regardless
// of `bool`. This is a materially different rule from "keep every pair
// regardless of type compatibility".
//
// # The four combinations
//
// [chplan.MixedVectorJoin] joins the two already-lowered Mixed
// VectorSetOp operands the same way
// [vectorVectorArithmeticOverMixedExpHistogramSetOp] does; each matched
// row pair carries its own pair of discriminators (L, R ∈ {float,
// histogram}). For EVERY comparison op (`==`, `!=`, `<`, `<=`, `>`,
// `>=`), `vectorElemBinop`'s answer is:
//
//   - float, float: the ordinary comparison — `keep = (lhs <op> rhs)`,
//     the surviving row's Value is `lhs` UNCHANGED (not a derived
//     boolean). No `err`, so this combination always reaches the
//     `bool`-modifier override when `bool` is present.
//   - float, histogram / histogram, float: EVERY op, WITH or WITHOUT
//     `bool`, answers `NewIncompatibleTypesInBinOpInfo` — an `err`, not a
//     `keep=false`. Always dropped outright, `bool` or not.
//   - histogram, histogram, `==`/`!=`: NOT an error — `keep =
//     hlhs.Equals(hrhs)` (or its negation for `!=`), and the surviving
//     row's payload is `hlhs`, LHS's own histogram, UNCHANGED (no merge,
//     no scale reconciliation) — the exact same structural-equality
//     fold `histogram_native_binop_eq.go` already implements for the
//     plain (non-mixed) both-histogram case, reused here field-by-field
//     rather than re-derived. Because this is `keep`, not `err`, it too
//     reaches the `bool`-modifier override when `bool` is present.
//   - histogram, histogram, `<`/`<=`/`>`/`>=`: ALSO
//     `NewIncompatibleTypesInBinOpInfo` — histograms have no ordering,
//     and reference answers this with the SAME error path as the
//     mismatched-type combinations above, `bool` or not. This is the one
//     combination worth stating explicitly because it is the most
//     surprising: EVEN WITH `bool`, `(a or b) > bool (a or b)` between
//     two histogram-shaped rows never emits a 0.0 — it drops the pair,
//     exactly as if `bool` were absent.
//
// Put differently: `bool` only ever changes what happens to a pair
// `vectorElemBinop` did NOT error on (float,float always; histogram,
// histogram only for `==`/`!=`) — from "filter on the comparison result"
// to "always keep, value is the comparison result as 1.0/0.0". It never
// rescues a pair that would otherwise have been dropped as an
// incompatible-type error.
//
// # Output shape
//
// Without `bool`: the surviving row forwards L's OWN canonical quartet
// and nine Histogram*Column fields, plus L's own discriminator
// (unambiguous — the only surviving combinations are (float,float) and
// (histogram,histogram), so L's and R's discriminators always agree on a
// kept row) — [chplan.RowShapeOf] resolves this to [chplan.MixedRowShape]
// via its *Project case, mirroring [lowerMixedVVScaledArithmetic]'s own
// shape. Unlike that arithmetic fold, MetricName is L's own name
// UNCHANGED rather than forced to `""`: comparisons never
// `changesMetricSchema` (reference's own `changesMetricSchema` switch
// lists only ADD/SUB/MUL/DIV/POW/MOD/ATAN2), so `resultMetric` never
// drops the name for a non-`bool` comparison.
//
// With `bool`: reference forces the histogram payload to nil and the
// name to "" for EVERY surviving row (`dropMetricName := ...&&
// returnBool`) — the output is always FLOAT-valued, a plain 4-column
// Sample [chplan.RowShapeOf] resolves to [chplan.SampleRowShape] via its
// default case, mirroring histogram_native_mixed_or_arithmetic.go's own
// drop-family Project.
// The Value fold is `if(<bothFloat>, toFloat64(<float compare>),
// toFloat64(<histogram field compare>))` for `==`/`!=` (the only two ops
// where the histogram,histogram branch can ever be reached — the `if`'s
// condition already excludes histogram,histogram for the ordering ops,
// so the histogram branch is unreachable there but still safe to build:
// [chplan.FnIf] evaluates it against real column values on a filtered-out
// row, just never surfaces the result).
//
// # What this file does NOT attempt
//
// group_left()/group_right() (Card other than CardOneToOne) IS supported,
// for the identical reason
// [vectorVectorArithmeticOverMixedExpHistogramSetOp]'s own header gives:
// [chplan.MixedVectorJoin]'s JOIN hands every output row its own
// independent L/R discriminator pair regardless of cardinality, so the
// keep/drop decision and payload fold below are UNCHANGED by Card — only
// the output Attributes (via [mixedVVOutputAttributesExpr]) need to learn
// the manySide+Include overlay; the Timestamp similarly picks the "many"
// side (mirroring plain [chplan.VectorJoin]'s `outerSide`). MetricName/
// Value/the nine Histogram*Column fields for the non-`bool` filter stay
// sourced from L (the operator's syntactic LHS) UNCONDITIONALLY, exactly
// as they already were — reference always preserves vector1's (LHS's) own
// sample for a bare V-V comparison regardless of which side plays "many"/
// "one" (see [lowerMixedVVCompareFilter]'s own doc), so no Card-dependent
// choice is needed there. A mixed-`or` operand paired with a plain
// (non-mixed) vector remains its own separately-tracked gap (the
// rejection-parity catalogue's existing `binary.go` divergence entry),
// untouched by this file.
func comparisonVectorVectorOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, returnBool, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || !b.Op.IsComparisonOperator() {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}

	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		// Exactly one side scalar is comparisonOverMixedExpHistogramSetOp's
		// own shape (histogram_native_mixed_or_comparison.go), not this
		// one.
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}

	card = chplan.CardOneToOne
	if b.VectorMatching != nil {
		switch b.VectorMatching.Card {
		case parser.CardOneToOne:
		case parser.CardManyToOne:
			card = chplan.CardManyToOne
		case parser.CardOneToMany:
			card = chplan.CardOneToMany
		default:
			// CardManyToMany: the parser only ever sets this for the
			// `and`/`or`/`unless` set operators, already excluded above
			// via !b.Op.IsComparisonOperator() — unreachable here in
			// practice, rejected defensively rather than silently
			// treated as one-to-one.
			return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
		}
		if len(b.VectorMatching.Include) > 0 {
			include = append([]string(nil), b.VectorMatching.Include...)
		}
	}

	lhsSetOp, lhsOk := mixedExpHistogramSetOp(b.LHS, s, ctx)
	if !lhsOk {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	rhsSetOp, rhsOk := mixedExpHistogramSetOp(b.RHS, s, ctx)
	if !rhsOk {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	return lhsSetOp, rhsSetOp, chOp, mixedExpHistogramMatch(b), card, include, b.ReturnBool, true
}

// lowerComparisonVectorVectorOverMixedExpHistogramSetOp lowers the shape
// [comparisonVectorVectorOverMixedExpHistogramSetOp] recognised: build the
// two Mixed operands, join them ([chplan.MixedVectorJoin]), and answer
// either the histogram-preserving filter (no `bool`,
// [lowerMixedVVCompareFilter]) or the always-float `bool` fold
// ([lowerMixedVVCompareBool]) — see this file's header for the
// four-combination semantics each answers.
func lowerComparisonVectorVectorOverMixedExpHistogramSetOp(lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, returnBool bool, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	leftNode, err := lowerMixedExpHistogramSetOp(lhsSetOp, s, ctx)
	if err != nil {
		return nil, err
	}
	rightNode, err := lowerMixedExpHistogramSetOp(rhsSetOp, s, ctx)
	if err != nil {
		return nil, err
	}

	join := &chplan.MixedVectorJoin{
		Left:             leftNode,
		Right:            rightNode,
		Match:            match,
		Card:             card,
		Include:          include,
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}

	if returnBool {
		return lowerMixedVVCompareBool(join, op, s), nil
	}
	return lowerMixedVVCompareFilter(join, op, s), nil
}

// mixedVVEqOrNe reports whether op is `==`/`!=` — the only two comparison
// ops for which reference's `vectorElemBinop` answers a histogram,
// histogram pair without an incompatible-types error (see this file's
// header).
func mixedVVEqOrNe(op chplan.BinaryOp) bool {
	return op == chplan.OpEq || op == chplan.OpNe
}

// mixedVVHistogramFieldColumns names the nine Histogram*Column fields
// [chplan.MixedVectorJoin] carries from both sides — the same field list
// [mixedVectorJoinFieldCols] (internal/chsql/mixed_vector_join.go) uses,
// duplicated here for the identical internal/promql-may-not-depend-on-
// internal/chsql reason [mixedJoinFieldAlias]'s own doc gives.
func mixedVVHistogramFieldColumns() []string {
	return []string{
		chplan.HistogramCountColumn, chplan.HistogramSumColumn, chplan.HistogramScaleColumn,
		chplan.HistogramZeroThresholdColumn, chplan.HistogramZeroCountColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
	}
}

// mixedVVHistogramFieldsExpr renders the field-by-field structural
// comparison [FloatHistogram.Equals] performs, reading both sides
// straight off the [chplan.MixedVectorJoin]'s own `_mvj_L_<field>` /
// `_mvj_R_<field>` columns — the AND of every field comparing equal for
// `==`, or its De Morgan dual (the OR of any one field comparing unequal)
// for `!=` (ne=true). Mirrors
// [histogramCompareFieldsExpr]'s (histogram_native_binop_eq.go) own
// construction exactly, minus that function's groupArrayIf indirection:
// [chplan.MixedVectorJoin] is a real SQL JOIN, so both sides' fields are
// already columns of the SAME row, with no UnionAll+Aggregate collection
// step needed to bring them together.
func mixedVVHistogramFieldsExpr(ne bool) chplan.Expr {
	op, combine := chplan.OpEq, chplan.OpAnd
	if ne {
		op, combine = chplan.OpNe, chplan.OpOr
	}
	var expr chplan.Expr
	for _, f := range mixedVVHistogramFieldColumns() {
		cond := &chplan.Binary{
			Op:    op,
			Left:  mixedJoinFieldRef(mixedVVJoinSideL, f),
			Right: mixedJoinFieldRef(mixedVVJoinSideR, f),
		}
		if expr == nil {
			expr = cond
			continue
		}
		expr = &chplan.Binary{Op: combine, Left: expr, Right: cond}
	}
	return expr
}

// lowerMixedVVCompareFilter answers a comparison WITHOUT `bool`: a
// [chplan.Filter] keeping only the combinations `vectorElemBinop` does
// not error on — float,float satisfying the comparison always; plus,
// for `==`/`!=` only, histogram,histogram satisfying the structural
// field-equality/inequality — forwarding L's own canonical quartet
// (MetricName UNCHANGED — comparisons never changesMetricSchema),
// Histogram*Column fields, and discriminator, ALL UNCONDITIONALLY from L
// regardless of Card: reference always preserves vector1's (the
// operator's syntactic LHS's) own sample for a bare V-V comparison, and
// that choice is independent of which side group_left()/group_right()
// names as "many". Only the output Attributes and Timestamp are
// Card-aware (via [mixedVVOutputAttributesExpr] / [mixedVVManySide]),
// mirroring plain [chplan.VectorJoin]'s own split between its
// hardcoded-LHS bare-comparison Value and its `outerSide`-picked
// Timestamp (internal/chsql/vector_join.go). See this file's header for
// why an ordering op (`<`/`<=`/`>`/`>=`) never admits a histogram,
// histogram pair.
func lowerMixedVVCompareFilter(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics) chplan.Node {
	bothFloat := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  mixedVVDiscEq(mixedVVJoinSideL, 0),
		Right: mixedVVDiscEq(mixedVVJoinSideR, 0),
	}
	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)
	floatCmp := &chplan.Binary{Op: op, Left: lValue, Right: rValue}
	keep := chplan.Expr(&chplan.Binary{Op: chplan.OpAnd, Left: bothFloat, Right: floatCmp})

	if mixedVVEqOrNe(op) {
		bothHist := &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  mixedVVDiscEq(mixedVVJoinSideL, 1),
			Right: mixedVVDiscEq(mixedVVJoinSideR, 1),
		}
		histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)
		histKeep := &chplan.Binary{Op: chplan.OpAnd, Left: bothHist, Right: histCmp}
		keep = &chplan.Binary{Op: chplan.OpOr, Left: keep, Right: histKeep}
	}

	filtered := &chplan.Filter{Input: join, Predicate: keep}

	projs := []chplan.Projection{
		{Expr: mixedJoinFieldRef(mixedVVJoinSideL, s.MetricNameColumn), Alias: s.MetricNameColumn},
		{
			Expr:  mixedVVOutputAttributesExpr(join.Match, join.Card, join.Include, s.AttributesColumn),
			Alias: s.AttributesColumn,
		},
		{Expr: mixedJoinFieldRef(mixedVVManySide(join.Card), s.TimestampColumn), Alias: s.TimestampColumn},
		{Expr: lValue, Alias: s.ValueColumn},
	}
	for _, col := range mixedVVHistogramFieldColumns() {
		projs = append(projs, chplan.Projection{Expr: mixedJoinFieldRef(mixedVVJoinSideL, col), Alias: col})
	}
	projs = append(projs, chplan.Projection{
		Expr:  mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn),
		Alias: mixedDiscriminatorColumn,
	})

	return &chplan.Project{Input: filtered, Projections: projs}
}

// lowerMixedVVCompareBool answers a comparison WITH `bool`: every
// combination `vectorElemBinop` does NOT error on is emitted
// unconditionally (the incompatible-type combinations — float,histogram;
// histogram,float; and, for the ordering ops, histogram,histogram — are
// still dropped, `bool` or not, per this file's header), Value is the
// comparison result as 1.0/0.0, MetricName is forced to "" (reference's
// `dropMetricName` for a `bool`-modified V-V binop), and the output has
// no histogram payload at all — a plain 4-column [chplan.SampleRowShape].
func lowerMixedVVCompareBool(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics) chplan.Node {
	bothFloat := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  mixedVVDiscEq(mixedVVJoinSideL, 0),
		Right: mixedVVDiscEq(mixedVVJoinSideR, 0),
	}
	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)
	floatCmp := &chplan.Binary{Op: op, Left: lValue, Right: rValue}

	keep := chplan.Expr(bothFloat)
	valueExpr := chplan.Expr(&chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{floatCmp}})

	if mixedVVEqOrNe(op) {
		bothHist := &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  mixedVVDiscEq(mixedVVJoinSideL, 1),
			Right: mixedVVDiscEq(mixedVVJoinSideR, 1),
		}
		histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)
		keep = &chplan.Binary{Op: chplan.OpOr, Left: bothFloat, Right: bothHist}
		valueExpr = &chplan.FuncCall{
			Fn: chplan.FnIf,
			Args: []chplan.Expr{
				bothFloat,
				&chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{floatCmp}},
				&chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{histCmp}},
			},
		}
	}

	filtered := &chplan.Filter{Input: join, Predicate: keep}

	return &chplan.Project{
		Input: filtered,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{
				Expr:  mixedVVOutputAttributesExpr(join.Match, join.Card, join.Include, s.AttributesColumn),
				Alias: s.AttributesColumn,
			},
			{Expr: mixedJoinFieldRef(mixedVVManySide(join.Card), s.TimestampColumn), Alias: s.TimestampColumn},
			{Expr: valueExpr, Alias: s.ValueColumn},
		},
	}
}
