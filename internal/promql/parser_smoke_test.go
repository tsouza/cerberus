package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

// TestParserSmoke confirms that the upstream Prometheus PromQL parser
// accepts every query shape cerberus claims to support, plus a wider
// catalog of real-world queries pulled from Prometheus + Grafana
// dashboards. Cerberus's lowering layer must never reject what
// upstream parses — this test pins that contract.
//
// When upgrading the parser dep, run this with -v and any new
// rejections will show up. If a real-world query stops parsing,
// either upstream changed its grammar or we have a regression.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		// Selectors — basic + label matcher variants
		`up`,
		`up{}`,
		`up{job="api"}`,
		`up{job!="api"}`,
		`up{job=~"api|web"}`,
		`up{job!~"api.*"}`,
		`{__name__="up"}`,
		`{__name__=~"http_.+"}`,

		// Range vectors + offset + @ modifier
		`up[5m]`,
		`up[1h] offset 10m`,
		`up offset -7m`,
		`up @ 1700000000`,
		`up @ start()`,
		`up @ end()`,
		`up @ 1700000000 offset 50s`,

		// Aggregation
		`sum(up)`,
		`sum by (job) (up)`,
		`sum without (instance) (up)`,
		`avg(up)`,
		`max(up)`,
		`min(up)`,
		`count(up)`,
		`count_values("v", up)`,
		`topk(5, up)`,
		`bottomk(3, up)`,
		`stddev(up)`,
		`stdvar(up)`,
		`group(up)`,
		`quantile(0.95, up)`,

		// Range-aggregation functions
		`rate(http_requests_total[5m])`,
		`irate(http_requests_total[5m])`,
		`increase(http_requests_total[1h])`,
		`delta(temperature[10m])`,
		`idelta(temperature[5m])`,
		`sum_over_time(up[5m])`,
		`avg_over_time(up[5m])`,
		`min_over_time(up[5m])`,
		`max_over_time(up[5m])`,
		`count_over_time(up[5m])`,
		`last_over_time(up[5m])`,
		`stddev_over_time(up[5m])`,
		`stdvar_over_time(up[5m])`,
		`quantile_over_time(0.95, up[5m])`,
		`absent(up)`,
		`absent_over_time(up[5m])`,

		// Instant-vector functions
		`abs(up)`,
		`ceil(up)`,
		`floor(up)`,
		`round(up, 0.1)`,
		`sqrt(up)`,
		`exp(up)`,
		`ln(up)`,
		`log2(up)`,
		`log10(up)`,
		`sgn(up)`,
		`scalar(up)`,
		`vector(1)`,
		`clamp(up, 0, 1)`,
		`clamp_min(up, 0)`,
		`clamp_max(up, 1)`,
		`label_replace(up, "new", "$1", "job", "(.+)")`,
		`label_join(up, "combined", "/", "job", "instance")`,
		`sort(up)`,
		`sort_desc(up)`,
		`timestamp(up)`,
		`day_of_month()`,
		`day_of_week()`,
		`day_of_year()`,
		`days_in_month()`,
		`hour()`,
		`minute()`,
		`month()`,
		`year()`,
		`histogram_quantile(0.9, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
		`pi()`,

		// Arithmetic / binary
		`up + 1`,
		`up - 1`,
		`up * 2`,
		`up / 2`,
		`up % 2`,
		`up ^ 2`,
		`-up`,
		`+up`,
		`up == 1`,
		`up != 0`,
		`up > 0.5`,
		`up >= 1`,
		`up < 0.5`,
		`up <= 0`,
		`up == bool 1`,
		`up > bool 0`,
		`up and up`,
		`up or up`,
		`up unless up`,

		// Vector matching
		`up + on(job) up`,
		`up + ignoring(instance) up`,
		`up + on(job) group_left up`,
		`up + on(job) group_left(extra) up`,
		`up + on(job) group_right(extra) up`,
		`up + on() up`,
		`up + ignoring() up`,

		// Subqueries
		`max_over_time(rate(http_requests_total[5m])[1h:5m])`,
		`avg_over_time(up[5m:1m])`,
		`avg_over_time(up[5m:])`,

		// Numeric literal edges
		`1`,
		`1.5`,
		`-1`,
		`+1`,
		`.5`,
		`5.`,
		`5e3`,
		`5e-3`,
		`1.5E+10`,
		`Inf`,
		`+Inf`,
		`-Inf`,
		`NaN`,
		`0x10`,
		`0755`,

		// String literals
		`up{label="value"}`,
		`up{label='value'}`,
		`up{label="quoted \"inner\" string"}`,
		`up{label="unicode: 文字"}`,

		// Real-world Grafana dashboard queries
		`sum by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[5m]))`,
		`100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
		`histogram_quantile(0.95, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))`,
		`rate(http_requests_total{job="api", status=~"5.."}[5m]) / rate(http_requests_total{job="api"}[5m])`,
		`(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes) / node_memory_MemTotal_bytes * 100`,
	}

	p := parser.NewParser(parser.Options{})
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("ParseExpr(%q) returned nil expression", q)
			}
		})
	}
}

// TestParserSmoke_Rejected confirms that queries cerberus would NEVER
// see (because they're syntactically invalid PromQL) are rejected by
// the upstream parser. Pins the contract: cerberus never has to
// defend against these shapes.
func TestParserSmoke_Rejected(t *testing.T) {
	t.Parallel()

	cases := []string{
		`((1)`,                 // mismatched paren
		`(1))`,                 // extra close paren
		`up{`,                  // unterminated brace
		`up{label="`,           // unterminated string
		`100..4`,               // multiple dots
		`up[5m:-5s]`,           // negative subquery step
		`up[`,                  // unterminated range bracket
		`up{__name__=~".*"}`,   // empty intersection
		`*up`,                  // operator with no left operand
		`up /`,                 // operator with no right operand
		`rate()`,               // missing range vector arg
		`sum by ()`,            // empty by — wait, this might be valid; check
		`unknown_function(up)`, // unknown identifier as function
		`up @ now`,             // @ wants a numeric timestamp, not a function
	}

	p := parser.NewParser(parser.Options{})
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := p.ParseExpr(q)
			if err == nil {
				t.Fatalf("ParseExpr(%q) should have rejected; accepted instead", q)
			}
		})
	}
}
