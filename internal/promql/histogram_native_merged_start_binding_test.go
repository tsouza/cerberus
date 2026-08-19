package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// mergedStartRenderSQL is the one token the merged bucket-range start
// contributes to the emitted query: the head of the arrayMin over the
// per-row downscaled offsets that
// expHistogramMergeBucketsBoundsExpr builds. Counting it is how this test
// sees every RENDER of that subtree, whether or not the render is bound.
const mergedStartRenderSQL = "arrayMin(arrayMap((sm, om) -> bitShiftRight(om, "

// mergedStartBindingSQL is the head of the hqLet binding
// expHistogramOverMergedBucketRangeExpr wraps each merged bucket-range in
// — `arrayMap(mst -> …, array(<start>))[1]`. Exactly one binding is opened
// per bucket-merge site, so its count is how many merged starts the query
// contains, independent of how many times each is read.
const mergedStartBindingSQL = "arrayMap(" + paramExpMergedStart + " -> "

// TestExpHistogramMergedStartIsBoundOncePerSite pins the merged bucket
// range's start to ONE render per bucket-merge site.
//
// mergedStart is an arrayMin over a per-row arrayMap — work linear in the
// merged group's rows — and every bucket-merge site reads it from inside
// its own per-target-bucket loop, plus once more for the range length.
// Because a chplan.Expr tree is a DAG in Go but the emitter renders it as
// a TREE (see hqLet), an unbound merged start is a full second copy of
// that arrayMin per reader, evaluated once per target bucket: rows x
// buckets work where rows + buckets suffices. That is the same hazard
// cerberus issue #2267 fixed for this position's arraySort calls, which is
// why the assertion is written as an equality rather than a ceiling —
// every render must be the binding's own, and a reader that re-renders it
// pushes the render count above the binding count.
//
// The queries below cover all four bucket-merge sites: the across-series
// merge (expHistogramMergeBucketsExpr), the window fold
// (expHistogramWindowBucketsExpr), the binop merge
// (histogramBinopMergedBucketsExpr) and the counter-reset mask
// (expHistogramResetPairBucketRegressedExpr) — the last two reached via a
// histogram + histogram binop and via rate() respectively.
func TestExpHistogramMergedStartIsBoundOncePerSite(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	// Any fixed instant/range works — the invariant is about emitted
	// expression structure, not about time. Pinning them keeps the test
	// deterministic.
	var (
		rangeStart = time.Unix(1_700_000_000, 0).UTC()
		rangeEnd   = rangeStart.Add(10 * time.Minute)
		rangeStep  = 30 * time.Second
	)

	cases := []struct {
		name string
		// query reaches at least one exponential-histogram bucket-merge
		// site. Each case names the site (or sites) it is here for.
		query string
		// rangeQuery selects LowerAtRange over Lower, which is what puts
		// the shape through the range-bucket fanout the production
		// `cerb:project;agg=1;rbf` plan shape uses.
		rangeQuery bool
		// sites documents which bucket-merge call site(s) the query
		// exercises, so a failure reads as a claim about a code path.
		sites string
	}{
		{
			name:  "instant_merge_across_series",
			query: `histogram_quantile(0.99, sum(http_server_duration_exp_hist))`,
			sites: "expHistogramMergeBucketsExpr (across-series merge)",
		},
		{
			name:  "instant_rate_window_fold_and_reset_mask",
			query: `histogram_quantile(0.99, sum(rate(http_server_duration_exp_hist[5m])))`,
			sites: "expHistogramWindowBucketsExpr (window fold) + expHistogramResetPairBucketRegressedExpr (counter-reset mask)",
		},
		{
			name:       "range_agg_by_one_label_is_the_production_oom_shape",
			query:      `histogram_quantile(0.99, sum by (http_request_method) (rate(http_server_duration_exp_hist[5m])))`,
			rangeQuery: true,
			sites:      "window fold + reset mask + across-series merge, under the range-bucket fanout",
		},
		{
			name:  "binop_merge",
			query: `http_server_duration_exp_hist + http_server_duration_exp_hist`,
			sites: "histogramBinopMergedBucketsExpr (binop merge)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}

			var plan chplan.Node
			if tc.rangeQuery {
				plan, err = LowerAtRange(context.Background(), expr, s, rangeStart, rangeEnd, rangeStep)
			} else {
				plan, err = Lower(context.Background(), expr, s)
			}
			if err != nil {
				t.Fatalf("lowering %q: %v", tc.query, err)
			}

			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			renders := strings.Count(sql, mergedStartRenderSQL)
			bindings := strings.Count(sql, mergedStartBindingSQL)

			// Guard the guard: a query that fell off the exponential
			// bucket-merge path entirely would satisfy "renders ==
			// bindings" at 0 == 0 for the wrong reason.
			if renders == 0 {
				t.Fatalf("emitted SQL for %q contains no merged bucket-range start — it never reached %s, so the equality below would be vacuous\nSQL: %s", tc.query, tc.sites, sql)
			}

			if renders != bindings {
				t.Errorf("emitted SQL for %q renders the merged bucket-range start %d time(s) but opens only %d binding(s) — every merged start must be bound once by expHistogramOverMergedBucketRangeExpr and read as `%s`, or ClickHouse re-evaluates an arrayMin over the group's rows once per target bucket (%s)\nSQL: %s", tc.query, renders, bindings, paramExpMergedStart, tc.sites, sql)
			}
		})
	}
}
