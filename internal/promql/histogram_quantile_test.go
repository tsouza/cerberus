package promql_test

import (
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
			plan, err := promql.Lower(expr, s)
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
			wantErr: "classic-histogram VectorSelector",
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
			if _, err := promql.Lower(expr, s); err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
