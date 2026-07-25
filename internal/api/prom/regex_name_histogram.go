package prom

import (
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Classic-histogram series live under two different alphabets: the OTel-CH
// exporter stores ONE row per observation under the bare `<base>` MetricName,
// while the PromQL wire surface exposes that row only as the synthetic
// companion series `<base>_bucket` / `<base>_count` / `<base>_sum` (see
// promql.HistogramSyntheticNames). A `__name__` matcher speaks the SYNTHETIC
// alphabet; the stored column holds the BASE alphabet.
//
// For an equality matcher the selector lowering bridges the two by stripping
// the companion suffix and looking the base up in the histogram table. A REGEX
// `__name__` matcher has no suffix to strip, so lowering it as a predicate over
// the stored MetricName column asks the wrong question — it tests the regex
// against a name the wire surface never advertises. The regex can therefore
// never select a histogram-derived series, and the metadata endpoints answer
// "success" with a silently incomplete result.
//
// This file closes that gap by evaluating the regex where it belongs: against
// the synthetic name set. The base names present in the request window are
// enumerated once (the same enumeration `/api/v1/label/__name__/values` uses),
// crossed with the suffixes the lowering serves, and each synthetic name that
// satisfies every `__name__` matcher is re-emitted as an EQUALITY-pinned
// matcher variant — handing the actual resolution back to the one code path
// that already knows how to read the histogram row.
//
// The pinned variants are a resource improvement as well as a correctness one:
// each carries an exact MetricName equality (the metric tables' primary-key
// prefix) instead of a regex over the whole column, so the added arms prune to
// the matched families rather than scanning the window's full name space.

// unpinnedMetricNameMatchers splits a single-VectorSelector match[] string into
// its `__name__` matchers and its remaining matchers, and reports whether the
// selector needs synthetic-name resolution — i.e. it carries at least one
// `__name__` matcher and NONE of them pins the name to an equality.
//
// The equality case is excluded because the lowering already routes it: a
// pinned `<base>_sum` strips to the histogram row directly, and re-deriving it
// here would only duplicate arms. A selector with no `__name__` matcher at all
// is likewise out of scope for this fan-out — it selects by attributes alone,
// and its candidate-table resolution is the scan-side concern of
// schema.Metrics.TablesForUnknownName rather than a name-set question.
//
// Parse failures return ok=false; the downstream matcherSQL call re-parses and
// surfaces the same error to the client with full diagnostics.
func unpinnedMetricNameMatchers(parser promparser.Parser, matcher string) (nameMatchers, other []*labels.Matcher, ok bool) {
	expr, err := parser.ParseExpr(matcher)
	if err != nil {
		return nil, nil, false
	}
	vs, vsErr := singleVectorSelector(expr)
	if vsErr != nil {
		return nil, nil, false
	}
	for _, m := range vs.LabelMatchers {
		if m.Name != model.MetricNameLabel {
			other = append(other, m)
			continue
		}
		if m.Type == labels.MatchEqual {
			return nil, nil, false
		}
		nameMatchers = append(nameMatchers, m)
	}
	if len(nameMatchers) == 0 {
		return nil, nil, false
	}
	return nameMatchers, other, true
}

// histogramSyntheticVariants returns one equality-pinned matcher variant per
// synthetic classic-histogram series name that satisfies EVERY `__name__`
// matcher of the selector. `bases` are the Prom-grammar base names present in
// the request window; each is expanded through the selector lowering's own
// routing so a variant can only name a series the query path can serve.
//
// The variant drops the original `__name__` matchers — they have already been
// evaluated in full against the synthetic name, and keeping them would re-apply
// the regex to the stored base name inside the lowering, where it cannot match.
// Every other matcher is preserved verbatim, so attribute predicates (including
// a `le` matcher against the bucket fan-out) still narrow each arm.
func histogramSyntheticVariants(nameMatchers, other []*labels.Matcher, bases []string, s schema.Metrics) []string {
	var out []string
	for _, base := range bases {
		for _, name := range promql.HistogramSyntheticNames(base, s) {
			if !matchesAll(nameMatchers, name) {
				continue
			}
			out = append(out, pinnedNameMatcherString(name, other))
		}
	}
	return out
}

// matchesAll reports whether every matcher accepts the given `__name__` value.
// Prometheus matcher semantics are used verbatim (fully anchored regexes,
// negation handled by the matcher itself), so the synthetic name set is
// filtered by exactly the predicate reference Prometheus would apply to its own
// series names.
func matchesAll(ms []*labels.Matcher, value string) bool {
	for _, m := range ms {
		if !m.Matches(value) {
			return false
		}
	}
	return true
}

// pinnedNameMatcherString renders `{__name__="<name>", <other matchers…>}` from
// parsed matchers rather than by rewriting the caller's matcher text: the
// original `__name__` value is an arbitrary regex whose bytes must not leak
// into the emitted selector, and labels.Matcher.String() already quotes and
// escapes each matcher back into PromQL grammar.
func pinnedNameMatcherString(name string, other []*labels.Matcher) string {
	pinned := &labels.Matcher{Type: labels.MatchEqual, Name: model.MetricNameLabel, Value: name}
	parts := make([]string, 0, 1+len(other))
	parts = append(parts, pinned.String())
	for _, m := range other {
		parts = append(parts, m.String())
	}
	return "{" + strings.Join(parts, ",") + "}"
}
