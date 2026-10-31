package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	settingAdaptiveAggregator = "enable_adaptive_aggregator"
	// adaptiveAggregatorOff is the literal value the #3986 measurement
	// validated, written out so a rule that stamped 1 (a no-op) would fail.
	adaptiveAggregatorOff = 0

	adaptiveExpMetric     = "svc_http_request_duration_seconds_native_exp_hist"
	adaptiveClassicMetric = "svc_http_request_duration_seconds_bucket"
)

var (
	adaptiveServer268 = chopt.Version{Major: 26, Minor: 8, Patch: 36}
	adaptiveServer266 = chopt.Version{Major: 26, Minor: 6, Patch: 8}
	adaptiveEvalEnd   = time.Unix(1787044440, 0).UTC()
)

// adaptiveDispatch resolves the auto selection on server and returns the
// boot wiring cmd/cerberus builds from it: the lowerer table and the
// settings rules.
func adaptiveDispatch(t *testing.T, server chopt.Version) (promql.RangeLowerers, engine.SettingsRules) {
	t.Helper()
	set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Capability: chopt.CapabilityAvailable}, server)
	if err != nil {
		t.Fatalf("resolve auto on %s: %v", server, err)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	return choptwire.RangeLowerers(set), rules
}

// lowerPromQLWith lowers q through lowerers, as an instant query at
// adaptiveEvalEnd or as a one-hour query_range at a one-minute step.
func lowerPromQLWith(t *testing.T, lowerers promql.RangeLowerers, q string, rangeQuery bool) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	start, step := adaptiveEvalEnd, time.Duration(0)
	if rangeQuery {
		start, step = adaptiveEvalEnd.Add(-time.Hour), time.Minute
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), start, adaptiveEvalEnd, step, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	return plan
}

// TestHistogramAdaptiveAggregatorOff_RealLowerings runs the
// enable_adaptive_aggregator=0 rule over what production lowers on a 26.8
// server: the auto selection, choptwire's lowerer table and settings rules,
// and the full shared settings seam. Exponential-histogram counter windows
// are stamped; classic windows and merges, bare-selector lookbacks, scalar
// windows, and any statement mixing one of them with a counter window are
// not, because the setting covers every subquery (#3986).
func TestHistogramAdaptiveAggregatorOff_RealLowerings(t *testing.T) {
	lowerers, rules := adaptiveDispatch(t, adaptiveServer268)
	if !rules.HistogramAdaptiveAggregatorOff {
		t.Fatalf("auto selection on %s leaves HistogramAdaptiveAggregatorOff off", adaptiveServer268)
	}

	plural := `histogram_quantiles(sum by (http_route) (rate(` + adaptiveExpMetric + `[5m])), "quantile", 0.5, 0.9, 0.95)`
	single := `histogram_quantile(0.95, sum by (http_route) (rate(` + adaptiveExpMetric + `[5m])))`
	classicRate := `histogram_quantile(0.95, sum by (http_route, le) (rate(` + adaptiveClassicMetric + `[5m])))`
	classicMerge := `histogram_quantile(0.95, sum by (http_route, le) (` + adaptiveClassicMetric + `))`
	bareExp := `histogram_quantile(0.9, sum by (http_route) (` + adaptiveExpMetric + `))`

	tests := []struct {
		name    string
		query   string
		stamped bool
	}{
		{"plural quantile over rate", plural, true},
		{"single quantile over rate", single, true},
		{"quantile over increase", `histogram_quantile(0.5, sum by (http_route) (increase(` + adaptiveExpMetric + `[5m])))`, true},
		{"quantile over irate", `histogram_quantile(0.5, sum by (http_route) (irate(` + adaptiveExpMetric + `[5m])))`, true},
		{"two counter windows", single + ` / on(http_route) ` + plural, true},
		{"classic quantile over rate", classicRate, false},
		{"classic merge", classicMerge, false},
		{"bare exponential selector", bareExp, false},
		{"exp sum_over_time", `histogram_quantile(0.9, sum by (http_route) (sum_over_time(` + adaptiveExpMetric + `[5m])))`, false},
		{"scalar rate", `sum by (job) (rate(http_requests_total[5m]))`, false},
		{"classic merge beside a counter window", classicMerge + ` / on(http_route) ` + single, false},
		{"bare exponential selector beside a counter window", bareExp + ` / on(http_route) ` + single, false},
		{"scalar rate beside a counter window", `rate(http_requests_total[5m]) and on() count(histogram_count(rate(` + adaptiveExpMetric + `[5m])))`, false},
	}
	for _, tc := range tests {
		for _, rangeQuery := range []bool{false, true} {
			mode := "instant"
			if rangeQuery {
				mode = "range"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				got := engine.SharedQuerySettingsForTest(rules, lowerPromQLWith(t, lowerers, tc.query, rangeQuery))[settingAdaptiveAggregator]
				if tc.stamped && got != adaptiveAggregatorOff {
					t.Errorf("%s = %v; want %v", settingAdaptiveAggregator, got, adaptiveAggregatorOff)
				}
				if !tc.stamped && got != nil {
					t.Errorf("%s = %v; want absent", settingAdaptiveAggregator, got)
				}
			})
		}
	}
}

// TestHistogramAdaptiveAggregatorOff_RealLoweringBelow268StampsNothing pins
// the version gate end to end: on 26.6 the auto selection leaves the feature
// out, so a counter window carries no setting a 26.6 server would reject.
func TestHistogramAdaptiveAggregatorOff_RealLoweringBelow268StampsNothing(t *testing.T) {
	lowerers, rules := adaptiveDispatch(t, adaptiveServer266)
	q := `histogram_quantile(0.95, sum by (http_route) (rate(` + adaptiveExpMetric + `[5m])))`
	if got := engine.SharedQuerySettingsForTest(rules, lowerPromQLWith(t, lowerers, q, false))[settingAdaptiveAggregator]; got != nil {
		t.Errorf("%s on %s = %v; want absent", settingAdaptiveAggregator, adaptiveServer266, got)
	}
}
