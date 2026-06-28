package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/util/constants"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// variantLabel is the reserved label LogQL stamps onto every series a
// `variants(...) of (...)` query produces, identifying which variant arm
// the series came from. The value is the variant's zero-based index
// rendered as a decimal string ("0", "1", …). Mirrors reference Loki's
// constants.VariantLabel ("__variant__"); we read it from the upstream
// package so the two never drift.
const variantLabel = constants.VariantLabel

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
// Lowering approach: cerberus has no shared-scan fusion across sibling
// metric arms, so each variant lowers independently through the ordinary
// [lower] dispatch (it is already a self-contained SampleExpr carrying
// its own selector — the `of (...)` arm is upstream's scan-fusion hint,
// not an extra data source). Each lowered arm is then re-projected into
// the canonical Sample shape (MetricName, Attributes, TimeUnix, Value)
// with `__variant__="<i>"` folded into its Attributes map, and the arms
// are concatenated with [chplan.UnionAll]. Because every arm already
// carries the canonical Sample columns, [Lang.ProjectSamples] recognises
// the union (via [isVariantUnion]) and forwards it unchanged.
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

	arms := make([]chplan.Node, 0, len(variants))
	for i, v := range variants {
		ve, ok := v.(syntax.Expr)
		if !ok {
			return nil, fmt.Errorf("logql: variant %d is not an Expr (%T)", i, v)
		}
		inner, err := lower(ve, s, lc)
		if err != nil {
			return nil, fmt.Errorf("logql: variant %d: %w", i, err)
		}
		arms = append(arms, variantSampleArm(inner, s, lc, i))
	}

	// A single-arm `variants(m) of (...)` is legal LogQL; it still tags
	// the series with `__variant__="0"`. UnionAll with one input renders
	// as the bare arm (no UNION ALL keyword), which is correct.
	return &chplan.UnionAll{Inputs: arms}, nil
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
					&chplan.LitString{V: fmt.Sprintf("%d", index)},
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

// isVariantUnion reports whether plan is the UnionAll a multi-variant
// lowering produces — a UnionAll whose every arm is already in the
// canonical Sample shape (a top-level Project aliasing `Attributes`).
// [Lang.ProjectSamples] uses this to forward the union unchanged rather
// than wrapping it in the generic metric Sample reshape (which would
// re-reference `ResourceAttributes`, a column the per-arm Project has
// already consumed into `Attributes`).
func isVariantUnion(plan chplan.Node) bool {
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
