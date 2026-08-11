package logql

import (
	"fmt"
	"strconv"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// variantLabel is the reserved label LogQL stamps onto every series a
// `variants(...) of (...)` query produces, identifying which variant arm
// the series came from. The value is the variant's zero-based index
// rendered as a decimal string ("0", "1", …). Matches reference Loki's
// constants.VariantLabel.
const variantLabel = "__variant__"

// lowerMultiVariant lowers LogQL's `variants(m0, m1, …) of ({selector}[r])`
// multi-variant metric form (grafana/loki/v3 pkg/logql/syntax
// MultiVariantExpr).
//
// Reference semantics: each variant `mi` is a complete metric expression
// (a range / vector aggregation over its own selector). The query
// evaluates every variant in one pass over the shared `of (...)` log
// selector and returns the UNION of all variants' series — each series
// keeps its native label set PLUS a synthetic `__variant__="<i>"` label
// carrying the variant's index. So
//
//	variants(count_over_time({app="foo"}[1m]),
//	         bytes_over_time({app="foo"}[1m])) of ({app="foo"}[1m])
//
// yields the count series tagged `__variant__="0"` and the bytes series
// tagged `__variant__="1"`, both keyed on the original `{app="foo"}`
// identity. (See grafana/loki pkg/logql/log/consolidated_variant_extractor.go
// — `appendVariantLabel` adds constants.VariantLabel with the arm index.)
//
// Lowering approach: each variant lowers independently through the
// ordinary [lower] dispatch (it is already a self-contained SampleExpr
// carrying its own selector — the `of (...)` arm is upstream's
// scan-fusion hint, not an extra data source), and the arms are then
// FUSED whenever they turn out to read the same log stream.
//
// [fuseVariantArms] decides that structurally rather than trusting the
// `of (...)` hint: the parser does not require an arm's selector to match
// it, so `variants(count_over_time({a="1"}[5m]),
// count_over_time({b="2"}[5m])) of ({a="1"}[5m])` is legal and genuinely
// reads two streams. When the arms' lowered subtrees ARE identical below
// their per-sample value expression, they collapse into ONE
// [chplan.RangeWindow] carrying a [chplan.RangeWindowVariant] per arm,
// which the emitter renders as a single grouped pass reduced once per arm
// and unpivoted — one scan for N arms instead of N. Arms that are not
// identical (or whose range function the fused emitter does not reduce)
// keep the per-arm shape, which is correct and merely costs a scan each.
//
// The saving is the whole reason `variants(...)` exists rather than N
// separate queries, and it is not recoverable at the SQL level: ClickHouse
// inlines a multiply-referenced CTE and re-reads the table per reference,
// so a shared subquery measures identically to duplicated arms (issue
// #1501 records 10.0M read_rows for a two-arm query against the fused
// shape's 5.0M, and 15.0M for three arms against the same 5.0M).
//
// Each arm (fused or not) is re-projected into the canonical Sample shape
// (MetricName, Attributes, TimeUnix, Value) with `__variant__` folded into
// its Attributes map — a literal `"<i>"` per arm in the unfused shape, the
// fused window's own variant column in the fused one. Because the result
// already carries the canonical Sample columns, [Lang.ProjectSamples]
// recognises it (via [isVariantPlan]) and forwards it unchanged.
//
// The variant index is stamped via `mapConcat(<srcAttrs>, map('__variant__',
// '<i>'))`: ClickHouse's mapConcat lets later keys win on conflict, and
// `__variant__` is a reserved name no user label collides with, so the
// ordering is immaterial in practice — placing the synthetic map second
// keeps the contract explicit.
func lowerMultiVariant(e *syntax.MultiVariantExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	variants := e.Variants()
	if len(variants) == 0 {
		return nil, fmt.Errorf("logql: variants(...) has no variant arms")
	}

	lowered := make([]chplan.Node, 0, len(variants))
	for i, v := range variants {
		ve, ok := v.(syntax.Expr)
		if !ok {
			return nil, fmt.Errorf("logql: variant %d is not an Expr (%T)", i, v)
		}
		inner, err := lower(ve, s, lc)
		if err != nil {
			return nil, fmt.Errorf("logql: variant %d: %w", i, err)
		}
		lowered = append(lowered, inner)
	}

	// Fused shape: one scan feeding every arm's aggregation.
	if fused, ok := fuseVariantArms(lowered); ok {
		return variantFusedSampleShape(fused, s, lc), nil
	}

	arms := make([]chplan.Node, 0, len(lowered))
	for i, inner := range lowered {
		arms = append(arms, variantSampleArm(inner, s, lc, i))
	}

	// A single-arm `variants(m) of (...)` is legal LogQL; it still tags
	// the series with `__variant__="0"`. UnionAll with one input renders
	// as the bare arm (no UNION ALL keyword), which is correct.
	return &chplan.UnionAll{Inputs: arms}, nil
}

// variantArmValueColumn names the per-sample value column the fused window
// reads for arm i. The unfused arms all project their value under the one
// [rangeAggSynthValueColumn] alias; fusing them into a single row means each
// needs its own column, so they are re-aliased by index.
func variantArmValueColumn(i int) string {
	return fmt.Sprintf("%s_%d", rangeAggSynthValueColumn, i)
}

// variantLabelFor renders the `__variant__` value for arm i — its zero-based
// index as a decimal string, matching reference Loki's appendVariantLabel.
func variantLabelFor(i int) string {
	return strconv.Itoa(i)
}

// fuseVariantArms collapses N independently-lowered variant arms into ONE
// RangeWindow that reads the shared log stream a single time, reporting
// ok=false when the arms do not in fact share one.
//
// The arms are fusible exactly when they are the same plan but for the
// per-sample value they compute, i.e. each arm is
//
//	RangeWindow{Func: fᵢ}
//	  Project[<shared identity>, <shared timestamp>, vᵢ AS Value]
//	    <shared subtree>
//
// with every RangeWindow field except Func equal, every projection except the
// `Value` one equal, and the subtrees below structurally equal. That is what
// `variants(...)` produces when its arms genuinely share the `of (...)`
// selector — and it is checked rather than assumed, because the parser does
// not force an arm's selector to match the `of (...)` one.
//
// A single arm is never fused: there is no second arm to share the pass with,
// and the ordinary path already emits exactly one scan for it.
func fuseVariantArms(arms []chplan.Node) (*chplan.RangeWindow, bool) {
	const minFusedArms = 2
	if len(arms) < minFusedArms {
		return nil, false
	}
	windows := make([]*chplan.RangeWindow, 0, len(arms))
	projects := make([]*chplan.Project, 0, len(arms))
	for _, arm := range arms {
		rw, ok := arm.(*chplan.RangeWindow)
		// An arm already carrying Variants is a fused window, which the
		// lowering never nests; and a function the fused emitter cannot
		// reduce keeps the whole query on the per-arm path.
		if !ok || len(rw.Variants) > 0 || rw.Identity || !chplan.FusibleWindowFunc(rw.Func) {
			return nil, false
		}
		proj, ok := rw.Input.(*chplan.Project)
		if !ok {
			return nil, false
		}
		windows = append(windows, rw)
		projects = append(projects, proj)
	}
	valueIdx, ok := sharedValueProjectionIndex(projects)
	if !ok {
		return nil, false
	}
	if !windowsAgreeModuloFunc(windows) {
		return nil, false
	}
	// The subtrees the arms' Projects read must be the SAME stream — this is
	// the check that makes the fusion honest rather than a bet on the
	// `of (...)` hint.
	for _, p := range projects[1:] {
		if !p.Input.Equal(projects[0].Input) {
			return nil, false
		}
	}

	// Shared input Project: every projection but the value one verbatim from
	// arm 0, then each arm's own value expression under its own alias.
	base := projects[0]
	shared := make([]chplan.Projection, 0, len(base.Projections)+len(projects)-1)
	shared = append(shared, base.Projections[:valueIdx]...)
	for i, p := range projects {
		shared = append(shared, chplan.Projection{
			Expr:  p.Projections[valueIdx].Expr,
			Alias: variantArmValueColumn(i),
		})
	}
	shared = append(shared, base.Projections[valueIdx+1:]...)

	fused := *windows[0]
	fused.Input = &chplan.Project{Input: base.Input, Projections: shared}
	// Func / ValueColumn describe no single arm now: each arm names its own
	// pair, and ValueColumn becomes the OUTPUT alias the unpivot writes.
	fused.Func = ""
	fused.ValueColumn = rangeAggSynthValueColumn
	fused.VariantColumn = variantLabel
	fused.Variants = make([]chplan.RangeWindowVariant, 0, len(windows))
	for i, w := range windows {
		fused.Variants = append(fused.Variants, chplan.RangeWindowVariant{
			Func:        w.Func,
			ValueColumn: variantArmValueColumn(i),
			Label:       variantLabelFor(i),
		})
	}
	return &fused, true
}

// sharedValueProjectionIndex locates the ONE projection position at which the
// arms' input Projects differ — the per-sample value expression. It reports
// ok=false unless every Project has the same projection list length, differs
// at exactly one position, and that position is the [rangeAggSynthValueColumn]
// alias in every arm.
//
// Requiring the differing position to be the value alias is what keeps the
// fusion sound: two arms differing in their IDENTITY projection describe
// different series, and folding them onto one grouped pass would report one
// arm's grouping for both.
func sharedValueProjectionIndex(projects []*chplan.Project) (int, bool) {
	base := projects[0]
	for _, p := range projects[1:] {
		if len(p.Projections) != len(base.Projections) {
			return 0, false
		}
	}
	valueIdx := -1
	for i := range base.Projections {
		same := true
		for _, p := range projects[1:] {
			if !projectionEqual(base.Projections[i], p.Projections[i]) {
				same = false
				break
			}
		}
		if same {
			continue
		}
		if valueIdx >= 0 {
			// More than one differing position: the arms diverge somewhere
			// the fused pass cannot represent.
			return 0, false
		}
		valueIdx = i
	}
	if valueIdx < 0 {
		// Every projection agrees. The arms compute the identical value, so
		// there is no per-arm column to fan out; leaving them alone keeps the
		// plan honest (and this is a degenerate query nobody writes).
		return 0, false
	}
	for _, p := range projects {
		if p.Projections[valueIdx].Alias != rangeAggSynthValueColumn {
			return 0, false
		}
	}
	return valueIdx, true
}

// projectionEqual compares two projections by alias and expression.
func projectionEqual(a, b chplan.Projection) bool {
	if a.Alias != b.Alias {
		return false
	}
	if a.Expr == nil || b.Expr == nil {
		return a.Expr == nil && b.Expr == nil
	}
	return a.Expr.Equal(b.Expr)
}

// windowsAgreeModuloFunc reports whether the arms' RangeWindows are identical
// apart from the range function each applies — the grid, the window, the
// grouping and the source columns must all match for one pass to serve them
// all. It compares through the IR's own [chplan.Node.Equal] with the Func
// neutralised and a shared Input, so a field added to RangeWindow later is
// covered without this predicate being updated.
func windowsAgreeModuloFunc(windows []*chplan.RangeWindow) bool {
	norm := func(w *chplan.RangeWindow) *chplan.RangeWindow {
		c := *w
		c.Func = ""
		// Input is compared separately (the arms' Projects differ by design
		// at the value expression); a shared stand-in keeps Equal focused on
		// the window's own fields.
		c.Input = &chplan.OneRow{}
		return &c
	}
	base := norm(windows[0])
	for _, w := range windows[1:] {
		if !base.Equal(norm(w)) {
			return false
		}
	}
	return true
}

// variantFusedSampleShape re-shapes the fused window into the canonical
// Sample contract, folding the window's own variant column into Attributes
// instead of the per-arm literal [variantSampleArm] stamps.
func variantFusedSampleShape(fused *chplan.RangeWindow, s schema.Logs, lc lowerCtx) chplan.Node {
	cols := logSampleColumns(fused, s)
	tsExpr := cols.timeExpr
	if !cols.hasNativeTime && !lc.End.IsZero() {
		tsExpr = timeLiteralExpr(lc.End)
	}
	attrs := &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: cols.attrsCol},
			&chplan.FuncCall{
				Name: "map",
				Args: []chplan.Expr{
					&chplan.LitString{V: variantLabel},
					&chplan.ColumnRef{Name: fused.VariantColumn},
				},
			},
		},
	}
	return &chplan.Project{
		Input: fused,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: tsExpr, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}
}

// variantSampleArm re-shapes a lowered variant arm into the canonical
// Sample contract (MetricName, Attributes, TimeUnix, Value) and folds the
// `__variant__="<index>"` label into its Attributes map.
//
// Source-column resolution reuses [logSampleColumns], so the arm keeps
// the right identity / timestamp columns regardless of its inner shape:
//
//   - a bare range aggregation (`count_over_time({...}[r])`) surfaces its
//     `ResourceAttributes` identity and, in matrix mode, its per-anchor
//     `anchor_ts`;
//   - a vector aggregation (`sum by (app) (count_over_time({...}[r]))`)
//     surfaces the canonical `Attributes` / `TimeUnix` aliases its
//     [wrapVectorAggregateForSample] wrap already produced.
//
// Instant-query timestamp anchoring mirrors [Lang.ProjectSamples]: a
// known request End anchors the synthetic TimeUnix inside the window
// (CH evaluates now64 at execution time, which is load-sensitive); a
// bare lowering with no window falls back to `now64(9)`.
func variantSampleArm(inner chplan.Node, s schema.Logs, lc lowerCtx, index int) chplan.Node {
	cols := logSampleColumns(inner, s)

	// TimeUnix source: forward the per-anchor / vector-aggregate column
	// when the inner shape already carries one; otherwise anchor an
	// instant sample at the request End (when known) so a matrix step
	// grid keeps the single point inside the window.
	tsExpr := cols.timeExpr
	if !cols.hasNativeTime && !lc.End.IsZero() {
		tsExpr = timeLiteralExpr(lc.End)
	}

	attrs := &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: cols.attrsCol},
			&chplan.FuncCall{
				Name: "map",
				Args: []chplan.Expr{
					&chplan.LitString{V: variantLabel},
					&chplan.LitString{V: variantLabelFor(index)},
				},
			},
		},
	}

	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: tsExpr, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}
}

// isVariantPlan reports whether plan is what a multi-variant lowering
// produces — either the UnionAll of per-arm subtrees, every arm already in
// the canonical Sample shape (a top-level Project aliasing `Attributes`), or
// the fused single-pass shape, a Sample-shaped Project over a RangeWindow
// carrying its arms in [chplan.RangeWindow.Variants].
//
// [Lang.ProjectSamples] uses this to forward the plan unchanged rather than
// wrapping it in the generic metric Sample reshape (which would re-reference
// `ResourceAttributes`, a column the variant Project has already consumed
// into `Attributes`).
func isVariantPlan(plan chplan.Node) bool {
	if p, ok := plan.(*chplan.Project); ok {
		rw, ok := p.Input.(*chplan.RangeWindow)
		return ok && len(rw.Variants) > 0 && isVectorAggregateSampleShape(p)
	}
	u, ok := plan.(*chplan.UnionAll)
	if !ok || len(u.Inputs) == 0 {
		return false
	}
	for _, arm := range u.Inputs {
		if !isVectorAggregateSampleShape(arm) {
			return false
		}
	}
	return true
}
