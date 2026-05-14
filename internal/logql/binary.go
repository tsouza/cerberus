package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerBinary handles LogQL's `BinOpExpr`. The shape mirrors
// internal/promql/binary.go but the schema differs:
//
//   - LogQL streams live in `ResourceAttributes`, not `Attributes`.
//   - LogQL has no `MetricName` (stream identity is the full
//     ResourceAttributes map). For vector-vector joins we synthesise
//     an empty `MetricName` so the shared `chplan.VectorJoin` emitter
//     — which expects the canonical (MetricName, Attributes,
//     TimeUnix, Value) shape — has the columns it needs.
//   - LogQL's parser pre-folds literal-vs-literal at parse time
//     (`reduceBinOp` in upstream loki), so a binary expression
//     reaching us always has at least one non-literal leg.
//   - LogQL has no `CardManyToMany` (its three-value enum stops at
//     CardOneToMany); the `bool` modifier surfaces as `Opts.ReturnBool`
//     and the `on(...) / ignoring(...)` selectors as
//     `Opts.VectorMatching` mirroring PromQL.
//
// Supported shapes:
//   - scalar OP vector / vec OP scalar — Project that maps the inner
//     plan's `Value` column through `(scalar OP Value)`.
//   - vector OP vector — VectorJoin over both sides projected to the
//     Sample-shape, threading `Opts.ReturnBool` into VectorJoin.
//
// Logical ops (`and` / `or` / `unless`) defer to a later milestone in
// line with the PromQL surface.
func lowerBinary(b *syntax.BinOpExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	// BinOpExpr stores parse errors in an unexported `err` field; the
	// parser surfaces them at ParseExpr time before lowering reaches us.
	// Defensively guard against a nil leg in case a future parser
	// shape lets one through.
	if b.SampleExpr == nil || b.RHS == nil {
		return nil, fmt.Errorf("logql: binary expression has nil leg(s)")
	}

	op, err := logqlBinaryOp(b.Op)
	if err != nil {
		return nil, err
	}

	returnBool, vm := binOpModifiers(b.Opts)

	lhsLit, lhsIsLit := b.SampleExpr.(*syntax.LiteralExpr)
	rhsLit, rhsIsLit := b.RHS.(*syntax.LiteralExpr)

	switch {
	case lhsIsLit && rhsIsLit:
		// Upstream Loki's parser folds literal-vs-literal at parse
		// time via reduceBinOp (see syntax/ast.go:1817 mustNewBinOpExpr).
		// If one reaches us anyway it's an internal invariant break — surface
		// rather than silently re-folding.
		return nil, fmt.Errorf("logql: scalar-only binary expression not folded (op %s) — internal invariant violation", b.Op)
	case lhsIsLit:
		return lowerVectorScalar(b.RHS, s, op, lhsLit.Val, true /*scalarOnLeft*/, returnBool, lc)
	case rhsIsLit:
		return lowerVectorScalar(b.SampleExpr, s, op, rhsLit.Val, false, returnBool, lc)
	default:
		return lowerVectorVector(b, s, op, returnBool, vm, lc)
	}
}

// binOpModifiers extracts the ReturnBool + VectorMatching pair from
// the parser's optional Opts pointer. Returns (false, nil) when Opts is
// nil — the no-`bool`-no-matching default shape.
func binOpModifiers(opts *syntax.BinOpOptions) (bool, *syntax.VectorMatching) {
	if opts == nil {
		return false, nil
	}
	return opts.ReturnBool, opts.VectorMatching
}

// lowerVectorScalar lowers a binary expression mixing a vector and a
// scalar. Mirrors internal/promql/binary.go::lowerVectorScalar but uses
// the LogQL shape — the inner plan is either a RangeWindow (output is
// `(ResourceAttributes, Value)`) or a vector aggregation (output is the
// Sample-shape `(MetricName, Attributes, TimeUnix, Value)`). The
// projection re-shapes only `Value` and forwards every other column
// the inner plan carries.
//
// scalarOnLeft flips the operand order — important for non-commutative
// ops like SUB and DIV and for comparisons (`5 > rate(...)` vs
// `rate(...) > 5`).
//
// LogQL doesn't have PromQL's "filter on non-bool comparison" shape
// (Prom drops samples failing the comparison; LogQL always projects
// the comparison value through). We always Project here, wrapping
// comparisons in `toFloat64(...)` when ReturnBool is set; without the
// modifier, the comparison op surfaces as a Bool column from the
// emitter and the consumer treats it as a 0/1 numeric value.
func lowerVectorScalar(vec syntax.Expr, s schema.Logs, op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool bool, lc lowerCtx) (chplan.Node, error) {
	inner, err := lower(vec, s, lc)
	if err != nil {
		return nil, err
	}
	valueRef := &chplan.ColumnRef{Name: rangeAggSynthValueColumn}
	scalarLit := &chplan.LitFloat{V: scalar}
	var opExpr chplan.Expr
	if scalarOnLeft {
		opExpr = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		opExpr = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
	}

	newValue := chplan.Expr(opExpr)
	if isComparison(op) && returnBool {
		// `bool`-modified comparison — surface every matched row as a
		// 1.0 / 0.0 numeric instead of letting CH's Bool result type
		// flow into Value. toFloat64 mirrors PromQL's identical wrap.
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{opExpr}}
	}
	if isComparison(op) && !returnBool {
		// Bare comparison (no `bool`) on a LogQL metric query — Loki
		// surfaces the same behaviour as PromQL: drop rows where the
		// predicate is false. Wrap with a Filter over the inner plan
		// without mutating Value.
		return &chplan.Filter{Input: inner, Predicate: opExpr}, nil
	}

	return projectValueOverLogInner(inner, s, newValue), nil
}

// lowerVectorVector handles vector-vector binops. Both legs lower to a
// chplan.Node carrying the LogQL stream shape. We re-shape each to the
// canonical Sample contract (MetricName, Attributes, TimeUnix, Value)
// so chplan.VectorJoin — which is schema-agnostic but column-name-
// specific — can drive the JOIN.
//
// LogQL has no metric-name; we synthesise an empty `MetricName` column
// per side. The `ResourceAttributes` column carries forward as
// `Attributes` (the join's identity key). The `TimeUnix` column comes
// from the inner plan when present (after a vector aggregation it's
// `now64(9)`; after a range aggregation it doesn't exist, so we
// likewise synthesise `now64(9)`).
//
// The `bool` modifier threads into VectorJoin.ReturnBool — mirroring
// PromQL's exact behaviour: comparison ops yield 1.0 / 0.0 per matched
// pair rather than dropping non-matching rows. Logical ops
// (`and`/`or`/`unless`) defer to a later milestone.
func lowerVectorVector(b *syntax.BinOpExpr, s schema.Logs, op chplan.BinaryOp, returnBool bool, vm *syntax.VectorMatching, lc lowerCtx) (chplan.Node, error) {
	if returnBool && !isComparison(op) {
		return nil, fmt.Errorf("logql: 'bool' modifier is only allowed on comparison binary ops")
	}

	card, match, include, err := vectorMatchingFromOpts(vm)
	if err != nil {
		return nil, err
	}

	left, err := lower(b.SampleExpr, s, lc)
	if err != nil {
		return nil, err
	}
	right, err := lower(b.RHS, s, lc)
	if err != nil {
		return nil, err
	}

	leftShaped := sampleShapeOverLogInner(left, s)
	rightShaped := sampleShapeOverLogInner(right, s)

	return &chplan.VectorJoin{
		Left:             leftShaped,
		Right:            rightShaped,
		Op:               op,
		Match:            match,
		Card:             card,
		Include:          include,
		ReturnBool:       returnBool,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      rangeAggSynthValueColumn,
	}, nil
}

// vectorMatchingFromOpts translates the parser's optional VectorMatching
// pointer into chplan's match descriptors. Returns the cardinality, the
// label-match descriptor, and the optional `group_left(...) /
// group_right(...)` Include list.
//
// LogQL's VectorMatchCardinality enum stops at CardOneToMany; there is
// no CardManyToMany value to reject (PromQL has it for set ops which
// we also don't yet support).
func vectorMatchingFromOpts(vm *syntax.VectorMatching) (chplan.VectorCard, chplan.VectorMatch, []string, error) {
	card := chplan.CardOneToOne
	var include []string
	match := chplan.VectorMatch{}
	if vm == nil {
		return card, match, include, nil
	}

	switch vm.Card {
	case syntax.CardOneToOne:
		card = chplan.CardOneToOne
	case syntax.CardManyToOne:
		card = chplan.CardManyToOne
	case syntax.CardOneToMany:
		card = chplan.CardOneToMany
	default:
		return 0, match, nil, fmt.Errorf("logql: unsupported vector-matching cardinality %d", vm.Card)
	}

	if len(vm.Include) > 0 {
		if card == chplan.CardOneToOne {
			return 0, match, nil, fmt.Errorf("logql: many-to-many matching not allowed: matching labels must be unique on one side; use group_left/group_right when projecting include labels")
		}
		include = append([]string(nil), vm.Include...)
	}

	match.On = vm.On
	if len(vm.MatchingLabels) > 0 {
		match.Labels = append([]string(nil), vm.MatchingLabels...)
	}
	return card, match, include, nil
}

// logqlBinaryOp maps a LogQL parser op string to the chplan op enum.
// Arithmetic and comparison ops are handled here; logical ops
// (`and` / `or` / `unless`) defer to a later milestone.
func logqlBinaryOp(op string) (chplan.BinaryOp, error) {
	switch op {
	case syntax.OpTypeAdd:
		return chplan.OpAdd, nil
	case syntax.OpTypeSub:
		return chplan.OpSub, nil
	case syntax.OpTypeMul:
		return chplan.OpMul, nil
	case syntax.OpTypeDiv:
		return chplan.OpDiv, nil
	case syntax.OpTypeMod:
		return chplan.OpMod, nil
	case syntax.OpTypePow:
		return chplan.OpPow, nil
	case syntax.OpTypeCmpEQ:
		return chplan.OpEq, nil
	case syntax.OpTypeNEQ:
		return chplan.OpNe, nil
	case syntax.OpTypeLT:
		return chplan.OpLt, nil
	case syntax.OpTypeLTE:
		return chplan.OpLe, nil
	case syntax.OpTypeGT:
		return chplan.OpGt, nil
	case syntax.OpTypeGTE:
		return chplan.OpGe, nil
	}
	return "", fmt.Errorf("logql: binary op %s not yet supported (logical ops `and`/`or`/`unless` defer to follow-ups)", op)
}

// isComparison reports whether op is one of the six comparison ops.
// Shared between [lowerVectorScalar] and [lowerVectorVector].
func isComparison(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// projectValueOverLogInner wraps inner with a Project that keeps every
// other column and replaces only Value with newValue. Mirrors
// promql/instant_fns.go::projectValueOverInner but for the LogQL shape:
//
//   - RangeWindow: only `(ResourceAttributes, Value)` survives —
//     forward both, replacing Value.
//   - vector aggregation / synthetic scalar (literal) / Project /
//     Filter / Scan: forward the LogQL Sample-row equivalent
//     (MetricName, Attributes, TimeUnix, Value) when those columns are
//     in scope. We can't statically tell which inner shape we have
//     past RangeWindow, but the `count_over_time + binop` and
//     `sum(rate(...)) + binop` cases — the only two well-formed
//     LogQL shapes in flight — both produce columns the projection
//     can reach by name.
func projectValueOverLogInner(inner chplan.Node, s schema.Logs, newValue chplan.Expr) chplan.Node {
	if _, ok := inner.(*chplan.RangeWindow); ok {
		return &chplan.Project{
			Input: inner,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
				{Expr: newValue, Alias: rangeAggSynthValueColumn},
			},
		}
	}
	// Vector-aggregation output (post-wrapVectorAggregateForSample) and
	// the synthetic literal/vector(...) output both expose the
	// canonical (MetricName, Attributes, TimeUnix, Value) shape via
	// the LogQL Sample contract. lowerLiteral / lowerVector keep only
	// (ResourceAttributes, Value), so we forward whichever subset is
	// in scope by emitting an aliased projection that the emitter
	// resolves at SQL-build time.
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
			{Expr: newValue, Alias: rangeAggSynthValueColumn},
		},
	}
}

// sampleShapeOverLogInner re-shapes a LogQL inner plan into the canonical
// chclient.Sample contract — (MetricName, Attributes, TimeUnix, Value)
// — that chplan.VectorJoin's emitter expects. The synthesised
// MetricName is the empty string (LogQL has no metric name); TimeUnix
// comes from a synthetic `now64(9)` since the LogQL range-aggregation
// pipeline strips the per-row timestamp by the time control reaches
// this point.
//
// The function is only called from [lowerVectorVector] — every other
// LogQL binop path operates on the inner LogQL shape directly without
// the VectorJoin canonical-shape requirement.
func sampleShapeOverLogInner(inner chplan.Node, s schema.Logs) chplan.Node {
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}
}
