package prom

import (
	"slices"
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"
)

// TestExpandUnderscoredMetricNameMatcher pins the catalog-side metric-
// name fan-out. Each case exercises one of the three shapes the
// helper must handle:
//
//   - `{__name__="<underscored>"}` form (what Drilldown-Metrics sends)
//   - bare `<underscored>` form (what `normalizeDottedSelectors`
//     would have produced for a Prom-grammar name that doesn't need
//     rewriting)
//   - underscored input with no rewritable underscore (`up`) — single-
//     element passthrough, byte-stable with the pre-fan-out callers.
func TestExpandUnderscoredMetricNameMatcher(t *testing.T) {
	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		matcher string
		// wantContains lists matcher strings the result MUST contain.
		// We check membership rather than full equality so the
		// powerset's stable-but-non-trivial ordering doesn't pin the
		// test to a specific candidate sequence.
		wantContains []string
		wantNoFanout bool
	}{
		{
			name:    "name_with_underscores_explicit_form",
			matcher: `{__name__="http_server_request_body_size"}`,
			wantContains: []string{
				`{__name__="http_server_request_body_size"}`,
				`{__name__="http.server.request.body.size"}`,
			},
		},
		{
			name:    "name_with_underscores_bare_form",
			matcher: `http_server_request_body_size`,
			wantContains: []string{
				`http_server_request_body_size`,
				`{__name__="http.server.request.body.size"}`,
			},
		},
		{
			name:    "name_with_underscores_explicit_form_with_label",
			matcher: `{__name__="http_server_request_body_size",job="api"}`,
			wantContains: []string{
				`{__name__="http_server_request_body_size",job="api"}`,
				`{__name__="http.server.request.body.size",job="api"}`,
			},
		},
		{
			name:    "single_word_no_fanout",
			matcher: `up`,
			wantContains: []string{
				`up`,
			},
			wantNoFanout: true,
		},
		{
			name:    "single_underscore_at_boundary_no_fanout",
			matcher: `_up`,
			wantContains: []string{
				`_up`,
			},
			wantNoFanout: true,
		},
		{
			name:    "synthetic_label_match_target_no_fanout",
			matcher: `{__name__="__address__"}`,
			wantContains: []string{
				`{__name__="__address__"}`,
			},
			wantNoFanout: true,
		},
		// Histogram-companion-suffixed names short-circuit so the
		// per-suffix lowering paths inside lowerVectorSelector handle
		// the OTel-CH histogram column projection. Mirrors
		// expandBareHistogramMatcher's suffix short-circuit.
		{
			name:    "bucket_suffix_no_fanout",
			matcher: `cerberus_clickhouse_bytes_read_bucket`,
			wantContains: []string{
				`cerberus_clickhouse_bytes_read_bucket`,
			},
			wantNoFanout: true,
		},
		{
			name:    "count_suffix_no_fanout",
			matcher: `{__name__="cerberus_queries_total"}`,
			wantContains: []string{
				`{__name__="cerberus_queries_total"}`,
			},
			wantNoFanout: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expandUnderscoredMetricNameMatcher(parser, c.matcher)
			if c.wantNoFanout && len(got) != 1 {
				t.Fatalf("want single-element passthrough, got %d variants: %v", len(got), got)
			}
			for _, want := range c.wantContains {
				if !slices.Contains(got, want) {
					t.Errorf("variant %q missing from %v", want, got)
				}
			}
			// First element is always the verbatim matcher (callers
			// rely on this for the original-shape arm).
			if got[0] != c.matcher {
				t.Errorf("first variant must be the input matcher, got %q want %q", got[0], c.matcher)
			}
		})
	}
}
