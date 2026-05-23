package prom

import (
	"strings"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/api/format"
)

// expandUnderscoredMetricNameMatcher takes a raw match[] string and,
// when the matcher pins `__name__` to a single equality value with at
// least one rewritable underscore, returns the original matcher PLUS
// one variant per OTel-dotted candidate produced by
// [format.PromLabelToOTelCandidates]. The catalog endpoint
// (`/api/v1/label/__name__/values`) runs every stored MetricName
// through `OTelToPromMetric` so dotted-stored names
// (`http.server.request.duration`) surface in the catalog as the
// Prom-grammar form (`http_server_request_duration`). When the user (or
// Drilldown-Metrics' label chip) then queries `match[]={__name__=
// "<underscored>"}`, the matcher lowering compares against the raw
// stored MetricName — which is still dotted — and returns zero rows.
//
// The fan-out runs the user's matcher against every plausible dotted
// re-expansion of the underscored input, mirroring the symmetric
// attribute-key fan-out in [internal/promql.attributeLookup] that
// resolves user-typed `cerberus_ql` to rows stored under the
// OTel-canonical `cerberus.ql` Attributes key.
//
// Returns the original matcher string as the first element so callers
// that union results from each variant still hit the matcher's
// original shape (a metric whose stored name already matches the
// underscored form — `cerberus_queries_total` — resolves via the
// first arm and the dotted variants contribute zero rows). Variants
// are deduped against the original, so a purely underscored value
// (`up`, `cerberus_queries_total`) returns the single-element slice
// and is byte-stable with the pre-fan-out callers.
//
// The parser is the caller's responsibility for the actual matcher
// SQL lowering; this helper uses its own parse pass to extract the
// `__name__` value. Parse failures fall through as the single-element
// slice — the downstream matcherSQL call will surface the same parse
// error to the client with full diagnostics.
//
// Composition with [expandBareHistogramMatcher]: the bare-histogram
// fan-out runs per-arm of THIS fan-out, so a catalog-published
// underscored alias of a dotted classic-histogram base name (the
// failure shape on `http_server_request_body_size` in
// `iterate-metrics-explorer.spec.ts`) resolves via the dotted
// candidate's bucket/count/sum companion variants. Callers chain the
// two helpers as a nested loop; the combined fan-out is bounded by
// `2^6 × 3 = 192` matcher variants in the pathological case (the
// powerset cap in `PromLabelToOTelCandidates` × the three histogram
// companion suffixes).
func expandUnderscoredMetricNameMatcher(parser promparser.Parser, matcher string) []string {
	value, ok := equalNameMatcherValue(parser, matcher)
	if !ok {
		return []string{matcher}
	}
	if !format.PromLabelNeedsDottedFallback(value) {
		return []string{matcher}
	}
	candidates := format.PromLabelToOTelCandidates(value)
	if len(candidates) <= 1 {
		return []string{matcher}
	}
	out := make([]string, 0, len(candidates))
	out = append(out, matcher)
	seen := map[string]struct{}{matcher: {}}
	for _, cand := range candidates {
		if cand == value {
			continue
		}
		variant := metricNameMatcherString(matcher, value, cand)
		if variant == "" {
			continue
		}
		if _, dup := seen[variant]; dup {
			continue
		}
		seen[variant] = struct{}{}
		out = append(out, variant)
	}
	return out
}

// equalNameMatcherValue reports whether the matcher names a single
// VectorSelector with a `__name__=<X>` equality matcher, returning the
// matcher value on a hit. Mirrors [bareHistogramBaseName] including
// the histogram-companion-suffix short-circuit: a name carrying
// `_bucket` / `_count` / `_sum` / `_total` is treated as
// already-routed (the per-suffix lowering paths inside
// [internal/promql.lowerVectorSelector] handle the OTel-CH histogram
// column projection), so the dotted-candidate fan-out skips it. The
// catalog endpoint only publishes base names (the suffix variants are
// derived companions), so the spec failures the fan-out targets all
// land on unsuffixed inputs anyway.
func equalNameMatcherValue(parser promparser.Parser, matcher string) (string, bool) {
	expr, err := parser.ParseExpr(matcher)
	if err != nil {
		return "", false
	}
	vs, vsErr := singleVectorSelector(expr)
	if vsErr != nil {
		return "", false
	}
	name := equalNameMatcher(vs.LabelMatchers)
	if name == "" {
		return "", false
	}
	for _, suf := range histogramCompanionSuffixes {
		if strings.HasSuffix(name, suf) {
			return "", false
		}
	}
	return name, true
}

// metricNameMatcherString rewrites a matcher string so its
// `__name__=<original>` equality pin becomes `__name__=<candidate>`,
// preserving the surrounding shape. Mirrors [companionMatcherString]
// but operates on arbitrary candidate replacements rather than the
// classic-histogram companion suffix rewrite.
//
// Returns "" when the rewrite isn't trivially safe (the matcher has
// been pre-rewritten into a shape the simple replacement doesn't
// recognise). The caller treats an empty return as "skip this
// variant" — the original-name arm still runs.
func metricNameMatcherString(matcher, original, candidate string) string {
	trimmed := strings.TrimSpace(matcher)
	// `__name__="<original>"` inside a `{…}` matcher group — the shape
	// normalizeDottedSelectors produces and the shape Grafana's
	// Drilldown-Metrics sends explicitly via `match[]={__name__=...}`.
	needle := `__name__="` + original + `"`
	if strings.Contains(trimmed, needle) {
		return strings.Replace(trimmed, needle, `__name__="`+candidate+`"`, 1)
	}
	// Bare-name selector — `<original>` or `<original>{…}`. The Prom
	// parser accepts bare metric names without an explicit `__name__=`
	// matcher; only the underscored bare form makes it here (the dotted
	// form would have failed the parser).
	if strings.HasPrefix(trimmed, original) {
		rest := trimmed[len(original):]
		if rest == "" || rest[0] == '{' {
			// The dotted candidate doesn't satisfy PromQL's bare-identifier
			// grammar, so emit the explicit `{__name__=...}` form. If the
			// caller's matcher carried a label group, splice into it.
			if rest == "" {
				return `{__name__="` + candidate + `"}`
			}
			// rest[0] == '{'. Check whether the group is empty (`{}`) so we
			// emit a single `{__name__="<cand>"}` rather than the malformed
			// `{__name__="<cand>"}{...}` shape.
			if len(rest) >= 2 && rest[1] == '}' {
				return `{__name__="` + candidate + `"}` + rest[2:]
			}
			return `{__name__="` + candidate + `",` + rest[1:]
		}
	}
	return ""
}
