package prom

import (
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// histogramCompanionSuffixes are the Prom-wire classic-histogram companion
// suffixes — the three series names Prom exposes for a single
// classic-histogram OTel-CH row. `_total` is included because the same
// `TableFor` heuristic treats it as a sum-table counter; expanding a
// counter-shaped name into histogram companions would point at an
// empty table.
var histogramCompanionSuffixes = []string{"_bucket", "_count", "_sum", "_total"}

// expandBareHistogramMatcher takes a raw match[] string and, when it is
// a single VectorSelector pinning `__name__` to a bare base name (no
// classic-histogram companion suffix), returns the original matcher PLUS
// three companion variants — `<base>_bucket`, `<base>_count`,
// `<base>_sum` — so the labels / series metadata surfaces can fan-out
// the lookup across the histogram table's companion rows.
//
// The OTel-CH classic histogram exporter writes one row under the bare
// `<base>` name in the histogram table; PromQL exposes that single row
// as three companion series (`_bucket` / `_count` / `_sum`). Cerberus's
// `__name__` listing also emits the bare base name (the row's MetricName
// column literally has that value), so Grafana's Metrics Explorer picks
// up the bare name and uses it as a match[] selector when fetching the
// metric's labels chip. Without the companion fan-out the bare-name
// matcher lowers to a gauge-table scan (TableFor's default), the
// histogram row never matches, and the labels chip surfaces "Unable to
// fetch labels".
//
// Returns the original matcher string as the first element so callers
// that union results from each variant still hit the matcher's original
// shape (e.g. a real gauge metric whose name doesn't carry any
// companion suffix). Variants are deduped against the original so the
// callers don't re-scan the same shape twice when the original already
// is a companion form (e.g. when Grafana sends `<base>_bucket` itself
// — that path returns the input list of length 1).
//
// The parser is the caller's responsibility for the actual matcher SQL
// lowering; this helper uses its own parse pass against the raw string
// since the lowering re-parses inside matcherSQL. Parse failures fall
// through as the single-element slice — the downstream matcherSQL call
// will surface the same parse error to the client with full diagnostics.
func expandBareHistogramMatcher(parser promparser.Parser, matcher, histogramTable string) []string {
	if histogramTable == "" {
		return []string{matcher}
	}
	base, ok := bareHistogramBaseName(parser, matcher)
	if !ok {
		return []string{matcher}
	}
	out := make([]string, 0, 1+len(histogramCompanionSuffixes)-1)
	out = append(out, matcher)
	seen := map[string]struct{}{matcher: {}}
	for _, suf := range []string{"_bucket", "_count", "_sum"} {
		variant := companionMatcherString(matcher, base, base+suf)
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

// bareHistogramBaseName reports whether the matcher names a bare metric
// (no classic-histogram companion suffix and no counter `_total` suffix)
// via a single VectorSelector with a `__name__=<X>` equality matcher.
// Returns the bare name on a hit.
//
// `__name__=~"…"` (regex) selectors are not expanded — the user has
// explicitly chosen a regex shape and the companion fan-out would
// double-count any match the regex already covers.
func bareHistogramBaseName(parser promparser.Parser, matcher string) (string, bool) {
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

// equalNameMatcher returns the value of the `__name__` equality matcher
// in ms, or "" if none is present (regex / negated matches don't
// qualify). Mirrors metricNameFromMatchers in internal/promql/lower.go.
func equalNameMatcher(ms []*labels.Matcher) string {
	for _, m := range ms {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

// companionMatcherString rewrites a bare-name matcher string into the
// equivalent matcher targeting a companion series. The rewrite preserves
// the surrounding shape — `name{label="x"}` becomes `companion{label="x"}`;
// `{__name__="name", …}` becomes `{__name__="companion", …}`; bare
// `name` becomes bare `companion`.
//
// Returns "" when the rewrite isn't trivially safe (e.g. the matcher
// has been pre-rewritten by normalizeDottedSelectors into a shape the
// simple replacement doesn't recognise). The caller treats an empty
// return as "skip this variant" — the bare-name arm still runs.
func companionMatcherString(matcher, base, companion string) string {
	trimmed := strings.TrimSpace(matcher)
	// `__name__="<base>"` inside a `{…}` matcher group — rewrite the
	// quoted value in place. This is the shape normalizeDottedSelectors
	// produces and the shape Grafana's Metrics Explorer sometimes sends
	// explicitly.
	needle := `__name__="` + base + `"`
	if strings.Contains(trimmed, needle) {
		return strings.Replace(trimmed, needle, `__name__="`+companion+`"`, 1)
	}
	// Bare-name selector — `<base>` or `<base>{…}`. Detect by checking
	// whether the matcher starts with the base name followed by either
	// end-of-string or a `{` matcher-group opener.
	if strings.HasPrefix(trimmed, base) {
		rest := trimmed[len(base):]
		if rest == "" || rest[0] == '{' {
			return companion + rest
		}
	}
	return ""
}
