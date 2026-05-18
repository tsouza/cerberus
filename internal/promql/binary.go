package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerBinary handles PromQL BinaryExpr.
//
// Supported shapes:
//   - scalar OP scalar → constant-folded at lowering time via
//     [TryFoldScalar] and emitted as a synthetic 1-row vector.
//     Comparison ops require the `bool` modifier (Prom's rule for
//     scalar-vs-scalar; without `bool` the parser rejects the query
//     before we ever see it).
//   - scalar OP vector / vec OP scalar with arithmetic op → Project that
//     maps Value through the op. If the scalar side is itself a foldable
//     scalar tree (`(1+2) + vec`), [tryScalarLiteral] reduces it to a
//     single LitFloat first.
//   - scalar OP vector / vec OP scalar with comparison op → Filter on
//     the comparison (bool modifier off) or Project producing 1.0/0.0
//     (bool modifier on)
//   - vector OP vector → VectorJoin with default / `on(...)` /
//     `ignoring(...)` matching (M1.6) plus `group_left(...)` /
//     `group_right(...)` cardinality modifiers (RC2 cardinality edges)
//   - vector set ops (`and` / `or` / `unless`) → VectorSetOp keyed on
//     the match-key signature. PromQL's parser enforces many-to-many
//     for these (no `group_left` / `group_right`); the chsql emitter
//     renders them as IN / NOT IN against a DISTINCT signature
//     subquery, or as a left + anti-right UNION ALL for `or`.
func lowerBinary(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// Scalar-only fold first: when BOTH sides resolve to a constant we
	// materialise a 1-row synthetic vector with the folded value.
	// TryFoldScalar walks NumberLiteral / ParenExpr / UnaryExpr /
	// arithmetic BinaryExpr / bool-comparison BinaryExpr; we also need
	// it to handle the deeply-nested arithmetic cases like
	// `(1+2)*(3+4)`. The walk only succeeds when both sides reduce, so
	// a `(1+2) + vec` mixed shape falls through to the vector/scalar
	// path below.
	if v, ok := TryFoldScalar(b); ok {
		return syntheticScalarVector(&chplan.LitFloat{V: v}, nil, s, ctx), nil
	}

	// Vector set operators (`and` / `or` / `unless`) take a separate
	// path: their result rows come from one side verbatim (and / unless)
	// or are a union of both sides (or) — there's no per-pair value
	// expression. PromQL's parser enforces many-to-many matching for
	// these, so the cardinality modifiers we honour for arithmetic /
	// comparison V-V binops don't apply.
	if b.Op.IsSetOperator() {
		return lowerVectorSetOp(b, s, ctx)
	}

	op, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, err
	}

	lhsScalar, lhsIsScalar := tryScalarLiteral(b.LHS)
	rhsScalar, rhsIsScalar := tryScalarLiteral(b.RHS)

	switch {
	case lhsIsScalar && rhsIsScalar:
		// Should not happen — TryFoldScalar above already covers every
		// shape tryScalarLiteral covers. Keep the error as a safety
		// net so a future divergence between the two surfaces here
		// instead of silently producing wrong SQL.
		return nil, fmt.Errorf("promql: scalar-only binary expression not folded (op %s) — internal invariant violation", b.Op.String())
	case lhsIsScalar:
		return lowerVectorScalar(b.RHS, s, op, lhsScalar, true /*scalarOnLeft*/, b.ReturnBool, ctx)
	case rhsIsScalar:
		return lowerVectorScalar(b.LHS, s, op, rhsScalar, false, b.ReturnBool, ctx)
	default:
		return lowerVectorVector(b, s, op, ctx)
	}
}

// lowerVectorVector handles the vector-vector case: both sides lower to a
// Node, and the result is a VectorJoin that the emitter renders as an
// INNER JOIN of per-series latest samples.
//
// Cardinality modifiers (`group_left` / `group_right`) and Include labels
// thread through to chplan.VectorJoin; the chsql emitter shapes the
// per-side aggregation accordingly. `CardManyToMany` is rejected — the
// parser only sets it for set operators (`and`/`or`/`unless`), which
// promBinaryOp doesn't yet support anyway, but we surface a clean
// "many-to-many matching not allowed" error to match Prometheus's wording
// rather than the lower-level "binary op not yet supported".
//
// The `bool` modifier on a comparison op (`lhs > bool rhs`) threads into
// `chplan.VectorJoin.ReturnBool`; the emitter wraps the per-pair binary
// result in `toFloat64(...)` so every matched pair surfaces as 1.0 / 0.0
// rather than the comparison dropping non-matching rows. The modifier is
// rejected for non-comparison ops to match Prometheus's parser-level
// guard ("bool modifier is only allowed for comparison operators").
func lowerVectorVector(b *parser.BinaryExpr, s schema.Metrics, op chplan.BinaryOp, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool && !isComparison(op) {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	card := chplan.CardOneToOne
	var include []string
	if b.VectorMatching != nil {
		switch b.VectorMatching.Card {
		case parser.CardOneToOne:
			card = chplan.CardOneToOne
		case parser.CardManyToOne:
			card = chplan.CardManyToOne
		case parser.CardOneToMany:
			card = chplan.CardOneToMany
		case parser.CardManyToMany:
			return nil, fmt.Errorf("promql: many-to-many matching not allowed: matching labels must be unique on one side")
		default:
			return nil, fmt.Errorf("promql: unsupported vector-matching cardinality %d", b.VectorMatching.Card)
		}
		if len(b.VectorMatching.Include) > 0 {
			if card == chplan.CardOneToOne {
				return nil, fmt.Errorf("promql: many-to-many matching not allowed: matching labels must be unique on one side; use group_left/group_right when projecting include labels")
			}
			include = append([]string(nil), b.VectorMatching.Include...)
		}
		// group_left/right without explicit on/ignoring is allowed
		// (matches the full-Attributes intersection, which the
		// emitter's "many" aggregation handles by construction).
	}

	left, err := lower(b.LHS, s, ctx)
	if err != nil {
		return nil, err
	}
	right, err := lower(b.RHS, s, ctx)
	if err != nil {
		return nil, err
	}

	// Synthetic-scalar fold: when BOTH legs lower to the canonical
	// 4-slot synthetic-vector shape ([syntheticScalarVector]), the
	// VectorJoin emit path collapses each side to one row via the
	// per-side argMax wrap and then joins on (MetricName, Attributes)
	// — yielding 1 × 1 = 1 row instead of Prom's N rows per step.
	// This shows up as 12 compat-lane shape-diffs for the
	// `time() OP time()` family (all 6 arithmetic + 6 bool comparisons).
	// The fix mirrors the literal-literal fold one level above: rebuild
	// a single Project over the shared synthetic source with the
	// combined value expression, skipping VectorJoin entirely.
	//
	// Gated on default matching (no on/ignoring/group_left/group_right
	// modifiers) — two empty-label legs only join correctly via the
	// full-Attributes intersection, and any `on(<label>)` / Include
	// shape against empty Attributes is semantically incoherent (Prom
	// errors out). Keeping the gate narrow means anything cardinality-
	// modifier-shaped still flows through the existing V-V path.
	if isSyntheticScalarPlan(left, s) && isSyntheticScalarPlan(right, s) && isDefaultMatching(b.VectorMatching) {
		return foldSyntheticBinary(left, right, op, b.ReturnBool, s), nil
	}

	// Asymmetric synthetic fold: when EXACTLY ONE leg is the synthetic-
	// scalar shape and the other is a real metric vector, the
	// VectorJoin's per-side argMax wrap would key the synthetic side on
	// (MetricName="", Attributes={}) and the metric side on its real
	// (MetricName, {instance, job, ...}) — the INNER JOIN on full
	// Attributes finds zero matches and the result is empty. Prom's
	// semantics broadcast the synthetic scalar across every (series,
	// step) row of the real side. We rebuild that here by promoting
	// the synthetic's Value expression into a per-step / per-row scalar
	// and routing through the vector-scalar binop shape (Project Value
	// through the op, or Filter for bare-comparison). The synthetic
	// side's Value expression may reference `anchor_ts` (range mode);
	// the metric side's outer projection re-aliases `anchor_ts` →
	// TimeUnix, so we rewrite ColumnRef{anchor_ts} → ColumnRef{TimeUnix}
	// before splicing.
	//
	// Bucket 6 from docs/compat-residual-audit-25898791664.md: the
	// `time() <op> metric` and `metric <op> time()` shapes collapse to
	// empty without this. Gated on default matching for the same
	// reasons as the both-synthetic fold above.
	if isDefaultMatching(b.VectorMatching) {
		lSynth := isSyntheticScalarPlan(left, s)
		rSynth := isSyntheticScalarPlan(right, s)
		switch {
		case lSynth && !rSynth:
			return foldSyntheticVectorBinary(left, right, op, true /*scalarOnLeft*/, b.ReturnBool, s), nil
		case !lSynth && rSynth:
			return foldSyntheticVectorBinary(right, left, op, false /*scalarOnLeft*/, b.ReturnBool, s), nil
		}
	}

	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}

	// Range mode (ctx.step > 0): both sides materialise per-step rows
	// (one per series × anchor) via wrapRangeLatestPerSeries / the
	// matrix RangeWindow. The V-V join must step-align so each anchor
	// joins its own pair, otherwise the per-side aggregation collapses
	// N anchors onto a single match-key (roleOne) or the join finds N×N
	// matches per series (roleMany). Lifting `StepAligned` onto the
	// VectorJoin lets the emitter add TimestampColumn to both the
	// per-side GROUP BY and the JOIN's ON clause; instant mode keeps
	// the byte-stable shape (StepAligned=false default).
	stepAligned := ctx.step > 0

	return &chplan.VectorJoin{
		Left:             left,
		Right:            right,
		Op:               op,
		Match:            match,
		Card:             card,
		Include:          include,
		ReturnBool:       b.ReturnBool,
		StepAligned:      stepAligned,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// isDefaultMatching reports whether the parser's VectorMatching slot
// is the "no explicit matching modifier" shape — nil, or set to the
// default one-to-one card with no matching labels / no include labels.
// The synthetic-scalar fold only fires for this shape; anything more
// specific (on/ignoring/group_left/group_right) flows through the
// regular VectorJoin path so cardinality / include semantics still
// route through the V-V emitter.
func isDefaultMatching(vm *parser.VectorMatching) bool {
	if vm == nil {
		return true
	}
	return vm.Card == parser.CardOneToOne &&
		len(vm.MatchingLabels) == 0 &&
		len(vm.Include) == 0 &&
		!vm.On
}

// foldSyntheticBinary builds the combined Project for a V-V binop
// where both legs are synthetic-scalar shapes. The returned plan is a
// single 4-slot Project over the shared `OneRow` / `StepGrid` source
// with the per-leg Value expressions woven together.
//
// The MetricName + Attributes + TimeUnix slots reuse the left leg's
// expressions verbatim — both legs lowered through
// [syntheticScalarVector] so the timestamp expression already
// reflects the requested instant (`now64(9)` / a literal anchor) or
// the per-step `anchor_ts` column. Choosing the left leg's timestamp
// over the right's is arbitrary; the values are equal by construction
// (both threaded from the same lowerCtx).
//
// Op-by-op Value shape:
//
//   - Arithmetic op: `(lhs_value OP rhs_value)`.
//   - Comparison op + `bool` modifier: `toFloat64(lhs_value OP rhs_value)`
//     (1.0 / 0.0 per step, matching PromQL's vector-bool semantics).
//   - Comparison op WITHOUT `bool`: Prom's V-V comparison-as-filter rule
//     applies — preserve LHS Value where the comparison holds, drop
//     rows where it doesn't. We keep `lhs_value` in the Value slot and
//     wrap the Project in a Filter on the comparison expression so
//     the rendered SQL drops non-matching rows at the WHERE level.
//     None of the 12 reported compat shape-diffs hit this branch
//     (all of them carry the `bool` modifier when comparing two
//     scalars), but we round it out for completeness — without the
//     modifier the Prom parser permits the shape and the comparator
//     in the harness would otherwise see another diff.
func foldSyntheticBinary(left, right chplan.Node, op chplan.BinaryOp, returnBool bool, s schema.Metrics) chplan.Node {
	lhsVal := syntheticValueExpr(left)
	rhsVal := syntheticValueExpr(right)
	cmpExpr := chplan.Expr(&chplan.Binary{Op: op, Left: lhsVal, Right: rhsVal})
	leftProject := left.(*chplan.Project)

	var newValue chplan.Expr
	switch {
	case isComparison(op) && returnBool:
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{cmpExpr}}
	case isComparison(op):
		// Bare V-V comparison: Value is the LHS sample value (Prom's
		// "preserve LHS where comparison holds" rule); the Filter
		// below drops non-matching rows.
		newValue = lhsVal
	default:
		newValue = cmpExpr
	}

	combined := &chplan.Project{
		Input: syntheticSource(left),
		Projections: []chplan.Projection{
			leftProject.Projections[0], // "" AS MetricName
			leftProject.Projections[1], // <empty-map> AS Attributes
			leftProject.Projections[2], // <ts_expr> AS TimeUnix
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}

	if isComparison(op) && !returnBool {
		return &chplan.Filter{
			Input:     combined,
			Predicate: cmpExpr,
		}
	}
	return combined
}

// foldSyntheticVectorBinary handles the asymmetric case where one leg
// of a vector-vector binop is a synthetic-scalar shape (e.g. `time()`,
// `vector(N)`, zero-arg date fn) and the other is a real metric vector.
// The synthetic side acts as a per-step / per-row scalar broadcast
// across every (series, step) row of the metric side — matching
// Prom's semantics for `time() + metric`, `metric > time()`, etc.
//
// synth is the synthetic-scalar leg; vec is the real-vector leg.
// scalarOnLeft tracks the original PromQL operand order so non-
// commutative ops (SUB, DIV, MOD, POW) and comparisons preserve
// orientation. returnBool wraps comparison ops in `toFloat64(...)`
// for the 1.0 / 0.0 vector-bool result.
//
// The result shape mirrors [lowerVectorScalar]:
//
//   - Arithmetic op: Project ["" AS MetricName, Attributes, TimeUnix,
//     (<synth_val> OP Value) AS Value] over the vec leg. The
//     MetricName column is rewritten to an empty literal per Prom's
//     derived-sample rule (`time() <op> metric` is not the same
//     series as `metric`, so `__name__` drops). Same rule as #359
//     applied to instant fns / scalar-vec binops / V-V binops.
//   - Comparison op + `bool` modifier: Project with
//     `toFloat64(<synth_val> OP Value) AS Value`; MetricName is
//     likewise emptied (bool-compared rows are derived samples).
//   - Comparison op WITHOUT `bool`: Filter on `<synth_val> OP Value`
//     keeping the vec leg's columns intact (Prom's "preserve LHS
//     where comparison holds" rule reduces here to "preserve vec
//     rows where comparison holds" since the synthetic side has no
//     labels of its own to keep — and Prom's bare-comparison rule
//     preserves __name__ when the op filters rather than transforms).
//
// Range mode rewiring: the synthetic leg's Value expression may
// reference `ColumnRef{anchor_ts}` (the per-step anchor introduced by
// [syntheticScalarVector] via [rewriteAnchorRefs]); the vec leg's
// outer projection from [wrapRangeLatestPerSeries] re-aliases
// `anchor_ts` → TimeUnix, so we rewrite ColumnRef{anchor_ts} →
// ColumnRef{TimeUnix} before splicing the synthetic value into the
// vec leg's row stream. Instant-mode synthetic Values are pure
// literal expressions (`toFloat64(toUnixTimestamp64Nano(<lit>) /
// 1e9)`) with no ColumnRefs, so the rewrite is a no-op there.
func foldSyntheticVectorBinary(synth, vec chplan.Node, op chplan.BinaryOp, scalarOnLeft, returnBool bool, s schema.Metrics) chplan.Node {
	synthVal := rewriteAnchorToTimeUnix(syntheticValueExpr(synth), s)
	vecValue := chplan.Expr(&chplan.ColumnRef{Name: s.ValueColumn})

	var lhs, rhs chplan.Expr
	if scalarOnLeft {
		lhs, rhs = synthVal, vecValue
	} else {
		lhs, rhs = vecValue, synthVal
	}
	opExpr := &chplan.Binary{Op: op, Left: lhs, Right: rhs}

	if isComparison(op) && !returnBool {
		return &chplan.Filter{Input: vec, Predicate: opExpr}
	}

	var newValue chplan.Expr = opExpr
	if isComparison(op) && returnBool {
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{opExpr}}
	}
	return &chplan.Project{
		Input: vec,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}
}

// rewriteAnchorToTimeUnix walks expr and replaces every
// `ColumnRef{anchor_ts}` with `ColumnRef{<TimestampColumn>}`. Used by
// [foldSyntheticVectorBinary] to thread a synthetic-leg Value
// expression (which references `anchor_ts` in range mode) onto the
// vector leg, whose outer projection has already renamed the per-step
// anchor column to the canonical TimeUnix slot.
//
// The walk covers FuncCall args and Binary children — the shape
// [syntheticScalarVector] / [rewriteAnchorRefs] produce. Other Expr
// types (literals, MapAccess, etc.) don't carry anchor_ts references
// in the synthetic-scalar shape and pass through unchanged.
func rewriteAnchorToTimeUnix(expr chplan.Expr, s schema.Metrics) chplan.Expr {
	if expr == nil {
		return nil
	}
	switch v := expr.(type) {
	case *chplan.ColumnRef:
		if v.Name == "anchor_ts" && v.Qualifier == "" {
			return &chplan.ColumnRef{Name: s.TimestampColumn}
		}
		return expr
	case *chplan.FuncCall:
		newArgs := make([]chplan.Expr, len(v.Args))
		for i, a := range v.Args {
			newArgs[i] = rewriteAnchorToTimeUnix(a, s)
		}
		return &chplan.FuncCall{Name: v.Name, Args: newArgs}
	case *chplan.Binary:
		return &chplan.Binary{
			Op:    v.Op,
			Left:  rewriteAnchorToTimeUnix(v.Left, s),
			Right: rewriteAnchorToTimeUnix(v.Right, s),
		}
	}
	return expr
}

// lowerVectorSetOp lowers a PromQL vector set operator (`and`, `or`,
// `unless`) into a chplan.VectorSetOp node. Each side lowers
// independently to a row-per-series shape (instant) or a row-per-
// (series, anchor) shape (range); the chsql emitter then filters /
// unions them by their match-key signature.
//
// PromQL's parser enforces many-to-many matching for set ops — it
// upgrades CardOneToOne to CardManyToMany at parse time, and rejects
// `group_left` / `group_right` explicitly ("set operations must
// always be many-to-many"). So this lowering deliberately ignores
// `b.VectorMatching.Card` / `b.VectorMatching.Include`: there is no
// cardinality knob to honour, only the match-key shape.
//
// The match-key signature defaults to the full Attributes map.
// `on(labels)` projects only the named keys; `ignoring(labels)`
// projects the complement. The emitter uses the same
// matchKeyGroupExpr helper as VectorJoin so the signature shape
// stays in sync across both V-V binop families.
func lowerVectorSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	kind, err := promVectorSetOpKind(b.Op)
	if err != nil {
		return nil, err
	}
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	left, err := lower(b.LHS, s, ctx)
	if err != nil {
		return nil, err
	}
	right, err := lower(b.RHS, s, ctx)
	if err != nil {
		return nil, err
	}

	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}

	return &chplan.VectorSetOp{
		Left:             left,
		Right:            right,
		Op:               kind,
		Match:            match,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// promVectorSetOpKind maps a PromQL parser set-op token to the chplan
// VectorSetOpKind constant.
func promVectorSetOpKind(op parser.ItemType) (chplan.VectorSetOpKind, error) {
	switch op {
	case parser.LAND:
		return chplan.VectorSetAnd, nil
	case parser.LOR:
		return chplan.VectorSetOr, nil
	case parser.LUNLESS:
		return chplan.VectorSetUnless, nil
	}
	return "", fmt.Errorf("promql: not a vector set operator: %s", op.String())
}

// promBinaryOp maps a PromQL parser op to the chplan op. Arithmetic and
// comparison ops are handled here; vector set ops (and / or / unless)
// take a separate path via [lowerVectorSetOp].
func promBinaryOp(op parser.ItemType) (chplan.BinaryOp, error) {
	switch op {
	case parser.ADD:
		return chplan.OpAdd, nil
	case parser.SUB:
		return chplan.OpSub, nil
	case parser.MUL:
		return chplan.OpMul, nil
	case parser.DIV:
		return chplan.OpDiv, nil
	case parser.MOD:
		return chplan.OpMod, nil
	case parser.POW:
		return chplan.OpPow, nil
	case parser.EQLC:
		return chplan.OpEq, nil
	case parser.NEQ:
		return chplan.OpNe, nil
	case parser.LSS:
		return chplan.OpLt, nil
	case parser.LTE:
		return chplan.OpLe, nil
	case parser.GTR:
		return chplan.OpGt, nil
	case parser.GTE:
		return chplan.OpGe, nil
	}
	if op.IsSetOperator() {
		// Set ops are dispatched via [lowerVectorSetOp] before this
		// function is called; reaching here means an internal mis-
		// routing — surface a clear invariant error rather than a
		// silently misleading "not yet supported".
		return "", fmt.Errorf("promql: vector set op %s should route through lowerVectorSetOp", op.String())
	}
	return "", fmt.Errorf("promql: binary op %s not yet supported", op.String())
}

func isComparison(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// tryScalarLiteral unwraps NumberLiteral, ParenExpr around a literal,
// UnaryExpr(±N), and nested scalar-only arithmetic / bool-comparison
// BinaryExpr at lowering time. Returns the folded value and true on a
// match, or 0+false otherwise. Delegates to [TryFoldScalar] so the
// surface stays in sync — any new scalar shape added there picks up
// here automatically (e.g. `(1+2) + vec` lowers as `3 + vec` because
// the LHS folds to 3).
func tryScalarLiteral(e parser.Expr) (float64, bool) {
	return TryFoldScalar(e)
}

// lowerVectorScalar lowers a binary expression mixing a vector and a
// scalar. Arithmetic ops are mapped through a Project that replaces
// `Value` with `(scalar OP Value)` or `(Value OP scalar)`. Comparison ops
// without the `bool` modifier emit a Filter (keep all samples where the
// predicate holds); with `bool` they emit a Project producing 1.0/0.0
// per sample.
//
// scalarOnLeft flips the operand order — important for non-commutative
// ops like SUB and DIV and for comparisons (`5 > up` vs `up > 5`).
func lowerVectorScalar(vec parser.Expr, s schema.Metrics, op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool bool, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lower(vec, s, ctx)
	if err != nil {
		return nil, err
	}
	valueRef := &chplan.ColumnRef{Name: s.ValueColumn}
	scalarLit := &chplan.LitFloat{V: scalar}
	var opExpr chplan.Expr
	if scalarOnLeft {
		opExpr = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		opExpr = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
	}

	if isComparison(op) && !returnBool {
		// `up > 0.5` — keep all columns, filter on the comparison.
		return &chplan.Filter{Input: inner, Predicate: opExpr}, nil
	}

	// Either arithmetic or `bool`-modified comparison — map Value
	// through and drop `__name__` per PromQL's derived-sample rule. The
	// bare-comparison path above (`Filter`) preserves all columns and
	// is correct: PromQL keeps LHS labels (including `__name__`) when
	// the comparison filters rather than transforms. See Pool-AU's
	// audit (#355) — this projection site accounts for ~36 of the 107
	// `__name__`-retention diffs (scalar-on-{left,right} arithmetic +
	// scalar `bool` compare + folded-scalar-in-bool cases).
	newValue := chplan.Expr(opExpr)
	if isComparison(op) && returnBool {
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{opExpr}}
	}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}, nil
}
