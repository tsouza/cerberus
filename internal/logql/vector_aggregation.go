package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerVectorAggregation handles `sum(rate(...))`, `avg by (job) (...)`,
// `count without (instance) (...)`, and friends. Mirrors the PromQL
// aggregation lowering: GROUP BY the requested labels (or all-but-the-
// excluded ones for `without`), apply the aggregator on the inner
// stream's `value` column.
//
// `topk` / `bottomk` / `approx_topk` change output shape (K rows per
// group) and route to [lowerTopK]; `sort` / `sort_desc` reorder rather
// than aggregate and route to [lowerSort].
func lowerVectorAggregation(e *syntax.VectorAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	if e.Left == nil {
		return nil, fmt.Errorf("logql: vector-aggregation has nil inner")
	}

	switch e.Operation {
	case syntax.OpTypeTopK, syntax.OpTypeBottomK, syntax.OpTypeApproxTopK:
		return lowerTopK(e, s, lc)
	case syntax.OpTypeSort, syntax.OpTypeSortDesc:
		return lowerSort(e, s, lc)
	}

	innerExpr, ok := e.Left.(syntax.Expr)
	if !ok {
		return nil, fmt.Errorf("logql: vector-aggregation inner is not an Expr (%T)", e.Left)
	}
	// Thread the outer by-clause labels down so the inner range
	// aggregation's identity wrap surfaces any top-level OTel-CH
	// scalar columns this aggregate groups by (SeverityText,
	// ServiceName, ...). The `without` clause is intentionally NOT
	// plumbed — exclusion semantics don't reference specific columns
	// to surface, they only strip keys from the existing identity.
	// See [lowerCtx.OuterByLabels] and [withDetectedLevelAndColumns]
	// for the consumer side.
	innerLc := lc
	if e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0 {
		innerLc = lc.withOuterByLabels(e.Grouping.Groups)
	}
	input, err := lower(innerExpr, s, innerLc)
	if err != nil {
		return nil, err
	}

	groupBy, aliases := vectorAggregationGroupBy(e, s)
	aggFunc, err := buildVectorAggFunc(e, s)
	if err != nil {
		return nil, err
	}

	// In range mode the inner plan is a matrix-shape RangeWindow (or a
	// nested aggregation that bottoms out at one) that exposes a
	// per-row anchor-timestamp column. The aggregation MUST include
	// that anchor in its GROUP BY keys — otherwise CH collapses every
	// per-step row of a series-set into one aggregate and the wire
	// matrix has a single value per series instead of one per step.
	// Mirrors the PromQL aggregate lowering's `rangeBucketed` path in
	// internal/promql/lower.go.
	//
	// The bucket column's name depends on plan depth: a direct matrix
	// RangeWindow surfaces `anchor_ts`; a nested vector-aggregation
	// surfaces `TimeUnix` (the wrap Project re-aliases `bucket_ts` to
	// match the canonical Sample shape). [matrixBucketColumn] resolves
	// which column the input exposes so nested aggregations
	// (`max(avg by (level) (avg_over_time(...)))`) bucket correctly
	// instead of collapsing the whole matrix into one row.
	const bucketAlias = "bucket_ts"
	rangeBucketed := lc.rangeMode() && isMatrixRangeWindow(input)
	if rangeBucketed {
		bucketCol := matrixBucketColumn(input)
		groupBy = append(groupBy, &chplan.ColumnRef{Name: bucketCol})
		aliases = append(aliases, bucketAlias)
	}

	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     aliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}
	userAliases := aliases
	if rangeBucketed {
		userAliases = aliases[:len(aliases)-1]
	}
	return wrapVectorAggregateForSample(agg, e, s, userAliases, rangeBucketed, bucketAlias), nil
}

// vectorAggregationGroupBy mirrors PromQL aggregateGroupBy, but Loki's
// LogQL groups against the carrier-stream attributes (ResourceAttributes
// in OTel-CH). The output shape exposes group-key columns as
// `gkey_0`, `gkey_1`, ... so the wrapping Project can build a
// Map(String, String) Attributes column from them.
//
// The synthesized `detected_level` label (and its `level` alias) is
// special-cased: instead of accessing the RangeWindow's downstream
// ResourceAttributes column under a key the seeder doesn't write, the
// group-key expression resolves to the same SeverityText-derived
// `multiIf(...)` normalisation upstream Loki's `normalizeLogLevel`
// produces. Without this, `sum by (level) (...)` collapses every record
// into one empty-value group (the user-facing reason 15 loki-compat
// `matrix length: expected=4 actual=1` failures persisted post-PR #545).
func vectorAggregationGroupBy(e *syntax.VectorAggregationExpr, s schema.Logs) ([]chplan.Expr, []string) {
	if e.Grouping == nil {
		return nil, nil
	}
	if e.Grouping.Without {
		// `without (k1, k2)`: group by the full ResourceAttributes map
		// minus the excluded keys. Normalise `level` to its
		// `detected_level` canonical form so the exclusion strips the
		// synthesized-severity key the inner range-aggregation
		// projection added (via withDetectedLevel). Mirroring the
		// `by (...)` alias surface keeps the two grouping shapes
		// symmetric.
		return []chplan.Expr{
				&chplan.MapWithoutKeys{
					Map:  &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
					Keys: canonicalLevelKeys(e.Grouping.Groups),
				},
			},
			[]string{"gkey_0"}
	}
	if len(e.Grouping.Groups) == 0 {
		return nil, nil
	}
	out := make([]chplan.Expr, 0, len(e.Grouping.Groups))
	aliases := make([]string, 0, len(e.Grouping.Groups))
	for i, label := range e.Grouping.Groups {
		out = append(out, levelAwareGroupKey(label, s))
		aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
	}
	return out, aliases
}

// levelAwareGroupKey returns the chplan expression that resolves to the
// group-by value for `label` in a vector-aggregation context (outer
// `sum by (...)` / `max without (...)`, etc., wrapping a range
// aggregation). The `detected_level` family (`detected_level` and its
// `level` alias) reaches through the augmented `ResourceAttributes` map
// under the canonical `detected_level` key — the inner range
// aggregation's [withDetectedLevel] projection is what populates that
// key from SeverityText (see detected_level.go). Reading the
// synthesized key from the map (rather than re-deriving from
// `SeverityText`) keeps the outer aggregation SQL self-contained:
// SeverityText is only visible inside the inner Scan, and the outer
// SELECT only sees the (ResourceAttributes, Value) tuple the RangeWindow
// projected.
//
// Other labels fall back to the standard map lookup. The inner
// range-aggregation `by/without` uses the SeverityText-derived
// expression directly via [levelAwareRangeGroupKey] because at that
// layer SeverityText is still in scope.
func levelAwareGroupKey(label string, s schema.Logs) chplan.Expr {
	if isDetectedLevelGroupingLabel(label) {
		// `detected_level` is a synthesised key the inner range
		// aggregation writes verbatim into the augmented map, so it
		// always lives under the underscored form — no dotted-fallback
		// needed.
		return &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Key: &chplan.LitString{V: detectedLevelLabel},
		}
	}
	if col, ok := topLevelLogColumnFor(label, s); ok {
		// Top-level OTel-CH columns (SeverityText, ServiceName, ...)
		// surface in the post-RangeWindow scope only as keys inside
		// the augmented identity map (see [withDetectedLevel] — the
		// inner range Project inflates the map with these columns when
		// the outer by-clause references them via OuterByLabels). The
		// outer aggregate reads them back via MapAccess on the column
		// name, matching the key the wrap wrote. Without this branch
		// `attributeLookupColumn` would look up the column name in
		// ResourceAttributes (where it isn't present) and the
		// aggregate would collapse every row into one
		// `{<col>:""}` series — the bug task #218 fixed.
		return &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Key: &chplan.LitString{V: col},
		}
	}
	// Non-top-level label in the post-RangeWindow augmented identity map.
	// The inner range-aggregation Project inflates the identity map with
	// structured-metadata + stream values for this key (see
	// [withDetectedLevelAndColumns] / OuterByLabels), so reading it back
	// from the ResourceAttributes-aliased identity column resolves the
	// coalesced value the inner layer wrote. The dotted-fallback chain
	// keeps `cerberus_ql`-style keys resolving against the inflated map.
	return attributeLookupColumn(s.ResourceAttributesColumn, label)
}

// levelAwareRangeGroupKey is the inner-range-aggregation companion of
// [levelAwareGroupKey]: it resolves the `detected_level` family
// (`detected_level` and its `level` alias) to the
// SeverityText-derived `multiIf(...)` expression directly, because the
// inner Project still has `SeverityText` in scope (the Project rides
// directly above the per-row Scan/Filter). The outer
// vector-aggregation reads from the post-RangeWindow scope where
// SeverityText is no longer visible, so it has to reach through the
// augmented map's `detected_level` key instead.
func levelAwareRangeGroupKey(label string, s schema.Logs) chplan.Expr {
	if isDetectedLevelGroupingLabel(label) {
		return detectedLevelExpr(s)
	}
	if matCol, ok := materializedColumnFor(label, s); ok {
		// At the inner range-aggregation layer the Scan/Filter sits
		// directly below, so the exporter's MATERIALIZED k8s.* column is
		// in scope and resolves to a bare ColumnRef — byte-for-byte
		// equivalent to `ResourceAttributes[<key>]` (the column's
		// MATERIALIZED expression) while avoiding the wide Map
		// decompression. NOTE the OUTER post-RangeWindow companion
		// [levelAwareGroupKey] is deliberately NOT routed here: after the
		// RangeWindow identity Project only the augmented map survives,
		// the bare column is out of scope, so it stays a map access.
		return topLevelColumnRef(matCol)
	}
	if col, ok := topLevelLogColumnFor(label, s); ok {
		// At the inner range-aggregation layer the top-level OTel
		// column (SeverityText, ServiceName, ...) is still in scope —
		// the inner Project reads directly from the Scan — so the
		// group-key resolves to a plain ColumnRef rather than a map
		// lookup. The outer aggregation's
		// [levelAwareGroupKey] hops through the augmented
		// ResourceAttributes map instead, since the post-RangeWindow
		// scope only exposes the identity map.
		return topLevelColumnRef(col)
	}
	// Non-top-level group key at the inner range-aggregation layer, where
	// both the ResourceAttributes (stream) and LogAttributes (structured
	// metadata) maps are still in scope. Resolve with reference-Loki
	// precedence structured-metadata > stream so `by (query_kind)` on an
	// OTel structured-metadata key groups by its real value rather than
	// collapsing every row into a single `{query_kind:""}` series (task
	// #59).
	return structuredOrStreamLookup(s, label)
}

// canonicalLevelKeys returns the input `groups` with every
// `detected_level`-family key (`detected_level` itself + its `level`
// alias) normalised to the canonical `detected_level` form. Used by
// the `without (...)` lowering so the exclusion strips the synthesized
// key the inner range-aggregation projection added via
// withDetectedLevel — both `without (level)` and
// `without (detected_level)` reach the same downstream map shape.
func canonicalLevelKeys(groups []string) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		if isDetectedLevelGroupingLabel(g) {
			out[i] = detectedLevelLabel
			continue
		}
		out[i] = g
	}
	return out
}

// lowerTopK lowers LogQL `topk(K, expr)` / `bottomk(K, expr)` /
// `approx_topk(K, expr)` — optionally with `by (...)` / `without (...)`
// partitioning (topk/bottomk only; approx_topk forbids grouping at the
// parser) — into a chplan.TopK over the Sample-shaped inner plan.
// Mirrors PromQL's lowerTopK (internal/promql/lower.go).
//
// Reference semantics (pkg/logql/evaluator.go::VectorAggEvaluator,
// OpTypeTopK / OpTypeBottomK arms): per evaluation step and per
// grouping key, keep the K samples with the largest (topk) / smallest
// (bottomk) values — every kept sample retains its FULL original
// label set; the grouping only partitions, it never projects labels.
// The parser guarantees K > 0 (mustNewVectorAggregationExpr rejects
// `topk(0, ...)` with "must be greater than 0").
//
// `approx_topk(K, expr)` is Loki's probabilistic top-k: upstream
// approximates the K largest series via a count-min sketch + heap
// because the candidate set can be too large to sort exactly at query
// time. Over ClickHouse cerberus has no such constraint, so it computes
// the EXACT top-K — a correctness superset of the approximation (the
// caller gets the true K largest series, never a sketch artefact).
// approx_topk therefore routes through the identical descending-top-K
// lowering as `topk`; the parser forbids its grouping clause and
// defaults a non-nil empty Grouping, so [topKPartition] yields the
// single global K-window upstream's ungrouped approx_topk produces.
//
// The inner plan is re-shaped to the canonical Sample contract first
// ([sampleShapeOverLogInner]) so the partition keys resolve against
// the `Attributes` map and — in range mode — the per-anchor timestamp
// rides under `TimeUnix`, which joins the partition list so the
// K-window fires per step rather than across the whole matrix
// (reference applies the heap per step by construction).
func lowerTopK(e *syntax.VectorAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	shaped, err := sortableShapedInner(e, s, lc)
	if err != nil {
		return nil, err
	}

	by := topKPartition(e)
	if lc.rangeMode() {
		by = append(by, &chplan.ColumnRef{Name: "TimeUnix"})
	}

	return &chplan.TopK{
		Input:    shaped,
		K:        int64(e.Params),
		By:       by,
		SortExpr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn},
		// topk + approx_topk keep the LARGEST K (descending); bottomk
		// keeps the smallest. approx_topk is exact-top-k here (see
		// lowerTopK doc), so it shares topk's descending sort.
		Desc:    e.Operation == syntax.OpTypeTopK || e.Operation == syntax.OpTypeApproxTopK,
		Columns: []string{"MetricName", "Attributes", "TimeUnix", rangeAggSynthValueColumn},
	}, nil
}

// topKPartition derives the partition expressions for topk/bottomk
// from the aggregation's Grouping, resolved against the canonical
// `Attributes` map the Sample-shaped inner exposes:
//
//   - no grouping (or `by ()`) → nil — one global K-window, matching
//     reference Loki's single empty grouping key.
//   - `by (l1, l2)` → one map-lookup per label. The `detected_level`
//     family (`detected_level` + its `level` alias) normalises to the
//     canonical key the range-aggregation identity wrap writes.
//   - `without (l1, l2)` → the Attributes map minus the (canonical-
//     ised) keys.
//
// Note the parser materialises a non-nil empty Grouping for bare
// `topk(K, v)` (mustNewVectorAggregationExpr defaults `gr =
// &Grouping{}`), so the nil-check alone doesn't gate.
func topKPartition(e *syntax.VectorAggregationExpr) []chplan.Expr {
	g := e.Grouping
	if g == nil || (!g.Without && len(g.Groups) == 0) {
		return nil
	}
	attrs := &chplan.ColumnRef{Name: "Attributes"}
	if g.Without {
		return []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  attrs,
			Keys: canonicalLevelKeys(g.Groups),
		}}
	}
	out := make([]chplan.Expr, 0, len(g.Groups))
	for _, label := range canonicalLevelKeys(g.Groups) {
		out = append(out, attributeLookupExpr(attrs, label))
	}
	return out
}

// lowerSort lowers LogQL `sort(expr)` / `sort_desc(expr)` into a
// chplan.OrderBy over the Sample-shaped inner plan.
//
// Reference semantics (pkg/logql/evaluator.go::VectorAggEvaluator,
// OpTypeSort / OpTypeSortDesc arms): every input sample is kept with
// its labels and value untouched; only the output order changes —
// ascending by value for `sort`, descending for `sort_desc`. The
// parser rejects `by (...)` grouping on both ops
// (validateSortGrouping), so there is no partition dimension to
// honour; an empty `by ()` parses and is a no-op like it is upstream.
func lowerSort(e *syntax.VectorAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	shaped, err := sortableShapedInner(e, s, lc)
	if err != nil {
		return nil, err
	}
	return &chplan.OrderBy{
		Input: shaped,
		Keys: []chplan.OrderKey{{
			Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn},
			Desc: e.Operation == syntax.OpTypeSortDesc,
		}},
	}, nil
}

// sortableShapedInner lowers the aggregation's inner expression and
// re-shapes it to the canonical Sample contract — the shared front
// half of [lowerTopK] and [lowerSort]. The outer by-clause labels
// thread down exactly like [lowerVectorAggregation]'s generic path so
// `topk(2, rate(...)) by (ServiceName)` surfaces top-level OTel-CH
// scalar columns into the identity map the partition keys read.
func sortableShapedInner(e *syntax.VectorAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	innerExpr, ok := e.Left.(syntax.Expr)
	if !ok {
		return nil, fmt.Errorf("logql: vector-aggregation inner is not an Expr (%T)", e.Left)
	}
	innerLc := lc
	if e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0 {
		innerLc = lc.withOuterByLabels(e.Grouping.Groups)
	}
	inner, err := lower(innerExpr, s, innerLc)
	if err != nil {
		return nil, err
	}
	return sampleShapeOverLogInner(inner, s), nil
}

// buildVectorAggFunc produces the AggFunc for the LogQL operator.
// Output-shape-changing ops (topk, bottomk, sort, sort_desc) are
// routed to [lowerTopK] / [lowerSort] before this function is
// reached.
func buildVectorAggFunc(e *syntax.VectorAggregationExpr, _ schema.Logs) (chplan.AggFunc, error) {
	const valueAlias = "Value"
	valueArg := &chplan.ColumnRef{Name: valueAlias}

	switch e.Operation {
	case syntax.OpTypeSum:
		return chplan.AggFunc{Name: "sum", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeAvg:
		return chplan.AggFunc{Name: "avg", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeMin:
		return chplan.AggFunc{Name: "min", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeMax:
		return chplan.AggFunc{Name: "max", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeCount:
		return chplan.AggFunc{Name: "count", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeStddev:
		return chplan.AggFunc{Name: "stddevPop", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeStdvar:
		return chplan.AggFunc{Name: "varPop", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil

	}
	return chplan.AggFunc{}, fmt.Errorf("logql: aggregation operation %q is unsupported", e.Operation)
}

// wrapVectorAggregateForSample mirrors PromQL's wrapAggregateForSample:
// re-shape the aggregate output into the Sample contract so the API
// layer can stream rows through chclient.Sample. LogQL aggregations
// drop `__name__`-equivalent stream identity except for the requested
// grouping labels.
//
//	MetricName  = ''
//	Attributes  = map('lbl0', gkey_0, ...)    for `by (...)`
//	            | gkey_0                        for `without (...)` (mapFilter output)
//	            | empty Map(String,String)      for unaggregated
//	TimeUnix    = now64(9)                      (instant mode)
//	            | <bucketAlias>                  (range mode: per-anchor anchor_ts)
//	Value       = <agg alias>
//
// In range mode (rangeBucketed=true) the inner Aggregate carries the
// per-anchor timestamp under bucketAlias; the outer Project re-aliases
// it to TimeUnix so the canonical Sample shape exposes one row per step
// instead of collapsing to one row per series at `now64(9)`.
func wrapVectorAggregateForSample(agg *chplan.Aggregate, e *syntax.VectorAggregationExpr, s schema.Logs, aliases []string, rangeBucketed bool, bucketAlias string) chplan.Node {
	var attrs chplan.Expr
	switch {
	case len(aliases) == 0:
		attrs = emptyAttrsMap()
	case e.Grouping != nil && e.Grouping.Without:
		attrs = &chplan.ColumnRef{Name: aliases[0]}
	default:
		args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
		for i, label := range e.Grouping.Groups {
			args = append(args, &chplan.LitString{V: label}, &chplan.ColumnRef{Name: aliases[i]})
		}
		attrs = &chplan.FuncCall{Name: "map", Args: args}
	}

	// Use the metrics-schema column names for the Sample contract; the
	// API layer reads MetricName/Attributes/TimeUnix/Value regardless of
	// which head produced the row.
	const (
		metricNameCol = "MetricName"
		attrsCol      = "Attributes"
		tsCol         = "TimeUnix"
		valueCol      = "Value"
	)

	tsExpr := chplan.NowNano()
	if rangeBucketed {
		tsExpr = &chplan.ColumnRef{Name: bucketAlias}
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: metricNameCol},
			{Expr: attrs, Alias: attrsCol},
			{Expr: tsExpr, Alias: tsCol},
			{Expr: &chplan.ColumnRef{Name: valueCol}, Alias: valueCol},
		},
	}
}

// emptyAttrsMap returns CH `CAST(map(), 'Map(String,String)')` for the
// no-grouping aggregation case. Same helper shape as the PromQL side.
func emptyAttrsMap() chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "map", Args: nil},
			&chplan.LitString{V: "Map(String,String)"},
		},
	}
}
