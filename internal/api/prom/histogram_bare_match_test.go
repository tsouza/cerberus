package prom

import (
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"
)

// TestExpandBareHistogramMatcher pins the contract the labels / series
// metadata handlers rely on for histogram-base lookups: a bare
// VectorSelector naming a metric whose name has no classic-histogram
// companion suffix fans out into three Prom-wire companion variants,
// and any matcher that already carries one of those suffixes (or is
// not a single VectorSelector) passes through unchanged.
//
// Companion: TestConformance_LabelsHistogramBareName in
// conformance_test.go exercises the full HTTP surface; this unit test
// pins the rewriter's per-case behaviour without the handler scaffold.
func TestExpandBareHistogramMatcher(t *testing.T) {
	t.Parallel()

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	const histogramTable = "otel_metrics_histogram"

	cases := []struct {
		name    string
		matcher string
		want    []string
	}{
		// Bare base name in the legacy `name` shape — Grafana's
		// Metrics Explorer renders this when the user clicks a
		// histogram tile and asks for its labels chip.
		{
			name:    "bare_name_short_form",
			matcher: "cerberus_clickhouse_bytes_read",
			want: []string{
				"cerberus_clickhouse_bytes_read",
				"cerberus_clickhouse_bytes_read_bucket",
				"cerberus_clickhouse_bytes_read_count",
				"cerberus_clickhouse_bytes_read_sum",
			},
		},
		// Bare base name with an attached `{…}` matcher group —
		// label predicates carry through unchanged into each variant.
		{
			name:    "bare_name_with_label_predicate",
			matcher: `cerberus_clickhouse_bytes_read{cerberus_ql="promql"}`,
			want: []string{
				`cerberus_clickhouse_bytes_read{cerberus_ql="promql"}`,
				`cerberus_clickhouse_bytes_read_bucket{cerberus_ql="promql"}`,
				`cerberus_clickhouse_bytes_read_count{cerberus_ql="promql"}`,
				`cerberus_clickhouse_bytes_read_sum{cerberus_ql="promql"}`,
			},
		},
		// Explicit `{__name__="…"}` form — the rewriter recognises
		// the quoted-name shape Grafana also sometimes emits.
		{
			name:    "explicit_name_matcher",
			matcher: `{__name__="cerberus_clickhouse_bytes_read"}`,
			want: []string{
				`{__name__="cerberus_clickhouse_bytes_read"}`,
				`{__name__="cerberus_clickhouse_bytes_read_bucket"}`,
				`{__name__="cerberus_clickhouse_bytes_read_count"}`,
				`{__name__="cerberus_clickhouse_bytes_read_sum"}`,
			},
		},
		// Bucket companion: already a histogram-shaped name, no
		// fan-out (the helper short-circuits on the suffix check).
		{
			name:    "bucket_companion_passthrough",
			matcher: "cerberus_clickhouse_bytes_read_bucket",
			want:    []string{"cerberus_clickhouse_bytes_read_bucket"},
		},
		// Count / sum companions also short-circuit.
		{
			name:    "count_companion_passthrough",
			matcher: "cerberus_clickhouse_bytes_read_count",
			want:    []string{"cerberus_clickhouse_bytes_read_count"},
		},
		{
			name:    "sum_companion_passthrough",
			matcher: "cerberus_clickhouse_bytes_read_sum",
			want:    []string{"cerberus_clickhouse_bytes_read_sum"},
		},
		// `_total` is the counter convention — passing it through
		// the histogram fan-out would scan an unrelated table.
		{
			name:    "total_counter_passthrough",
			matcher: "cerberus_queries_total",
			want:    []string{"cerberus_queries_total"},
		},
		// Regex matcher — explicit user intent, the helper leaves
		// the original shape alone.
		{
			name:    "regex_name_matcher_passthrough",
			matcher: `{__name__=~"cerberus_.+"}`,
			want:    []string{`{__name__=~"cerberus_.+"}`},
		},
		// Malformed matcher — parse fails, helper returns the
		// single-element slice and the downstream matcherSQL
		// surfaces the parse error to the client.
		{
			name:    "parse_error_passthrough",
			matcher: "((not_a_selector",
			want:    []string{"((not_a_selector"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := expandBareHistogramMatcher(parser, tc.matcher, histogramTable)
			if !stringSlicesEqual(got, tc.want) {
				t.Fatalf("expandBareHistogramMatcher(%q)\n got:  %v\n want: %v", tc.matcher, got, tc.want)
			}
		})
	}
}

// TestExpandBareHistogramMatcher_NoHistogramTable pins the
// empty-histogram-table short-circuit: deployments that don't configure
// the classic-histogram table must see no fan-out (the lookup would
// scan an absent table and surface a CH error).
func TestExpandBareHistogramMatcher_NoHistogramTable(t *testing.T) {
	t.Parallel()

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	got := expandBareHistogramMatcher(parser, "cerberus_clickhouse_bytes_read", "")
	want := []string{"cerberus_clickhouse_bytes_read"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("expandBareHistogramMatcher with empty HistogramTable\n got:  %v\n want: %v", got, want)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
