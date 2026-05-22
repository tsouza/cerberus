package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramCompanion_RoutesToHistogramAndSumUnion pins the
// classic-histogram `_count` / `_sum` companion-suffix rewrite at the
// lowering layer. The fix landed in two phases:
//
//  1. PR #710 — added the histogram-arm projection that aliases
//     `toFloat64(Count)` / `toFloat64(Sum)` as `Value` so the
//     downstream Sample-row contract holds.
//  2. (this follow-up) — added the sum-arm so OTel-hostmetrics
//     counters that ship under suffixed names
//     (`system_cpu_logical_count`, `system_processes_count`,
//     `system_filesystem_inodes_count`,
//     `system_processes_created_count`, …) resolve too. The lowering
//     now emits a chplan.UnionAll over both physical layouts.
//
// The OTel-CH histogram exporter writes a single row per observation
// under the BARE metric name (`<X>`), carrying parallel Count + Sum +
// BucketCounts + ExplicitBounds columns. Prometheus convention
// surfaces the same histogram as three companion series — `<X>_bucket`
// (handled by stripBucketSuffix → PR #637), `<X>_count`, `<X>_sum`.
// The OTel-hostmetrics receiver, in parallel, emits cumulative
// counters under suffixed names (`system_cpu_logical_count`) directly
// in the sum table — no histogram row exists for those.
//
// The rewrite assembles a two-arm UnionAll:
//
//   - histogram arm: Scan(otel_metrics_histogram) → Filter MetricName
//     = '<bare>' → Project [MetricName='<suffixed>' (literal),
//     Attributes, TimeUnix, Value=toFloat64(Count|Sum)].
//   - sum arm:       Scan(otel_metrics_sum) → Filter MetricName =
//     '<suffixed>' → Project [MetricName, Attributes, TimeUnix, Value].
//
// The test exercises the canonical Grafana shape `rate(<X>_count[5m])`
// — the user-visible bug surface — and asserts the emitted plan has
// the right per-arm shape.
func TestLower_HistogramCompanion_RoutesToHistogramAndSumUnion(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name         string
		query        string
		bareName     string
		suffixedName string
		wantColumn   string // expected source column inside toFloat64(...) on histogram arm
	}{
		{
			name:         "rate_count",
			query:        `rate(http_server_request_duration_count[5m])`,
			bareName:     "http_server_request_duration",
			suffixedName: "http_server_request_duration_count",
			wantColumn:   s.CountColumn,
		},
		{
			name:         "rate_sum",
			query:        `rate(http_server_request_duration_sum[5m])`,
			bareName:     "http_server_request_duration",
			suffixedName: "http_server_request_duration_sum",
			wantColumn:   s.SumColumn,
		},
		{
			name:         "bare_count_selector_with_lwr_wrap",
			query:        `http_server_request_duration_count`,
			bareName:     "http_server_request_duration",
			suffixedName: "http_server_request_duration_count",
			wantColumn:   s.CountColumn,
		},
		{
			name:         "bare_sum_selector_with_lwr_wrap",
			query:        `http_server_request_duration_sum`,
			bareName:     "http_server_request_duration",
			suffixedName: "http_server_request_duration_sum",
			wantColumn:   s.SumColumn,
		},
		{
			name:         "hostmetrics_logical_count",
			query:        `system_cpu_logical_count`,
			bareName:     "system_cpu_logical",
			suffixedName: "system_cpu_logical_count",
			wantColumn:   s.CountColumn,
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

			// Walk the plan tree to find Scan nodes. The companion
			// union emits two: one against the histogram table, one
			// against the sum table.
			scans := collectScans(plan)
			if len(scans) != 2 {
				t.Fatalf("want 2 Scan nodes (histogram + sum union), got %d", len(scans))
			}
			tables := []string{scans[0].Table, scans[1].Table}
			if tables[0] != s.HistogramTable || tables[1] != s.SumTable {
				t.Fatalf("Scan tables = %v; want [%q, %q]",
					tables, s.HistogramTable, s.SumTable)
			}

			// The histogram-arm Project must alias the right source
			// column (Count or Sum) as `Value`.
			histProject := findCompanionProject(plan, scans[0])
			if histProject == nil {
				t.Fatalf("histogram arm: want Project over Scan(%s); got none",
					s.HistogramTable)
			}
			if !projectAliasesValue(histProject, tc.wantColumn, s.ValueColumn) {
				t.Fatalf("histogram arm Project does not alias toFloat64(%s) AS %s; projections=%+v",
					tc.wantColumn, s.ValueColumn, histProject.Projections)
			}

			// The sum-arm Project must pass `Value` through unchanged.
			sumProject := findCompanionProject(plan, scans[1])
			if sumProject == nil {
				t.Fatalf("sum arm: want Project over Scan(%s); got none", s.SumTable)
			}
			if !projectPassesValue(sumProject, s.ValueColumn) {
				t.Fatalf("sum arm Project does not pass %s through unchanged; projections=%+v",
					s.ValueColumn, sumProject.Projections)
			}

			// Both arm-level filters must reference their own
			// MetricName literal: the histogram arm reads the BARE
			// name; the sum arm reads the SUFFIXED name.
			if !planReferencesMetricName(plan, tc.bareName) {
				t.Fatalf("plan does not filter on bare MetricName=%q (histogram arm)", tc.bareName)
			}
			if !planReferencesMetricName(plan, tc.suffixedName) {
				t.Fatalf("plan does not filter on suffixed MetricName=%q (sum arm)", tc.suffixedName)
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

// findCompanionProject locates the Project node sitting above the
// supplied Scan in the plan tree, walking past any intervening
// Filter (the per-arm scan-side MetricName / matcher Filter the
// companion-union arms emit). Returns nil when no Project sits above
// the Scan along this Filter? → Project chain.
func findCompanionProject(root chplan.Node, scan *chplan.Scan) *chplan.Project {
	var found *chplan.Project
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil || found != nil {
			return
		}
		if p, ok := node.(*chplan.Project); ok {
			if projectInputReachesScan(p.Input, scan) {
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

// projectInputReachesScan reports whether `input` is `scan` directly,
// or a Filter whose Input chain bottoms out on `scan`. Used by
// findCompanionProject to span the Project → Filter → Scan shape the
// per-arm companion union emits when the matcher list carries
// non-MetricName matchers (or even just the rewritten `MetricName=`
// matcher — every arm always wraps the Scan in a Filter for the bare /
// suffixed name predicate).
func projectInputReachesScan(input chplan.Node, scan *chplan.Scan) bool {
	for input != nil {
		if input == scan {
			return true
		}
		f, ok := input.(*chplan.Filter)
		if !ok {
			return false
		}
		input = f.Input
	}
	return false
}

// projectPassesValue reports whether the supplied Project has a
// projection that reads `<valueAlias>` (a bare ColumnRef) under the
// same alias — i.e. the sum-arm shape that passes the canonical
// `Value` column through without casting. The histogram arm uses
// `projectAliasesValue` instead because its source column is
// `Count` / `Sum` and requires a `toFloat64` cast.
func projectPassesValue(p *chplan.Project, valueAlias string) bool {
	for _, proj := range p.Projections {
		if proj.Alias != valueAlias {
			continue
		}
		col, ok := proj.Expr.(*chplan.ColumnRef)
		if !ok {
			continue
		}
		if col.Name == valueAlias {
			return true
		}
	}
	return false
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
