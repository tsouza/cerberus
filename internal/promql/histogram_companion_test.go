package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramCompanion_RoutesToHistogramTable pins the
// classic-histogram `_count` / `_sum` companion-suffix rewrite at the
// lowering layer.
//
// The OTel-CH histogram exporter writes a single row per observation
// under the BARE metric name (`<X>`), carrying parallel Count + Sum +
// BucketCounts + ExplicitBounds columns. Prometheus convention
// surfaces the same histogram as three companion series — `<X>_bucket`
// (handled by stripBucketSuffix → PR #637), `<X>_count`, `<X>_sum`.
// Without this rewrite, a Grafana panel querying `rate(<X>_count[5m])`
// emitted `MetricName='<X>_count'` against the sum table and silently
// returned "No data".
//
// The rewrite:
//
//  1. Routes the Scan to schema.Metrics.HistogramTable
//     (`otel_metrics_histogram`), not SumTable.
//  2. Strips the `_count` / `_sum` suffix off the `__name__` matcher so
//     the WHERE clause resolves against the BARE name.
//  3. Wraps the Scan in a Project that aliases `toFloat64(Count)` /
//     `toFloat64(Sum)` as `Value`, so the downstream Sample-row
//     contract (and the RangeWindow / LWR / arithmetic pipeline above
//     it) reads through unchanged.
//
// The test exercises the canonical Grafana shape `rate(<X>_count[5m])`
// — the user-visible bug surface — and asserts the emitted plan
// targets the histogram table with the bare metric name.
func TestLower_HistogramCompanion_RoutesToHistogramTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name       string
		query      string
		wantColumn string // expected source column inside toFloat64(...)
	}{
		{
			name:       "rate_count",
			query:      `rate(http_server_request_duration_count[5m])`,
			wantColumn: s.CountColumn,
		},
		{
			name:       "rate_sum",
			query:      `rate(http_server_request_duration_sum[5m])`,
			wantColumn: s.SumColumn,
		},
		{
			name:       "bare_count_selector_with_lwr_wrap",
			query:      `http_server_request_duration_count`,
			wantColumn: s.CountColumn,
		},
		{
			name:       "bare_sum_selector_with_lwr_wrap",
			query:      `http_server_request_duration_sum`,
			wantColumn: s.SumColumn,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			// Walk the plan tree to find the Scan node. There must be
			// exactly one and it must target the histogram table.
			scans := collectScans(plan)
			if len(scans) != 1 {
				t.Fatalf("want 1 Scan node, got %d", len(scans))
			}
			if scans[0].Table != s.HistogramTable {
				t.Fatalf("Scan.Table = %q; want %q (histogram table)",
					scans[0].Table, s.HistogramTable)
			}

			// The companion Project must wrap the Scan so the downstream
			// pipeline sees a synthesised `Value` column. Find the
			// Project whose Input is the Scan and assert its `Value`
			// projection casts the right source column.
			project := findCompanionProject(plan, scans[0])
			if project == nil {
				t.Fatalf("want a Project whose Input is the Scan and which projects toFloat64(%s) AS %s; got none",
					tc.wantColumn, s.ValueColumn)
			}
			if !projectAliasesValue(project, tc.wantColumn, s.ValueColumn) {
				t.Fatalf("companion Project does not alias toFloat64(%s) AS %s; projections=%+v",
					tc.wantColumn, s.ValueColumn, project.Projections)
			}

			// The filter (or scan, when no matchers) must reference the
			// BARE metric name — i.e. with the `_count` / `_sum` suffix
			// stripped. Walk the tree for Binary nodes comparing
			// MetricName and assert the literal is the bare name.
			if !planReferencesMetricName(plan, "http_server_request_duration") {
				t.Fatalf("plan does not filter on bare MetricName='http_server_request_duration'")
			}
			if planReferencesMetricName(plan, "http_server_request_duration_count") ||
				planReferencesMetricName(plan, "http_server_request_duration_sum") {
				t.Fatalf("plan still references the suffixed MetricName — rewrite did not strip the suffix")
			}
		})
	}
}

// TestLower_HistogramCompanion_PreservesCountersWithTotalSuffix pins
// that the rewrite only applies to `_count` / `_sum` and leaves
// `_total` (the OTel-CH counter convention) untouched. The fix is
// scoped to histogram-companion suffixes; counter routing is the
// existing TableFor heuristic.
func TestLower_HistogramCompanion_PreservesCountersWithTotalSuffix(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`rate(http_server_requests_total[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	scans := collectScans(plan)
	if len(scans) != 1 {
		t.Fatalf("want 1 Scan, got %d", len(scans))
	}
	if scans[0].Table != s.SumTable {
		t.Fatalf("Scan.Table = %q; want %q (sum table — _total stays on sum table)",
			scans[0].Table, s.SumTable)
	}
	if !planReferencesMetricName(plan, "http_server_requests_total") {
		t.Fatalf("plan should still reference the full `_total` MetricName")
	}
}

// TestLower_HistogramCompanion_NoHistogramTable verifies the rewrite
// is a no-op when the schema has no histogram table configured (an
// edge case for custom schemas that disable classic histograms).
// Falls back to the existing TableFor / SumTable routing.
func TestLower_HistogramCompanion_NoHistogramTable(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	s.HistogramTable = "" // disable histogram routing

	expr, err := p.ParseExpr(`rate(http_request_duration_count[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	scans := collectScans(plan)
	if len(scans) != 1 {
		t.Fatalf("want 1 Scan, got %d", len(scans))
	}
	if scans[0].Table != s.SumTable {
		t.Fatalf("Scan.Table = %q; want %q (sum table — fallback when no histogram table)",
			scans[0].Table, s.SumTable)
	}
}

// collectScans walks the plan tree and returns every Scan node it
// encounters. The companion rewrite scopes to one Scan per selector,
// so callers assert len()==1 and read the Table off the first.
func collectScans(n chplan.Node) []*chplan.Scan {
	var out []*chplan.Scan
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil {
			return
		}
		if s, ok := node.(*chplan.Scan); ok {
			out = append(out, s)
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(n)
	return out
}

// findCompanionProject locates the Project node whose Input is the
// supplied Scan. Returns nil when the Scan flows into the downstream
// pipeline directly (the non-companion path).
func findCompanionProject(root chplan.Node, scan *chplan.Scan) *chplan.Project {
	var found *chplan.Project
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil || found != nil {
			return
		}
		if p, ok := node.(*chplan.Project); ok {
			if p.Input == scan {
				found = p
				return
			}
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(root)
	return found
}

// projectAliasesValue reports whether the supplied Project has a
// projection that aliases `toFloat64(<sourceColumn>)` as the canonical
// `Value` column.
func projectAliasesValue(p *chplan.Project, sourceColumn, valueAlias string) bool {
	for _, proj := range p.Projections {
		if proj.Alias != valueAlias {
			continue
		}
		call, ok := proj.Expr.(*chplan.FuncCall)
		if !ok || call.Name != "toFloat64" || len(call.Args) != 1 {
			continue
		}
		col, ok := call.Args[0].(*chplan.ColumnRef)
		if !ok {
			continue
		}
		if col.Name == sourceColumn {
			return true
		}
	}
	return false
}

// planReferencesMetricName reports whether the supplied plan contains
// any `MetricName = <literal>` Binary node where the literal equals
// the supplied name. Used to assert the suffix-strip rewrites the
// emitted filter to the bare metric name.
func planReferencesMetricName(n chplan.Node, want string) bool {
	found := false
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil || found {
			return
		}
		if f, ok := node.(*chplan.Filter); ok {
			if exprReferencesMetricName(f.Predicate, want) {
				found = true
				return
			}
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(n)
	return found
}

func exprReferencesMetricName(e chplan.Expr, want string) bool {
	if e == nil {
		return false
	}
	if b, ok := e.(*chplan.Binary); ok {
		if isMetricNameEq(b, want) {
			return true
		}
		if exprReferencesMetricName(b.Left, want) || exprReferencesMetricName(b.Right, want) {
			return true
		}
	}
	return false
}

func isMetricNameEq(b *chplan.Binary, want string) bool {
	if b.Op != chplan.OpEq {
		return false
	}
	col, ok := b.Left.(*chplan.ColumnRef)
	if !ok || col.Name != "MetricName" {
		return false
	}
	lit, ok := b.Right.(*chplan.LitString)
	if !ok {
		return false
	}
	return lit.V == want || strings.EqualFold(lit.V, want)
}
