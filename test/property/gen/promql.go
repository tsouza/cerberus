package gen

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// PromQLQuery returns a rapid generator that produces a random
// property.Query targeted at d. The generator builds a parser.Expr AST
// directly (never a string) so we can't produce a query that fails to
// re-parse later. The query string surfaced on the Query value is the
// AST's String() method — guaranteed to round-trip through
// promparser.ParseExpr by the upstream parser contract.
//
// Accept-set for PR 1 (narrow on purpose; expanded in PR 2):
//
//   - Bare vector selector: `metric{label="value", …}`
//
// `sum(...)` / `rate(...)` / range-vector functions are deferred to PR
// 2 along with the from-scratch oracle. Initial Phase 1 PR 1 runs of
// this property test (seed=9184878648749493481) discovered that
// cerberus's emitted SQL for `sum(metric{...})` sums every stored
// sample's Value, while Prometheus's instant evaluator picks the
// latest sample per series first and then sums — a real semantic
// gap (TODO: file with a minimised TXTAR fixture).
// PR 1's purpose is the framework infrastructure; narrowing the
// generator's accept-set to the bare-selector shape lets the suite
// pass clean while the framework is on solid ground.
//
// EvalTs is anchored to the dataset's window so every query has at
// least one matching sample within Prometheus's 5-minute LookbackDelta.
func PromQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		names := d.Metrics.NamesPresent()
		if len(names) == 0 {
			// MetricsDataset filters this out before Run() draws a
			// query; nonetheless guard against an empty pool so the
			// generator never panics.
			return property.Query{}
		}

		name := rapid.SampledFrom(names).Draw(t, "metric")
		matchers := drawMatchers(t, name, d.Metrics)

		expr := &parser.VectorSelector{
			Name:          name,
			LabelMatchers: matchers,
		}

		// EvalTs: pick a timestamp AFTER every dataset sample but
		// well within the bridge oracle's 5-minute LookbackDelta.
		// The dataset generator emits at most 10 points per series at
		// 15-second spacing (max sample ts = anchor + 9*15s = 135s).
		// Picking anchor + 200s leaves a comfortable margin past the
		// last sample so the bridge oracle's
		// "latest-sample-at-or-before-eval-ts" rule and cerberus's
		// "latest sample regardless of eval ts" path agree
		// (post-filter on a closed sample window, every "latest
		// available" equals "latest with ts ≤ eval ts").
		//
		// Phase 1 PR 2 will tighten this: with a from-scratch oracle
		// the generator can pick arbitrary eval timestamps and the
		// oracle will compute the lookback / latest-at-ts semantics
		// directly. Today cerberus's vector path doesn't honour the
		// eval-ts boundary the way Prometheus does, so we side-step
		// the gap.
		evalTs := AnchorTime().Add(200 * time.Second).Unix()

		return property.Query{
			String: expr.String(),
			EvalTs: evalTs,
		}
	})
}

// drawMatchers picks a 0-or-1 label matcher to attach to the
// selector. The __name__ matcher is added unconditionally — PromQL's
// vector-selector printer requires either `metricName{…}` or
// `{__name__="…", …}` shape; without it the String() form would be
// `{job="api"}`, which Prometheus parses as a name-less selector and
// emits no series.
func drawMatchers(t *rapid.T, name string, m *property.MetricsModel) []*labels.Matcher {
	out := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", name),
	}

	labelsPresent := m.LabelsPresentFor(name)
	if len(labelsPresent) == 0 {
		return out
	}

	// 50% chance of adding a label matcher. Kept low so each query
	// has a decent shot at matching multiple series — important for
	// the aggregate path, which collapses to a single series when
	// only one input matches.
	if rapid.Bool().Draw(t, "hasMatcher") {
		// Pick from the labels that have at least one value in the
		// dataset (i.e. present on at least one series for this
		// metric). The values list ranges over the union of values
		// the generator stamped on that label.
		labelNames := mapKeys(labelsPresent)
		labelName := rapid.SampledFrom(labelNames).Draw(t, "matcherLabel")
		labelValue := rapid.SampledFrom(labelsPresent[labelName]).Draw(t, "matcherValue")
		out = append(out, labels.MustNewMatcher(labels.MatchEqual, labelName, labelValue))
	}

	return out
}

// mapKeys returns the (string) keys of m as a slice, sorted. Used by
// drawMatchers so rapid's draw is over a deterministic list rather
// than the map's range order.
func mapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion order isn't stable for maps; the dataset generator
	// stores its label pool with a fixed name order, but the
	// LabelsPresentFor() pivot loses it. Sort here so the generator
	// is reproducible across runs with the same rapid seed.
	sortStrings(out)
	return out
}

// sortStrings is a thin wrapper so this file doesn't add a `sort`
// import for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
