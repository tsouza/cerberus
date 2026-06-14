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
//   - vector set ops (`and` / `or` / `unless`) — VectorSetOp over both
//     sides projected to the Sample-shape; see [lowerVectorSetOp].
func lowerBinary(b *syntax.BinOpExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	// BinOpExpr stores parse errors in an unexported `err` field; the
	// parser surfaces them at ParseExpr time before lowering reaches us.
	// Defensively guard against a nil leg in case a future parser
	// shape lets one through.
	if b.SampleExpr == nil || b.RHS == nil {
		return nil, fmt.Errorf("logql: binary expression has nil leg(s)")
	}

	// Vector set operators (`and` / `or` / `unless`) take a separate
	// path: their result rows come from one side verbatim (and /
	// unless) or are a union of both sides (or) — there's no per-pair
	// value expression. Loki's parser rejects literal legs on set ops
	// (mustNewBinOpExpr: "unexpected literal for ... logical/set binary
	// operation"), so both legs here are vector-shaped.
	if syntax.IsLogicalBinOp(b.Op) {
		return lowerVectorSetOp(b, s, lc)
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
// (`and`/`or`/`unless`) are routed to [lowerVectorSetOp] upstream of
// this call.
//
// Unlike PromQL (whose parser rejects `bool` on non-comparison ops),
// Loki's parser ACCEPTS shapes like `a + bool b` and its evaluator
// silently ignores the modifier: syntax.MergeBinOp's arithmetic
// mergers never consult the `filter` flag the modifier maps to
// (pkg/logql/syntax/ast.go::MergeBinOp — only the six comparison
// mergers branch on it). Mirror that by dropping the modifier here
// instead of rejecting — rejecting was a wrong rejection vs reference
// Loki (rejection-parity catalogue site lowerVectorVector#e04c7f18).
func lowerVectorVector(b *syntax.BinOpExpr, s schema.Logs, op chplan.BinaryOp, returnBool bool, vm *syntax.VectorMatching, lc lowerCtx) (chplan.Node, error) {
	returnBool = returnBool && isComparison(op)

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
		Left:       leftShaped,
		Right:      rightShaped,
		Op:         op,
		Match:      match,
		Card:       card,
		Include:    include,
		ReturnBool: returnBool,
		// Range mode (lc.Step > 0): both legs carry per-anchor rows
		// (forwarded by sampleShapeOverLogInner), so the join must
		// step-align — TimestampColumn joins the per-side GROUP BY
		// and the ON clause, mirroring promql/binary.go. Instant mode
		// keeps the byte-stable single-timestamp shape.
		StepAligned:      lc.Step > 0,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      rangeAggSynthValueColumn,
	}, nil
}

// lowerVectorSetOp lowers a LogQL vector set operator (`and`, `or`,
// `unless`) into a chplan.VectorSetOp node. Mirrors
// internal/promql/binary.go::lowerVectorSetOp — both legs lower
// independently and are re-shaped to the canonical Sample contract via
// [sampleShapeOverLogInner]; the chsql emitter then filters / unions
// them by their match-key signature.
//
// Reference semantics (pkg/logql/evaluator.go::vectorAnd / vectorOr /
// vectorUnless): per evaluation step, each side's samples are keyed by
// `matchingSignature` — the full label set by default, the named keys
// for `on(...)`, the complement for `ignoring(...)` — and
//
//   - `and` keeps LHS samples whose signature appears on the RHS,
//   - `unless` keeps LHS samples whose signature does NOT appear on
//     the RHS,
//   - `or` keeps all LHS samples plus RHS samples whose signature does
//     not appear on the LHS.
//
// The three reference evaluators ignore `Opts.ReturnBool` and
// `VectorMatching.Include` entirely (only vectorBinop — the
// arithmetic / comparison path — consults them), so the lowering
// drops both instead of rejecting: Loki's parser accepts
// `a and bool b` / `a and on(x) group_left b` and evaluates them
// identically to the unmodified form.
func lowerVectorSetOp(b *syntax.BinOpExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	kind, err := logqlVectorSetOpKind(b.Op)
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

	_, vm := binOpModifiers(b.Opts)
	match := chplan.VectorMatch{}
	if vm != nil {
		match.Labels = append([]string(nil), vm.MatchingLabels...)
		match.On = vm.On
	}

	return &chplan.VectorSetOp{
		Left:             sampleShapeOverLogInner(left, s),
		Right:            sampleShapeOverLogInner(right, s),
		Op:               kind,
		Match:            match,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      rangeAggSynthValueColumn,
	}, nil
}

// logqlVectorSetOpKind maps a LogQL parser set-op string to the chplan
// VectorSetOpKind constant.
func logqlVectorSetOpKind(op string) (chplan.VectorSetOpKind, error) {
	switch op {
	case syntax.OpTypeAnd:
		return chplan.VectorSetAnd, nil
	case syntax.OpTypeOr:
		return chplan.VectorSetOr, nil
	case syntax.OpTypeUnless:
		return chplan.VectorSetUnless, nil
	}
	return "", fmt.Errorf("logql: not a vector set operator: %s", op)
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
	// `append([]string(nil), nil...)` returns nil and
	// `append([]string(nil), []string{}...)` also returns nil, so the
	// guard `if len(vm.MatchingLabels) > 0` is observationally a no-op:
	// it produced the same nil `match.Labels` on the empty path.
	// Removing it lets the assignment also serve as a clear-copy of any
	// caller-allocated slice (we never alias the parser's MatchingLabels)
	// and eliminates a CONDITIONALS_BOUNDARY mutation site that was
	// equivalent under append's nil-input semantics.
	match.Labels = append([]string(nil), vm.MatchingLabels...)
	return card, match, include, nil
}

// includeLabelsFromBinop returns the labels listed in `group_left(...)` /
// `group_right(...)` of the binop's VectorMatching. Returns an empty
// (non-nil) slice when the binop has no Opts, no VectorMatching, or
// when the matching declares no Include labels.
//
// Sources `b.Opts.VectorMatching.Include`. The returned slice is a fresh
// copy — callers may retain or mutate it without aliasing the parser AST.
func includeLabelsFromBinop(b *syntax.BinOpExpr) []string {
	if b == nil || b.Opts == nil || b.Opts.VectorMatching == nil {
		return []string{}
	}
	src := b.Opts.VectorMatching.Include
	if len(src) == 0 {
		return []string{}
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// logqlBinaryOp maps a LogQL parser op string to the chplan op enum.
// Arithmetic and comparison ops are handled here; logical ops
// (`and` / `or` / `unless`) are routed to [lowerVectorSetOp] before
// this is called, so an unmatched op is a genuinely unknown operator.
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
	return "", fmt.Errorf("logql: unknown binary op %s", op)
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

// logSampleColumns resolves where the canonical Sample columns live in
// an inner LogQL metric plan's output scope. Shared by every wrap
// layer that re-projects an inner plan (vector-scalar binops, the
// vector-join leg shaping, label_replace) and mirrored by
// [Lang.ProjectSamples] — keeping the resolution in one place is what
// stops the layers drifting (the pre-#757 drift surfaced as 502
// `Unknown expression identifier 'anchor_ts' / 'ResourceAttributes'`
// for every range-mode LogQL binop).
//
//   - Sample-shaped inner ([wrapVectorAggregateForSample] /
//     [lowerVectorVector] / the wraps below): the canonical columns
//     exist verbatim — forward MetricName / Attributes / TimeUnix.
//   - Matrix-shape RangeWindow (range mode): stream identity is the
//     raw ResourceAttributes column and the per-anchor timestamp is
//     exposed under [matrixBucketColumn] (`anchor_ts`). Forwarding it
//     is what keeps one row per step alive through the wrap; the old
//     `now64(9)` synthesis collapsed the step grid.
//   - Everything else (instant RangeWindow, the synthetic
//     literal / vector(n) scalar): only (ResourceAttributes, Value)
//     are in scope; synthesise `now64(9)` like the instant pipeline
//     always has.
type logSampleShape struct {
	metricName chplan.Expr
	attrsCol   string
	timeExpr   chplan.Expr
	// hasNativeTime reports whether timeExpr forwards a real per-row
	// timestamp column the inner plan exposes (a vector-aggregate
	// `TimeUnix` or a matrix `anchor_ts`) rather than a synthesised
	// `now64(9)`. Callers that anchor instant samples at the request
	// window (e.g. the variant lowering) gate on this so they only
	// override the synthetic-now case.
	hasNativeTime bool
}

func logSampleColumns(inner chplan.Node, s schema.Logs) logSampleShape {
	if isVectorAggregateSampleShape(inner) {
		return logSampleShape{
			metricName:    &chplan.ColumnRef{Name: "MetricName"},
			attrsCol:      "Attributes",
			timeExpr:      &chplan.ColumnRef{Name: "TimeUnix"},
			hasNativeTime: true,
		}
	}
	if isMatrixRangeWindow(inner) {
		return logSampleShape{
			metricName:    &chplan.LitString{V: ""},
			attrsCol:      s.ResourceAttributesColumn,
			timeExpr:      &chplan.ColumnRef{Name: matrixBucketColumn(inner)},
			hasNativeTime: true,
		}
	}
	return logSampleShape{
		metricName: &chplan.LitString{V: ""},
		attrsCol:   s.ResourceAttributesColumn,
		timeExpr:   chplan.NowNano(),
	}
}

// projectValueOverLogInner wraps inner with a Project that re-shapes
// the row into the canonical Sample contract (MetricName, Attributes,
// TimeUnix, Value), replacing only Value with newValue. Mirrors
// promql/instant_fns.go::projectValueOverInner but for the LogQL
// shapes — the source columns come from [logSampleColumns], so a
// matrix-shape inner keeps its per-anchor timestamp and a vector-
// aggregation inner keeps its Attributes / TimeUnix aliases. Emitting
// the full canonical shape (instead of the historical two-column
// `(ResourceAttributes, Value)` form) means [Lang.ProjectSamples] and
// any enclosing binop see a Sample-shaped scope regardless of how
// deeply wraps nest.
func projectValueOverLogInner(inner chplan.Node, s schema.Logs, newValue chplan.Expr) chplan.Node {
	cols := logSampleColumns(inner, s)
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: cols.attrsCol}, Alias: "Attributes"},
			{Expr: cols.timeExpr, Alias: "TimeUnix"},
			{Expr: newValue, Alias: rangeAggSynthValueColumn},
		},
	}
}

// sampleShapeOverLogInner re-shapes a LogQL inner plan into the canonical
// chclient.Sample contract — (MetricName, Attributes, TimeUnix, Value)
// — that chplan.VectorJoin's emitter expects. The synthesised
// MetricName is the empty string (LogQL has no metric name).
//
// The function is only called from [lowerVectorVector] — every other
// LogQL binop path operates on the inner LogQL shape directly without
// the VectorJoin canonical-shape requirement.
//
// Source-column resolution is [logSampleColumns]: a vector-aggregation
// leg forwards its existing Attributes / TimeUnix aliases, a
// matrix-shape RangeWindow leg forwards `ResourceAttributes` plus its
// per-anchor `anchor_ts` (so a step-aligned join sees one row per
// (series, anchor) on each side), and the synthetic literal /
// vector(n) / instant shapes synthesise `now64(9)`. The historical
// unconditional `now64(9)` here squashed every range-mode leg onto a
// single timestamp — the per-side argMax dedup then collapsed the
// matrix to one row per series and every range-mode LogQL join
// returned an empty matrix.
func sampleShapeOverLogInner(inner chplan.Node, s schema.Logs) chplan.Node {
	cols := logSampleColumns(inner, s)
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: cols.attrsCol}, Alias: "Attributes"},
			{Expr: cols.timeExpr, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}
}

// isVectorAggregateSampleShape reports whether inner already carries the
// canonical Sample contract (MetricName, Attributes, TimeUnix, Value)
// — i.e. came out of [wrapVectorAggregateForSample] or
// [lowerVectorVector]. The signal is one of:
//
//   - a top-level `*chplan.Project` whose alias list includes
//     `Attributes` (RangeWindow / lowerLiteral / lowerVector /
//     label_replace all alias `ResourceAttributes`, so the
//     `Attributes` alias is specific to the vector-aggregate
//     re-shape).
//   - a top-level `*chplan.VectorJoin` — its emitter projects
//     `L.Attributes` / `L.TimeUnix` / `L.Value` (with the value-fold
//     `L.Value <op> R.Value`) so the post-join scope already exposes
//     `Attributes`, not `ResourceAttributes`. Without this branch a
//     query like `vector(1) + vector(1)` (Grafana's Loki health probe)
//     surfaces as ClickHouse `code: 47 Unknown expression identifier
//     'ResourceAttributes'` when [Lang.ProjectSamples] wraps the join
//     output.
//   - a top-level `*chplan.VectorSetOp` — its emitter's outer SELECT
//     projects the canonical (MetricName, Attributes, TimeUnix, Value)
//     column list verbatim (see internal/chsql/vector_set_op.go).
//   - a top-level `*chplan.AbsentOverTime` — its emitter synthesises
//     the canonical 4-column Sample shape directly (see
//     internal/chsql/absent_over_time.go).
//   - a top-level `*chplan.TopK` / `*chplan.OrderBy` — both are
//     row-preserving wraps (`LIMIT K BY` / `ORDER BY`); the LogQL
//     lowering only ever builds them over a [sampleShapeOverLogInner]
//     canonical projection, so recurse into the input.
func isVectorAggregateSampleShape(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.VectorJoin:
		return true
	case *chplan.VectorSetOp:
		return true
	case *chplan.AbsentOverTime:
		return true
	case *chplan.TopK:
		return isVectorAggregateSampleShape(v.Input)
	case *chplan.OrderBy:
		return isVectorAggregateSampleShape(v.Input)
	case *chplan.Filter:
		// A bare comparison (`sum by (svc) (...) > 0`) wraps the inner
		// plan in a Filter without re-projecting — the Sample columns
		// (or their absence) pass through untouched, so recurse.
		return isVectorAggregateSampleShape(v.Input)
	case *chplan.Project:
		for _, proj := range v.Projections {
			if proj.Alias == "Attributes" {
				return true
			}
		}
	}
	return false
}
