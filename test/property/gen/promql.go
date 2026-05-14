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
// Accept-set as of Phase 1 PR 2 (widened from PR 1 with the from-
// scratch oracle in place):
//
//   - Bare vector selector: `metric{label="value", …}`
//   - Aggregation:           `sum(metric{...})`,
//     `sum by(label)(metric{...})`
//   - Range function:        `rate(metric{...}[60s])`,
//     `sum(rate(metric{...}[60s]))`
//
// PR 1 narrowed the set to bare selectors because the bridge oracle
// (Prom's own engine) and cerberus disagreed on two real semantic
// points: cerberus's `sum` sums every stored sample rather than the
// LWR per series, and cerberus's vector path doesn't honour the
// eval-ts boundary the way Prom does. The from-scratch oracle (PR 2)
// implements both rules correctly — so widening the generator surfaces
// those production-side divergences as property-test failures (as
// intended). The property test is currently t.Skip'd in the test
// file with a pointer to the tracked follow-up production fixes; the
// generator is wired up the way a green production path will exercise
// once those fixes land.
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

		expr := drawExpr(t, name, matchers)

		// EvalTs: pick a timestamp AFTER every dataset sample but
		// well within the 5-minute LookbackDelta (Prom's default,
		// which the from-scratch oracle also uses).
		//
		// The dataset generator emits at most 10 points per series
		// at 15-second spacing (max sample ts = anchor + 9*15s =
		// 135s). Picking anchor + 200s leaves a comfortable margin
		// past the last sample so the per-series LWR rule has a
		// fresh sample to surface.
		evalTs := AnchorTime().Add(200 * time.Second).Unix()

		return property.Query{
			String: expr.String(),
			EvalTs: evalTs,
		}
	})
}

// drawExpr picks the random expression shape per the PR 2 accept-set.
// Each draw is uniform over the candidate shapes; the breakdown:
//
//   - bare vector selector             — exercises the LWR rule
//   - sum(selector)                    — exercises aggregation + LWR
//   - sum by(label)(selector)          — exercises grouping
//   - rate(selector[60s])              — exercises range function
//   - sum(rate(selector[60s]))         — exercises composition
//
// Aggregations strip __name__; the bare selector keeps it. Both
// shapes are valid Prom queries.
func drawExpr(t *rapid.T, name string, matchers []*labels.Matcher) parser.Expr {
	shape := rapid.IntRange(0, 4).Draw(t, "shape")
	sel := &parser.VectorSelector{Name: name, LabelMatchers: matchers}
	switch shape {
	case 0:
		return sel
	case 1:
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel}
	case 2:
		// `sum by(<label>)`: pick a label from the dataset's pool.
		// Falls back to no-grouping if no labels are available.
		group := pickLabelName(t)
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel, Grouping: group}
	case 3:
		return &parser.Call{
			Func: parser.Functions["rate"],
			Args: []parser.Expr{
				&parser.MatrixSelector{VectorSelector: sel, Range: 60 * time.Second},
			},
		}
	case 4:
		return &parser.AggregateExpr{
			Op: parser.SUM,
			Expr: &parser.Call{
				Func: parser.Functions["rate"],
				Args: []parser.Expr{
					&parser.MatrixSelector{VectorSelector: sel, Range: 60 * time.Second},
				},
			},
		}
	}
	return sel
}

func pickLabelName(t *rapid.T) []string {
	// A nil/empty group means "no grouping" — equivalent to bare
	// `sum(...)`. Otherwise pick one of the pool's label names.
	if !rapid.Bool().Draw(t, "useGroup") {
		return nil
	}
	return []string{rapid.SampledFrom(LabelNamePool).Draw(t, "groupLabel")}
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
