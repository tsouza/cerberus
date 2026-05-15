package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
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

// TestLower_HistogramQuantile_OverAggregation_NativeRejected confirms
// the aggregated-input path bails out on native (exp) histograms via
// the Phase 2 stub (lowerHistogramQuantileNativeAgg). The native path's
// bucket arithmetic differs enough that mirroring the classic-path
// lowering is a separate milestone — see docs/native-histogram-plan.md
// § Phase 2 for the deferred design.
func TestLower_HistogramQuantile_OverAggregation_NativeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(0.95, sum by(le) (rate(http_server_duration_exp_hist[5m])))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	if _, err := promql.Lower(context.Background(), expr, s); err == nil {
		t.Fatalf("expected error for aggregated native histogram, got nil")
	} else {
		if !strings.Contains(err.Error(), "native") {
			t.Errorf("error %q should mention native histograms", err.Error())
		}
		// The Phase 2 stub points readers at the deferred design doc so
		// they can pick up the work without re-deriving the algorithm.
		if !strings.Contains(err.Error(), "native-histogram-plan.md") {
			t.Errorf("error %q should cite docs/native-histogram-plan.md", err.Error())
		}
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
		{
			name:    "non-scalar phi",
			query:   `histogram_quantile(scalar(other), foo)`,
			wantErr: "requires a scalar-literal phi",
		},
		{
			name:    "non-VectorSelector arg",
			query:   `histogram_quantile(0.5, vector(1))`,
			wantErr: "histogram VectorSelector",
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
