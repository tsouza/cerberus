package chplan

// HasJoin reports whether node contains any join-bearing chplan.Node
// anywhere in its plan tree — a node whose ClickHouse emission renders a
// real SQL JOIN with a hash-table build side, as opposed to a plan-IR node
// whose NAME merely contains "Join" or whose doc talks about a
// relational-algebra join informally. chplan.LabelJoin lowers PromQL's
// label_join() string function and never emits a SQL JOIN — it is an Expr,
// not a Node, and can never reach the switch below at all.
// chplan.VectorSetOp's own doc calls PromQL `and`/`unless` a "semi-join" /
// "anti-join" over label signatures, but its chsql emitter
// (internal/chsql/vector_set_op.go) renders that as a `WHERE ... IN
// (subquery)` / `NOT IN (subquery)` predicate, never a JOIN clause — so it
// is excluded too. Both are exactly the EMISSION-BEHAVIOUR-vs-name-heuristic
// traps TestHasJoin_CoversEveryJoinEmittingNode exists to keep this
// enumeration honest against.
//
// This is the SOLE enumeration of join-carrier node kinds in the codebase.
// internal/engine/spill.go's applyJoinSpillSettings (the memory-spill
// guard for a join's hash-table build side), internal/engine/plan_shape_id.go's
// planShapeID (the `join` log_comment modifier), and
// internal/routememo/key.go's KeyFor (the routing-memo fingerprint) all
// call this rather than keeping their own switch, so the set cannot drift
// between callers again the way it did before cerberus issue #2886/#3008.
//
// The sweep is WalkDeep, not Walk: a join nested inside a ScalarSubquery's
// or InSubquery's Expr slot still emits a real ClickHouse JOIN when the
// query runs, and Walk's Children()-only traversal cannot see it. When the
// answer gates a resource bound — the join-spill memory setting — that
// blind spot is a production OOM exposure, not a missed optimisation; see
// WalkDeep's own doc for the general case.
//
// Covers every join chplan.WalkDeep can observe on the plan pre-emission —
// derived not from the three sites' old, individually-incomplete switches,
// but from a full sweep of internal/chsql for every call to QueryBuilder's
// own Join method, matched back to the Node type whose emitter issues it.
// That sweep is what found two carriers NONE of the three pre-existing
// enumerations (including spill.go's) knew about:
//
//   - VectorJoin / HistogramVectorJoin / HistogramFloatVectorJoin /
//     MixedVectorJoin — PromQL vector matching: one-to-one, the
//     group_left()/group_right() many-to-one shape, and the mixed-family
//     join. VectorJoin's own ManyToManyMatchMessage throwIf guard exists
//     precisely because a many-to-many match here can build an unbounded
//     hash table.
//   - InfoJoin — PromQL's info() label-enrichment join.
//   - StructuralJoin — TraceQL's structural (descendant/child/sibling) joins.
//   - CrossJoin — the unconditional Cartesian product.
//   - NestedSetAnnotate — TraceQL's structure-tab left/right/parent
//     numbering. internal/chsql/nested_set_annotate.go's emitNestedSetAnnotate
//     unconditionally LEFT JOINs its input against the recursive-CTE
//     numbering subquery (and buildNestedSetNumbering's own recursive step
//     INNER JOINs the CTE to itself) — every NestedSetAnnotate node is a
//     join, with no gating field to check.
//   - MetricsCompare with a non-nil RootLookup — TraceQL compare()'s root-span
//     lookup. chplan.MetricsCompare's own doc names TraceIDColumn "join key
//     for RootLookup"; internal/chsql/metrics_compare.go's compareBaseQuery
//     (the single funnel every MetricsCompare emission path renders through)
//     LEFT JOINs RootLookup exactly when it is set, mirroring RangeWindow's
//     DeltaPrefixAggregateInput shape on the same IR struct.
//   - RangeWindow with a non-nil DeltaPrefixAggregateInput — the
//     delta-prefix LEFT JOIN (internal/chsql/range_window.go's
//     deltaPrefixAggregateSource) that side-feeds the day-bucket aggregate
//     input into the raw window.
//   - RangeWindow with OuterRange == 0 (instant shape), a populated
//     TemporalityColumn, and a counter Func (rate / increase, per
//     IsCounterRangeWindowFunc — not delta, which is a gauge delta) — the
//     SEPARATE, narrower join cerberus issue #3014 found the predicate
//     above did not cover: instant emitWindowedArrayExtrapolated's default
//     fallback (instantDeltaPrefixSource, used whenever the
//     DeltaPrefixAggregateInput-backed mechanism above is not ALSO opted
//     into) LEFT/CROSS JOINs a per-series prefix-sum subquery back onto the
//     window unconditionally on every such query — no DeltaPrefixAggregateInput
//     needed to trigger it. Any RangeWindow already caught by the arm above
//     (a non-nil DeltaPrefixAggregateInput) is also caught by this one when
//     it is instant-shaped, but is reported redundantly-true either way, so
//     the two arms are combined with OR rather than requiring
//     DeltaPrefixAggregateInput == nil here.
//
// ClickHouse's ARRAY JOIN (RangeWindowStaleResample, RangeBucketGridNative,
// RangeWindowGridNative, RangeWindowGridNativeVectorAgg, …) is deliberately
// excluded: it is a row-multiplying unnest with no second relation and no
// hash-table build side, not the JOIN operator max_bytes_before_external_join
// bounds.
func HasJoin(node Node) bool {
	found := false
	WalkDeep(node, func(n Node) bool {
		switch v := n.(type) {
		case *VectorJoin:
			found = true
		case *HistogramVectorJoin:
			found = true
		case *HistogramFloatVectorJoin:
			found = true
		case *MixedVectorJoin:
			found = true
		case *InfoJoin:
			found = true
		case *StructuralJoin:
			found = true
		case *CrossJoin:
			found = true
		case *NestedSetAnnotate:
			found = true
		case *MetricsCompare:
			if v.RootLookup != nil {
				found = true
			}
		case *RangeWindow:
			if v.DeltaPrefixAggregateInput != nil ||
				(v.OuterRange == 0 && v.TemporalityColumn != "" && IsCounterRangeWindowFunc(v.Func)) {
				found = true
			}
		}
		return !found
	})
	return found
}
