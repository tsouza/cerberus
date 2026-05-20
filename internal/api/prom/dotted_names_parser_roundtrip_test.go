package prom

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// TestNormalizeDottedSelectors_ParserRoundtrip is the layer the
// string-equality cases in dotted_names_test.go can't cover: it feeds
// every successfully-rewritten query through the real PromQL parser
// and asserts (a) the parser accepts it without error and (b) the
// resulting AST contains a VectorSelector with a `__name__` matcher
// whose value equals the original dotted token. The string-equality
// layer would silently accept a buggy rewrite that produces invalid
// PromQL or drops the metric name; this layer would not.
func TestNormalizeDottedSelectors_ParserRoundtrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		in           string
		wantDotted   []string // dotted names that must appear as __name__ matchers
		wantVectors  int      // minimum VectorSelector count
	}{
		{
			name:        "bare_dotted_metric",
			in:          `http.server.request.duration`,
			wantDotted:  []string{"http.server.request.duration"},
			wantVectors: 1,
		},
		{
			name:        "inside_rate",
			in:          `rate(http.server.request.duration[5m])`,
			wantDotted:  []string{"http.server.request.duration"},
			wantVectors: 1,
		},
		{
			name:        "inside_aggregation",
			in:          `sum by (le) (rate(http.server.request.duration_bucket[5m]))`,
			wantDotted:  []string{"http.server.request.duration_bucket"},
			wantVectors: 1,
		},
		{
			name:        "binop_both_sides_dotted",
			in:          `a.b.c + x.y.z`,
			wantDotted:  []string{"a.b.c", "x.y.z"},
			wantVectors: 2,
		},
		{
			name:        "histogram_quantile_classic_dotted",
			in:          `histogram_quantile(0.95, sum by (le) (rate(http.server.duration_bucket{job="api"}[5m])))`,
			wantDotted:  []string{"http.server.duration_bucket"},
			wantVectors: 1,
		},
		{
			name:        "dotted_with_label_filter",
			in:          `http.server.request.duration{job="api"}`,
			wantDotted:  []string{"http.server.request.duration"},
			wantVectors: 1,
		},
		{
			name:        "dotted_with_empty_braces",
			in:          `http.server.request.duration{}`,
			wantDotted:  []string{"http.server.request.duration"},
			wantVectors: 1,
		},
		{
			name:        "dotted_with_multi_matcher",
			in:          `http.server.request.duration{job="api",code=~"5.."}`,
			wantDotted:  []string{"http.server.request.duration"},
			wantVectors: 1,
		},
		// noop case — no dot in metric name; the rewritten query must
		// still parse, and the (non-dotted) metric name must appear via
		// either VectorSelector.Name OR a __name__ matcher.
		{
			name:        "noop_no_dots",
			in:          `rate(http_requests_total[5m])`,
			wantDotted:  nil,
			wantVectors: 1,
		},
		// Numeric literal containing a dot is not an identifier-start,
		// so it must round-trip through the parser intact (no spurious
		// rewrite, no parse error).
		{
			name:        "noop_numeric_literal_with_dot",
			in:          `1.5 * rate(x[5m])`,
			wantDotted:  nil,
			wantVectors: 1,
		},
	}

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rewritten := normalizeDottedSelectors(tc.in)
			expr, err := parser.ParseExpr(rewritten)
			if err != nil {
				t.Fatalf("parser rejected rewritten query: in=%q rewritten=%q err=%v",
					tc.in, rewritten, err)
			}
			if expr == nil {
				t.Fatalf("parser returned nil expr for rewritten=%q", rewritten)
			}

			gotDotted := collectMetricNames(expr)
			if len(gotDotted) < tc.wantVectors {
				t.Errorf("VectorSelector count: got %d, want >= %d (rewritten=%q)",
					len(gotDotted), tc.wantVectors, rewritten)
			}
			for _, want := range tc.wantDotted {
				if !containsName(gotDotted, want) {
					t.Errorf("missing __name__ matcher for %q in AST of rewritten=%q; saw=%v",
						want, rewritten, gotDotted)
				}
			}
		})
	}
}

// TestNormalizeDottedSelectors_ParserRejectsRawDotted pins the
// contract that motivates this whole rewrite: the upstream PromQL
// parser, without the rewrite, refuses dotted metric names in
// selector position. If a future upstream bump made the parser
// accept dotted identifiers natively, this test would start failing
// and prompt re-evaluation of whether the rewrite is still needed.
func TestNormalizeDottedSelectors_ParserRejectsRawDotted(t *testing.T) {
	t.Parallel()

	rawDotted := []string{
		`http.server.request.duration`,
		`rate(http.server.request.duration[5m])`,
		`sum by (le) (rate(http.server.duration_bucket[5m]))`,
	}
	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	for _, in := range rawDotted {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, err := parser.ParseExpr(in); err == nil {
				t.Errorf("parser accepted raw dotted name %q; if upstream now supports dotted identifiers natively, the rewrite is obsolete and this test should be re-evaluated", in)
			}
		})
	}
}

// collectMetricNames walks an AST and returns the metric name that
// each VectorSelector resolves to — either via .Name OR via an `=`
// matcher on `__name__`. We honour both spellings because the rewrite
// always emits the `__name__` form but a non-rewritten name like
// `http_requests_total` lands in .Name.
func collectMetricNames(expr promparser.Expr) []string {
	var names []string
	promparser.Inspect(expr, func(n promparser.Node, _ []promparser.Node) error {
		vs, ok := n.(*promparser.VectorSelector)
		if !ok {
			return nil
		}
		// Prefer matcher-form name (the rewrite always emits this).
		for _, m := range vs.LabelMatchers {
			if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
				names = append(names, m.Value)
				return nil
			}
		}
		// Fall back to VectorSelector.Name (untouched native-ASCII
		// metric names land here).
		if vs.Name != "" {
			names = append(names, vs.Name)
		}
		return nil
	})
	return names
}

func containsName(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
