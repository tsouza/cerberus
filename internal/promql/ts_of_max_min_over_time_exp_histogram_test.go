package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_TsOfMaxMinOverTimeDropSamples pins cerberus issue
// #2482's ts_of_max_over_time / ts_of_min_over_time half: both dispatch
// through the SAME compareOverTime helper min_over_time / max_over_time
// use, just with returnTimestamp=true (tsouza/prometheus's
// promql/functions.go: funcTsOfMaxOverTime / funcTsOfMinOverTime call
// compareOverTime(..., returnTimestamp: true), and compareOverTime itself
// early-returns `enh.Out, nil` on `len(samples.Floats) == 0` before it
// ever reads samples.Histograms — the identical guard min_over_time /
// max_over_time joined [rangeVectorFloatOnlyDropFuncs] over per cerberus
// issue #2480). A bare exp-histogram matrix selector therefore answers an
// empty vector, matching reference exactly, and previously fell through
// to expHistogramSelectorRouting's catch-all rejection instead.
func TestLower_ExpHistogram_TsOfMaxMinOverTimeDropSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`ts_of_max_over_time(latency_exp_hist[5m])`,
		`ts_of_min_over_time(latency_exp_hist[5m])`,
		// An `@`-pinned broadcast reaches the same drop path — the
		// histogram-selector recognition happens before the grid-shape
		// switch, mirroring the sibling min_over_time/max_over_time
		// coverage in range_vector_float_only_reducer_exp_histogram_test.go.
		`ts_of_max_over_time(latency_exp_hist[5m] @ 1767225601)`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			assertEmptyFloatProjection(t, plan)
		})
	}
}

// TestLower_ExpHistogram_TsOfMaxMinOverTimeRangeMode pins the query_range
// shape for the same two reducers — see
// TestLower_ExpHistogram_FloatOnlyRangeVectorReducersRangeMode for why
// range mode needs its own coverage (LowerAtRange threads a non-zero
// step, which would otherwise route through the gridFanout branch these
// reducers never reach once the histogram-selector check fires first).
func TestLower_ExpHistogram_TsOfMaxMinOverTimeRangeMode(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := time.Minute

	queries := []string{
		`ts_of_max_over_time(latency_exp_hist[5m])`,
		`ts_of_min_over_time(latency_exp_hist[5m])`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", query, err)
			}
			assertEmptyFloatProjection(t, plan)
		})
	}
}
