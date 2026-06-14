package promql

import (
	"fmt"
	"slices"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// targetInfoMetric is the default info metric name PromQL's `info(v)`
// enriches from when no second-argument `__name__` matcher narrows it —
// the OpenTelemetry `target_info` resource-attribute series. Mirrors the
// reference engine's `targetInfo` constant (promql/info.go).
const targetInfoMetric = "target_info"

// infoIdentityLabels are the labels the info-enrichment join keys on. The
// reference engine hard-codes `{instance, job}` (promql/info.go's
// `identifyingLabels`); the SQL join matches base ↔ info series on these.
var infoIdentityLabels = []string{"instance", "job"}

// lowerInfo lowers PromQL's
//
//	info(v)
//	info(v, {label matchers})
//
// the label-enrichment join. The reference engine (promql/info.go::
// evalInfo) joins target-identifying data labels from a companion
// `target_info`-style series onto the input vector `v` by the data-source
// identity labels (`instance` / `job`), enriching each input series' label
// set while leaving the sample values + timestamps unchanged.
//
// Lowering:
//
//   - arg[0] (`v`) lowers normally to the canonical per-series-latest
//     Sample shape.
//   - The info metric (default `target_info`, or the name a second-arg
//     `__name__` matcher selects) lowers through the same VectorSelector
//     path so the info side is also per-series-latest — each base series
//     matches at most one info series per identity key.
//   - The two sides wrap in a chplan.InfoJoin keyed on `{instance, job}`.
//
// The optional second argument is a label-selector: its `__name__`
// matcher (if any) picks the info metric; its non-`__name__` matchers
// both filter the info series and restrict which info labels copy onto
// the output (the InfoJoin.DataLabels list).
func lowerInfo(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 1 || len(c.Args) > 2 {
		return nil, fmt.Errorf("promql: info expects 1 or 2 arguments, got %d", len(c.Args))
	}

	base, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	infoName := targetInfoMetric
	var dataMatchers []*labels.Matcher
	if len(c.Args) == 2 {
		sel, ok := c.Args[1].(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("promql: info(...) second argument must be a label selector, got %T", c.Args[1])
		}
		for _, m := range sel.LabelMatchers {
			if m.Name == model.MetricNameLabel {
				if m.Type != labels.MatchEqual {
					return nil, fmt.Errorf("promql: info(...) second-argument __name__ matcher must be an equality match, got %q", m.Type)
				}
				infoName = m.Value
				continue
			}
			dataMatchers = append(dataMatchers, m)
		}
	}

	infoNode, err := lowerInfoMetric(infoName, dataMatchers, s, ctx)
	if err != nil {
		return nil, err
	}

	dataLabels := infoDataLabelNames(dataMatchers)

	return &chplan.InfoJoin{
		Input:            base,
		Info:             infoNode,
		IdentityLabels:   slices.Clone(infoIdentityLabels),
		DataLabels:       dataLabels,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// lowerInfoMetric lowers the info-metric side of the join: a synthetic
// VectorSelector for `infoName` carrying any data-label matchers, run
// through the regular VectorSelector lowering so the info side gets the
// same per-series-latest (LWR) / per-step-matrix treatment as the base
// vector.
func lowerInfoMetric(infoName string, dataMatchers []*labels.Matcher, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	matchers := make([]*labels.Matcher, 0, len(dataMatchers)+1)
	matchers = append(matchers, labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, infoName))
	matchers = append(matchers, dataMatchers...)

	sel := &parser.VectorSelector{
		Name:          infoName,
		LabelMatchers: matchers,
	}
	return lowerVectorSelector(sel, s, ctx)
}

// infoDataLabelNames returns the de-duplicated set of label names the
// second-argument matchers reference (excluding `__name__`). Non-empty
// → the InfoJoin copies only these info labels onto the output; empty →
// every non-identity info label copies (the default `info(v)` case).
func infoDataLabelNames(dataMatchers []*labels.Matcher) []string {
	if len(dataMatchers) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	names := make([]string, 0, len(dataMatchers))
	for _, m := range dataMatchers {
		if _, ok := seen[m.Name]; ok {
			continue
		}
		seen[m.Name] = struct{}{}
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}
