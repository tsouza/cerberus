package promql

import (
	"fmt"
	"math"
	"sort"

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
	}
	return nil, fmt.Errorf("oracle: unsupported aggregation op %s", a.Op)
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

