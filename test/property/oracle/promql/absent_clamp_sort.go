package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// evalAbsent implements `absent(v)`: emits a single label-less-except-
// matchers row with value 1 when v's inner vector is empty, otherwise
// emits nothing. Matches Prom's funcAbsent + the common
// createLabelsForAbsentFunction case (a bare vector selector's equality
// matchers become the output's label set) — the property generator
// only ever draws a bare selector as absent()'s argument, so the fuller
// Prom algorithm (which also handles selectors joined by `and`/binary
// ops) isn't needed here.
func (e *Evaluator) evalAbsent(c *parser.Call, evalTsMs int64) (value, error) {
	if len(c.Args) != 1 {
		return value{}, fmt.Errorf("oracle: absent() expects 1 arg, got %d", len(c.Args))
	}
	inner, err := e.evalAny(c.Args[0], evalTsMs)
	if err != nil {
		return value{}, err
	}
	if inner.Kind != kindVec {
		return value{}, fmt.Errorf("oracle: absent() argument must be vector")
	}
	if len(inner.Vec) > 0 {
		return value{Kind: kindVec, Vec: nil}, nil
	}
	return value{Kind: kindVec, Vec: []VectorRow{
		{Labels: absentLabels(c.Args[0]), T: evalTsMs, V: 1},
	}}, nil
}

// absentLabels extracts the equality matchers off a bare vector
// selector (excluding __name__) — Prom's createLabelsForAbsentFunction
// for the single-selector case.
func absentLabels(expr parser.Expr) map[string]string {
	out := map[string]string{}
	vs, ok := expr.(*parser.VectorSelector)
	if !ok {
		return out
	}
	for _, m := range vs.LabelMatchers {
		if m.Type == labels.MatchEqual && m.Name != MetricNameLabel {
			out[m.Name] = m.Value
		}
	}
	return out
}

// evalClamp implements clamp(v, min, max) / clamp_min(v, min) /
// clamp_max(v, max) — elementwise, matching Prom's promql/functions.go
// funcClamp / funcClampMin / funcClampMax. clamp() with max < min
// yields an empty vector (Prom's documented degenerate case).
func (e *Evaluator) evalClamp(name string, c *parser.Call, evalTsMs int64) (value, error) {
	wantArgs := map[string]int{"clamp": 3, "clamp_min": 2, "clamp_max": 2}[name]
	if len(c.Args) != wantArgs {
		return value{}, fmt.Errorf("oracle: %s() expects %d args, got %d", name, wantArgs, len(c.Args))
	}
	vecVal, err := e.evalAny(c.Args[0], evalTsMs)
	if err != nil {
		return value{}, err
	}
	if vecVal.Kind != kindVec {
		return value{}, fmt.Errorf("oracle: %s() first argument must be vector", name)
	}
	scalarArg := func(idx int) (float64, error) {
		sv, err := e.evalAny(c.Args[idx], evalTsMs)
		if err != nil {
			return 0, err
		}
		if sv.Kind != kindScalar {
			return 0, fmt.Errorf("oracle: %s() argument %d must be scalar", name, idx)
		}
		return sv.Scalar, nil
	}

	var clampFn func(f float64) float64
	switch name {
	case "clamp":
		minVal, err := scalarArg(1)
		if err != nil {
			return value{}, err
		}
		maxVal, err := scalarArg(2)
		if err != nil {
			return value{}, err
		}
		if maxVal < minVal {
			return value{Kind: kindVec, Vec: nil}, nil
		}
		clampFn = func(f float64) float64 { return math.Max(minVal, math.Min(maxVal, f)) }
	case "clamp_min":
		minVal, err := scalarArg(1)
		if err != nil {
			return value{}, err
		}
		clampFn = func(f float64) float64 { return math.Max(minVal, f) }
	case "clamp_max":
		maxVal, err := scalarArg(1)
		if err != nil {
			return value{}, err
		}
		clampFn = func(f float64) float64 { return math.Min(maxVal, f) }
	default:
		return value{}, fmt.Errorf("oracle: unsupported clamp variant %q", name)
	}

	out := make([]VectorRow, len(vecVal.Vec))
	for i, r := range vecVal.Vec {
		out[i] = VectorRow{Labels: DropLabel(r.Labels, MetricNameLabel), T: r.T, V: clampFn(r.V)}
	}
	sortVectorRows(out)
	return value{Kind: kindVec, Vec: out}, nil
}

// evalSort implements sort()/sort_desc(). The comparator (framework's
// CompareOutcomes) groups rows by label set for comparison, so the
// series ORDER PromQL specifies has no observable effect on the
// property test's pass/fail outcome — it only reorders the same set of
// (label, value) rows the input already carried, drops none, changes
// none. A pass-through therefore mirrors Prom's actual output set
// faithfully for this comparator; only the JSON array order (which the
// comparator doesn't see) would differ.
func (e *Evaluator) evalSort(c *parser.Call, evalTsMs int64) (value, error) {
	if len(c.Args) != 1 {
		return value{}, fmt.Errorf("oracle: sort()/sort_desc() expects 1 arg, got %d", len(c.Args))
	}
	inner, err := e.evalAny(c.Args[0], evalTsMs)
	if err != nil {
		return value{}, err
	}
	if inner.Kind != kindVec {
		return value{}, fmt.Errorf("oracle: sort()/sort_desc() argument must be vector")
	}
	return inner, nil
}
