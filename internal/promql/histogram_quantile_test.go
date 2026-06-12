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

// TestLower_HistogramQuantile_Classic asserts the lowering shape for the
// classic-histogram path: histogram_quantile(phi, <VectorSelector>)
// produces a Project(HistogramQuantile(Scan|Filter, ...)) tree targeting
// the OTel-CH histogram table directly (no `_bucket` heuristic).
func TestLower_HistogramQuantile_Classic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name      string
		query     string
		wantPhi   float64
		wantTable string
	}{
		{
			name:      "p50 bare selector",
			query:     `histogram_quantile(0.5, http_server_request_duration)`,
			wantPhi:   0.5,
			wantTable: s.HistogramTable,
		},
		{
			name:      "p99 with label matcher",
			query:     `histogram_quantile(0.99, http_server_request_duration{job="api"})`,
			wantPhi:   0.99,
			wantTable: s.HistogramTable,
		},
		{
			name:      "phi=1 boundary",
			query:     `histogram_quantile(1, http_server_request_duration)`,
			wantPhi:   1,
			wantTable: s.HistogramTable,
		},
		{
			name:      "phi=0 boundary",
			query:     `histogram_quantile(0, http_server_request_duration)`,
			wantPhi:   0,
			wantTable: s.HistogramTable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			pj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("want top-level *chplan.Project, got %T", plan)
			}
			hq, ok := pj.Input.(*chplan.HistogramQuantile)
			if !ok {
				t.Fatalf("want Project.Input = *chplan.HistogramQuantile, got %T", pj.Input)
			}
			if hq.Phi != tc.wantPhi {
				t.Errorf("Phi = %v, want %v", hq.Phi, tc.wantPhi)
			}
			if hq.BucketCountsColumn != s.BucketCountsColumn {
				t.Errorf("BucketCountsColumn = %q, want %q", hq.BucketCountsColumn, s.BucketCountsColumn)
			}
			if hq.ExplicitBoundsColumn != s.ExplicitBoundsColumn {
				t.Errorf("ExplicitBoundsColumn = %q, want %q", hq.ExplicitBoundsColumn, s.ExplicitBoundsColumn)
			}
			// Walk to find the Scan and assert the target table is the
			// classic-histogram table (not the SumTable or GaugeTable).
			var scan *chplan.Scan
			chplan.Walk(hq.Input, func(n chplan.Node) bool {
				if sc, ok := n.(*chplan.Scan); ok {
					scan = sc
					return false
				}
				return true
			})
			if scan == nil {
				t.Fatalf("no Scan node under HistogramQuantile.Input")
			}
			if scan.Table != tc.wantTable {
				t.Errorf("Scan.Table = %q, want %q", scan.Table, tc.wantTable)
			}
		})
	}
}

// TestLower_HistogramQuantile_OverAggregation locks the chplan shape
// for the canonical Prom idiom `histogram_quantile(phi, sum by(le)
// (rate(<sel>[range])))`. The lowering must:
//
//   - Land HistogramQuantile at the same place in the tree as the bare
//     selector case (Project at the root, HistogramQuantile underneath).
//   - Drop `le` from the by-clause (cerberus's classic histograms carry
//     the distribution in parallel arrays, not in per-bucket Attributes
//     entries).
//   - Aggregate via sumForEach(BucketCounts) + any(ExplicitBounds), so
//     the bucket distribution is preserved while merging across series.
//   - Filter the Scan to the rate's time window.
func TestLower_HistogramQuantile_OverAggregation(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		query   string
		wantPhi float64
		wantAgg bool
	}{
		{
			name:    "sum by(le) over rate",
			query:   `histogram_quantile(0.95, sum by(le) (rate(http_server_request_duration[5m])))`,
			wantPhi: 0.95,
			wantAgg: true,
		},
		{
			name:    "sum by(le, job) over rate",
			query:   `histogram_quantile(0.99, sum by(le, job) (rate(http_server_request_duration[5m])))`,
			wantPhi: 0.99,
			wantAgg: true,
		},
		{
			name:    "sum without over rate",
			query:   `histogram_quantile(0.5, sum without(instance) (rate(http_server_request_duration[5m])))`,
			wantPhi: 0.5,
			wantAgg: true,
		},
		{
			name:    "bare rate (no sum wrapper)",
			query:   `histogram_quantile(0.5, rate(http_server_request_duration[5m]))`,
			wantPhi: 0.5,
			wantAgg: true,
		},
		{
			name:    "increase variant",
			query:   `histogram_quantile(0.5, sum by(le) (increase(http_server_request_duration[10m])))`,
			wantPhi: 0.5,
			wantAgg: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			pj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("want top-level *chplan.Project, got %T", plan)
			}
			hq, ok := pj.Input.(*chplan.HistogramQuantile)
			if !ok {
				t.Fatalf("want Project.Input = *chplan.HistogramQuantile, got %T", pj.Input)
			}
			if hq.Phi != tc.wantPhi {
				t.Errorf("Phi = %v, want %v", hq.Phi, tc.wantPhi)
			}

			// Walk the tree under HistogramQuantile.Input — must contain
			// an Aggregate node with `sumForEach` + `any` aggregators and
			// a Scan against the classic histogram table.
			var foundAgg *chplan.Aggregate
			var foundScan *chplan.Scan
			chplan.Walk(hq.Input, func(n chplan.Node) bool {
				switch v := n.(type) {
				case *chplan.Aggregate:
					if foundAgg == nil {
						foundAgg = v
					}
				case *chplan.Scan:
					if foundScan == nil {
						foundScan = v
					}
				}
				return true
			})
			if tc.wantAgg && foundAgg == nil {
				t.Fatalf("expected an Aggregate node in the tree, found none")
			}
			if foundScan == nil {
				t.Fatalf("expected a Scan node in the tree, found none")
			}
			if foundScan.Table != s.HistogramTable {
				t.Errorf("Scan.Table = %q, want %q", foundScan.Table, s.HistogramTable)
			}

			// Validate the aggregate functions: sumForEach(BucketCounts)
			// + any(ExplicitBounds).
			if foundAgg != nil {
				if len(foundAgg.AggFuncs) != 2 {
					t.Errorf("Aggregate.AggFuncs = %d funcs, want 2", len(foundAgg.AggFuncs))
				} else {
					if foundAgg.AggFuncs[0].Name != "sumForEach" {
						t.Errorf("AggFuncs[0].Name = %q, want sumForEach", foundAgg.AggFuncs[0].Name)
					}
					if foundAgg.AggFuncs[1].Name != "any" {
						t.Errorf("AggFuncs[1].Name = %q, want any", foundAgg.AggFuncs[1].Name)
					}
				}
			}
		})
	}
}

// TestLower_HistogramQuantile_OverAggregation_LeDropped pins the rule
// that `le` is silently dropped from `sum by(le)` clauses on the
// classic-histogram path. The bucket distribution lives in the
// parallel BucketCounts × ExplicitBounds arrays — there is no `le`
// label per row to group on — so `sum by(le)` semantically collapses
// to a single group.
func TestLower_HistogramQuantile_OverAggregation_LeDropped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(0.95, sum by(le) (rate(http_server_request_duration[5m])))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	pj := plan.(*chplan.Project)
	hq := pj.Input.(*chplan.HistogramQuantile)

	var foundAgg *chplan.Aggregate
	chplan.Walk(hq.Input, func(n chplan.Node) bool {
		if v, ok := n.(*chplan.Aggregate); ok {
			foundAgg = v
		}
		return true
	})
	if foundAgg == nil {
		t.Fatalf("no Aggregate found")
	}
	if len(foundAgg.GroupBy) != 0 {
		t.Errorf("Aggregate.GroupBy = %d expressions, want 0 (le must be dropped, no other labels)",
			len(foundAgg.GroupBy))
	}
}

// TestLower_HistogramQuantile_OverAggregation_Native pins the chplan
// shape for `histogram_quantile(phi, sum by(...) (rate(<sel>_exp_hist[r])))`.
// The native lowering mirrors the classic aggregated-input path but
// threads exp-histogram merge math through the inner Project
// (scale-fold + offset-align + zero-pad + element-wise sum). The
// lowering must:
//
//   - Produce a Project(HistogramQuantileNative(Project(Aggregate(Filter(Scan))))) tree.
//   - Drop `le` from any by-clause (same rule as the classic native
//     path — exp-histogram bucket data lives in PositiveBucketCounts,
//     not as a per-bucket label).
//   - Aggregate via min(Scale) + sum(ZeroCount). max(ZeroThreshold)
//     is absent on the default schema — the upstream OTel-CH
//     exp-histogram DDL persists no zero_threshold column; only a
//     custom schema configuring ZeroThresholdColumn gets the extra
//     aggregate.
//   - groupArray of every per-row exp-histogram column.
//   - Filter the Scan to the rate's time window.
func TestLower_HistogramQuantile_OverAggregation_Native(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		query   string
		wantPhi float64
	}{
		{
			name:    "sum by(le) over rate on exp_hist",
			query:   `histogram_quantile(0.95, sum by(le) (rate(http_server_duration_exp_hist[5m])))`,
			wantPhi: 0.95,
		},
		{
			name:    "sum by(le, job) over rate on exp_hist",
			query:   `histogram_quantile(0.99, sum by(le, job) (rate(http_server_duration_exp_hist[5m])))`,
			wantPhi: 0.99,
		},
		{
			name:    "sum without over rate on exp_hist",
			query:   `histogram_quantile(0.5, sum without(instance) (rate(http_server_duration_exp_hist[5m])))`,
			wantPhi: 0.5,
		},
		{
			name:    "bare rate (no sum wrapper) on exp_hist",
			query:   `histogram_quantile(0.5, rate(http_server_duration_exp_hist[5m]))`,
			wantPhi: 0.5,
		},
		{
			name:    "increase variant on exp_hist",
			query:   `histogram_quantile(0.5, sum by(le) (increase(http_server_duration_exp_hist[10m])))`,
			wantPhi: 0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			pj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("want top-level *chplan.Project, got %T", plan)
			}
			hq, ok := pj.Input.(*chplan.HistogramQuantileNative)
			if !ok {
				t.Fatalf("want Project.Input = *chplan.HistogramQuantileNative, got %T", pj.Input)
			}
			if hq.Phi != tc.wantPhi {
				t.Errorf("Phi = %v, want %v", hq.Phi, tc.wantPhi)
			}

			var foundAgg *chplan.Aggregate
			var foundScan *chplan.Scan
			chplan.Walk(hq.Input, func(n chplan.Node) bool {
				switch v := n.(type) {
				case *chplan.Aggregate:
					if foundAgg == nil {
						foundAgg = v
					}
				case *chplan.Scan:
					if foundScan == nil {
						foundScan = v
					}
				}
				return true
			})
			if foundAgg == nil {
				t.Fatalf("expected an Aggregate node in the tree, found none")
			}
			if foundScan == nil {
				t.Fatalf("expected a Scan node in the tree, found none")
			}
			if foundScan.Table != s.ExpHistogramTable {
				t.Errorf("Scan.Table = %q, want %q", foundScan.Table, s.ExpHistogramTable)
			}

			// Validate the aggregate function set: min(Scale) +
			// sum(ZeroCount) + groupArray of every per-row
			// exp-histogram column (no max(ZeroThreshold) on the
			// default schema — see the function doc above).
			if len(foundAgg.AggFuncs) != 7 {
				t.Errorf("Aggregate.AggFuncs = %d funcs, want 7", len(foundAgg.AggFuncs))
			}
			want := map[string]bool{
				"min": false, "sum": false, "groupArray": false,
			}
			for _, af := range foundAgg.AggFuncs {
				if _, ok := want[af.Name]; ok {
					want[af.Name] = true
				}
			}
			for name, seen := range want {
				if !seen {
					t.Errorf("Aggregate missing expected aggregator %q", name)
				}
			}
		})
	}
}

// TestLower_HistogramQuantile_OverAggregation_Native_LeDropped pins the
// rule that `le` is silently dropped from `sum by(le)` clauses on the
// native aggregated-input path too. Mirrors
// TestLower_HistogramQuantile_OverAggregation_LeDropped on the classic
// side: exp-histogram bucket distribution lives in
// PositiveBucketCounts (offset by PositiveOffset), not as a per-bucket
// label, so `sum by(le)` semantically collapses to a single group.
func TestLower_HistogramQuantile_OverAggregation_Native_LeDropped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(0.95, sum by(le) (rate(http_server_duration_exp_hist[5m])))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	pj := plan.(*chplan.Project)
	hq := pj.Input.(*chplan.HistogramQuantileNative)

	var foundAgg *chplan.Aggregate
	chplan.Walk(hq.Input, func(n chplan.Node) bool {
		if v, ok := n.(*chplan.Aggregate); ok {
			foundAgg = v
		}
		return true
	})
	if foundAgg == nil {
		t.Fatalf("no Aggregate found")
	}
	if len(foundAgg.GroupBy) != 0 {
		t.Errorf("Aggregate.GroupBy = %d expressions, want 0 (le must be dropped, no other labels)",
			len(foundAgg.GroupBy))
	}
}

// TestLower_HistogramQuantile_NativeBareRange pins the chplan shape
// for `histogram_quantile(phi, <exp-hist-VectorSelector>)` under range
// mode (LowerAtRange with step > 0): the lowering must fan a per-step
// LWR window across the anchor grid so each anchor in `[start, end]`
// produces its own quantile row instead of every step collapsing onto
// a single `now64(9)` instant. The shape is the single-pass
// RangeBucketFanout variant of the native instant path: the same
// HistogramQuantileNative IR drives the quantile interpolation, but
// the GroupBy carries anchor_ts as the leading partition key and the
// outer Project surfaces anchor_ts as TimeUnix.
func TestLower_HistogramQuantile_NativeBareRange(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second

	expr, err := p.ParseExpr(`histogram_quantile(0.95, http_server_duration_exp_hist{service="api"})`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	pj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("want top-level *chplan.Project, got %T", plan)
	}
	hq, ok := pj.Input.(*chplan.HistogramQuantileNative)
	if !ok {
		t.Fatalf("want Project.Input = *chplan.HistogramQuantileNative, got %T", pj.Input)
	}
	if hq.Phi != 0.95 {
		t.Errorf("Phi = %v, want 0.95", hq.Phi)
	}
	// The native range path partitions by anchor_ts + Attributes — that
	// is the per-step + per-series key the matrix pivot consumes.
	if len(hq.GroupByAliases) != 2 || hq.GroupByAliases[0] != "anchor_ts" || hq.GroupByAliases[1] != s.AttributesColumn {
		t.Errorf("GroupByAliases = %v, want [anchor_ts, %s]", hq.GroupByAliases, s.AttributesColumn)
	}
	// The outer Project must surface anchor_ts as TimeUnix (rather than
	// the now64(9) hardcode the instant-mode lowering keeps for one-row
	// queries).
	var sawAnchorAsTime bool
	for _, pr := range pj.Projections {
		if pr.Alias == s.TimestampColumn {
			if cr, ok := pr.Expr.(*chplan.ColumnRef); ok && cr.Name == "anchor_ts" {
				sawAnchorAsTime = true
			}
		}
	}
	if !sawAnchorAsTime {
		t.Errorf("outer Project must project anchor_ts AS %s (got projections: %+v)",
			s.TimestampColumn, pj.Projections)
	}
	// The tree must contain the single-pass RangeBucketFanout node — the
	// bounded sample-side anchor fan-out — and must NOT contain the old
	// O(rows × N) StepGrid CROSS JOIN scaffold it superseded (#804-style
	// rework for the histogram range path).
	var sawFanout, sawStepGrid, sawCrossJoin bool
	chplan.Walk(hq.Input, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.RangeBucketFanout:
			sawFanout = true
		case *chplan.StepGrid:
			sawStepGrid = true
		case *chplan.CrossJoin:
			sawCrossJoin = true
		}
		return true
	})
	if !sawFanout {
		t.Errorf("expected a RangeBucketFanout in the subtree, found none")
	}
	if sawStepGrid {
		t.Errorf("range lowering must not contain a StepGrid (single-pass invariant)")
	}
	if sawCrossJoin {
		t.Errorf("range lowering must not contain a CrossJoin (single-pass invariant)")
	}
}

// TestLower_HistogramQuantile_NativeBareRange_InstantStillNow64 pins
// the negative complement: instant-mode lowering (step == 0) keeps the
// pre-existing `now64(9)` shape so existing fixtures stay byte-stable.
// Only range mode opts into the StepGrid scaffold.
func TestLower_HistogramQuantile_NativeBareRange_InstantStillNow64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(0.95, http_server_duration_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	pj := plan.(*chplan.Project)
	// Instant-mode must NOT contain StepGrid/CrossJoin.
	var sawStepGrid, sawCrossJoin bool
	chplan.Walk(pj, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.StepGrid:
			sawStepGrid = true
		case *chplan.CrossJoin:
			sawCrossJoin = true
		}
		return true
	})
	if sawStepGrid {
		t.Errorf("instant-mode lowering must not contain a StepGrid")
	}
	if sawCrossJoin {
		t.Errorf("instant-mode lowering must not contain a CrossJoin")
	}
}

// TestLower_HistogramQuantile_Errors covers the rejected shapes so the
// error messages stay observable.
func TestLower_HistogramQuantile_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "arity mismatch",
			query:   `histogram_quantile(0.5)`,
			wantErr: "histogram_quantile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				// histogram_quantile(0.5) may fail at parse time — that's
				// fine, the prom parser owns arity for builtin signatures.
				if !strings.Contains(err.Error(), tc.wantErr) && tc.name != "arity mismatch" {
					t.Fatalf("ParseExpr: %v", err)
				}
				return
			}
			if _, err := promql.Lower(context.Background(), expr, s); err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLower_HistogramQuantile_BucketSuffixStrip pins the Grafana
// classic-histogram dashboard pattern:
//
//	histogram_quantile(0.95, sum by (le) (rate(<X>_bucket[5m])))
//
// OTel-CH stores the same data as one row per observation under the
// bare metric name `<X>` (no `_bucket` suffix; the BucketCounts +
// ExplicitBounds arrays carry the distribution). Pre-fix, cerberus
// passed the `__name__='<X>_bucket'` matcher through verbatim, the
// emitted WHERE filtered the histogram table for a non-existent
// MetricName, and every dashboard p95 panel rendered "No data".
//
// The fix (stripBucketSuffix in histogram_quantile.go) trims `_bucket`
// off the `__name__` matcher before the predicate emits. Pin both:
//
//  1. The emitted SQL filters on the BARE name (no `_bucket`).
//  2. The SQL does NOT carry the unstripped `_bucket`-suffixed form.
//
// Coverage at every classic-histogram entry point:
//   - bare selector (instant): `histogram_quantile(phi, <X>_bucket)`
//   - aggregated (instant):    `histogram_quantile(phi, sum by(le) (rate(<X>_bucket[r])))`
//   - bare selector (range):   same as bare-instant under range mode
//   - aggregated (range):      same as agg-instant under range mode
func TestLower_HistogramQuantile_BucketSuffixStrip(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// Same metric across all cases — `cerberus_queries_duration_seconds`
	// mirrors the actual self-telemetry histogram the dashboard targets.
	const bare = "cerberus_queries_duration_seconds"
	const suffixed = bare + "_bucket"

	cases := []struct {
		name    string
		query   string
		ctxStep time.Duration // 0 == instant, > 0 == range mode
	}{
		{
			name:  "bare_instant",
			query: `histogram_quantile(0.95, ` + suffixed + `)`,
		},
		{
			name:  "agg_instant",
			query: `histogram_quantile(0.95, sum by (le) (rate(` + suffixed + `[5m])))`,
		},
		{
			name:    "bare_range",
			query:   `histogram_quantile(0.95, ` + suffixed + `)`,
			ctxStep: 30 * time.Second,
		},
		{
			name:    "agg_range",
			query:   `histogram_quantile(0.95, sum by (le) (rate(` + suffixed + `[5m])))`,
			ctxStep: 30 * time.Second,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			var plan chplan.Node
			if tc.ctxStep > 0 {
				start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				end := start.Add(time.Hour)
				plan, err = promql.LowerAtRange(context.Background(), expr, s, start, end, tc.ctxStep)
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
			// The chsql emitter renders string literals (including the
			// MetricName predicate value) as `?` placeholders carried in
			// the `args` slice — they don't appear inlined in the SQL.
			// Check both: (a) the SQL text never carries the suffixed
			// name (no path inlines `_bucket`), (b) the args carry the
			// BARE name and never the suffixed name, and (c) the SQL
			// targets the classic-histogram table (so the strip didn't
			// misroute the lowering to a non-histogram table).
			if strings.Contains(sql, suffixed) {
				t.Errorf("emitted SQL contains the unstripped suffixed name %q — "+
					"the histogram table is keyed by the BARE metric name; "+
					"a `_bucket`-suffixed predicate matches zero rows and "+
					"every dashboard p95 panel reads 'No data'.\nfull SQL:\n%s",
					suffixed, sql)
			}
			if !strings.Contains(sql, "`"+s.HistogramTable+"`") {
				t.Errorf("emitted SQL does not target the classic-histogram table %q — "+
					"the strip dropped too much or the histogram path is "+
					"misrouting to a non-histogram table.\nfull SQL:\n%s",
					s.HistogramTable, sql)
			}
			var sawBare, sawSuffixed bool
			for _, a := range args {
				str, ok := a.(string)
				if !ok {
					continue
				}
				if str == bare {
					sawBare = true
				}
				if str == suffixed {
					sawSuffixed = true
				}
			}
			if sawSuffixed {
				t.Errorf("emitted SQL params carry the unstripped suffixed name %q — "+
					"the histogram table is keyed by the BARE metric name; "+
					"a `_bucket`-suffixed predicate matches zero rows.\nargs: %v",
					suffixed, args)
			}
			if !sawBare {
				t.Errorf("emitted SQL params do not carry the bare metric name %q — "+
					"the strip dropped too much.\nargs: %v",
					bare, args)
			}
		})
	}
}
