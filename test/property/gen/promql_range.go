package gen

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// RangeQuery is a generated PromQL query string plus the query_range
// grid (start/end/step, unix seconds) to evaluate it over — the
// range-path counterpart to PromQLQuery's instant-only property.Query.
type RangeQuery struct {
	ShapeID  ShapeID
	String   string
	StartSec int64
	EndSec   int64
	StepSec  int64
}

// RangeGridStartOffsets is the pool of grid-start offsets (relative to
// AnchorTime()) PromQLRangeQuery draws from. Deliberately includes
// offsets BEFORE the dataset's first sample (AnchorTime()+0) and PAST
// its last sample (AnchorTime()+135s, see MetricsDataset), so the
// generated grid slides the query_range window across, before, and
// beyond the seeded data — the axis the recurring window-anchor bug
// class (#1499: range endpoints anchoring to now64() instead of the
// request's [start,end]) lives on. A grid that only ever started
// inside the safe data window couldn't distinguish a correctly
// [start,end]-anchored implementation from one that silently
// substitutes wall-clock `now` for every anchor whose true window
// happens to still contain data by coincidence.
var RangeGridStartOffsets = []time.Duration{
	-60 * time.Second,
	0,
	60 * time.Second,
	200 * time.Second,
}

// RangeGridSteps is the pool of query_range step sizes (seconds)
// PromQLRangeQuery draws from. The smallest step densely samples the
// anchor sweep (catching a window that's mis-anchored only over a
// narrow sub-range); the largest step still produces a grid spanning
// a meaningful fraction of RangeGridStartOffsets.
var RangeGridSteps = []int64{15, 30, 60}

// rangeGridPoints bounds how many anchors a single generated grid
// produces (end = start + (rangeGridPoints-1)*step): enough to
// exercise a real matrix per iteration while keeping each property
// iteration's HTTP round-trip (one oracle eval + one cerberus request
// per anchor) cheap.
const rangeGridPoints = 5

// PromQLRangeQuery returns a rapid generator that produces a random
// RangeQuery targeted at d, restricted to expression shapes that can't
// trip the known (separately tracked, see promql_test.go's doc
// comment) aggregate-empty-GroupBy bug: every grouped-sum draw is
// FORCED to a non-empty grouping label, and the ungrouped-sum /
// sum(rate(...)) shapes from PromQLQuery's pool are excluded entirely.
// That lets the grid safely include start offsets before and past the
// dataset's data window (RangeGridStartOffsets) without spuriously
// tripping that unrelated, already-documented bug — the whole point of
// exercising those offsets is to stress the window-anchor property,
// not to rediscover a different known issue.
func PromQLRangeQuery(d property.Dataset) *rapid.Generator[RangeQuery] {
	return rapid.Custom(func(t *rapid.T) RangeQuery {
		shapeID := rapid.SampledFrom(PromQLRangeShapeIDs()).Draw(t, "rangeShapeID")
		return drawPromQLRangeQuery(t, d, shapeID)
	})
}

// PromQLRangeQueryForShape fixes the query-range generator to one exact
// roster member for deterministic one-per-shape execution.
func PromQLRangeQueryForShape(d property.Dataset, shapeID ShapeID) *rapid.Generator[RangeQuery] {
	if !containsShapeID(promQLRangeShapeRoster[:], shapeID) {
		panic("gen/promql-range: unknown shape " + string(shapeID))
	}
	return rapid.Custom(func(t *rapid.T) RangeQuery {
		return drawPromQLRangeQuery(t, d, shapeID)
	})
}

func drawPromQLRangeQuery(t *rapid.T, d property.Dataset, shapeID ShapeID) RangeQuery {
	names := d.Metrics.NamesPresent()
	if len(names) == 0 {
		// The property test rejects this generator defect before execution;
		// preserve a non-panicking diagnostic value for direct callers.
		return RangeQuery{ShapeID: shapeID}
	}

	name := rapid.SampledFrom(names).Draw(t, "metric")
	matchers := drawMatchers(t, name, d.Metrics)
	groupLabels := mapKeys(d.Metrics.LabelsPresentFor(name))
	expr := drawRangeExpr(t, name, matchers, groupLabels, shapeID, maxComposableWrapDepth)

	startOffset := rapid.SampledFrom(RangeGridStartOffsets).Draw(t, "startOffset")
	step := rapid.SampledFrom(RangeGridSteps).Draw(t, "step")

	startSec := AnchorTime().Add(startOffset).Unix()
	endSec := startSec + int64(rangeGridPoints-1)*step

	return RangeQuery{
		ShapeID:  shapeID,
		String:   expr.String(),
		StartSec: startSec,
		EndSec:   endSec,
		StepSec:  step,
	}
}

// promQLRangeBaseShapeRoster is the non-wrapping subset of
// promQLRangeShapeRoster (gen/shapes.go): every shape drawRangeExpr's
// promQLRangeLabelReplaceShape case may pick as the inner expression it
// wraps in label_replace(...). Mirrors promQLBaseShapeRoster's role for the
// instant generator (promql.go).
var promQLRangeBaseShapeRoster = []ShapeID{
	promQLRangeSelectorShape,
	promQLRangeSumByShape,
	promQLRangeRateShape,
}

// drawRangeExpr picks a restricted expression shape for the range-
// query generator: bare selector, ALWAYS-grouped sum-by, bare
// rate(), or label_replace(...) wrapping any of the three. Deliberately
// excludes the ungrouped `sum(...)` and `sum(rate(...))` shapes drawExpr
// draws for the instant-query generator (see PromQLRangeQuery's doc comment
// for why).
//
// The label_replace wrapper is what makes this generator able to reach
// #2383's reproducer shape (`label_replace(sum by (uri)(rate(X[5m])), ...)`
// under /api/v1/query_range) — the range-mode `bucket_ts` alias
// guardKeysOnTimestamp had to learn to recognize
// (internal/promql/duplicate_labelset_guard.go) only matters once a
// label-set-rewriting operation sits ABOVE a range-bucketed Aggregate, which
// no shape in this file's roster could produce before.
//
// wrapBudget mirrors drawExpr's: see [maxComposableWrapDepth]'s doc comment
// in promql.go for why it's threaded explicitly rather than left implicit.
func drawRangeExpr(
	t *rapid.T,
	name string,
	matchers []*labels.Matcher,
	groupLabels []string,
	shapeID ShapeID,
	wrapBudget int,
) parser.Expr {
	sel := &parser.VectorSelector{Name: name, LabelMatchers: matchers}
	switch shapeID {
	case promQLRangeSelectorShape:
		return sel
	case promQLRangeSumByShape:
		// Non-empty grouping only — never fall back to bare sum(...),
		// and draw from the selected metric's observed labels.
		if len(groupLabels) == 0 {
			panic("gen/promql-range: sum-by shape has no observed grouping label")
		}
		group := []string{rapid.SampledFrom(groupLabels).Draw(t, "groupLabel")}
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel, Grouping: group}
	case promQLRangeRateShape:
		return &parser.Call{
			Func: parser.Functions["rate"],
			Args: []parser.Expr{drawRangeSelector(t, name, matchers)},
		}
	case promQLRangeLabelReplaceShape:
		if wrapBudget <= 0 {
			panic("gen/promql-range: label-replace shape exceeded maxComposableWrapDepth")
		}
		innerShape := rapid.SampledFrom(promQLRangeBaseShapeRoster).Draw(t, "rangeLabelReplaceInnerShape")
		inner := drawRangeExpr(t, name, matchers, groupLabels, innerShape, wrapBudget-1)
		return drawLabelReplaceWrap(t, inner, groupLabels)
	}
	panic("gen/promql-range: unhandled shape " + string(shapeID))
}
