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
//
// Logical ops (`and`/`or`/`unless`) defer to a later milestone.
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

	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}

	return &chplan.VectorJoin{
		Left:             left,
		Right:            right,
		Op:               op,
		Match:            match,
		Card:             card,
		Include:          include,
		ReturnBool:       b.ReturnBool,
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

// promBinaryOp maps a PromQL parser op to the chplan op. Arithmetic and
// comparison ops are handled here; logical ops (and/or/unless) defer.
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
	return "", fmt.Errorf("promql: binary op %s not yet supported (logical ops `and`/`or`/`unless` land in M1.x follow-ups)", op.String())
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

	// Either arithmetic or `bool`-modified comparison — map Value through.
	newValue := chplan.Expr(opExpr)
	if isComparison(op) && returnBool {
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{opExpr}}
	}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}, nil
}
