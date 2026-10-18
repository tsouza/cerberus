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

// TestLowerMetadataRange_AppliesTheWindow pins wrapMetadataFullRange's
// closed [start, end] window: with both bounds set, the enumeration scan
// is filtered on both, so a series with no sample inside the window is not
// listed.
func TestLowerMetadataRange_AppliesTheWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`{__name__=~"up|down"}`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := LowerMetadataRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []string{"`TimeUnix` >= ", "`TimeUnix` <= "} {
		if !strings.Contains(sql, bound) {
			t.Errorf("metadata lowering over [start, end] has no %q bound:\n%s", bound, sql)
		}
	}
}

// TestLowerMetadata_CompanionUnionPicksItsSeam pins the metadata seam the
// `_count` / `_sum` companion union ends in: the series enumeration
// collapses the union over the window, and a catalog lowering projects its
// one catalog column instead.
func TestLowerMetadata_CompanionUnionPicksItsSeam(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	s := schema.DefaultOTelMetrics()
	const name = "http_request_duration_seconds_count"
	if _, col, ok := s.HistogramCompanionColumn(name); !ok || !needCompanionUnion(s, col, name, strings.TrimSuffix(name, "_count")) {
		t.Fatalf("%s does not take the companion union on the default schema; this test pins that path", name)
	}
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(name)
	if err != nil {
		t.Fatal(err)
	}
	unionUnder := func(t *testing.T, plan chplan.Node) chplan.Node {
		t.Helper()
		var parent chplan.Node
		chplan.Walk(plan, func(n chplan.Node) bool {
			for _, c := range n.Children() {
				if _, ok := c.(*chplan.UnionAll); ok && parent == nil {
					parent = n
				}
			}
			return true
		})
		if parent == nil {
			t.Fatalf("no companion UnionAll under the %T plan", plan)
		}
		return parent
	}

	series, err := LowerMetadataRange(context.Background(), expr, s, start, end)
	if err != nil {
		t.Fatal(err)
	}
	var collapsed bool
	chplan.Walk(series, func(n chplan.Node) bool {
		if agg, ok := n.(*chplan.Aggregate); ok && slicesContainsAlias(agg.AggFuncs, "meta_ts") {
			collapsed = true
		}
		return true
	})
	if !collapsed {
		t.Errorf("series enumeration over the companion union (a %T plan) has no window collapse", series)
	}
	unionUnder(t, series)

	names, err := LowerMetadataLabelNames(context.Background(), expr, s, start, end)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := names.(*chplan.Project)
	if !ok || len(project.Projections) != 1 || project.Projections[0].Alias != MetadataNameColumn {
		t.Errorf("label-name catalog over the companion union is a %T, not the one-column %s catalog projection", names, MetadataNameColumn)
	}
	unionUnder(t, names)
}

// slicesContainsAlias reports whether one of aggs is aliased alias.
func slicesContainsAlias(aggs []chplan.AggFunc, alias string) bool {
	for _, a := range aggs {
		if a.Alias == alias {
			return true
		}
	}
	return false
}
