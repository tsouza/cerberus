package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	settingTwoLevelThresholdBytes = "group_by_two_level_threshold_bytes"
	// classicLayoutTwoLevelBytes is the literal threshold the replays
	// validated (8 MiB), written out so a rule that stamped 0 (which disables
	// the threshold) or 1 (which converts a small metric's aggregate at once,
	// the fixed cost #4532 measured) would fail.
	classicLayoutTwoLevelBytes = 8388608
)

var (
	layoutServer263 = chopt.Version{Major: 26, Minor: 3, Patch: 17}
	layoutServer267 = chopt.Version{Major: 26, Minor: 7, Patch: 38}
	layoutServer248 = chopt.Version{Major: 24, Minor: 8, Patch: 14}
)

// lowerWithStaleMarkers lowers q as lowerPromQLWith does, against a metrics
// schema whose Flags column was probed, which is what makes a classic
// `_bucket` selection carry the stale-marker layout.
func lowerWithStaleMarkers(t *testing.T, lowerers promql.RangeLowerers, q string, rangeQuery bool) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	metrics := schema.DefaultOTelMetrics()
	metrics.FlagsColumnProbed = true
	start, step := adaptiveEvalEnd, time.Duration(0)
	if rangeQuery {
		start, step = adaptiveEvalEnd.Add(-time.Hour), time.Minute
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, metrics, start, adaptiveEvalEnd, step, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	return plan
}

// TestClassicBucketLayoutTwoLevel_RealLowerings runs the two-level rule over
// what production lowers on servers inside the feature's auto range: a
// classic `_bucket` selection, alone or under an aggregation, carries the
// stale-marker layout and is stamped; plans without the layout are not.
func TestClassicBucketLayoutTwoLevel_RealLowerings(t *testing.T) {
	bucket := `http_server_request_duration_bucket`
	tests := []struct {
		name    string
		query   string
		stamped bool
	}{
		{"bucket selection", bucket + `{method="POST"}`, true},
		{"sum by le over buckets", `sum by (le) (` + bucket + `)`, true},
		{"count over buckets", `count(` + bucket + `)`, true},
		{"classic quantile over a scaled bucket selection", `histogram_quantile(0.9, ` + bucket + ` * 2)`, true},
		{"classic quantile over a bucket merge", `histogram_quantile(0.95, sum by (le) (` + bucket + `))`, false},
		{"classic quantile over a bucket rate", `histogram_quantile(0.95, sum by (le) (rate(` + bucket + `[5m])))`, false},
		{"scalar counter rate", `sum by (job) (rate(http_requests_total[5m]))`, false},
		{"gauge selection", `node_memory_bytes{instance="a"}`, false},
	}
	for _, server := range []chopt.Version{layoutServer263, layoutServer267} {
		lowerers, rules := adaptiveDispatch(t, server)
		if !rules.ClassicBucketLayoutTwoLevel {
			t.Fatalf("auto selection on %s leaves ClassicBucketLayoutTwoLevel off", server)
		}
		for _, tc := range tests {
			for _, rangeQuery := range []bool{false, true} {
				mode := "instant"
				if rangeQuery {
					mode = "range"
				}
				t.Run(server.String()+"/"+tc.name+"/"+mode, func(t *testing.T) {
					got := engine.SharedQuerySettingsForTest(rules, lowerWithStaleMarkers(t, lowerers, tc.query, rangeQuery))[settingTwoLevelThresholdBytes]
					if tc.stamped && got != classicLayoutTwoLevelBytes {
						t.Errorf("%s = %v; want %v", settingTwoLevelThresholdBytes, got, classicLayoutTwoLevelBytes)
					}
					if !tc.stamped && got != nil {
						t.Errorf("%s = %v; want absent", settingTwoLevelThresholdBytes, got)
					}
				})
			}
		}
	}
}

// TestClassicBucketLayoutTwoLevel_OutsideTheAutoRangeStampsNothing pins the
// auto range end to end: on 26.8, where the stamp still cut the layout's peak
// but the instant selection ran slower stamped (264 -> 317 ms on 26.8.36.3),
// and on 24.8, below the measured lines, the auto selection leaves the
// feature out and a bucket selection carries no stamp.
func TestClassicBucketLayoutTwoLevel_OutsideTheAutoRangeStampsNothing(t *testing.T) {
	for _, server := range []chopt.Version{adaptiveServer268, layoutServer248} {
		lowerers, rules := adaptiveDispatch(t, server)
		if rules.ClassicBucketLayoutTwoLevel {
			t.Fatalf("auto selection on %s turns ClassicBucketLayoutTwoLevel on", server)
		}
		q := `sum by (le) (http_server_request_duration_bucket)`
		if got := engine.SharedQuerySettingsForTest(rules, lowerWithStaleMarkers(t, lowerers, q, false))[settingTwoLevelThresholdBytes]; got != nil {
			t.Errorf("%s on %s = %v; want absent", settingTwoLevelThresholdBytes, server, got)
		}
	}
}
