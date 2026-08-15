package optimizer

import (
	"sort"

	"github.com/tsouza/cerberus/internal/chplan"
)

// ProjectionPushdown narrows a Scan's column list to the union of columns
// referenced by an immediately-enclosing Project (and any intervening
// Filter). With the OTel CH schema's wide tables (~10+ columns including
// resource attributes), this is a real win: only the columns the plan
// actually consumes get read.
//
// Two shapes match:
//
//  1. `Project(Scan)` — narrow Scan.Columns to the column refs the
//     Projections touch.
//  2. `Project(Filter(Scan))` — push the projection THROUGH the Filter
//     down to the Scan. Safe iff Scan.Columns is empty (the Filter's
//     predicate evaluates on Scan's row shape, which after narrowing
//     must still include every column the predicate touches). The
//     narrowed Scan.Columns is the UNION of refs(Projections) ∪
//     refs(Filter.Predicate); the Filter stays in place between the
//     Project and the (now-narrowed) Scan.
//
// Shape (2) handles the `Project(Filter(Scan))` chain that lowerings
// emit directly (a projection wrapping a label-filtered scan): without
// this widening the pushdown would stop at the intervening Filter and
// leave the Scan reading every column.
//
// Two further shapes match a *mid-tree stage* node — an Aggregate or a
// RangeWindow — sitting directly over Scan or Filter(Scan):
//
//  3. `Aggregate(Scan)` / `Aggregate(Filter(Scan))` — narrow the Scan to
//     the union of the columns the Aggregate's GroupBy keys, AggFunc
//     params/args, and Having predicate read, plus the Filter predicate's
//     columns when present.
//  4. `RangeWindow(Scan)` / `RangeWindow(Filter(Scan))` — narrow the Scan
//     to the union of TimestampColumn + ValueColumn (the per-sample pair
//     the windowed-array idiom reads), TemporalityColumn when set, each
//     fused Variants arm's ValueColumn, the GroupBy + ScalarExprs column
//     refs, plus the Filter predicate's columns when present.
//  5. `RangeWindowNative(Scan)` / `RangeWindowNative(Filter(Scan))` — the
//     experimental ClickHouse-native `timeSeriesRateToGrid` lowering.
//     emitRangeWindowNative reads EXACTLY three things off the inner Scan:
//     the per-series GroupBy identity refs (the `GROUP BY` of the inner
//     SELECT), and the (TimestampColumn, ValueColumn) pair fed positionally
//     into the timeSeriesRateToGrid aggregate. Every other identifier the
//     native emit names (`grid` / `grid_ts` / `grid_val` / `anchor_ts`) is
//     SYNTHETIC — produced inside the subquery, never read off the Scan —
//     so the narrowed column set is the same union shape as shape (4) minus
//     ScalarExprs (the native node carries none). This is the
//     correctness-critical arm: dropping any of MetricName-class identity
//     columns the GroupBy walks, or the ts/value pair, 502s at runtime with
//     `Unknown expression identifier`. nativeRangeWindowColumns enumerates
//     exactly the emit's reads, and applyStageScan unions in the Filter
//     predicate's columns so the predicate stays evaluable.
//
// Four more shapes match a mid-tree stage node sitting directly over Scan
// or Filter(Scan) — the array-valued / matrix-pipeline siblings of (3)/(4):
//
//  6. `RangeBucketFanout(Scan)` / `RangeBucketFanout(Filter(Scan))` — the
//     single-pass array-aggregate fan-out backing the histogram range
//     lowerings. narrows the Scan to TimestampCol (the argMax tie-break /
//     fan-out distance column, also the `HAVING uniqExact(...)` argument)
//     plus the columns the GroupBy keys and each AggFunc's Params/Args
//     read. The emitter's fanout SELECT projects `*` from Input, but only
//     to hand those exact columns through to the collapse SELECT — see
//     rangeBucketFanoutColumns.
//  7. `RangeLWR(Scan)` / `RangeLWR(Filter(Scan))` — the single-pass
//     last-with-respect-to fan-out for a bare instant-vector selector.
//     Narrows the Scan to exactly the canonical 4-column Sample identity:
//     MetricNameCol, AttributesCol, TimestampCol, ValueCol.
//  8. `MetricsAggregate(Scan)` / `MetricsAggregate(Filter(Scan))` — the
//     TraceQL metrics-pipeline aggregate (`rate()` / `*_over_time()`).
//     Its child slot is named Inner, not Input. Narrows the Scan to the
//     union of the GroupBy key refs and Attr (nil for rate /
//     count_over_time). When this MetricsAggregate is itself wrapped by a
//     RangeWindow (the matrix path, `RangeWindow(MetricsAggregate(...))`),
//     a companion case on the RangeWindow arm — reached after this one,
//     since the walk is bottom-up — widens the Scan this shape narrowed to
//     also include the RangeWindow's own TimestampColumn, which
//     emitRangeWindowMetrics reads directly off the same Scan for
//     time-bucketing. See applyRangeWindowOverMetricsAggregate.
//  9. `HistogramQuantile(Scan)` / `HistogramQuantile(Filter(Scan))` — the
//     classic-histogram quantile interpolation. Narrows the Scan to
//     BucketCountsColumn, ExplicitBoundsColumn, plus the GroupBy refs.
//     MetricNameColumn / AttributesColumn / TimestampColumn are NOT
//     included: emitHistogramQuantile never reads them off Input — they
//     are metadata the wrapping lowering uses to build the Sample-contract
//     Project ABOVE this node (AttributesColumn's own read, when present,
//     already rides in via GroupBy). PhiExpr is likewise excluded — a
//     computed phi is a self-contained ScalarSubquery over a SEPARATE
//     plan, never a reference into this node's own Input.
//
// Shapes (3)–(9) are what makes the pushdown reach the inner Scan of the
// canonical metrics shape `Project(Aggregate(Filter(Scan)))` (and the
// matrix `Project(RangeWindow(Filter(Scan)))`, and the array/matrix
// pipeline shapes above): the Project's pushdown in shapes (1)/(2) stops
// at the stage node, so without these arms the inner Scan still reads
// every column (`SELECT *`). Firing on the stage node itself — wherever
// the FixedPoint walk reaches it — pushes the narrowed column set THROUGH
// the stage down to the Scan.
type ProjectionPushdown struct{}

func (ProjectionPushdown) Name() string { return "projection-pushdown" }

func (ProjectionPushdown) Apply(n chplan.Node) (chplan.Node, bool) {
	switch node := n.(type) {
	case *chplan.Project:
		switch child := node.Input.(type) {
		case *chplan.Scan:
			return applyProjectScan(node, child)
		case *chplan.Filter:
			return applyProjectFilterScan(node, child)
		default:
			return n, false
		}
	case *chplan.Aggregate:
		return applyStageScan(node, node.Input, aggregateColumns(node))
	case *chplan.RangeWindow:
		if ma, ok := node.Input.(*chplan.MetricsAggregate); ok {
			return applyRangeWindowOverMetricsAggregate(node, ma)
		}
		return applyStageScan(node, node.Input, rangeWindowColumns(node))
	case *chplan.RangeWindowNative:
		return applyStageScan(node, node.Input, nativeRangeWindowColumns(node))
	case *chplan.RangeWindowResample:
		return applyStageScan(node, node.Input, resampleRangeWindowColumns(node))
	case *chplan.RangeBucketFanout:
		return applyStageScan(node, node.Input, rangeBucketFanoutColumns(node))
	case *chplan.RangeLWR:
		return applyStageScan(node, node.Input, rangeLWRColumns(node))
	case *chplan.MetricsAggregate:
		return applyStageScan(node, node.Inner, metricsAggregateColumns(node))
	case *chplan.HistogramQuantile:
		return applyStageScan(node, node.Input, histogramQuantileColumns(node))
	default:
		return n, false
	}
}

// applyStageScan pushes a stage node's required-column set THROUGH an
// optional intervening Filter down to the inner Scan, narrowing the Scan
// in place. `stage` is the Aggregate / RangeWindow being rewritten,
// `input` is stage's child (the Scan or Filter(Scan)), and stageCols is
// the set of base columns the stage's own emit reads.
//
// The inner Scan is located as either `input` directly (Scan) or
// `input.(*Filter).Input` (Filter(Scan)). Any other shape bails — this
// keeps the rule total and avoids narrowing under a MetricsAggregate /
// subquery input the stage emitters read differently.
//
// The idempotence guard `len(scan.Columns) > 0` mirrors the two existing
// helpers: it stops the rule re-firing under the FixedPoint strategy and
// avoids fighting MVSubstitution, which deliberately clears Columns when
// it rewrites a Scan onto a rollup MV.
func applyStageScan(stage, input chplan.Node, stageCols []string) (chplan.Node, bool) {
	switch in := input.(type) {
	case *chplan.Scan:
		if len(in.Columns) > 0 {
			return stage, false
		}
		cols := unionSortedColumns(stageCols, nil)
		if len(cols) == 0 {
			return stage, false
		}
		newScan := *in
		newScan.Columns = cols
		return cloneStageOverInput(stage, &newScan), true
	case *chplan.Filter:
		scan, ok := in.Input.(*chplan.Scan)
		if !ok || len(scan.Columns) > 0 {
			return stage, false
		}
		cols := unionSortedColumns(stageCols, predicateColumns(in.Predicate))
		if len(cols) == 0 {
			return stage, false
		}
		newScan := *scan
		newScan.Columns = cols
		newFilter := *in
		newFilter.Input = &newScan
		return cloneStageOverInput(stage, &newFilter), true
	default:
		return stage, false
	}
}

// cloneStageOverInput returns a shallow clone of the stage node (Aggregate
// or RangeWindow) with its Input replaced by newInput. The clone keeps the
// optimizer's no-mutate-in-place contract: the original tree is untouched,
// the rewritten subtree is fresh.
func cloneStageOverInput(stage, newInput chplan.Node) chplan.Node {
	switch s := stage.(type) {
	case *chplan.Aggregate:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeWindow:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeWindowNative:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeWindowResample:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeBucketFanout:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeLWR:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.MetricsAggregate:
		clone := *s
		clone.Inner = newInput
		return &clone
	case *chplan.HistogramQuantile:
		clone := *s
		clone.Input = newInput
		return &clone
	default:
		return stage
	}
}

// aggregateColumns returns the sorted, deduped set of base columns an
// Aggregate's own emit reads: the union of every GroupBy key expression,
// every AggFunc's Params + Args, and Having. emitAggregate
// (chsql/emit_node.go) renders exactly these — GroupBy in the
// SELECT/GROUP BY, AggFuncs as the reducers, Having as a real SQL HAVING
// evaluated against the same row shape — so this is the complete set the
// narrowed Scan must carry.
//
// Having matters even though it never rides as a GroupBy key or an
// AggFunc arg: duplicateLabelsetGuardExpr (internal/promql/lower.go)
// plants `throwIf(uniqExact(MetricName) > 1, …) = 0` as an Aggregate's
// Having specifically because MetricName is NOT a GroupBy key for that
// shape (the guard exists to detect distinct names colliding under one
// dropped-name group) — so a narrowed Scan that only unions GroupBy +
// AggFuncs strips MetricName out from under a HAVING clause that
// references it, and ClickHouse fails outer-scope resolution with error
// 47 (UNKNOWN_IDENTIFIER).
func aggregateColumns(a *chplan.Aggregate) []string {
	var roots []chplan.Expr
	roots = append(roots, a.GroupBy...)
	roots = append(roots, aggFuncExprs(a.AggFuncs)...)
	roots = append(roots, a.Having)
	return stageColumns(nil, roots...)
}

// rangeWindowColumns returns the sorted, deduped set of base columns a
// RangeWindow's row-shape emit reads: the bare-string TimestampColumn +
// ValueColumn (the per-sample pair the windowed-array idiom consumes —
// added literally, they are plain column names, not Exprs), TemporalityColumn
// when set, each fused Variants arm's ValueColumn, plus the column refs
// walked out of GroupBy and ScalarExprs.
//
// TemporalityColumn matters even though the doc on that field frames it as
// an opt-in extra: whenever it is non-empty, emitWindowedArrayExtrapolated
// (chsql/range_window.go) reads `any(<TemporalityColumn>)` off Input inside
// the same innermost SELECT this pushdown narrows, unconditionally — so
// omitting it here narrows the Scan out from under a column the emit still
// references, and ClickHouse fails outer-scope resolution with error 47
// (UNKNOWN_IDENTIFIER). The default OTel-CH lowering never exposes the gap
// because augmentSelectorAttributes (internal/promql/lower.go) wraps the
// selector in a Project that carries TemporalityColumn whenever the schema
// sets both ResourceAttributesColumn and AggregationTemporalityColumn — a
// Project input applyStageScan declines outright. A schema that clears
// ResourceAttributesColumn (and has no dedicated top-level-column overlay)
// makes that Project a no-op that augmentSelectorAttributes skips
// (isBareAttributesRef, internal/promql/resource_attributes.go), so a
// `rate()` / `increase()` over an unambiguous Sum/Histogram-table selector
// on such a schema reaches this function with a bare Filter(Scan) Input and
// TemporalityColumn still set — the gap is live, not merely hypothetical.
//
// The fused Variants arms matter for the same reason each stage's own
// per-arm value column matters everywhere else in this file: LogQL's
// `variants(...)` construct (internal/logql/variants.go) fuses several
// range-function arms into one window, each naming its own ValueColumn,
// and the emitter (chsql/range_window_variants.go) reads every arm's
// column directly off Input. Today every LogQL fused Input arrives wrapped
// in a Project, which applyStageScan declines — so, like TemporalityColumn
// above, this is currently reached through a Project gate that keeps the
// omission latent rather than live. Collecting it here anyway keeps this
// function a complete, self-contained description of "every base column
// RangeWindow's own emit reads off Input", so the day a lowering emits a
// fused window directly over Scan/Filter(Scan), or applyStageScan grows a
// Project arm, the pushdown already narrows correctly instead of 502ing.
//
// Only the row-shape (PromQL / LogQL) RangeWindow over Scan / Filter(Scan)
// reaches this: the Apply switch routes a RangeWindow whose Input is a
// *chplan.MetricsAggregate — the TraceQL matrix mode, which ignores
// ValueColumn and reads the per-span Timestamp off its MetricsAggregate.Inner
// — to applyRangeWindowOverMetricsAggregate instead, since that shape needs
// to WIDEN an already-narrowed Scan rather than narrow one directly.
func rangeWindowColumns(r *chplan.RangeWindow) []string {
	bare := []string{r.TimestampColumn, r.ValueColumn, r.TemporalityColumn}
	for _, v := range r.Variants {
		bare = append(bare, v.ValueColumn)
	}
	var roots []chplan.Expr
	roots = append(roots, r.GroupBy...)
	roots = append(roots, r.ScalarExprs...)
	return stageColumns(bare, roots...)
}

// applyRangeWindowOverMetricsAggregate handles the TraceQL matrix shape
// `RangeWindow(MetricsAggregate(Scan))` / `RangeWindow(MetricsAggregate(Filter(Scan)))`.
// applyStageScan's own RangeWindow case never reaches this shape (its inner
// switch only matches Scan / Filter(Scan) directly), so by design it is the
// MetricsAggregate arm — firing first, since applyToTree walks bottom-up —
// that narrows the Scan beneath ma. But metricsAggregateColumns only
// enumerates the columns MetricsAggregate's OWN emit reads (GroupBy + Attr);
// per MetricsAggregate's doc comment, emitRangeWindowMetrics additionally
// reads the wrapping RangeWindow's OWN TimestampColumn slot directly off
// that same underlying Scan for time-bucketing. A MetricsAggregate node
// carries no reference to its parent, so the MetricsAggregate arm cannot
// know about this requirement — only the RangeWindow arm, which sees the
// already-rewritten (possibly already-narrowed) MetricsAggregate as its
// Input, can supply it.
//
// This widens the already-narrowed inner Scan by unioning in
// r.TimestampColumn whenever it is missing. It deliberately does NOT use
// applyStageScan/cloneStageOverInput: those assume the stage sits directly
// over Scan/Filter(Scan) and bail on any other input shape (including
// MetricsAggregate) by construction — this is a distinct shape needing a
// widen instead of a first narrow.
func applyRangeWindowOverMetricsAggregate(r *chplan.RangeWindow, ma *chplan.MetricsAggregate) (chplan.Node, bool) {
	if r.TimestampColumn == "" {
		return r, false
	}
	switch inner := ma.Inner.(type) {
	case *chplan.Scan:
		widened, ok := widenScanColumns(inner, r.TimestampColumn)
		if !ok {
			return r, false
		}
		newMA := *ma
		newMA.Inner = widened
		newR := *r
		newR.Input = &newMA
		return &newR, true
	case *chplan.Filter:
		scan, ok := inner.Input.(*chplan.Scan)
		if !ok {
			return r, false
		}
		widened, ok := widenScanColumns(scan, r.TimestampColumn)
		if !ok {
			return r, false
		}
		newFilter := *inner
		newFilter.Input = widened
		newMA := *ma
		newMA.Inner = &newFilter
		newR := *r
		newR.Input = &newMA
		return &newR, true
	default:
		return r, false
	}
}

// widenScanColumns unions extraCol into scan.Columns, returning the widened
// clone + true when it made a difference. It reports (nil, false) — no
// widening needed — in two cases: scan.Columns is still empty (SELECT *
// trivially already carries extraCol; narrowing it to just extraCol here
// would be a REGRESSION, pre-empting whatever set the eventual narrowing
// arm was going to apply), or extraCol is already present in the narrowed
// set (idempotence: a second FixedPoint iteration over an already-widened
// tree must report no change).
func widenScanColumns(scan *chplan.Scan, extraCol string) (*chplan.Scan, bool) {
	if len(scan.Columns) == 0 {
		return nil, false
	}
	widened := unionSortedColumns(scan.Columns, []string{extraCol})
	if len(widened) == len(scan.Columns) {
		return nil, false
	}
	newScan := *scan
	newScan.Columns = widened
	return &newScan, true
}

// nativeRangeWindowColumns returns the sorted, deduped set of base columns
// a RangeWindowNative's emit reads off the inner Scan. emitRangeWindowNative
// (chsql/range_window_native.go) reads EXACTLY three things off the Scan:
//
//   - TimestampColumn and ValueColumn — fed positionally into the
//     timeSeriesRateToGrid aggregate's second paren group (`Col(...)` each).
//   - the column refs walked out of GroupBy — the inner SELECT's series keys
//     and `GROUP BY` list (rendered by collectGroupByFrags).
//   - the column refs walked out of Recollapse — the deferred label-shaping
//     tower the middle (merge) level evaluates, which reads raw columns
//     (ResourceAttributes, ServiceName, …) the two-level shape only ever saw
//     through a Project that no longer exists once the shaping is hoisted.
//
// Every other identifier the native emit names — `grid`, `grid_ts`,
// `grid_val`, `anchor_ts`, `grid_state`, `shaped_key_<i>` — is SYNTHETIC
// (produced inside the subquery via the timeSeriesRateToGrid /
// timeSeriesRange / ARRAY JOIN machinery), so it is never a Scan read and must
// NOT be added here. The native node carries no ScalarExprs (unlike
// RangeWindow), so this set is strictly {TimestampColumn, ValueColumn} ∪
// refs(GroupBy) ∪ refs(Recollapse). Dropping any of these — in particular the
// identity columns the GroupBy walks (the MetricName-class #860/#861 failure)
// — 502s the native query at runtime, so the enumeration must match the emit's
// reads exactly.
//
// The Recollapse walk is deliberately redundant with the emitter's own
// containment guard (chsql.requireRecollapseColumnsGrouped), which rejects any
// deferred shaping expression reading a column that is not already a GroupBy
// key: enumeration alone would leave the emitter free to reference an
// ungrouped column, while containment alone would leave this pushdown one
// invariant-relaxation away from re-opening the #860/#861 dropped-column
// class. Both ship.
func nativeRangeWindowColumns(r *chplan.RangeWindowNative) []string {
	bare := []string{r.TimestampColumn, r.ValueColumn}
	var roots []chplan.Expr
	roots = append(roots, r.GroupBy...)
	roots = append(roots, projectionExprs(r.Recollapse)...)
	return stageColumns(bare, roots...)
}

// resampleRangeWindowColumns returns the sorted, deduped set of base columns a
// RangeWindowResample's emit reads off the inner Scan. emitRangeWindowResample
// (chsql/range_window_resample.go) reads EXACTLY four named columns:
//
//   - TimestampCol and ValueCol — fed positionally into the
//     timeSeriesResampleToGridWithStaleness aggregate's second paren group.
//   - MetricNameCol and AttributesCol — the inner SELECT's series keys and
//     `GROUP BY` list (and the canonical 4-column Sample identity columns).
//
// Every other identifier the emit names — `grid`, `grid_ts`, `grid_val`,
// `anchor_ts` — is SYNTHETIC (produced inside the subquery), so none is a Scan
// read. The node carries the column names as bare strings (no Exprs), so the
// set is strictly those four. Dropping any of them — in particular the identity
// columns — 502s the query at runtime, so the enumeration must match the emit's
// reads exactly (the same #860/#861 failure class the native-rate path guards).
func resampleRangeWindowColumns(r *chplan.RangeWindowResample) []string {
	return stageColumns([]string{r.MetricNameCol, r.AttributesCol, r.TimestampCol, r.ValueCol})
}

// rangeBucketFanoutColumns returns the sorted, deduped set of base columns
// a RangeBucketFanout's emit reads off the inner Input. emitRangeBucketFanout
// (chsql/range_bucket_fanout.go) projects `*` from Input into the fanout
// SELECT, but the collapse SELECT that consumes it only ever references
// TimestampCol (the argMax tie-break / fan-out distance column, also the
// HAVING uniqExact(...) argument when MinSamples > 1), the GroupBy source
// columns, and each AggFunc's Params + Args — so narrowing the Scan to
// exactly that union is safe.
func rangeBucketFanoutColumns(r *chplan.RangeBucketFanout) []string {
	var roots []chplan.Expr
	roots = append(roots, r.GroupBy...)
	roots = append(roots, aggFuncExprs(r.AggFuncs)...)
	return stageColumns([]string{r.TimestampCol}, roots...)
}

// rangeLWRColumns returns the sorted, deduped set of base columns a
// RangeLWR's emit reads off the inner Input. emitRangeLWR reads EXACTLY
// four named columns — MetricNameCol, AttributesCol, TimestampCol,
// ValueCol (mirrors resampleRangeWindowColumns).
func rangeLWRColumns(r *chplan.RangeLWR) []string {
	return stageColumns([]string{r.MetricNameCol, r.AttributesCol, r.TimestampCol, r.ValueCol})
}

// metricsAggregateColumns returns the sorted, deduped set of base columns
// a MetricsAggregate's own emit reads off its Inner input: the GroupBy
// key expressions plus Attr (the *_over_time / quantile_over_time operand;
// nil for rate / count_over_time). TimestampColumn is read off a WRAPPING
// RangeWindow's own slot when one is present, not off this node — a
// MetricsAggregate node has no parent pointer, so this function cannot
// enumerate it; applyRangeWindowOverMetricsAggregate widens the Scan this
// rule narrows to include it once the RangeWindow's own Apply case runs.
func metricsAggregateColumns(m *chplan.MetricsAggregate) []string {
	var roots []chplan.Expr
	roots = append(roots, m.GroupBy...)
	roots = append(roots, m.Attr)
	return stageColumns(nil, roots...)
}

// histogramQuantileColumns returns the sorted, deduped set of base columns
// a HistogramQuantile's own emit reads off Input: BucketCountsColumn and
// ExplicitBoundsColumn plus refs walked out of GroupBy. MetricNameColumn /
// AttributesColumn / TimestampColumn are NOT included: emitHistogramQuantile
// never references them — they are metadata the wrapping lowering uses to
// build the Sample contract Project ABOVE this node. PhiExpr is likewise
// excluded — it is a self-contained ScalarSubquery over a SEPARATE plan.
func histogramQuantileColumns(h *chplan.HistogramQuantile) []string {
	return stageColumns([]string{h.BucketCountsColumn, h.ExplicitBoundsColumn}, h.GroupBy...)
}

// stageColumns is the one derivation every *Columns enumerator above
// funnels through: "the set of base-relation column names this node's own
// emit reads off its input" is always bare column names (`bare`, empty
// entries skipped) union the ColumnRef leaves reachable from a set of
// expression roots (`roots`), sorted and deduped so the narrowed Scan's
// Columns is reproducible across runs. Before this helper existed the
// eight enumerators repeated this shape by hand — two of them
// (resampleRangeWindowColumns / rangeLWRColumns) with byte-identical
// bodies modulo the receiver type — which is exactly how aggregateColumns
// silently omitted Having and rangeWindowColumns silently omitted
// TemporalityColumn and the fused Variants arms: a field added to a plan
// node was eight places to remember, not one.
func stageColumns(bare []string, roots ...chplan.Expr) []string {
	seen := map[string]struct{}{}
	for _, c := range bare {
		if c != "" {
			seen[c] = struct{}{}
		}
	}
	collect := collectColumn(seen)
	for _, r := range roots {
		walkExpr(r, collect)
	}
	return sortedColumnSet(seen)
}

// aggFuncExprs flattens an AggFunc list's Params + Args, in order, into one
// Expr slice for feeding to stageColumns. Shared by aggregateColumns and
// rangeBucketFanoutColumns, whose AggFunc loops were character-for-character
// identical modulo the loop variable name.
func aggFuncExprs(fns []chplan.AggFunc) []chplan.Expr {
	var out []chplan.Expr
	for _, af := range fns {
		out = append(out, af.Params...)
		out = append(out, af.Args...)
	}
	return out
}

// projectionExprs flattens a Projection list's Expr fields, in order, into
// one Expr slice for feeding to stageColumns.
func projectionExprs(projs []chplan.Projection) []chplan.Expr {
	out := make([]chplan.Expr, 0, len(projs))
	for _, p := range projs {
		out = append(out, p.Expr)
	}
	return out
}

// sortedColumnSet flattens a column-name set into a sorted, deduped slice.
// The single sink every column-collecting function in this file — stageColumns,
// predicateColumns, referencedColumns — funnels through, so the narrowed
// Scan's Columns is reproducible across runs.
func sortedColumnSet(seen map[string]struct{}) []string {
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// applyProjectScan handles the seed `Project(Scan)` shape.
func applyProjectScan(p *chplan.Project, s *chplan.Scan) (chplan.Node, bool) {
	if len(s.Columns) > 0 {
		return p, false
	}
	cols := referencedColumns(p.Projections)
	if len(cols) == 0 {
		return p, false
	}
	newScan := *s
	newScan.Columns = cols
	newProject := *p
	newProject.Input = &newScan
	return &newProject, true
}

// applyProjectFilterScan handles `Project(Filter(Scan))`: push the
// projection's column set THROUGH the Filter down to the Scan, unioning
// in any columns the Filter's predicate references so the predicate
// remains evaluable on the narrowed row shape.
func applyProjectFilterScan(p *chplan.Project, f *chplan.Filter) (chplan.Node, bool) {
	s, ok := f.Input.(*chplan.Scan)
	if !ok || len(s.Columns) > 0 {
		return p, false
	}
	projCols := referencedColumns(p.Projections)
	if len(projCols) == 0 {
		return p, false
	}
	cols := unionSortedColumns(projCols, predicateColumns(f.Predicate))

	newScan := *s
	newScan.Columns = cols
	newFilter := *f
	newFilter.Input = &newScan
	newProject := *p
	newProject.Input = &newFilter
	return &newProject, true
}

// predicateColumns returns the set of ColumnRef names the predicate
// references, deduped and sorted. Bare-column refs only — qualified
// refs (joins, not in scope here) are not expected on a Filter directly
// over a Scan.
func predicateColumns(e chplan.Expr) []string {
	return stageColumns(nil, e)
}

// collectColumn returns a walkExpr visitor that records every column
// name an expression node reads into seen. Three node kinds carry one:
// ColumnRef (by definition), NestedArrayExists, whose Column field
// names the Nested carrier (e.g. `Events`) as a plain string rather
// than a child ColumnRef, and BoundedTraceScope, whose TraceIDColumn is
// read as the LHS of its `<TraceId> IN (...)` predicate (a bare
// outer-scope column, not a child ColumnRef) — omitting it would let
// ProjectionPushdown prune TraceId off the Scan beneath a gated leaf and
// emit an UNKNOWN_IDENTIFIER (the failure mode walkExpr's own doc names).
func collectColumn(seen map[string]struct{}) func(chplan.Expr) {
	return func(sub chplan.Expr) {
		switch v := sub.(type) {
		case *chplan.ColumnRef:
			seen[v.Name] = struct{}{}
		case *chplan.NestedArrayExists:
			seen[v.Column] = struct{}{}
		case *chplan.BoundedTraceScope:
			seen[v.TraceIDColumn] = struct{}{}
		}
	}
}

// unionSortedColumns merges two already-sorted, deduped column-name
// slices into a single sorted+deduped slice. Stable, deterministic
// output keeps Scan.Columns reproducible across runs.
func unionSortedColumns(a, b []string) []string {
	// No capacity hint: `len(a)+len(b)` is only an upper-bound pre-size
	// (a and b may overlap), so it has no observable effect on the
	// result — a gremlins ARITHMETIC_BASE mutant on the `+` is
	// equivalent and unkillable. Dropping the hint removes the dead
	// arithmetic; the union result is identical.
	seen := make(map[string]struct{})
	for _, n := range a {
		seen[n] = struct{}{}
	}
	for _, n := range b {
		seen[n] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// referencedColumns returns the set of ColumnRef names referenced anywhere
// in projs, deduped and sorted for deterministic emission.
func referencedColumns(projs []chplan.Projection) []string {
	return stageColumns(nil, projectionExprs(projs)...)
}

// walkExpr visits e and every Expr reachable from it. Every chplan
// Expr kind that holds child Exprs must appear here: a missing case
// silently hides the columns the subtree reads, ProjectionPushdown
// then prunes them from the Scan, and ClickHouse fails outer-scope
// resolution with error 47 (UNKNOWN_IDENTIFIER). That exact escape —
// FieldAccess.Source unwalked, so `| select(span.http.method)`'s
// SpanAttributes carrier was pruned on the plain-filter /api/search
// arm — 502'd Grafana's showcase select panel; see
// internal/api/tempo/search_select_plain_filter_chdb_test.go for the
// end-to-end pin. There's no chplan-side helper for this yet because
// the optimizer is so far the only caller; if we add a second consumer
// it should graduate into chplan.
func walkExpr(e chplan.Expr, visit func(chplan.Expr)) {
	if e == nil {
		return
	}
	visit(e)
	switch v := e.(type) {
	case *chplan.Binary:
		walkExpr(v.Left, visit)
		walkExpr(v.Right, visit)
	case *chplan.InList:
		walkExpr(v.Left, visit)
		for _, e := range v.List {
			walkExpr(e, visit)
		}
	case *chplan.FuncCall:
		for _, a := range v.Args {
			walkExpr(a, visit)
		}
	case *chplan.MapAccess:
		walkExpr(v.Map, visit)
		walkExpr(v.Key, visit)
	case *chplan.FieldAccess:
		walkExpr(v.Source, visit)
	case *chplan.Subscript:
		walkExpr(v.Container, visit)
		walkExpr(v.Key, visit)
	case *chplan.Lambda:
		walkExpr(v.Body, visit)
	case *chplan.LabelJoin:
		walkExpr(v.Map, visit)
	case *chplan.LabelReplace:
		walkExpr(v.Map, visit)
	case *chplan.LineContent:
		walkExpr(v.Source, visit)
	case *chplan.MapWithoutEmptyValues:
		walkExpr(v.Map, visit)
	case *chplan.MapWithoutKeys:
		walkExpr(v.Map, visit)
	case *chplan.NestedArrayExists:
		walkExpr(v.Value, visit)
	}
}
