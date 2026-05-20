package prom

import "testing"

// TestNormalizeDottedSelectors covers the OTel-dotted-name rewrite at
// the unit layer. The handler wires this in front of `parser.ParseExpr`
// so a Grafana query containing `http.server.request.duration` lowers
// the way `{__name__="http.server.request.duration"}` would.
func TestNormalizeDottedSelectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// no rewrite — query contains no dotted identifiers
		{
			name: "noop_no_dots",
			in:   `rate(http_requests_total[5m])`,
			want: `rate(http_requests_total[5m])`,
		},
		{
			name: "noop_numeric_literal_with_dot",
			in:   `1.5 * rate(x[5m])`,
			want: `1.5 * rate(x[5m])`,
		},
		// bare metric name with dots
		{
			name: "bare_dotted_metric",
			in:   `http.server.request.duration`,
			want: `{__name__="http.server.request.duration"}`,
		},
		// dotted metric inside a function call
		{
			name: "inside_rate",
			in:   `rate(http.server.request.duration[5m])`,
			want: `rate({__name__="http.server.request.duration"}[5m])`,
		},
		// dotted metric inside an aggregation
		{
			name: "inside_aggregation",
			in:   `sum by (le) (rate(http.server.request.duration_bucket[5m]))`,
			want: `sum by (le) (rate({__name__="http.server.request.duration_bucket"}[5m]))`,
		},
		// dotted metric on both sides of a binop
		{
			name: "binop_both_sides_dotted",
			in:   `a.b.c + x.y.z`,
			want: `{__name__="a.b.c"} + {__name__="x.y.z"}`,
		},
		// dotted metric with label matcher — Grafana's metric picker
		// drops the user into this shape when they pick a metric AND
		// add a label filter.
		{
			name: "dotted_with_label_filter",
			in:   `http.server.request.duration{job="api"}`,
			want: `{__name__="http.server.request.duration"}{job="api"}`,
		},
		// string content must not be rewritten — a dotted string inside
		// `"..."` is a label-value, not a metric name.
		{
			name: "preserve_string_content",
			in:   `up{job="my.api.service"}`,
			want: `up{job="my.api.service"}`,
		},
		{
			name: "preserve_string_content_with_escape",
			in:   `up{job="my.api.\"escaped\".service"}`,
			want: `up{job="my.api.\"escaped\".service"}`,
		},
		// backtick string — Loki / cerberus harness pattern; the
		// classifier still has to honor it. The parser is PromQL but
		// the function should not corrupt backtick strings.
		{
			name: "preserve_backtick_string",
			in:   "up{job=`my.api.service`}",
			want: "up{job=`my.api.service`}",
		},
		// histogram_quantile classic shape with dotted name (a
		// composition of every code path that the rewrite has to walk
		// past — function call, agg-by, rate, range-vec selector,
		// label matcher).
		{
			name: "histogram_quantile_classic_dotted",
			in:   `histogram_quantile(0.95, sum by (le) (rate(http.server.duration_bucket{job="api"}[5m])))`,
			want: `histogram_quantile(0.95, sum by (le) (rate({__name__="http.server.duration_bucket"}{job="api"}[5m])))`,
		},
		// empty input must round-trip
		{
			name: "empty",
			in:   ``,
			want: ``,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeDottedSelectors(tc.in)
			if got != tc.want {
				t.Errorf("normalizeDottedSelectors(%q):\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeDottedSelectors_Idempotent pins idempotency — running
// the rewrite twice produces the same result as running it once. The
// second pass should see only `{__name__="..."}` form (already inside
// a label-matcher string) and leave it untouched.
func TestNormalizeDottedSelectors_Idempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`rate(http.server.request.duration[5m])`,
		`sum by (job) (a.b + x.y.z)`,
		`up{job="my.api"}`,
		`histogram_quantile(0.95, sum by (le) (rate(http.server.duration_bucket{job="api"}[5m])))`,
	}
	for _, in := range inputs {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			first := normalizeDottedSelectors(in)
			second := normalizeDottedSelectors(first)
			if first != second {
				t.Errorf("not idempotent:\n  in:     %q\n  first:  %q\n  second: %q", in, first, second)
			}
		})
	}
}
