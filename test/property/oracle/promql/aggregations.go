package promql

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// evalAggregation evaluates an AggregateExpr against an already-
// evaluated input vector. The input is the result of e.evalVector
// on the inner expression; this function only does the grouping +
// aggregation operator dispatch.
//
// Per Prom semantics:
//
//   - Without `by`/`without`, the entire vector aggregates to one
//     result with an empty label set.
//   - `by(l1, l2)` groups by those labels; each group's result keeps
//     only those labels (plus none of __name__).
//   - `without(l1, l2)` drops the listed labels (and __name__) from
//     each input series's label set; series whose stripped labels
//     match get aggregated together.
func (e *Evaluator) evalAggregation(a *parser.AggregateExpr, input []VectorRow, evalTsMs int64) ([]VectorRow, error) {
	groups := make(map[string]*aggGroup)
	keys := make([]string, 0)

	for _, r := range input {
		var groupLabels map[string]string
		if a.Without {
			groupLabels = DropLabels(r.Labels, a.Grouping)
		} else {
			groupLabels = KeepLabels(r.Labels, a.Grouping)
		}
		key := labelKey(groupLabels)
		g, ok := groups[key]
		if !ok {
			g = &aggGroup{labels: groupLabels}
			groups[key] = g
			keys = append(keys, key)
		}
		g.rows = append(g.rows, r)
	}

	sort.Strings(keys)
	out := make([]VectorRow, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		vs, err := applyAggregator(a, g.rows)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			out = append(out, VectorRow{
				Labels: v.labels,
				T:      evalTsMs,
				V:      v.value,
			})
		}
	}
	sortVectorRows(out)
	return out, nil
}

type aggGroup struct {
	labels map[string]string
	rows   []VectorRow
}

// aggResult is one row produced by an aggregator. Most aggregators
// emit one row per group; topk/bottomk emit up to k rows per group,
// each preserving the original input's label set.
type aggResult struct {
	labels map[string]string
	value  float64
}

func applyAggregator(a *parser.AggregateExpr, rows []VectorRow) ([]aggResult, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	// Group label set is the same for every row in the group; use the
	// first row's pre-grouped labels as the prototype.
	var groupLabels map[string]string
	if a.Without {
		groupLabels = DropLabels(rows[0].Labels, a.Grouping)
	} else {
		groupLabels = KeepLabels(rows[0].Labels, a.Grouping)
	}

	switch a.Op {
	case parser.SUM:
		var s float64
		for _, r := range rows {
			s += r.V
		}
		return []aggResult{{labels: groupLabels, value: s}}, nil
	case parser.AVG:
		var s float64
		for _, r := range rows {
			s += r.V
		}
		return []aggResult{{labels: groupLabels, value: s / float64(len(rows))}}, nil
	case parser.MIN:
		m := rows[0].V
		for _, r := range rows[1:] {
			if r.V < m || math.IsNaN(m) {
				m = r.V
			}
		}
		return []aggResult{{labels: groupLabels, value: m}}, nil
	case parser.MAX:
		m := rows[0].V
		for _, r := range rows[1:] {
			if r.V > m || math.IsNaN(m) {
				m = r.V
			}
		}
		return []aggResult{{labels: groupLabels, value: m}}, nil
	case parser.COUNT:
		return []aggResult{{labels: groupLabels, value: float64(len(rows))}}, nil
	case parser.TOPK, parser.BOTTOMK:
		k, err := aggregatorParam(a)
		if err != nil {
			return nil, err
		}
		return topKBottomK(rows, k, a.Op == parser.BOTTOMK), nil
	case parser.QUANTILE:
		phi, err := aggregatorParamFloat(a)
		if err != nil {
			return nil, err
		}
		return []aggResult{{labels: groupLabels, value: quantileAggregate(phi, rows)}}, nil
	case parser.COUNT_VALUES:
		label, err := aggregatorParamString(a)
		if err != nil {
			return nil, err
		}
		return countValues(a, label, rows), nil
	case parser.LIMITK:
		k, err := aggregatorParam(a)
		if err != nil {
			return nil, err
		}
		return limitK(rows, k), nil
	case parser.LIMIT_RATIO:
		r, err := aggregatorParamFloat(a)
		if err != nil {
			return nil, err
		}
		return limitRatio(r, rows), nil
	}
	return nil, fmt.Errorf("oracle: unsupported aggregation op %s", a.Op)
}

// aggregatorParamString extracts the constant label name for
// count_values(label, expr) — the only aggregator whose parameter is
// a string literal rather than a number.
func aggregatorParamString(a *parser.AggregateExpr) (string, error) {
	if a.Param == nil {
		return "", fmt.Errorf("oracle: aggregator %s requires a parameter", a.Op)
	}
	s, ok := a.Param.(*parser.StringLiteral)
	if !ok {
		return "", fmt.Errorf("oracle: aggregator %s param must be StringLiteral, got %T", a.Op, a.Param)
	}
	return s.Val, nil
}

// countValues implements count_values(label, expr) (Prom engine.go::
// aggregationCountValues): every input row is stamped with `label`
// set to its value rendered via Prom's exact formatting
// (strconv.FormatFloat(v, 'f', -1, 64)), and rows are then re-grouped
// by the resulting FULL label set — the with/without projection PLUS
// the minted label — one output row per distinct group, valued at
// the group's row count.
//
// Minting the label BEFORE computing the projection (rather than
// adding it to the caller's already-narrowed groupLabels) mirrors
// Prom's own two-step and handles `by`/`without` uniformly: for `by`
// mode Prom explicitly appends the value label to the grouping key
// even when the caller didn't name it, so KeepLabels's normal
// keep-list is widened with an explicit re-set after projection; for
// `without` mode the minted label naturally survives the projection
// unless the caller listed it explicitly in without(...), which
// DropLabels already handles correctly with no special-casing.
func countValues(a *parser.AggregateExpr, label string, rows []VectorRow) []aggResult {
	type bucket struct {
		labels map[string]string
		count  int
	}
	buckets := make(map[string]*bucket)
	keys := make([]string, 0)
	for _, r := range rows {
		merged := CopyLabels(r.Labels)
		merged[label] = strconv.FormatFloat(r.V, 'f', -1, 64)

		var out map[string]string
		if a.Without {
			out = DropLabels(merged, a.Grouping)
		} else {
			out = KeepLabels(merged, a.Grouping)
			out[label] = merged[label]
		}

		key := labelKey(out)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{labels: out}
			buckets[key] = b
			keys = append(keys, key)
		}
		b.count++
	}

	sort.Strings(keys)
	result := make([]aggResult, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		result = append(result, aggResult{labels: b.labels, value: float64(b.count)})
	}
	return result
}

// maxUint64AsFloat is math.MaxUint64 widened to float64, matching
// Prom's HashRatioSampler.SampleOffset (promql/engine.go) and
// cerberus's own ratioOffsetExpr SQL lowering
// (internal/promql/lower.go) — the divisor that turns a series's
// label hash into a value in [0, 1).
const maxUint64AsFloat = float64(math.MaxUint64)

// limitRatio implements limit_ratio(r, expr) (Prom engine.go::
// HashRatioSampler.AddRatioSampleWithOffset): a per-row, per-series
// deterministic keep/drop test independent of `by`/`without`
// grouping — Prom applies the identical ratio test to every sample
// regardless of which aggregation group it lands in (confirmed by
// reading engine.go's aggregation loop: the `!group.seen` and later
// branches both call ratiosampler.AddRatioSample with no group-scoped
// state), and cerberus's own lowering matches: lowerLimitRatio emits
// a flat chplan.Filter with no groupBy dependency. So this ignores
// a.Grouping/groupLabels entirely and filters rows independently,
// which composes correctly with evalAggregation's outer with/without
// grouping loop since the per-row survival decision never depends on
// group membership.
//
// r == 0 is a hard empty result (Prom returns before any group is
// even created). r is clamped to [-1, 1]; r >= 0 keeps offset < r,
// r < 0 keeps offset >= 1+r. Surviving rows keep their full original
// label set minus __name__, matching topKBottomK's convention.
func limitRatio(r float64, rows []VectorRow) []aggResult {
	if r == 0 {
		return nil
	}
	if r > 1 {
		r = 1
	} else if r < -1 {
		r = -1
	}

	result := make([]aggResult, 0, len(rows))
	for _, row := range rows {
		offset := float64(labels.FromMap(row.Labels).Hash()) / maxUint64AsFloat
		var keep bool
		if r >= 0 {
			keep = offset < r
		} else {
			keep = offset >= 1+r
		}
		if keep {
			result = append(result, aggResult{
				labels: DropLabel(row.Labels, MetricNameLabel),
				value:  row.V,
			})
		}
	}
	return result
}

// limitK implements the oracle-observable boundary of limitk(k, expr):
// K<1 keeps nothing, and K at or beyond the group's cardinality keeps
// everything (each surviving row keeps its full original label set
// minus __name__, matching topKBottomK's convention). Prometheus's
// real selection in between those endpoints is documented as
// "whatever K rows the storage/evaluation iterator produces first per
// group" (engine.go: `if int64(len(group.heap)) < k`), with no
// ordering contract shared with ClickHouse's own `LIMIT k BY <group>`
// — so this intentionally does NOT try to reproduce cerberus's exact
// mid-range row selection; callers exercising limitk against real
// cerberus output must restrict themselves to the two endpoints this
// implements. See https://github.com/tsouza/cerberus/issues/1693.
func limitK(rows []VectorRow, k int) []aggResult {
	if k < 1 {
		return nil
	}
	if k > len(rows) {
		k = len(rows)
	}
	result := make([]aggResult, 0, k)
	for _, row := range rows[:k] {
		result = append(result, aggResult{
			labels: DropLabel(row.Labels, MetricNameLabel),
			value:  row.V,
		})
	}
	return result
}

// aggregatorParamFloat is aggregatorParam's float counterpart, for
// `quantile(phi, …)` — phi is a real-valued scalar, not an int count.
func aggregatorParamFloat(a *parser.AggregateExpr) (float64, error) {
	if a.Param == nil {
		return 0, fmt.Errorf("oracle: aggregator %s requires a parameter", a.Op)
	}
	n, ok := a.Param.(*parser.NumberLiteral)
	if !ok {
		return 0, fmt.Errorf("oracle: aggregator %s param must be NumberLiteral, got %T", a.Op, a.Param)
	}
	return n.Val, nil
}

// quantileAggregate implements the `quantile(phi, vector)` aggregator:
// Prom's textbook sample-quantile over the group's values (promql/
// quantile.go::quantile) — sort ascending, then weighted-average
// interpolate between the two samples straddling rank = phi*(n-1).
// phi < 0 / > 1 / NaN follow Prom's documented out-of-domain results.
func quantileAggregate(phi float64, rows []VectorRow) float64 {
	if math.IsNaN(phi) {
		return math.NaN()
	}
	if phi < 0 {
		return math.Inf(-1)
	}
	if phi > 1 {
		return math.Inf(1)
	}
	values := make([]float64, len(rows))
	for i, r := range rows {
		values[i] = r.V
	}
	sort.Float64s(values)

	n := float64(len(values))
	rank := phi * (n - 1)
	lowerIndex := math.Max(0, math.Floor(rank))
	upperIndex := math.Min(n-1, lowerIndex+1)
	weight := rank - math.Floor(rank)
	return values[int(lowerIndex)]*(1-weight) + values[int(upperIndex)]*weight
}

// aggregatorParam extracts the constant k for topk/bottomk. PromQL
// allows any scalar expression here, but the property test only
// generates NumberLiteral; reject anything else so unexpected
// shapes don't silently miscount.
func aggregatorParam(a *parser.AggregateExpr) (int, error) {
	if a.Param == nil {
		return 0, fmt.Errorf("oracle: aggregator %s requires a parameter", a.Op)
	}
	n, ok := a.Param.(*parser.NumberLiteral)
	if !ok {
		return 0, fmt.Errorf("oracle: aggregator %s param must be NumberLiteral, got %T", a.Op, a.Param)
	}
	return int(n.Val), nil
}

// topKBottomK returns the top-k or bottom-k rows by value. Each row
// preserves its original (full, non-grouped) label set — the
// grouping affects WHICH set of rows we pick top-k from, not the
// emitted label shape.
//
// However, the __name__ label is stripped (Prom convention for
// aggregation outputs).
func topKBottomK(rows []VectorRow, k int, bottom bool) []aggResult {
	if k <= 0 {
		return nil
	}
	sorted := make([]VectorRow, len(rows))
	copy(sorted, rows)
	if bottom {
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].V < sorted[j].V
		})
	} else {
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].V > sorted[j].V
		})
	}
	if k > len(sorted) {
		k = len(sorted)
	}
	out := make([]aggResult, 0, k)
	for _, r := range sorted[:k] {
		out = append(out, aggResult{
			labels: DropLabel(r.Labels, MetricNameLabel),
			value:  r.V,
		})
	}
	return out
}
