package promql_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestCallSubqueryFanout_KeyCarriesSeriesIdentity pins the group key the
// doubly-nested call-subquery fanout reduces a NAME-PRESERVING outer
// function under (cerberus issue #3240), and pins it at the plan level
// because no query can distinguish the right key from the wrong one today.
//
// Reference folds per series and keys its output on `Metric.Hash()`, so
// two series sharing their attributes and differing only in `__name__` are
// two output series; an Attributes-only key merges them and publishes
// whichever name the argMax/argMin pick selected, deleting the other
// series. The Mixed row shape's half of that is asserted at the wire by
// internal/api/prom's
// TestQuery_CallSubqueryNamePreserving_KeepsBothSeries_ChDB.
//
// This test asserts the key on the lowered plan rather than on the wire,
// so it runs on every lane instead of only the chdb-tagged ones, and so a
// key that regressed would be named as a key rather than as a series
// count. Both queries are MixedRowShape: measured with a lowering probe,
// a HistogramRowShape input never reaches this composition at all — every
// `and`/`or` of two exp-histogram arms is intercepted by the singly-nested
// continuations first (tsouza/cerberus#3253), which is also why the
// dispatcher's own `shape == HistogramRowShape` arms are unreached today.
func TestCallSubqueryFanout_KeyCarriesSeriesIdentity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 12, 0, 0, time.UTC)

	cases := []struct {
		name    string
		query   string
		aliases []string
	}{
		{
			// last_over_time PRESERVES `__name__`, so the reduction must
			// carry it in the key rather than argMax-pick it out of a
			// merged group.
			name:    "name-preserving keys on Attributes and MetricName",
			query:   "last_over_time(((latency_exp_hist) or (latency_float))[3m:1m])[4m:1m]",
			aliases: []string{s.AttributesColumn, s.MetricNameColumn},
		},
		{
			// count_over_time DROPS it, and must keep the narrower key —
			// widening it would make the distinct-`__name__` count that
			// raises reference's duplicate-labelset error always answer 1.
			name:    "name-dropping keys on Attributes alone",
			query:   "count_over_time(((latency_exp_hist) or (latency_float))[3m:1m])[4m:1m]",
			aliases: []string{s.AttributesColumn},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := outerRangeFanoutAliases(t, tc.query, s, evalTS)
			if !reflect.DeepEqual(got, tc.aliases) {
				t.Fatalf("%s: outer-range fanout group key = %v, want %v", tc.query, got, tc.aliases)
			}
		})
	}
}

// outerRangeFanoutAliases lowers query and returns the GroupByAliases of
// the one RangeBucketFanout running in OuterRange mode — the doubly-nested
// composition's own grid, as opposed to the ambient-grid fanouts the inner
// set-op's arms each build.
func outerRangeFanoutAliases(t *testing.T, query string, s schema.Metrics, at time.Time) []string {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	var found [][]string
	chplan.Walk(plan, func(n chplan.Node) bool {
		if fanout, ok := n.(*chplan.RangeBucketFanout); ok && fanout.OuterRange != 0 {
			found = append(found, fanout.GroupByAliases)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%s: found %d OuterRange-mode RangeBucketFanout(s), want exactly 1 — "+
			"this composition builds one outer grid", query, len(found))
	}
	return found[0]
}
