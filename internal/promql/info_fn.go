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

// infoNameFallbackPattern is the synthetic `__name__` regex the reference
// engine substitutes when a second-argument selector carries ONLY negated
// name matchers: with no positive matcher to anchor the scan, upstream
// narrows the candidate set to the conventional `*_info` metric family
// rather than scanning every metric. Mirrors promql/info.go's
// `effectiveInfoNameMatchers`.
const infoNameFallbackPattern = ".+_info"

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
// matchers (of any type — equality, regex, or negated) pick the info
// metrics; its non-`__name__` matchers both filter the info series and
// restrict which info labels copy onto the output (the
// InfoJoin.DataLabels list). Those same non-`__name__` matchers also
// decide whether a base sample that matches no info series survives at
// all — see infoDropsUnmatched.
func lowerInfo(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 1 || len(c.Args) > 2 {
		return nil, fmt.Errorf("promql: info expects 1 or 2 arguments, got %d", len(c.Args))
	}

	base, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	var nameMatchers, dataMatchers []*labels.Matcher
	if len(c.Args) == 2 {
		sel, ok := c.Args[1].(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("promql: info(...) second argument must be a label selector, got %T", c.Args[1])
		}
		for _, m := range sel.LabelMatchers {
			if m.Name == model.MetricNameLabel {
				nameMatchers = append(nameMatchers, m)
				continue
			}
			dataMatchers = append(dataMatchers, m)
		}
	}
	nameMatchers = effectiveInfoNameMatchers(nameMatchers)

	infoNode, err := lowerInfoMetric(nameMatchers, dataMatchers, s, ctx)
	if err != nil {
		return nil, err
	}

	dataLabels := infoDataLabelNames(dataMatchers)

	return &chplan.InfoJoin{
		Input:            base,
		Info:             infoNode,
		IdentityLabels:   slices.Clone(infoIdentityLabels),
		DataLabels:       dataLabels,
		DropUnmatched:    infoDropsUnmatched(dataMatchers),
		MergeInfoMetrics: !pinsSingleInfoMetric(nameMatchers),
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// effectiveInfoNameMatchers returns the `__name__` matcher set that
// actually selects info series, given the ones a second-argument
// selector spelled out. Ports promql/info.go's function of the same
// name:
//
//   - At least one positive matcher (`=` or `=~`) → use the set as-is;
//     the negated matchers ride along as extra narrowing.
//   - Only negated matchers → prepend the synthetic `.+_info` regex so
//     the scan is anchored to the conventional info-metric family
//     instead of every metric in the store.
//   - No matchers at all (bare `info(v)`, or a selector carrying only
//     data-label matchers) → the default `target_info` equality.
func effectiveInfoNameMatchers(nameMatchers []*labels.Matcher) []*labels.Matcher {
	for _, m := range nameMatchers {
		if m.Type == labels.MatchEqual || m.Type == labels.MatchRegexp {
			return nameMatchers
		}
	}
	if len(nameMatchers) > 0 {
		fallback := labels.MustNewMatcher(labels.MatchRegexp, model.MetricNameLabel, infoNameFallbackPattern)
		return append([]*labels.Matcher{fallback}, nameMatchers...)
	}
	return []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, targetInfoMetric),
	}
}

// pinsSingleInfoMetric reports whether the effective `__name__` matcher
// set can only ever select one info metric — a lone equality matcher.
// Anything else (a regex, a negation, or several matchers) may match
// several info metrics at once, and the reference engine then unions the
// data labels of every matched info series onto a single output sample
// (promql/info.go::combineWithInfoVector iterates `seenInfoMetrics` and
// merges into one label builder). The join emitter needs that signal to
// fold its per-info-metric join rows back into one row per base sample.
func pinsSingleInfoMetric(nameMatchers []*labels.Matcher) bool {
	return len(nameMatchers) == 1 && nameMatchers[0].Type == labels.MatchEqual
}

// infoDropsUnmatched reports whether a base sample that matches no info
// series must be dropped from the result rather than passed through
// unenriched. The reference engine keeps such a sample only when every
// data-label matcher also matches the empty string
// (promql/info.go::combineWithInfoVector's `allMatchersMatchEmpty`
// branch): a matcher like `k="v"` or `k=~".+"` cannot be satisfied by a
// series carrying no `k` at all, so the sample is not part of the
// result. `k=~".*"` / `k!="v"` do match the empty string, so unmatched
// samples survive.
func infoDropsUnmatched(dataMatchers []*labels.Matcher) bool {
	for _, m := range dataMatchers {
		if !m.Matches("") {
			return true
		}
	}
	return false
}

// lowerInfoMetric lowers the info-metric side of the join: a synthetic
// VectorSelector carrying the effective `__name__` matchers plus any
// data-label matchers, run through the regular VectorSelector lowering
// so the info side gets the same per-series-latest (LWR) / per-step-
// matrix treatment as the base vector — and so regex / negated name
// matchers reuse the same unpinned-name scan fan-out an ordinary
// `{__name__=~"…"}` selector gets.
func lowerInfoMetric(nameMatchers, dataMatchers []*labels.Matcher, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	matchers := make([]*labels.Matcher, 0, len(nameMatchers)+len(dataMatchers))
	matchers = append(matchers, nameMatchers...)
	matchers = append(matchers, dataMatchers...)

	sel := &parser.VectorSelector{
		// Empty unless a `__name__` equality pins the name, matching the
		// parser's own convention for `{__name__=~"…"}` selectors.
		Name:          metricNameFromMatchers(matchers),
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
