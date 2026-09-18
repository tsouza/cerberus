package engine

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// planShapeFacts is everything the per-query settings rules read off a plan,
// gathered by ONE traversal (inspectPlanShape). Each rule is then a lookup on
// this record rather than its own full-tree walk: before this existed the
// dispatch seam ran eleven independent walks per query — one per predicate,
// most of them chplan.WalkDeep with a closure each — over the same nodes, and
// the walk cost showed up directly in the per-request allocation count
// (internal/api/prom's TestAllocs_HandleQuery_Small).
//
// Two traversal depths are recorded, because the predicates that were merged
// did not all agree on one:
//
//   - the DEEP facts follow both Node.Children and the plan subtrees embedded
//     in Expr slots (chplan.WalkDeep's reach). Every fact that gates a
//     ClickHouse setting or a memory bound is deep, because a node a walk
//     fails to see is a bound that never fires.
//   - the SPINE facts (the spine* fields) follow Node.Children only
//     (chplan.Walk's reach). They feed the aggregation-in-order and
//     condition-cache eligibility checks, which reason about the row-flow
//     spine: an Aggregate inside a scalar subquery is not "the" aggregate the
//     sort key is matched against.
type planShapeFacts struct {
	// plan is the inspected plan, kept for planShapeID (the log_comment
	// shape id), which renders the tree rather than reading a fact.
	plan chplan.Node

	// hasJoin is chplan.HasJoin's answer: any node chplan.IsJoinNode
	// classifies as join-bearing.
	hasJoin bool
	// hasCompare: a chplan.MetricsCompare node — TraceQL compare().
	hasCompare bool
	// nativeHistogramAnalyzerHazard: a HistogramQuantileNative or
	// HistogramProjection node, or a RangeBucketFanout whose RowType carries
	// the exponential-only HistogramFieldScale column — see
	// applyNativeHistogramAnalyzerFix for the two shapes and why both.
	nativeHistogramAnalyzerHazard bool
	// sortedSlabOverTime: a RangeWindow with SortedSlabOverTime set.
	sortedSlabOverTime bool
	// expHistogramWindowGrouping: BOTH an exponential-histogram node AND a
	// RangeBucketFanout — see applyExpHistogramTwoLevelBound for why neither
	// conjunct alone is the predicate.
	expHistogramWindowGrouping bool
	// tsGridNative: a node from the experimental timeSeries*ToGrid family —
	// see planHasTSGridNative for the member list.
	tsGridNative bool
	// hasStructuralJoin: a chplan.StructuralJoin, whose recursive closure is
	// keyed on TraceId by construction (eligibleForTraceIDBitmapFilter).
	hasStructuralJoin bool
	// traceIDPredicate: an equality / membership predicate on the bare
	// trace-id column named at inspection time — the other half of
	// eligibleForTraceIDBitmapFilter.
	traceIDPredicate bool
	// hasNowExpr: a literal now()/now64() FuncCall anywhere in any Expr tree
	// (the result-cache rule's second, independent leg).
	hasNowExpr bool

	// lazyLimits counts Limit(OrderBy(...)) nodes with a positive Count;
	// lazyLimit is the Count of the last one seen. EligibleForLazyMaterialization
	// requires exactly one.
	lazyLimits int
	lazyLimit  int64

	// Result-cache window facts, over every chplan.GridCarrier:
	//   - rangeCarriers counts the carriers with Step > 0;
	//   - zeroDataEnd is set when any carrier's DataWindowEnd is the zero
	//     time (the "resolve at emit time" sentinel, i.e. a live edge);
	//   - latestDataEnd is the max DataWindowEnd across all carriers;
	//   - zeroRangeGrid is set when a range-mode carrier's Start or End is
	//     the zero time.
	rangeCarriers int
	zeroDataEnd   bool
	latestDataEnd time.Time
	zeroRangeGrid bool

	// expHistogramNode / windowedFanout are the two conjuncts
	// expHistogramWindowGrouping is derived from once the walk completes.
	expHistogramNode bool
	windowedFanout   bool

	// Spine facts (chplan.Walk reach only): the sole Aggregate and its
	// count, the sole physical Scan table and its count (spineScanUnion is
	// set by a union or table-less Scan, which has no single sort key), and
	// whether a Filter / Scan sits on the spine at all.
	spineAggregate  *chplan.Aggregate
	spineAggregates int
	spineScanTable  string
	spineScans      int
	spineScanUnion  bool
	spineHasFilter  bool
	spineHasScan    bool
}

// inspectPlanShape gathers planShapeFacts for plan in one traversal.
// traceIDColumn is the schema's trace-id column name, read at inspection time
// so the trace-id predicate fact is a bool rather than a set of column names
// (an empty name matches nothing, exactly as eligibleForTraceIDBitmapFilter
// always required).
//
// perf-sentinel: memory-bounding — this is the single plan walk every
// memory-bounding predicate reads (compare(), the sorted slab, the
// exp-histogram window, the join spill's chplan.HasJoin). A shape it fails to
// see is a bound that never fires.
func inspectPlanShape(plan chplan.Node, traceIDColumn string) planShapeFacts {
	f := planShapeFacts{plan: plan}
	f.visit(plan, true, traceIDColumn)
	f.expHistogramWindowGrouping = f.expHistogramNode && f.windowedFanout
	return f
}

// visit records n's facts and descends into its children (on the same spine
// flag) and into the plan subtrees embedded in its Expr slots (off the
// spine), inspecting every sub-expression on the way — the same reach
// chplan.WalkDeep composed with chplan.InspectNodeExprs + chplan.InspectExpr
// has.
func (f *planShapeFacts) visit(n chplan.Node, spine bool, traceIDColumn string) {
	if n == nil {
		return
	}
	f.recordNode(n, spine)
	for _, c := range n.Children() {
		f.visit(c, spine, traceIDColumn)
	}
	chplan.InspectNodeExprs(n, func(e chplan.Expr) {
		chplan.InspectExprNodes(e, func(sub chplan.Expr) bool {
			f.recordExpr(sub, traceIDColumn)
			return true
		}, func(embedded chplan.Node) {
			f.visit(embedded, false, traceIDColumn)
		})
	})
}

// recordNode records the node-kind facts for n.
func (f *planShapeFacts) recordNode(n chplan.Node, spine bool) {
	if gc, ok := n.(chplan.GridCarrier); ok {
		f.recordGridCarrier(gc)
	}
	if chplan.IsJoinNode(n) {
		f.hasJoin = true
	}
	switch v := n.(type) {
	case *chplan.MetricsCompare:
		f.hasCompare = true
	case *chplan.StructuralJoin:
		f.hasStructuralJoin = true
	case *chplan.HistogramQuantileNative, *chplan.HistogramProjection:
		f.nativeHistogramAnalyzerHazard = true
		f.expHistogramNode = true
	case *chplan.RangeBucketFanout:
		f.windowedFanout = true
		if _, ok := v.RowType().FindHistogramField(chplan.HistogramFieldScale); ok {
			f.nativeHistogramAnalyzerHazard = true
			f.expHistogramNode = true
		}
	case *chplan.RangeWindow:
		if v.SortedSlabOverTime {
			f.sortedSlabOverTime = true
		}
		if v.DownsampleTier || v.NativeGroupArray {
			f.tsGridNative = true
		}
	case *chplan.RangeWindowGridNative, *chplan.RangeWindowGridNativeInstant,
		*chplan.RangeWindowStaleResample, *chplan.RangeBucketGridNative:
		f.tsGridNative = true
	case *chplan.Limit:
		if v.Count > 0 {
			if _, isOrderBy := v.Input.(*chplan.OrderBy); isOrderBy {
				f.lazyLimits++
				f.lazyLimit = v.Count
			}
		}
	case *chplan.Aggregate:
		if spine {
			f.spineAggregate = v
			f.spineAggregates++
		}
	case *chplan.Scan:
		if spine {
			f.spineHasScan = true
			if len(v.UnionTables) > 0 || v.Table == "" {
				f.spineScanUnion = true
			} else {
				f.spineScanTable = v.Table
				f.spineScans++
			}
		}
	case *chplan.Filter:
		if spine {
			f.spineHasFilter = true
		}
	}
}

// recordGridCarrier records the result-cache window facts of one carrier —
// the DATA edge (DataWindowEnd, offset-adjusted) for every carrier, and the
// request grid's Start/End only for range-mode ones, exactly as
// eligibleForResultCache judges them.
func (f *planShapeFacts) recordGridCarrier(gc chplan.GridCarrier) {
	dataEnd := gc.DataWindowEnd()
	if dataEnd.IsZero() {
		f.zeroDataEnd = true
	} else if dataEnd.After(f.latestDataEnd) {
		f.latestDataEnd = dataEnd
	}
	start, end, step := gc.EvalGrid()
	if step <= 0 {
		return
	}
	f.rangeCarriers++
	if start.IsZero() || end.IsZero() {
		f.zeroRangeGrid = true
	}
}

// recordExpr records the expression-level facts for one sub-expression: a
// literal now()/now64() call, and a predicate that tests the bare trace-id
// column for equality or membership (traceIDPredicateExpr).
func (f *planShapeFacts) recordExpr(e chplan.Expr, traceIDColumn string) {
	if fc, ok := e.(*chplan.FuncCall); ok && (fc.Fn == chplan.FnNow || fc.Fn == chplan.FnNow64) {
		f.hasNowExpr = true
	}
	if traceIDColumn != "" && !f.traceIDPredicate && traceIDPredicateExpr(e, traceIDColumn) {
		f.traceIDPredicate = true
	}
}

// lazyMaterializationLimit returns the sole Limit(OrderBy(...)) Count, or
// ok=false when there is none or more than one — see
// EligibleForLazyMaterialization for why exactly one.
func (f planShapeFacts) lazyMaterializationLimit() (limit int64, ok bool) {
	if f.lazyLimits != 1 {
		return 0, false
	}
	return f.lazyLimit, true
}

// singleAggregate returns the sole spine Aggregate, or ok=false when there is
// none or more than one.
func (f planShapeFacts) singleAggregate() (*chplan.Aggregate, bool) {
	if f.spineAggregates != 1 {
		return nil, false
	}
	return f.spineAggregate, true
}

// singleScanTable returns the one physical table the spine scans, or
// ok=false when there is not exactly one (zero Scans, a multi-table union,
// or two Scans from a join).
func (f planShapeFacts) singleScanTable() (table string, ok bool) {
	if f.spineScanUnion || f.spineScans != 1 {
		return "", false
	}
	return f.spineScanTable, true
}
