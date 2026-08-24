package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_InfoJoinPreservesSamples pins cerberus issue
// #2509: `info(v)` used to hard-reject any histogram-valued base vector
// via expHistogramSelectorRouting's catch-all — lowerInfo routed arg[0]
// through the generic lower() dispatch with no lowerExpHistogramValuedShape
// check, so a bare exponential-histogram selector (or any other
// histogram-valued shape: sum()/avg(), rate()/increase(), ...) fell
// through to that rejection.
//
// Reference Prometheus's info() (promql/info.go::evalInfo/addToSeries)
// never drops a histogram sample in the base vector — the float-only
// check applies only to the INFO metric's own samples — so it copies
// `sample.H` through unchanged while still joining the target-identifying
// data labels onto it. The fix recognises a histogram-valued base via
// lowerExpHistogramValuedShape and builds a chplan.InfoJoin with its new
// Histogram field set, which widens the emitted join to forward the base
// side's nine Histogram*Column outputs alongside the canonical quartet
// (see chplan.InfoJoin.Histogram's doc comment).
func TestLower_ExpHistogram_InfoJoinPreservesSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		// Bare selector, default target_info.
		`info(latency_exp_hist)`,
		// Bare selector, explicit second-arg name matcher.
		`info(latency_exp_hist, {__name__="build_info"})`,
		// Bare selector, data-label matcher (also exercises DropUnmatched).
		`info(latency_exp_hist, {version=~".+"})`,
		// A histogram-valued AGGREGATE, not a literal selector.
		`info(sum(latency_exp_hist))`,
		`info(avg(latency_exp_hist))`,
		// A histogram-valued RANGE FUNCTION.
		`info(rate(latency_exp_hist[5m]))`,
		`info(increase(latency_exp_hist[5m]))`,
		// A regex name matcher that may select several info metrics —
		// exercises Histogram alongside MergeInfoMetrics.
		`info(latency_exp_hist, {__name__=~".*_info"})`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want histogram — the sample must be joined, not rejected", query, shape)
			}
			join, ok := plan.(*chplan.InfoJoin)
			if !ok {
				t.Fatalf("LowerAt(%q) plan root = %T, want *chplan.InfoJoin", query, plan)
			}
			if !join.Histogram {
				t.Fatalf("LowerAt(%q): InfoJoin.Histogram = false, want true", query)
			}
		})
	}
}

// TestLower_ExpHistogram_InfoIgnoresSelfMatchingBase pins the ignore-set
// carve-out (promql/info.go::evalInfo's ignoreSeries) for a
// histogram-valued base: a base series that is ITSELF the selected info
// metric is passed through unchanged, whatever shape it is — no join, no
// error. staticBaseMetricName resolves a bare exp-histogram selector
// exactly like a float one, so lowerInfo's existing ignore-branch applies
// unmodified once the histogram base lowers correctly.
func TestLower_ExpHistogram_InfoIgnoresSelfMatchingBase(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	expr := parseExprExp(t, `info(latency_exp_hist, {__name__="latency_exp_hist"})`)
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: unexpected error: %v", err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
		t.Fatalf("plan publishes %s, want histogram", shape)
	}
	if _, ok := plan.(*chplan.InfoJoin); ok {
		t.Fatalf("plan root = *chplan.InfoJoin, want the bare base passed through unenriched")
	}
}

// TestLower_ExpHistogram_InfoJoinEmitsHistogramColumns checks the actual
// SQL shape: the base (L) side's nine chplan.Histogram*Column outputs
// must ride through the join under their plain names, both in the plain
// LEFT JOIN case and in the MergeInfoMetrics fold (where they must also
// join the GROUP BY key — a column outside the GROUP BY key and not
// wrapped in an aggregate is a ClickHouse ILLEGAL_AGGREGATION error).
func TestLower_ExpHistogram_InfoJoinEmitsHistogramColumns(t *testing.T) {
	t.Parallel()

	histogramCols := []string{
		"HistogramCount", "HistogramSum", "HistogramScale",
		"HistogramZeroThreshold", "HistogramZeroCount",
		"HistogramPositiveOffset", "HistogramPositiveBucketCounts",
		"HistogramNegativeOffset", "HistogramNegativeBucketCounts",
	}

	t.Run("plain join", func(t *testing.T) {
		t.Parallel()
		sql, _ := emitInfoQuery(t, `info(latency_exp_hist)`)
		for _, col := range histogramCols {
			want := "L.`" + col + "` AS `" + col + "`"
			if !strings.Contains(sql, want) {
				t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
			}
		}
	})

	t.Run("merged info metrics fold", func(t *testing.T) {
		t.Parallel()
		sql, _ := emitInfoQuery(t, `info(latency_exp_hist, {__name__=~".*_info"})`)
		if !strings.Contains(sql, "groupArrayArray(") {
			t.Fatalf("expected the multi-metric fold; full SQL:\n%s", sql)
		}
		for _, col := range histogramCols {
			selectWant := "L.`" + col + "` AS `" + col + "`"
			if !strings.Contains(sql, selectWant) {
				t.Errorf("expected SQL to contain %q; full SQL:\n%s", selectWant, sql)
			}
			groupByWant := "L.`" + col + "`"
			if strings.Count(sql, groupByWant) < 2 {
				t.Errorf("expected %q to appear in both SELECT and GROUP BY; full SQL:\n%s", groupByWant, sql)
			}
		}
	})
}
