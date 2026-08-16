package logql

import (
	"fmt"
	"slices"
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
	if top, ok := fuseVariants(lowered); ok {
		return variantFusedSampleShape(top, s, lc), nil
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

// variantValueSlotColumn names one DISTINCT per-sample value column the fused
// window reads. Several arms may point at the same slot when they apply
// different reducers to the same value expression.
func variantValueSlotColumn(slot int) string {
	return fmt.Sprintf("%s_%d", rangeAggSynthValueColumn, slot)
}

// variantLabelFor renders the `__variant__` value for arm i — its zero-based
// index as a decimal string, matching reference Loki's appendVariantLabel.
func variantLabelFor(i int) string {
	return strconv.Itoa(i)
}

// fuseVariants collapses N independently-lowered variant arms into ONE plan
// that reads the shared log stream a single time, reporting ok=false when the
// arms do not in fact share one.
//
// An arm is a range aggregation, optionally under a per-arm pipeline — the
// vector-aggregation wrap `sum by (level) (count_over_time({…}[r]))` puts an
// Aggregate and a re-shaping Project above the window. [peelVariantWrap]
// splits each arm into that wrap and its window, [fuseVariantArms] fuses the
// windows, and the wrap is re-applied ONCE with the fused window's variant
// column threaded through it, so the arms stay separated by
// `__variant__` at every grouping above the window.
//
// The wrap must be identical across arms: a per-arm pipeline that differs
// (`variants(sum by (svc) (count_over_time(…)), count_over_time(…))`) cannot
// run as one pipeline, so those arms keep the per-arm shape.
func fuseVariants(arms []chplan.Node) (chplan.Node, bool) {
	const minFusedArms = 2
	if len(arms) < minFusedArms {
		return nil, false
	}
	wraps := make([][]chplan.Node, 0, len(arms))
	windows := make([]chplan.Node, 0, len(arms))
	for _, arm := range arms {
		wrap, rw, ok := peelVariantWrap(arm)
		if !ok {
			return nil, false
		}
		wraps = append(wraps, wrap)
		windows = append(windows, rw)
	}
	if !variantWrapsAgree(wraps) {
		return nil, false
	}
	fused, ok := fuseVariantArms(windows)
	if !ok {
		return nil, false
	}
	return rebuildVariantWrap(wraps[0], fused, fused.VariantColumn)
}

// maxVariantWrapDepth bounds the per-arm pipeline peel. The wraps the LogQL
// lowering produces above a range aggregation are at most an Aggregate plus
// its re-shaping Project; the bound stops an unexpected deep plan from being
// walked (and re-applied) node by node.
const maxVariantWrapDepth = 4

// peelVariantWrap splits a lowered arm into the pipeline above its range
// window (outermost first) and the window itself. It reports ok=false for an
// arm whose pipeline holds a node the variant column cannot be threaded
// through, since re-applying such a node once for all arms would merge rows
// that must stay separated by arm.
func peelVariantWrap(arm chplan.Node) ([]chplan.Node, *chplan.RangeWindow, bool) {
	var wrap []chplan.Node
	n := arm
	for range maxVariantWrapDepth {
		switch v := n.(type) {
		case *chplan.RangeWindow:
			return wrap, v, true
		case *chplan.Project:
			wrap = append(wrap, v)
			n = v.Input
		case *chplan.Aggregate:
			// A Having clause filters post-aggregation rows; threading the
			// variant column through it would need the predicate rewritten
			// per arm, which the fused single pipeline cannot express.
			if v.Having != nil {
				return nil, nil, false
			}
			wrap = append(wrap, v)
			n = v.Input
		default:
			return nil, nil, false
		}
	}
	return nil, nil, false
}

// variantWrapsAgree reports whether every arm's pipeline above the window is
// the same pipeline. The comparison rebuilds each wrap over a shared stand-in
// so it sees the pipeline alone, not the windows the arms differ in.
func variantWrapsAgree(wraps [][]chplan.Node) bool {
	base, ok := rebuildVariantWrap(wraps[0], &chplan.OneRow{}, "")
	if !ok {
		return false
	}
	for _, w := range wraps[1:] {
		if len(w) != len(wraps[0]) {
			return false
		}
		other, ok := rebuildVariantWrap(w, &chplan.OneRow{}, "")
		if !ok || !base.Equal(other) {
			return false
		}
	}
	return true
}

// rebuildVariantWrap re-applies a peeled pipeline (outermost first) over
// input, threading variantCol through every node so the arms stay separated:
// an Aggregate gains it as a group key, a Project gains it as a passthrough
// projection. An empty variantCol rebuilds the pipeline unchanged, which is
// what [variantWrapsAgree] compares.
//
// It reports ok=false if a node already carries the column, since the plan
// would then have two columns of that name and the arms' identity would
// depend on which one a consumer read.
func rebuildVariantWrap(wrap []chplan.Node, input chplan.Node, variantCol string) (chplan.Node, bool) {
	n := input
	for i := len(wrap) - 1; i >= 0; i-- {
		switch v := wrap[i].(type) {
		case *chplan.Project:
			// An empty Projections slice is the pass-through `SELECT *`
			// form, and Replacements is its `* REPLACE (...)` modifier.
			// Appending a projection to either would turn the whole
			// pass-through into a one-column SELECT and silently drop every
			// other column, so refuse rather than rewrite a shape this was
			// not designed for.
			if len(v.Projections) == 0 || len(v.Replacements) > 0 {
				return nil, false
			}
			c := *v
			c.Input = n
			c.Projections = slices.Clone(v.Projections)
			if variantCol != "" {
				for _, p := range c.Projections {
					if p.Alias == variantCol {
						return nil, false
					}
				}
				c.Projections = append(c.Projections, chplan.Projection{
					Expr:  &chplan.ColumnRef{Name: variantCol},
					Alias: variantCol,
				})
			}
			n = &c
		case *chplan.Aggregate:
			c := *v
			c.Input = n
			c.GroupBy = slices.Clone(v.GroupBy)
			c.GroupByAliases = slices.Clone(v.GroupByAliases)
			if variantCol != "" {
				if slices.Contains(c.GroupByAliases, variantCol) {
					return nil, false
				}
				c.GroupBy = append(c.GroupBy, &chplan.ColumnRef{Name: variantCol})
				// GroupByAliases is a parallel slice when non-empty, so it
				// only grows alongside GroupBy when it is already in use.
				if len(c.GroupByAliases) > 0 {
					c.GroupByAliases = append(c.GroupByAliases, variantCol)
				}
			}
			n = &c
		default:
			return nil, false
		}
	}
	return n, true
}

// fuseVariantArms collapses N independently-lowered range windows into ONE
// window that reads the shared log stream a single time, reporting ok=false
// when the arms do not in fact share one.
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
	// arm 0, then each DISTINCT value expression under its own slot alias.
	// Multiple arms can read one slot when they differ only in the reducer.
	base := projects[0]
	valueRepresentatives := make([]chplan.Projection, 0, len(projects))
	armValueColumns := make([]string, 0, len(projects))
	for _, p := range projects {
		value := p.Projections[valueIdx]
		slot := slices.IndexFunc(valueRepresentatives, func(existing chplan.Projection) bool {
			return projectionEqual(existing, value)
		})
		if slot < 0 {
			slot = len(valueRepresentatives)
			valueRepresentatives = append(valueRepresentatives, value)
		}
		armValueColumns = append(armValueColumns, variantValueSlotColumn(slot))
	}
	var shared []chplan.Projection
	shared = append(shared, base.Projections[:valueIdx]...)
	for slot, p := range valueRepresentatives {
		shared = append(shared, chplan.Projection{
			Expr:  p.Expr,
			Alias: variantValueSlotColumn(slot),
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
			ValueColumn: armValueColumns[i],
			Label:       variantLabelFor(i),
		})
	}
	return &fused, true
}

// sharedValueProjectionIndex locates the per-sample value projection. It
// reports ok=false unless every Project has the same projection list length,
// differs at no more than one position, and the selected position is the
// [rangeAggSynthValueColumn] alias in every arm. Zero differing positions is
// the shared-value case: the arms apply different reducers to one value slot.
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
		if armsAgreeAtProjection(projects, i) {
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
		// Every projection agrees. Locate the one canonical value alias so
		// the fused Project can expose it once and point every arm at it.
		for i, p := range base.Projections {
			if p.Alias != rangeAggSynthValueColumn {
				continue
			}
			if valueIdx >= 0 {
				return 0, false
			}
			valueIdx = i
		}
		if valueIdx < 0 {
			return 0, false
		}
	}
	for _, p := range projects {
		if p.Projections[valueIdx].Alias != rangeAggSynthValueColumn {
			return 0, false
		}
	}
	return valueIdx, true
}

// armsAgreeAtProjection reports whether every arm projects the same thing at
// position i, so the caller can find the ONE position they differ at.
func armsAgreeAtProjection(projects []*chplan.Project, i int) bool {
	for _, p := range projects[1:] {
		if !projectionEqual(projects[0].Projections[i], p.Projections[i]) {
			return false
		}
	}
	return true
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
		Fn: chplan.FnMapMerge,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: cols.attrsCol},
			&chplan.FuncCall{
				Fn: chplan.FnMap,
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

// variantFusedSampleShape re-shapes the fused plan into the canonical Sample
// contract, folding the fused window's own variant COLUMN into Attributes
// where [variantSampleArm] stamps a per-arm literal.
func variantFusedSampleShape(top chplan.Node, s schema.Logs, lc lowerCtx) chplan.Node {
	cols := logSampleColumns(top, s)
	tsExpr := cols.timeExpr
	if !cols.hasNativeTime && !lc.End.IsZero() {
		tsExpr = timeLiteralExpr(lc.End)
	}
	attrs := &chplan.FuncCall{
		Fn: chplan.FnMapMerge,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: cols.attrsCol},
			&chplan.FuncCall{
				Fn: chplan.FnMap,
				Args: []chplan.Expr{
					&chplan.LitString{V: variantLabel},
					&chplan.ColumnRef{Name: variantLabel},
				},
			},
		},
	}
	return &chplan.Project{
		Input: top,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: tsExpr, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}
}

// fusedVariantWindow returns the fused RangeWindow at the base of plan, if
// plan is a variant pipeline built over one. It walks the Project / Aggregate
// wrap [rebuildVariantWrap] re-applies.
func fusedVariantWindow(plan chplan.Node) (*chplan.RangeWindow, bool) {
	_, rw, ok := peelVariantWrap(plan)
	if !ok {
		return nil, false
	}
	return rw, len(rw.Variants) > 0
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
		if _, fused := fusedVariantWindow(p); fused {
			return isVectorAggregateSampleShape(p)
		}
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
