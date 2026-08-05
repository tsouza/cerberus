package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramQuantile_UnpinnedNameUsesBucketWireName pins the
// fix for the `compatibility/prometheus` divergence on
//
//	histogram_quantile(0.9, {__name__=~"demo_api_request_duration_seconds_.+"})
//
// Reference Prometheus resolves that selector against the WIRE names a
// classic histogram exposes — `<base>_bucket`, `<base>_count`,
// `<base>_sum` — then drops every input series without an `le` label,
// leaving the bucket ladder and a real quantile. Cerberus applied the
// regex to the BARE stored MetricName (`demo_api_request_duration_seconds`),
// which the pattern's mandatory `_.+` tail cannot match, so the filter
// selected zero rows and the query answered the empty vector.
//
// stripBucketSuffix only ever rewrote `__name__=` (MatchEqual) matchers,
// so it could not carry an unpinned matcher across the wire-name gap.
// histogramQuantileMatcherPredicate evaluates unpinned `__name__`
// matchers against the synthetic `concat(MetricName, '_bucket')`
// expression instead.
//
// The test asserts the emitted SQL, not the plan, because the emitter is
// where a missing synthetic-name arm becomes an unmatchable WHERE.
func TestLower_HistogramQuantile_UnpinnedNameUsesBucketWireName(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// bucketWireLiteral is the inlined synthetic suffix the name
	// expression concatenates onto the stored MetricName.
	const bucketWireLiteral = "'_bucket'"
	const namePattern = "demo_api_request_duration_seconds_.+"

	cases := []struct {
		name  string
		query string
		// step 0 == instant mode, > 0 == range mode. Both lower through
		// their own classic-histogram entry point, and the pre-fix bug
		// reproduced on both.
		step time.Duration
		// wantWireName is false for the pinned `__name__=` control,
		// which must keep emitting the bare-column predicate.
		wantWireName bool
	}{
		{
			name:         "regex_bare_instant",
			query:        `histogram_quantile(0.9, {__name__=~"` + namePattern + `"})`,
			wantWireName: true,
		},
		{
			name:         "regex_bare_range",
			query:        `histogram_quantile(0.9, {__name__=~"` + namePattern + `"})`,
			step:         30 * time.Second,
			wantWireName: true,
		},
		{
			name:         "regex_agg_instant",
			query:        `histogram_quantile(0.9, sum by (le) (rate({__name__=~"` + namePattern + `"}[5m])))`,
			wantWireName: true,
		},
		{
			name:         "regex_agg_range",
			query:        `histogram_quantile(0.9, sum by (le) (rate({__name__=~"` + namePattern + `"}[5m])))`,
			step:         30 * time.Second,
			wantWireName: true,
		},
		{
			// PromQL rejects a selector whose every matcher can match the
			// empty string, so the negated arm is paired with a positive
			// one — which is also the realistic "everything but X" shape.
			name:         "negated_regex_bare_instant",
			query:        `histogram_quantile(0.9, {__name__=~"demo_.+", __name__!~"` + namePattern + `"})`,
			wantWireName: true,
		},
		{
			name:         "pinned_equal_bare_instant_keeps_bare_column",
			query:        `histogram_quantile(0.9, demo_api_request_duration_seconds_bucket)`,
			wantWireName: false,
		},
		{
			name:         "pinned_equal_agg_range_keeps_bare_column",
			query:        `histogram_quantile(0.9, sum by (le) (rate(demo_api_request_duration_seconds_bucket[5m])))`,
			step:         30 * time.Second,
			wantWireName: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			var plan chplan.Node
			if tc.step > 0 {
				start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				plan, err = promql.LowerAtRange(context.Background(), expr, s, start, start.Add(time.Hour), tc.step)
			} else {
				plan, err = promql.Lower(context.Background(), expr, s)
			}
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}

			if !strings.Contains(sql, "`"+s.HistogramTable+"`") {
				t.Fatalf("emitted SQL does not target classic-histogram table %q — "+
					"the selector was misrouted away from the bucket ladder.\nfull SQL:\n%s",
					s.HistogramTable, sql)
			}

			gotWireName := strings.Contains(sql, bucketWireLiteral)
			if gotWireName != tc.wantWireName {
				if tc.wantWireName {
					t.Fatalf("emitted SQL has no %s wire-name arm — an unpinned `__name__` matcher "+
						"tests the Prometheus wire name `<base>_bucket`, which no OTel-CH column holds; "+
						"filtering the bare stored name matches zero rows and the query answers the "+
						"empty vector where reference Prometheus answers a quantile.\nfull SQL:\n%s",
						bucketWireLiteral, sql)
				}
				t.Fatalf("emitted SQL grew a %s wire-name arm for a PINNED `__name__=` matcher — "+
					"the pinned path must keep filtering the bare MetricName column that "+
					"stripBucketSuffix produces.\nfull SQL:\n%s", bucketWireLiteral, sql)
			}

			if !tc.wantWireName {
				return
			}
			// The bound pattern is anchored with `^(?:...)$` (issue
			// #1741 — ClickHouse's match() is a substring search, but
			// Prometheus `__name__` regex matchers are always fully
			// anchored), so the arg carries the wrapped form rather
			// than the bare namePattern.
			wantArg := "^(?:" + namePattern + ")$"
			var sawPattern bool
			for _, a := range args {
				if str, ok := a.(string); ok && str == wantArg {
					sawPattern = true
					break
				}
			}
			if !sawPattern {
				t.Fatalf("emitted SQL params do not carry the anchored `__name__` regex %q — "+
					"the matcher was dropped rather than routed to the wire name.\nargs: %v",
					wantArg, args)
			}
		})
	}
}
