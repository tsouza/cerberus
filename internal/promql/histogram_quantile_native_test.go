package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramQuantile_Native asserts the lowering shape for the
// exp-histogram path: histogram_quantile(phi, <name>_exp_hist) produces
// a Project(HistogramQuantileNative(Scan|Filter, ...)) tree targeting
// the OTel-CH exp-histogram table.
func TestLower_HistogramQuantile_Native(t *testing.T) {
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
			query:     `histogram_quantile(0.5, http_server_duration_exp_hist)`,
			wantPhi:   0.5,
			wantTable: s.ExpHistogramTable,
		},
		{
			name:      "p99 with label matcher",
			query:     `histogram_quantile(0.99, http_server_duration_exp_hist{job="api"})`,
			wantPhi:   0.99,
			wantTable: s.ExpHistogramTable,
		},
		{
			name:      "phi=1 boundary",
			query:     `histogram_quantile(1, http_server_duration_exp_hist)`,
			wantPhi:   1,
			wantTable: s.ExpHistogramTable,
		},
		{
			name:      "phi=0 boundary",
			query:     `histogram_quantile(0, http_server_duration_exp_hist)`,
			wantPhi:   0,
			wantTable: s.ExpHistogramTable,
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
			if hq.PositiveBucketCountsColumn != s.PositiveBucketCountsColumn {
				t.Errorf("PositiveBucketCountsColumn = %q, want %q", hq.PositiveBucketCountsColumn, s.PositiveBucketCountsColumn)
			}
			if hq.PositiveOffsetColumn != s.PositiveOffsetColumn {
				t.Errorf("PositiveOffsetColumn = %q, want %q", hq.PositiveOffsetColumn, s.PositiveOffsetColumn)
			}
			if hq.ScaleColumn != s.ScaleColumn {
				t.Errorf("ScaleColumn = %q, want %q", hq.ScaleColumn, s.ScaleColumn)
			}
			if hq.ZeroCountColumn != s.ZeroCountColumn {
				t.Errorf("ZeroCountColumn = %q, want %q", hq.ZeroCountColumn, s.ZeroCountColumn)
			}
			if hq.ZeroThresholdColumn != s.ZeroThresholdColumn {
				t.Errorf("ZeroThresholdColumn = %q, want %q", hq.ZeroThresholdColumn, s.ZeroThresholdColumn)
			}
			// Walk to find the Scan and assert the target table is the
			// exp-histogram table (not the classic histogram table or
			// the SumTable / GaugeTable).
			var scan *chplan.Scan
			chplan.Walk(hq.Input, func(n chplan.Node) bool {
				if sc, ok := n.(*chplan.Scan); ok {
					scan = sc
					return false
				}
				return true
			})
			if scan == nil {
				t.Fatalf("no Scan node under HistogramQuantileNative.Input")
			}
			if scan.Table != tc.wantTable {
				t.Errorf("Scan.Table = %q, want %q", scan.Table, tc.wantTable)
			}
		})
	}
}

// TestLower_HistogramQuantile_NativeVsClassicRouting asserts that the
// metric-name suffix determines which IR node fires. A metric that
// does not match ExpHistogramSuffix routes to the classic path; one
// that does routes to the native path. This locks the dispatch
// contract documented in lowerHistogramQuantile.
func TestLower_HistogramQuantile_NativeVsClassicRouting(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	type want int
	const (
		wantClassic want = iota
		wantNative
	)

	cases := []struct {
		name  string
		query string
		want  want
	}{
		{
			name:  "classic suffix-less metric",
			query: `histogram_quantile(0.9, request_duration)`,
			want:  wantClassic,
		},
		{
			name:  "classic with _bucket suffix is still classic — TableFor not consulted",
			query: `histogram_quantile(0.9, request_duration_bucket)`,
			want:  wantClassic,
		},
		{
			name:  "native suffix routes to native path",
			query: `histogram_quantile(0.9, request_duration_exp_hist)`,
			want:  wantNative,
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
			switch tc.want {
			case wantClassic:
				if _, ok := pj.Input.(*chplan.HistogramQuantile); !ok {
					t.Fatalf("want classic *chplan.HistogramQuantile, got %T", pj.Input)
				}
			case wantNative:
				if _, ok := pj.Input.(*chplan.HistogramQuantileNative); !ok {
					t.Fatalf("want *chplan.HistogramQuantileNative, got %T", pj.Input)
				}
			}
		})
	}
}

// TestLower_HistogramQuantile_NativeRoutingDisabled exercises the
// disable knob: empty ExpHistogramSuffix means the native path never
// fires and even `_exp_hist`-suffixed metrics route to classic. This
// is the escape hatch for deployments that don't follow the suffix
// convention.
func TestLower_HistogramQuantile_NativeRoutingDisabled(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.ExpHistogramSuffix = ""
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(0.9, request_duration_exp_hist)`)
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
	if _, ok := pj.Input.(*chplan.HistogramQuantile); !ok {
		t.Fatalf("want classic *chplan.HistogramQuantile (routing disabled), got %T", pj.Input)
	}
}

// TestLower_HistogramQuantile_NativeComputedPhi pins the computed-phi
// acceptance on the exp-histogram branch: `histogram_quantile(
// scalar(x), my_exp_hist)` lowers into a HistogramQuantileNative whose
// PhiExpr slot carries the scalar subquery — reference Prometheus
// evaluates phi per query, so the old scalar-literal-only rejection
// was a wrong rejection.
func TestLower_HistogramQuantile_NativeComputedPhi(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`histogram_quantile(scalar(other), my_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	pj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	hq, ok := pj.Input.(*chplan.HistogramQuantileNative)
	if !ok {
		t.Fatalf("Project input = %T, want *chplan.HistogramQuantileNative", pj.Input)
	}
	if hq.PhiExpr == nil {
		t.Fatalf("PhiExpr is nil — computed phi must ride the PhiExpr slot")
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "isNaN(") {
		t.Fatalf("computed-phi SQL missing the isNaN(phi) guard:\n%s", sql)
	}
}

// TestLower_HistogramValueFns pins the native-histogram value-function
// acceptance — histogram_count(up) and family were wrong rejections
// (reference accepts any instant-vector argument and SKIPS float
// samples). Selector args scan the exp-histogram table (non-native
// names match no rows — the reference's empty result); non-selector
// args fold to a constant-false Filter. The exact bucket arithmetic is
// pinned by the chdb fixtures (histogram_{count,sum,avg,stddev,stdvar,
// fraction}_exp.txtar) whose expected values were derived from the
// pinned reference engine's simpleHistogramFunc / histogramVariance /
// HistogramFraction.
func TestLower_HistogramValueFns(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	for _, q := range []string{
		`histogram_count(up)`,
		`histogram_sum(my_exp_hist)`,
		`histogram_avg(my_exp_hist)`,
		`histogram_stddev(my_exp_hist)`,
		`histogram_stdvar(my_exp_hist)`,
		`histogram_fraction(0.2, 0.8, my_exp_hist)`,
		`histogram_fraction(scalar(low), scalar(high), my_exp_hist)`,
		`histogram_count(sum(up))`,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Fatalf("Emit(%q): %v", q, err)
		}
	}
}
