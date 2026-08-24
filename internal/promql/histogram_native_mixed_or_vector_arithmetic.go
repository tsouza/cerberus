package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_vector_arithmetic.go lowers a vector-vector
// arithmetic binop (`+`, `-`, `*`, `/`) whose BOTH operands are themselves
// a mixed float/histogram `or` (histogram_native_mixed_or.go's own
// #2330/#2335 shape) — cerberus issue #2449's seventh wrapper family, and
// the piece every prior pass on this issue (sum/avg #2346,
// label_replace/label_join, single-arg math functions, drop-family
// scalar arithmetic, scalar comparisons, and MUL/histogram-left-DIV
// scalar scaling) named as its own remaining scope: unlike every wrapper
// above, BOTH sides of the join can independently be float- or
// histogram-shaped per matched row pair, so the SAME output column
// (Value, and each of the nine Histogram*Column fields) reads a
// different real interpretation depending on which of four combinations
// a given pair turns out to carry — the discriminator-keyed
// `chplan.Case`/CH `if()` mechanism the issue itself named (built here as
// `chplan.FnIf`/`chplan.FnMultiIf` [chplan.FuncCall]s — chplan grew no
// dedicated Case node, since a FuncCall Fn was already the lighter-weight
// way to reach CH's `if()`/`multiIf()`), rather than a mechanical
// extension of any single-histogram-side wrapper already landed.
//
// # The four combinations
//
// [chplan.MixedVectorJoin] joins the two already-lowered Mixed
// VectorSetOp operands on labels; each matched row pair carries its own
// pair of discriminators (L, R ∈ {float, histogram}). Reference
// Prometheus's `vectorElemBinop` (promql/engine.go, verified against the
// vendored `tsouza/prometheus:cerberus-parser` fork, NOT assumed from
// this issue's own acceptance text) is the SAME function VectorBinop
// (vector-vector) and VectorscalarBinop (vector-scalar) both call — its
// `hlhs`/`hrhs` nil-ness switch is exactly the four combinations here,
// and is the authoritative answer this file follows:
//
//   - float, float: every arithmetic op computes normally
//     (`lhs +/-*// rhs`).
//   - float, histogram (L float, R histogram): ONLY `*` keeps —
//     `hrhs.Copy().Mul(lhs)`, R's histogram scaled by L's float value.
//     `+`, `-`, `/` all drop the pair (`NewIncompatibleTypesInBinOpInfo`,
//     `keep=false`).
//   - histogram, float (L histogram, R float): `*` and `/` keep —
//     `hlhs.Copy().Mul(rhs)` / `hlhs.Copy().Div(rhs)`. `+`, `-` drop.
//   - histogram, histogram: `+` and `-` keep in reference (a genuine
//     histogram merge/subtract, `hlhs.Copy().Add(hrhs)` /
//     `.Sub(hrhs)`); `*`, `/` drop (reference has no histogram×histogram
//     product/quotient).
//
// This file computes the first three combinations exactly as reference
// does. The FOURTH — histogram,histogram `+`/`-` — is NOT computed here;
// every histogram,histogram pair is dropped for EVERY op, including
// `+`/`-`, where reference would keep one. This is a deliberate,
// documented scope cut, not an oversight: a real histogram+histogram
// merge needs bucket-scale reconciliation across the pair's two
// independently-scaled bucket ladders — the UnionAll+groupArray+Aggregate
// machinery histogram_native_binop.go's mergeTwoHistogramProjections
// already built for the plain (non-mixed) both-histogram case — which
// does not fit inside a flat per-row JOIN projection the way a scale-only
// fold (MUL/DIV, all three combinations above) does. Building that
// machinery INSIDE this join is a materially different, separately-scoped
// piece of work, tracked as remaining scope under #2449 rather than
// attempted here (see this file's registration point in lower.go and the
// PR that added this file for the full "what remains" accounting).
// `*`/`/` dropping histogram,histogram matches reference exactly — no gap
// there.
//
// Comparisons (`==`, `!=`, `<`, `<=`, `>`, `>=`, with/without `bool`) are
// ALSO out of this file's scope, for the same reason
// histogram_native_mixed_or_arithmetic.go's header gives for the scalar
// case: reference's `bool`-modifier semantics for a vector-vector
// comparison keep EVERY matched pair (emitting 1.0/0.0) regardless of
// type compatibility — a structurally different shape from this file's
// "drop the incompatible pair outright" fold, and its own separately-
// scoped follow-on, tracked under #2449.
//
// group_left()/group_right() (any Card other than CardOneToOne) is
// likewise out of scope: broadcasting the "many" side while ALSO
// discriminating each row's own payload compounds the four-combination
// fold with the Include-label broadcast [chplan.MixedVectorJoin]'s own
// doc names as the reason it carries no Card field at all.
// [vectorVectorArithmeticOverMixedExpHistogramSetOp] rejects that shape
// outright (falls through to the pre-existing rejection) rather than
// mis-widening it.
func vectorVectorArithmeticOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || b.Op.IsSetOperator() || b.Op.IsComparisonOperator() {
		return nil, nil, "", chplan.VectorMatch{}, false
	}
	switch b.Op {
	case parser.ADD, parser.SUB, parser.MUL, parser.DIV:
	default:
		// POW, MOD, ATAN2: reference drops every histogram-involving
		// combination for these ops too, but a bare float,float pair
		// still computes normally — left unattempted here (falls through
		// to the pre-existing rejection even for the float,float case),
		// tracked as remaining scope under #2449 alongside comparisons.
		return nil, nil, "", chplan.VectorMatch{}, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, "", chplan.VectorMatch{}, false
	}

	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		// Exactly one side scalar is arithmeticOverMixedExpHistogramSetOp's
		// / mulOrDivScaleOverMixedExpHistogramSetOp's shape, not this one.
		return nil, nil, "", chplan.VectorMatch{}, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, "", chplan.VectorMatch{}, false
	}

	if b.VectorMatching != nil && b.VectorMatching.Card != parser.CardOneToOne {
		// group_left()/group_right() — deliberately out of scope, see
		// this file's header.
		return nil, nil, "", chplan.VectorMatch{}, false
	}

	lhsSetOp, lhsOk := mixedExpHistogramSetOp(b.LHS, s, ctx)
	if !lhsOk {
		return nil, nil, "", chplan.VectorMatch{}, false
	}
	rhsSetOp, rhsOk := mixedExpHistogramSetOp(b.RHS, s, ctx)
	if !rhsOk {
		return nil, nil, "", chplan.VectorMatch{}, false
	}
	return lhsSetOp, rhsSetOp, chOp, mixedExpHistogramMatch(b), true
}

// mixedVVJoinSideL / mixedVVJoinSideR name the two sides of a
// [chplan.MixedVectorJoin] the way [chplan.MixedVectorJoin]'s own emitter
// (internal/chsql/mixed_vector_join.go) qualifies its output columns.
const (
	mixedVVJoinSideL = "L"
	mixedVVJoinSideR = "R"
)

// mixedJoinFieldAlias names a MixedVectorJoin output column. MUST match
// chsql's mixedVectorJoinAlias (internal/chsql/mixed_vector_join.go)
// byte-for-byte; the two are independent literals rather than a shared
// constant because internal/promql may not depend on internal/chsql (see
// .go-arch-lint.yml — the same boundary [chplan.ManyToManyMatchMessage]'s
// doc explains, and the same duplication histogram_native_binop_card.go's
// histJoinFieldAlias already carries for HistogramVectorJoin).
func mixedJoinFieldAlias(side, col string) string {
	return "_mvj_" + side + "_" + col
}

// mixedJoinFieldRef is a ColumnRef reading side's own value of col off a
// MixedVectorJoin's output.
func mixedJoinFieldRef(side, col string) chplan.Expr {
	return &chplan.ColumnRef{Name: mixedJoinFieldAlias(side, col)}
}

// mixedVVDiscEq renders `<side>'s own discriminator = <want>` (0 or 1).
func mixedVVDiscEq(side string, want int64) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  mixedJoinFieldRef(side, mixedDiscriminatorColumn),
		Right: &chplan.LitInt{V: want},
	}
}

// mixedVVOutputAttributesExpr renders the CardOneToOne output Attributes
// expression: the join's LHS-side Attributes, reduced to the matching
// label set per Prometheus's `resultMetric` Keep/Del rule — the
// chplan.Expr-level mirror of chsql's outputMatchSetFrag
// (internal/chsql/vector_join.go), needed here because
// [chplan.MixedVectorJoin] is deliberately "dumb" about output shaping
// (see its own doc) and internal/promql cannot reach chsql's private
// helper.
func mixedVVOutputAttributesExpr(m chplan.VectorMatch, attrs chplan.Expr) chplan.Expr {
	if len(m.Labels) == 0 {
		if m.On {
			// on() with no labels: Keep() with nothing yields an empty
			// map. A constant-false mapFilter predicate keeps the
			// Map(String, String) type.
			return &chplan.FuncCall{
				Fn: chplan.FnMapFilter,
				Args: []chplan.Expr{
					&chplan.Lambda{Params: []string{"k", "v"}, Body: &chplan.LitBool{V: false}},
					attrs,
				},
			}
		}
		// Default matching or ignoring() with no labels: nothing to drop.
		return attrs
	}
	list := make([]chplan.Expr, len(m.Labels))
	for i, lbl := range m.Labels {
		list[i] = &chplan.LitString{V: lbl}
	}
	// on(labels): Keep() -> k IN (...). ignoring(labels): Del() -> the
	// negated form (InList.Negated), the flat constant-depth equivalent
	// of `NOT (k IN (...))`.
	return &chplan.FuncCall{
		Fn: chplan.FnMapFilter,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body:   &chplan.InList{Left: &chplan.BareIdent{Name: "k"}, List: list, Negated: !m.On},
			},
			attrs,
		},
	}
}

// lowerVectorVectorArithmeticOverMixedExpHistogramSetOp lowers the shape
// [vectorVectorArithmeticOverMixedExpHistogramSetOp] recognised: build the
// two Mixed VectorSetOp operands, join them
// ([chplan.MixedVectorJoin]), and answer either the float-only fold
// (`+`/`-`, [lowerMixedVVFloatOnlyArithmetic]) or the scaled fold
// (`*`/`/`, [lowerMixedVVScaledArithmetic]) — see this file's header for
// the four-combination semantics each answers.
func lowerVectorVectorArithmeticOverMixedExpHistogramSetOp(lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}

	if op == chplan.OpAdd || op == chplan.OpSub {
		return lowerMixedVVFloatOnlyArithmetic(join, op, s), nil
	}
	return lowerMixedVVScaledArithmetic(join, op, s), nil
}

// lowerMixedVVFloatOnlyArithmetic answers `+`/`-`: reference keeps ONLY
// the float,float combination for these two ops (see this file's
// header), so the output is a plain canonical-quartet fold — no
// discriminator, no Histogram*Column fields, [chplan.RowShapeOf] resolves
// it to [chplan.SampleRowShape] via its default case, exactly like
// histogram_native_mixed_or_arithmetic.go's own drop-family Project.
func lowerMixedVVFloatOnlyArithmetic(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics) chplan.Node {
	keepFF := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  mixedVVDiscEq(mixedVVJoinSideL, 0),
		Right: mixedVVDiscEq(mixedVVJoinSideR, 0),
	}
	filtered := &chplan.Filter{Input: join, Predicate: keepFF}

	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)

	return &chplan.Project{
		Input: filtered,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{
				Expr:  mixedVVOutputAttributesExpr(join.Match, mixedJoinFieldRef(mixedVVJoinSideL, s.AttributesColumn)),
				Alias: s.AttributesColumn,
			},
			{Expr: mixedJoinFieldRef(mixedVVJoinSideL, s.TimestampColumn), Alias: s.TimestampColumn},
			{Expr: &chplan.Binary{Op: op, Left: lValue, Right: rValue}, Alias: s.ValueColumn},
		},
	}
}

// lowerMixedVVScaledArithmetic answers `*`/`/`: reference keeps THREE of
// the four combinations for these two ops (float,float; and, depending on
// op, float,histogram and/or histogram,float — see this file's header),
// so the output is the full fourteen-column Mixed shape,
// [chplan.RowShapeOf] resolving it to [chplan.MixedRowShape] via its
// *Project case (this Project republishes [mixedDiscriminatorColumn]).
//
// Value is scaled UNCONDITIONALLY (`L.Value <op> R.Value` on every
// surviving row) rather than branched per combination — the same
// harmless-placeholder-churn argument histogram_native_mixed_or_scale.go's
// header makes for its own single-histogram-side Value fold: on a
// float,float row both operands are real and the arithmetic is genuine;
// on a float,histogram or histogram,float row exactly one operand is the
// meaningless [chplan.HistogramProjection] placeholder, and decode never
// reads Value on a discriminator-1 row regardless of what that
// placeholder scaled into.
//
// Each of the nine Histogram*Column fields DOES need a per-row source
// pick ([mixedVVFlipSource]): the real histogram sits on L for a
// histogram,float pair and on R for a float,histogram pair, so which
// side's own field value is meaningful genuinely differs by row — unlike
// Value, reading the wrong side's placeholder here would leak a
// non-placeholder-shaped answer (the OTHER side's own real, differently-
// scaled histogram field) rather than a harmless zero.
func lowerMixedVVScaledArithmetic(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics) chplan.Node {
	var keep chplan.Expr
	if op == chplan.OpDiv {
		// DIV only ever keeps histogram,float (plus float,float) — a
		// histogram-shaped R never scales a numerator (see this file's
		// header: reference has no float,histogram or
		// histogram,histogram DIV).
		keep = mixedVVDiscEq(mixedVVJoinSideR, 0)
	} else {
		// MUL keeps every combination except histogram,histogram.
		bothHist := &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  mixedVVDiscEq(mixedVVJoinSideL, 1),
			Right: mixedVVDiscEq(mixedVVJoinSideR, 1),
		}
		keep = &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{bothHist}}
	}
	filtered := &chplan.Filter{Input: join, Predicate: keep}

	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)
	rIsHist := mixedVVDiscEq(mixedVVJoinSideR, 1)

	var discExpr chplan.Expr
	if op == chplan.OpDiv {
		// Kept rows are float,float (0,0) or histogram,float (1,0) — the
		// output discriminator is always L's own, forwarded unchanged.
		discExpr = mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn)
	} else {
		lIsHist := mixedVVDiscEq(mixedVVJoinSideL, 1)
		discExpr = &chplan.FuncCall{
			Fn: chplan.FnIf,
			Args: []chplan.Expr{
				&chplan.Binary{Op: chplan.OpOr, Left: lIsHist, Right: rIsHist},
				&chplan.LitInt{V: 1}, &chplan.LitInt{V: 0},
			},
		}
	}

	projs := []chplan.Projection{
		{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
		{
			Expr:  mixedVVOutputAttributesExpr(join.Match, mixedJoinFieldRef(mixedVVJoinSideL, s.AttributesColumn)),
			Alias: s.AttributesColumn,
		},
		{Expr: mixedJoinFieldRef(mixedVVJoinSideL, s.TimestampColumn), Alias: s.TimestampColumn},
		{Expr: &chplan.Binary{Op: op, Left: lValue, Right: rValue}, Alias: s.ValueColumn},
	}

	scalarFields := []string{chplan.HistogramCountColumn, chplan.HistogramSumColumn, chplan.HistogramZeroCountColumn}
	for _, col := range scalarFields {
		whenR := scaleHistogramScalarExpr(op, mixedJoinFieldRef(mixedVVJoinSideR, col), lValue)
		whenL := scaleHistogramScalarExpr(op, mixedJoinFieldRef(mixedVVJoinSideL, col), rValue)
		projs = append(projs, chplan.Projection{Expr: mixedVVFlipSource(op, rIsHist, whenR, whenL), Alias: col})
	}

	ladderFields := []string{chplan.HistogramPositiveBucketCountsColumn, chplan.HistogramNegativeBucketCountsColumn}
	for _, col := range ladderFields {
		whenR := scaleHistogramLadderExpr(op, mixedJoinFieldRef(mixedVVJoinSideR, col), lValue)
		whenL := scaleHistogramLadderExpr(op, mixedJoinFieldRef(mixedVVJoinSideL, col), rValue)
		projs = append(projs, chplan.Projection{Expr: mixedVVFlipSource(op, rIsHist, whenR, whenL), Alias: col})
	}

	forwardedFields := []string{
		chplan.HistogramScaleColumn, chplan.HistogramZeroThresholdColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramNegativeOffsetColumn,
	}
	for _, col := range forwardedFields {
		whenR := mixedJoinFieldRef(mixedVVJoinSideR, col)
		whenL := mixedJoinFieldRef(mixedVVJoinSideL, col)
		projs = append(projs, chplan.Projection{Expr: mixedVVFlipSource(op, rIsHist, whenR, whenL), Alias: col})
	}

	projs = append(projs, chplan.Projection{Expr: discExpr, Alias: mixedDiscriminatorColumn})

	return &chplan.Project{Input: filtered, Projections: projs}
}

// mixedVVFlipSource picks, per row, which side's field value feeds a
// histogram-shaped output field of a MUL/DIV vector-vector fold. MUL may
// keep either operand order (float,histogram or histogram,float), so it
// branches on rIsHist — whether R is the histogram-shaped operand for
// THIS row. DIV only ever keeps histogram,float (see
// [lowerMixedVVScaledArithmetic]), so it always reads from L
// unconditionally: on the ALSO-kept float,float rows this resolves to
// scaling L's own zero placeholder by R's real float value, which is
// exactly as harmless as histogram_native_mixed_or_scale.go's own
// single-histogram-side placeholder scaling (decode never reads a
// Histogram*Column field on a discriminator-0 row).
func mixedVVFlipSource(op chplan.BinaryOp, rIsHist, whenR, whenL chplan.Expr) chplan.Expr {
	if op == chplan.OpDiv {
		return whenL
	}
	return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{rIsHist, whenR, whenL}}
}
