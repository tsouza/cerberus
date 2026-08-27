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
// does. The FOURTH — histogram,histogram `+`/`-` — IS also computed here,
// since cerberus issue #2449's remaining "histogram,histogram +/-" wrapper
// family: [lowerMixedVVAdditiveArithmetic] keeps a histogram,histogram
// pair for `+`/`-` too, folding it through a genuine bucket-scale
// reconciliation rather than a scale-only fold. That reconciliation is
// NOT reimplemented here — histogram_native_binop.go's
// [mergeTwoHistogramProjections] already built it for the plain
// (non-mixed) both-histogram case via UnionAll+groupArray+Aggregate, and
// the structural pieces it composes ([plainArraySum],
// [expHistogramMergeOffsetExpr], [histogramBinopMergedBucketsExpr], and
// the shared budget guard [histogramBinopBucketWidthBudgetGuardExpr]) are
// reused UNCHANGED here — only the mechanism that FEEDS them the
// "group's" two-element arrays differs: the non-mixed path collects them
// via an Aggregate's groupArray over a two-row UnionAll, while this path
// already has its two operands JOINED into one row per matched pair
// ([chplan.MixedVectorJoin]), so [mixedVVHistMergeInputProjections]
// builds the SAME two-element arrays directly via `array(L, R)` in a
// plain Project — no Aggregate needed, since the "group" is always
// exactly the one matched row's own L/R pair. See
// [mixedVVHistMergeInputProjections] / [mixedVVHistMergeOutputProjections]
// for the composition with this file's own four-combination discriminator
// fold. `*`/`/` dropping histogram,histogram matches reference exactly —
// no gap there.
//
// Comparisons (`==`, `!=`, `<`, `<=`, `>`, `>=`, with/without `bool`) were
// ALSO out of this file's scope, for the reason
// histogram_native_mixed_or_arithmetic.go's header gives for the scalar
// case: a comparison lowers through a structurally different Filter /
// bool-Project shape than this file's single arithmetic Project. They are
// now answered by histogram_native_mixed_or_vector_comparison.go — whose
// own header corrects an assumption this paragraph used to make (that
// reference's `bool` modifier keeps every matched pair regardless of type
// compatibility; it does not — an incompatible-type pair is dropped
// outright, `bool` or not, because reference's `err` check runs before
// the `bool`-modifier override).
//
// POW/MOD/ATAN2 (`^`, `%`, `atan2`) are a separate case from the four
// arithmetic ops above, verified against the SAME vendored
// `tsouza/prometheus:cerberus-parser` fork's `vectorElemBinop`: reference
// drops EVERY histogram-involving combination for these three ops —
// float,histogram; histogram,float; AND histogram,histogram all hit
// `NewIncompatibleTypesInBinOpInfo` — unlike MUL/DIV, which keep three of
// the four combinations, POW/MOD/ATAN2 keep ONLY float,float. That float,
// float result is computed exactly as the plain (non-mixed) float path
// already does, via [chplan.OpPow]/[chplan.OpMod]/[chplan.OpAtan2]
// (internal/chsql/builder.go's exprBinary renders `pow(l, r)`/Go-modulo/
// `atan2(l, r)`). [lowerMixedVVFloatOnlyArithmetic] answers this: a
// single keep predicate (both discriminators = 0) ahead of an
// unconditional Value fold and an unconditional forward of L's own
// (guaranteed-placeholder, post-filter) Histogram*Column fields — no
// per-combination branching needed, since only one combination ever
// survives. This is cerberus issue #2449's mixed-or vector-vector
// POW/MOD/ATAN2 piece; a mixed `or` operand paired with a plain
// (non-mixed) vector remains open under the same issue.
//
// group_left()/group_right() (Card other than CardOneToOne) IS supported:
// see this file's [mixedVVManySide] / [mixedVVOneSide] and
// [mixedVVOutputAttributesExpr] — cerberus issue #2449's ninth wrapper
// family. Broadcasting the "many" side while ALSO discriminating each
// row's own payload does NOT compound with the four-combination fold the
// way an earlier pass on this issue assumed: [chplan.MixedVectorJoin]'s
// JOIN still hands every output row its own independent L/R discriminator
// pair regardless of cardinality (a "many"-side row broadcast against the
// SAME collapsed "one"-side row still produces one genuine L/R pair per
// output row), so the per-row Value/Histogram*/discriminator projections
// below are UNCHANGED by Card — only the output Attributes (manySide's
// own full Attributes, optionally overlaid with the "one" side's Include
// labels) and the representative Timestamp (the "many" side's own row,
// mirroring plain [chplan.VectorJoin]'s `outerSide` pick in
// internal/chsql/vector_join.go's emitVectorJoin) need to learn which
// side is which. [vectorVectorArithmeticOverMixedExpHistogramSetOp]
// accepts CardManyToOne/CardOneToMany and threads Card + Include through
// to [chplan.MixedVectorJoin]; the chsql emitter's [mixedVectorJoinRoles]
// (internal/chsql/mixed_vector_join.go) resolves the per-side roleMany/
// roleOne split the identical way plain VectorJoin's own vectorJoinRoles
// does.
func vectorVectorArithmeticOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || b.Op.IsSetOperator() || b.Op.IsComparisonOperator() {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	switch b.Op {
	case parser.ADD, parser.SUB, parser.MUL, parser.DIV, parser.POW, parser.MOD, parser.ATAN2:
	default:
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}

	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		// Exactly one side scalar is arithmeticOverMixedExpHistogramSetOp's
		// / mulOrDivScaleOverMixedExpHistogramSetOp's shape, not this one.
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
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
			// via b.Op.IsSetOperator() — unreachable here in practice,
			// rejected defensively rather than silently treated as
			// one-to-one.
			return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
		}
		if len(b.VectorMatching.Include) > 0 {
			include = append([]string(nil), b.VectorMatching.Include...)
		}
	}

	lhsSetOp, lhsOk := mixedExpHistogramSetOp(b.LHS, s, ctx)
	if !lhsOk {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	rhsSetOp, rhsOk := mixedExpHistogramSetOp(b.RHS, s, ctx)
	if !rhsOk {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	return lhsSetOp, rhsSetOp, chOp, mixedExpHistogramMatch(b), card, include, true
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

// mixedVVManySide and mixedVVOneSide report which of "L"/"R" (the join's
// Left/Right — the operator's syntactic LHS/RHS operand) plays the
// "many"/"one" role for card: CardManyToOne (`group_left`) keeps Left's
// own per-series granularity; CardOneToMany (`group_right`) keeps
// Right's. CardOneToOne has no "many"/"one" distinction (both sides are
// already collapsed to the same matching key by construction), so
// mixedVVManySide defaults to L — the side [mixedVVOutputAttributesExpr]'s
// own CardOneToOne branch already read unconditionally before this pair
// existed. Mirrors internal/promql/histogram_native_binop_card.go's
// histogramCardManySide / histogramCardOneSide for
// [chplan.HistogramVectorJoin]'s identical shape.
func mixedVVManySide(card chplan.VectorCard) string {
	if card == chplan.CardOneToMany {
		return mixedVVJoinSideR
	}
	return mixedVVJoinSideL
}

func mixedVVOneSide(card chplan.VectorCard) string {
	if card == chplan.CardOneToMany {
		return mixedVVJoinSideL
	}
	return mixedVVJoinSideR
}

// mixedVVOutputAttributesExpr renders the join's output Attributes
// expression — the chplan.Expr-level mirror of chsql's
// outputAttributesFrag (internal/chsql/vector_join.go) /
// internal/promql's own histogramCardOutputAttributesExpr
// (histogram_native_binop_card.go), needed here because
// [chplan.MixedVectorJoin] is deliberately "dumb" about output shaping
// (see its own doc) and internal/promql cannot reach chsql's private
// helper.
//
// CardOneToOne: the join's LHS-side Attributes, reduced to the matching
// label set per Prometheus's `resultMetric` Keep/Del rule.
//
// CardManyToOne/CardOneToMany (group_left()/group_right()): the "many"
// side's own full Attributes, optionally overlaid with the "one" side's
// Include labels via `mapConcat` (CH's later-argument-wins map merge).
// The CardOneToOne Keep/Del reduction does NOT apply here — Prometheus
// skips it for a non-one-to-one cardinality, keeping the many side's full
// labels (then overlaying Include). Bare group_left/right (no Include
// labels) leaves the "many" side's Attributes unchanged.
func mixedVVOutputAttributesExpr(m chplan.VectorMatch, card chplan.VectorCard, include []string, attrsCol string) chplan.Expr {
	if card != chplan.CardOneToOne {
		many := mixedJoinFieldRef(mixedVVManySide(card), attrsCol)
		if len(include) == 0 {
			return many
		}
		one := mixedJoinFieldRef(mixedVVOneSide(card), attrsCol)
		list := make([]chplan.Expr, len(include))
		for i, lbl := range include {
			list[i] = &chplan.LitString{V: lbl}
		}
		overlay := &chplan.FuncCall{
			Fn: chplan.FnMapFilter,
			Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{"k", "v"},
					Body:   &chplan.InList{Left: &chplan.BareIdent{Name: "k"}, List: list},
				},
				one,
			},
		}
		return &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{many, overlay}}
	}

	attrs := mixedJoinFieldRef(mixedVVJoinSideL, attrsCol)
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
// ([chplan.MixedVectorJoin]), and answer either the additive fold
// (`+`/`-`, [lowerMixedVVAdditiveArithmetic]) or the scaled fold
// (`*`/`/`, [lowerMixedVVScaledArithmetic]) — see this file's header for
// the four-combination semantics each answers.
func lowerVectorVectorArithmeticOverMixedExpHistogramSetOp(lhsSetOp, rhsSetOp *parser.BinaryExpr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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

	switch op {
	case chplan.OpAdd, chplan.OpSub:
		return lowerMixedVVAdditiveArithmetic(join, op, s, ctx.resourceBounds.HistogramMergeMaxCostUnits), nil
	case chplan.OpMul, chplan.OpDiv:
		return lowerMixedVVScaledArithmetic(join, op, s), nil
	default:
		// POW, MOD, ATAN2: see this file's header — only float,float
		// survives.
		return lowerMixedVVFloatOnlyArithmetic(join, op, s), nil
	}
}

// lowerMixedVVAdditiveArithmetic answers `+`/`-`: reference keeps the
// float,float combination (plain float arithmetic) AND the
// histogram,histogram combination (a genuine merge/subtract, see this
// file's header) for these two ops; float,histogram and histogram,float
// both drop. The output is therefore the full fourteen-column Mixed
// shape — [chplan.RowShapeOf] resolves it to [chplan.MixedRowShape] via
// its *Project case, mirroring [lowerMixedVVScaledArithmetic]'s own
// shape, unlike this function's own float-only predecessor.
//
// Shape:
//
//	Project [canonical quartet + nine Histogram*Column fields + discriminator]
//	  Filter keep=(L.disc = R.disc) AND <merge budget guard, #2428>
//	    Project [mixedVVHistMergeInputProjections: the merge's two-element
//	             arrays, built directly from the join's own L/R columns,
//	             plus every raw join field the outer Project still needs]
//	      MixedVectorJoin
//
// L.disc = R.disc is the keep predicate: both 0 (float,float) or both 1
// (histogram,histogram) survive; a mismatched pair (exactly one side
// histogram-shaped) does not, matching reference's own drop for
// float,histogram / histogram,float under `+`/`-`.
//
// Value is folded unconditionally (`L.Value op R.Value`) on every
// surviving row, exactly as [lowerMixedVVScaledArithmetic]'s own header
// justifies for its Value fold: on a float,float row both operands are
// real; on a histogram,histogram row both are the meaningless
// [chplan.HistogramProjection] placeholder Value=0, and decode never
// reads Value on a discriminator-1 row. The nine Histogram*Column fields
// are folded unconditionally too, via [mixedVVHistMergeOutputProjections]
// — see that function's doc for why running the merge fold over a
// float,float row's own all-zero, empty-bucket placeholders is provably
// harmless rather than merely assumed so.
//
// maxCostUnits is the caller's already-resolved histogram-merge cost
// ceiling (ctx.resourceBounds.HistogramMergeMaxCostUnits, cerberus issue
// #2667), passed straight through to [histogramBinopBucketWidthBudgetGuardExpr].
func lowerMixedVVAdditiveArithmetic(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics, maxCostUnits int64) chplan.Node {
	mergeInputs := &chplan.Project{
		Input:       join,
		Projections: mixedVVHistMergeInputProjections(op, s),
	}

	sameType := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn),
		Right: mixedJoinFieldRef(mixedVVJoinSideR, mixedDiscriminatorColumn),
	}
	filtered := &chplan.Filter{
		Input:     mergeInputs,
		Predicate: andExpr(sameType, histogramBinopBucketWidthBudgetGuardExpr(maxCostUnits)),
	}

	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)

	projs := []chplan.Projection{
		{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
		{
			Expr:  mixedVVOutputAttributesExpr(join.Match, join.Card, join.Include, s.AttributesColumn),
			Alias: s.AttributesColumn,
		},
		{Expr: mixedJoinFieldRef(mixedVVManySide(join.Card), s.TimestampColumn), Alias: s.TimestampColumn},
		{Expr: &chplan.Binary{Op: op, Left: lValue, Right: rValue}, Alias: s.ValueColumn},
	}
	projs = append(projs, mixedVVHistMergeOutputProjections()...)
	projs = append(projs, chplan.Projection{
		Expr:  mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn),
		Alias: mixedDiscriminatorColumn,
	})

	return &chplan.Project{Input: filtered, Projections: projs}
}

// mixedVVMergedZeroThresholdAlias names the intermediate merged
// ZeroThreshold column [mixedVVHistMergeInputProjections] computes and
// [mixedVVHistMergeOutputProjections] reads back. Unlike
// histogram_native_binop.go's non-mixed merge — whose
// [histogramBinopMergeProjections] forwards `s.ZeroThresholdColumn`
// directly and only when the PHYSICAL schema persists one — this join's
// own L/R columns always publish a HistogramZeroThreshold value (real or
// the mixed-or set-op's own 0. placeholder, see
// [mixedVectorSetOpHistogramPlaceholderCols]), so the merge here always
// has one to fold regardless of what the physical schema stores.
const mixedVVMergedZeroThresholdAlias = "_mvv_merged_zero_threshold"

// mixedVVForwardedJoinCols lists the join fields
// [mixedVVHistMergeInputProjections]'s intermediate Project must forward
// unchanged (under their ORIGINAL `_mvj_<side>_<col>` aliases) so that
// [lowerMixedVVAdditiveArithmetic]'s own downstream reads —
// mixedJoinFieldRef, [mixedVVOutputAttributesExpr], the manySide
// Timestamp pick, the sameType discriminator check — keep working
// unmodified against the Project now sitting between them and the raw
// join.
func mixedVVForwardedJoinCols(s schema.Metrics) []string {
	return []string{s.ValueColumn, s.AttributesColumn, s.TimestampColumn, mixedDiscriminatorColumn}
}

// mixedVVHistMergeInputProjections builds the two-element `array(L, R)`
// groups [histogramBinopMergedBucketsExpr] / [expHistogramMergeOffsetExpr]
// / [histogramBinopBucketWidthBudgetGuardExpr] expect under their
// well-known hq* aliases (histogram_quantile.go /
// histogram_native_binop.go) — the SAME aliases
// histogram_native_binop.go's own [histogramBinopMergeAggs] collects via
// an Aggregate's groupArray over a two-row UnionAll for the non-mixed
// case. This join already has both operands aligned on one row per
// matched pair, so the "group of two" is simply `array(L.field,
// R.field)` — no Aggregate needed.
//
// For `-`, R's count-bearing fields (Count, Sum, ZeroCount, both bucket
// ladders) are negated before entering the array — matching reference's
// own Sub-as-negated-Add semantics, and mirroring
// [scaleHistogramProjection]'s identical negation for the non-mixed
// binop's `hpR` (histogram_native_binop.go's
// [lowerExpHistogramHistogramBinop]). Scale, ZeroThreshold and both
// offsets are structural and stay unchanged for either op, exactly as
// [scaleHistogramProjection]'s own passthroughCols do.
//
// Also forwards every raw join field [lowerMixedVVAdditiveArithmetic]'s
// own Filter/Project still need (see [mixedVVForwardedJoinCols]) under
// their original aliases, so this Project is a strict superset of the
// join's own output rather than a narrowing.
func mixedVVHistMergeInputProjections(op chplan.BinaryOp, s schema.Metrics) []chplan.Projection {
	lRef := func(col string) chplan.Expr { return mixedJoinFieldRef(mixedVVJoinSideL, col) }
	rRef := func(col string) chplan.Expr { return mixedJoinFieldRef(mixedVVJoinSideR, col) }
	arr2 := func(l, r chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{l, r}}
	}
	negLitMinus1 := &chplan.LitFloat{V: -1}
	negScalar := func(e chplan.Expr) chplan.Expr {
		if op != chplan.OpSub {
			return e
		}
		return &chplan.Binary{Op: chplan.OpMul, Left: e, Right: negLitMinus1}
	}
	negLadder := func(e chplan.Expr) chplan.Expr {
		if op != chplan.OpSub {
			return e
		}
		return scaleHistogramLadderExpr(chplan.OpMul, e, negLitMinus1)
	}

	projs := []chplan.Projection{
		{
			Expr: &chplan.FuncCall{Fn: chplan.FnLeast, Args: []chplan.Expr{
				lRef(chplan.HistogramScaleColumn), rRef(chplan.HistogramScaleColumn),
			}},
			Alias: hqAggMergedScaleAlias,
		},
		{
			Expr: &chplan.FuncCall{Fn: chplan.FnGreatest, Args: []chplan.Expr{
				lRef(chplan.HistogramZeroThresholdColumn), rRef(chplan.HistogramZeroThresholdColumn),
			}},
			Alias: mixedVVMergedZeroThresholdAlias,
		},
		{Expr: arr2(lRef(chplan.HistogramScaleColumn), rRef(chplan.HistogramScaleColumn)), Alias: hqAggScalesArrayAlias},
		{Expr: arr2(lRef(chplan.HistogramPositiveOffsetColumn), rRef(chplan.HistogramPositiveOffsetColumn)), Alias: hqAggPosOffsetsArrayAlias},
		{
			Expr:  arr2(lRef(chplan.HistogramPositiveBucketCountsColumn), negLadder(rRef(chplan.HistogramPositiveBucketCountsColumn))),
			Alias: hqAggPosBucketsArrayAlias,
		},
		{Expr: arr2(lRef(chplan.HistogramNegativeOffsetColumn), rRef(chplan.HistogramNegativeOffsetColumn)), Alias: hqAggNegOffsetsArrayAlias},
		{
			Expr:  arr2(lRef(chplan.HistogramNegativeBucketCountsColumn), negLadder(rRef(chplan.HistogramNegativeBucketCountsColumn))),
			Alias: hqAggNegBucketsArrayAlias,
		},
		{Expr: arr2(lRef(chplan.HistogramCountColumn), negScalar(rRef(chplan.HistogramCountColumn))), Alias: hqMergeCountsArrayAlias},
		{Expr: arr2(lRef(chplan.HistogramSumColumn), negScalar(rRef(chplan.HistogramSumColumn))), Alias: hqMergeSumsArrayAlias},
		{Expr: arr2(lRef(chplan.HistogramZeroCountColumn), negScalar(rRef(chplan.HistogramZeroCountColumn))), Alias: hqMergeZeroCountsArrayAlias},
	}

	for _, side := range []string{mixedVVJoinSideL, mixedVVJoinSideR} {
		for _, col := range mixedVVForwardedJoinCols(s) {
			projs = append(projs, chplan.Projection{
				Expr:  mixedJoinFieldRef(side, col),
				Alias: mixedJoinFieldAlias(side, col),
			})
		}
	}
	return projs
}

// mixedVVHistMergeOutputProjections folds
// [mixedVVHistMergeInputProjections]'s two-element arrays into the merged
// histogram row, reusing histogram_native_binop.go's / histogram_quantile.go's
// structural fold expressions VERBATIM: [plainArraySum] for the
// plain-summed scalar fields (Count, Sum, ZeroCount — see
// [histogramBinopMergeProjections]'s own doc for why this is plain,
// uncompensated arithmetic rather than the Kahan-compensated cross-series
// fold), [expHistogramMergeOffsetExpr] for both offsets, and
// [histogramBinopMergedBucketsExpr] for both bucket ladders.
//
// Aliased to the [chplan.Histogram*Column] canonical Mixed-contract names
// rather than to `s.*Column` (the physical schema names
// [histogramBinopMergeProjections] itself uses) because this Project's
// output IS the Mixed row shape directly — unlike the non-mixed merge,
// nothing here wraps the result in another [chplan.HistogramProjection]
// to translate schema names back to the canonical ones.
//
// Evaluated on a float,float row too (its L/R inputs are both the mixed
// `or` set-op's own all-zero, empty-bucket Histogram* placeholders,
// [mixedVectorSetOpHistogramPlaceholderCols]): merged Scale =
// least(0,0) = 0; merged ZeroThreshold = greatest(0,0) = 0; every
// plainArraySum over [0, ±0] = 0; and the merged bucket ladder — per
// [expHistogramMergeBucketsBoundsExpr]'s own documented handling of an
// empty-array row ("Rows with empty arrays produce (om + 0 - 1) = om - 1
// — slightly below their start, which is fine since they contribute
// nothing") — resolves to length 0, i.e. `[]`. That is exactly the SAME
// placeholder shape [mixedVectorSetOpHistogramPlaceholderCols] itself
// uses, so a float,float row's Histogram* output is the correct
// placeholder regardless of which fold produced it.
func mixedVVHistMergeOutputProjections() []chplan.Projection {
	return []chplan.Projection{
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeCountsArrayAlias}), Alias: chplan.HistogramCountColumn},
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeSumsArrayAlias}), Alias: chplan.HistogramSumColumn},
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: chplan.HistogramScaleColumn},
		{Expr: &chplan.ColumnRef{Name: mixedVVMergedZeroThresholdAlias}, Alias: chplan.HistogramZeroThresholdColumn},
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeZeroCountsArrayAlias}), Alias: chplan.HistogramZeroCountColumn},
		{
			Expr:  expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
			Alias: chplan.HistogramPositiveOffsetColumn,
		},
		{
			Expr:  histogramBinopMergedBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
			Alias: chplan.HistogramPositiveBucketCountsColumn,
		},
		{
			Expr:  expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
			Alias: chplan.HistogramNegativeOffsetColumn,
		},
		{
			Expr:  histogramBinopMergedBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
			Alias: chplan.HistogramNegativeBucketCountsColumn,
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
		keep = mixedVVDiscEq(mixedVVJoinSideR, mixedDiscriminatorFloat)
	} else {
		// MUL keeps every combination except histogram,histogram.
		bothHist := &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  mixedVVDiscEq(mixedVVJoinSideL, mixedDiscriminatorHistogram),
			Right: mixedVVDiscEq(mixedVVJoinSideR, mixedDiscriminatorHistogram),
		}
		keep = &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{bothHist}}
	}
	filtered := &chplan.Filter{Input: join, Predicate: keep}

	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)
	rIsHist := mixedVVDiscEq(mixedVVJoinSideR, mixedDiscriminatorHistogram)

	var discExpr chplan.Expr
	if op == chplan.OpDiv {
		// Kept rows are float,float (0,0) or histogram,float (1,0) — the
		// output discriminator is always L's own, forwarded unchanged.
		discExpr = mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn)
	} else {
		lIsHist := mixedVVDiscEq(mixedVVJoinSideL, mixedDiscriminatorHistogram)
		discExpr = &chplan.FuncCall{
			Fn: chplan.FnIf,
			Args: []chplan.Expr{
				&chplan.Binary{Op: chplan.OpOr, Left: lIsHist, Right: rIsHist},
				&chplan.LitInt{V: mixedDiscriminatorHistogram}, &chplan.LitInt{V: mixedDiscriminatorFloat},
			},
		}
	}

	projs := []chplan.Projection{
		{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
		{
			Expr:  mixedVVOutputAttributesExpr(join.Match, join.Card, join.Include, s.AttributesColumn),
			Alias: s.AttributesColumn,
		},
		{Expr: mixedJoinFieldRef(mixedVVManySide(join.Card), s.TimestampColumn), Alias: s.TimestampColumn},
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

// mixedVVFloatOnlyHistogramColumns lists the nine Histogram*Column
// fields [lowerMixedVVFloatOnlyArithmetic] forwards unconditionally from
// L. Order does not matter (each is a distinct Projection alias); listed
// in the same scalar/scale/offset/bucket-ladder grouping this file's
// other two folds use.
func mixedVVFloatOnlyHistogramColumns() []string {
	return []string{
		chplan.HistogramCountColumn, chplan.HistogramSumColumn, chplan.HistogramZeroCountColumn,
		chplan.HistogramScaleColumn, chplan.HistogramZeroThresholdColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
	}
}

// lowerMixedVVFloatOnlyArithmetic answers `^`/`%`/`atan2` (POW/MOD/
// ATAN2): unlike [lowerMixedVVAdditiveArithmetic] and
// [lowerMixedVVScaledArithmetic], reference keeps ONLY the float,float
// combination for these three ops (see this file's header) — every
// histogram-involving pair drops. The keep predicate is therefore
// `L.disc = 0 AND R.disc = 0` rather than the additive fold's "same
// type" test, and no per-combination source pick is needed for the nine
// Histogram*Column fields: a surviving row is always float,float, so L's
// own Histogram*Column fields are already the mixed `or` set-op's
// all-zero, empty-bucket placeholder (the same placeholder
// [lowerMixedVVAdditiveArithmetic]'s own header argues is harmless to
// republish on a float,float row), and forwarding them unconditionally
// is exactly as safe as reading L's Value unconditionally below — decode
// never reads either on a discriminator-0 row.
//
// Shape:
//
//	Project [canonical quartet + nine Histogram*Column fields (forwarded
//	         from L) + discriminator (forwarded from L, always 0 here)]
//	  Filter keep=(L.disc = 0 AND R.disc = 0)
//	    MixedVectorJoin
func lowerMixedVVFloatOnlyArithmetic(join *chplan.MixedVectorJoin, op chplan.BinaryOp, s schema.Metrics) chplan.Node {
	keep := andExpr(mixedVVDiscEq(mixedVVJoinSideL, mixedDiscriminatorFloat), mixedVVDiscEq(mixedVVJoinSideR, mixedDiscriminatorFloat))
	filtered := &chplan.Filter{Input: join, Predicate: keep}

	lValue := mixedJoinFieldRef(mixedVVJoinSideL, s.ValueColumn)
	rValue := mixedJoinFieldRef(mixedVVJoinSideR, s.ValueColumn)

	projs := []chplan.Projection{
		{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
		{
			Expr:  mixedVVOutputAttributesExpr(join.Match, join.Card, join.Include, s.AttributesColumn),
			Alias: s.AttributesColumn,
		},
		{Expr: mixedJoinFieldRef(mixedVVManySide(join.Card), s.TimestampColumn), Alias: s.TimestampColumn},
		{Expr: &chplan.Binary{Op: op, Left: lValue, Right: rValue}, Alias: s.ValueColumn},
	}
	for _, col := range mixedVVFloatOnlyHistogramColumns() {
		projs = append(projs, chplan.Projection{Expr: mixedJoinFieldRef(mixedVVJoinSideL, col), Alias: col})
	}
	projs = append(projs, chplan.Projection{
		Expr:  mixedJoinFieldRef(mixedVVJoinSideL, mixedDiscriminatorColumn),
		Alias: mixedDiscriminatorColumn,
	})

	return &chplan.Project{Input: filtered, Projections: projs}
}
