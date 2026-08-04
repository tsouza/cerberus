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
		names := d.Metrics.NamesPresent()
		if len(names) == 0 {
			// MetricsDataset filters this out before Run() draws a
			// query; guard anyway so the generator never panics.
			return RangeQuery{}
		}

		name := rapid.SampledFrom(names).Draw(t, "metric")
		matchers := drawMatchers(t, name, d.Metrics)
		expr := drawRangeExpr(t, name, matchers)

		startOffset := rapid.SampledFrom(RangeGridStartOffsets).Draw(t, "startOffset")
		step := rapid.SampledFrom(RangeGridSteps).Draw(t, "step")

		startSec := AnchorTime().Add(startOffset).Unix()
		endSec := startSec + int64(rangeGridPoints-1)*step

		return RangeQuery{
			String:   expr.String(),
			StartSec: startSec,
			EndSec:   endSec,
			StepSec:  step,
		}
	})
}

// drawRangeExpr picks a restricted expression shape for the range-
// query generator: bare selector, ALWAYS-grouped sum-by, or bare
// rate(). Deliberately excludes the ungrouped `sum(...)` and
// `sum(rate(...))` shapes drawExpr draws for the instant-query
// generator (see PromQLRangeQuery's doc comment for why).
func drawRangeExpr(t *rapid.T, name string, matchers []*labels.Matcher) parser.Expr {
	shape := rapid.IntRange(0, 2).Draw(t, "rangeShape")
	sel := &parser.VectorSelector{Name: name, LabelMatchers: matchers}
	switch shape {
	case 0:
		return sel
	case 1:
		// Non-empty grouping only — never fall back to bare sum(...),
		// unlike pickLabelName's 50%-chance-of-nil behaviour used by
		// the instant generator.
		group := []string{rapid.SampledFrom(LabelNamePool).Draw(t, "groupLabel")}
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel, Grouping: group}
	case 2:
		return &parser.Call{
			Func: parser.Functions["rate"],
			Args: []parser.Expr{drawRangeSelector(t, name, matchers)},
		}
	}
	return sel
}
