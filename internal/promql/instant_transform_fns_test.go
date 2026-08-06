package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// instantTransformSubqueryProbe maps every member of
// [instantTransformFns] to a representative subquery that exercises it in
// subquery-inner position. The map is asserted to be exactly the table's
// key set, so a name added to the table without a probe fails here rather
// than shipping unexercised: the membership rule is a claim about
// LOWERABILITY, and a table entry cerberus cannot actually lower would
// turn a clean rejection into a SQL-time failure.
var instantTransformSubqueryProbe = map[string]string{
	// Unary math + clamp/round.
	"abs":       `max_over_time(abs(temperature)[5m:1m])`,
	"ceil":      `max_over_time(ceil(temperature)[5m:1m])`,
	"floor":     `max_over_time(floor(temperature)[5m:1m])`,
	"round":     `max_over_time(round(temperature, 2)[5m:1m])`,
	"sqrt":      `max_over_time(sqrt(temperature)[5m:1m])`,
	"exp":       `max_over_time(exp(temperature)[5m:1m])`,
	"ln":        `max_over_time(ln(temperature)[5m:1m])`,
	"log2":      `max_over_time(log2(temperature)[5m:1m])`,
	"log10":     `max_over_time(log10(temperature)[5m:1m])`,
	"sgn":       `max_over_time(sgn(temperature)[5m:1m])`,
	"clamp":     `max_over_time(clamp(temperature, 0, 10)[5m:1m])`,
	"clamp_min": `max_over_time(clamp_min(temperature, 0)[5m:1m])`,
	"clamp_max": `max_over_time(clamp_max(temperature, 10)[5m:1m])`,

	// Trigonometric family + degree/radian conversion.
	"acos":  `max_over_time(acos(temperature)[5m:1m])`,
	"acosh": `max_over_time(acosh(temperature)[5m:1m])`,
	"asin":  `max_over_time(asin(temperature)[5m:1m])`,
	"asinh": `max_over_time(asinh(temperature)[5m:1m])`,
	"atan":  `max_over_time(atan(temperature)[5m:1m])`,
	"atanh": `max_over_time(atanh(temperature)[5m:1m])`,
	"cos":   `max_over_time(cos(temperature)[5m:1m])`,
	"cosh":  `max_over_time(cosh(temperature)[5m:1m])`,
	"sin":   `max_over_time(sin(temperature)[5m:1m])`,
	"sinh":  `max_over_time(sinh(temperature)[5m:1m])`,
	"tan":   `max_over_time(tan(temperature)[5m:1m])`,
	"tanh":  `max_over_time(tanh(temperature)[5m:1m])`,
	"deg":   `max_over_time(deg(temperature)[5m:1m])`,
	"rad":   `max_over_time(rad(temperature)[5m:1m])`,

	// Label rewrites.
	"label_replace": `max_over_time(label_replace(temperature, "copy", "$1", "host", "(.*)")[5m:1m])`,
	"label_join":    `max_over_time(label_join(temperature, "joined", "-", "host")[5m:1m])`,

	// Date components + timestamp, one-argument form.
	"year":          `max_over_time(year(temperature)[5m:1m])`,
	"month":         `max_over_time(month(temperature)[5m:1m])`,
	"day_of_month":  `max_over_time(day_of_month(temperature)[5m:1m])`,
	"day_of_week":   `max_over_time(day_of_week(temperature)[5m:1m])`,
	"day_of_year":   `max_over_time(day_of_year(temperature)[5m:1m])`,
	"days_in_month": `max_over_time(days_in_month(temperature)[5m:1m])`,
	"hour":          `max_over_time(hour(temperature)[5m:1m])`,
	"minute":        `max_over_time(minute(temperature)[5m:1m])`,
	"timestamp":     `max_over_time(timestamp(temperature)[5m:1m])`,

	// Sorting.
	"sort":               `max_over_time(sort(temperature)[5m:1m])`,
	"sort_desc":          `max_over_time(sort_desc(temperature)[5m:1m])`,
	"sort_by_label":      `max_over_time(sort_by_label(temperature, "host")[5m:1m])`,
	"sort_by_label_desc": `max_over_time(sort_by_label_desc(temperature, "host")[5m:1m])`,

	// Native-histogram value accessors.
	"histogram_count":    `max_over_time(histogram_count(latency_exp_hist)[5m:1m])`,
	"histogram_sum":      `max_over_time(histogram_sum(latency_exp_hist)[5m:1m])`,
	"histogram_avg":      `max_over_time(histogram_avg(latency_exp_hist)[5m:1m])`,
	"histogram_stddev":   `max_over_time(histogram_stddev(latency_exp_hist)[5m:1m])`,
	"histogram_stdvar":   `max_over_time(histogram_stdvar(latency_exp_hist)[5m:1m])`,
	"histogram_fraction": `max_over_time(histogram_fraction(0, 1, latency_exp_hist)[5m:1m])`,
}

// TestInstantTransformFnsAreAllLowerable walks the whole
// [instantTransformFns] table and lowers a representative subquery for
// each member, in both instant and range mode. It is the audit leg of the
// membership rule: admitting a name is a promise that cerberus can
// actually lower that call under a subquery, and this is what keeps the
// promise honest as the table grows.
func TestInstantTransformFnsAreAllLowerable(t *testing.T) {
	t.Parallel()

	for name := range instantTransformFns {
		if _, ok := instantTransformSubqueryProbe[name]; !ok {
			t.Errorf("instantTransformFns member %q has no probe query in instantTransformSubqueryProbe", name)
		}
	}
	for name := range instantTransformSubqueryProbe {
		if _, ok := instantTransformFns[name]; !ok {
			t.Errorf("probe query for %q but %q is not in instantTransformFns", name, name)
		}
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	const step = time.Minute

	for name, query := range instantTransformSubqueryProbe {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			if _, err := LowerAt(context.Background(), expr, s, start, end); err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if _, err := LowerAtRange(context.Background(), expr, s, start, end, step); err != nil {
				t.Fatalf("LowerAtRange(%q): %v", query, err)
			}
		})
	}
}

// TestInstantTransformCallRejectsZeroArgForms pins the arity half of
// [isInstantTransformCall]: the zero-argument date forms share a name
// with an admitted member but synthesise an anchor-stamped row instead of
// mapping one, so they must not ride the Identity wrap.
func TestInstantTransformCallRejectsZeroArgForms(t *testing.T) {
	t.Parallel()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for _, name := range []string{
		"year", "month", "day_of_month", "day_of_week",
		"day_of_year", "days_in_month", "hour", "minute",
	} {
		query := "max_over_time(" + name + "()[5m:1m])"
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		if _, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics()); err == nil {
			t.Errorf("Lower(%q): expected a rejection for the anchor-synthesising zero-arg form, got nil", query)
		}
	}
}
