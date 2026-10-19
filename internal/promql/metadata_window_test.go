package promql

import (
	"context"
	"math"
	"slices"
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

// TestLowerMetadataRange_ExactBoundsBefore1900 pins the window bounds a
// metadata lowering emits outside the span a time literal names exactly.
// ClickHouse parses a literal earlier than 1900-01-01 as that day, so a
// bound between 1677-09-21 and 1900 compares Unix nanoseconds instead, and
// a bound no DateTime64(9) sample can lie beyond is omitted.
func TestLowerMetadataRange_ExactBoundsBefore1900(t *testing.T) {
	t.Parallel()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`up`)
	if err != nil {
		t.Fatal(err)
	}
	start1800 := time.Date(1800, time.January, 1, 0, 0, 0, 0, time.UTC)
	end1850 := time.Date(1850, time.January, 1, 0, 0, 0, 0, time.UTC)
	modern := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	const (
		nanosGe   = "(toUnixTimestamp64Nano(`TimeUnix`) >= ?)"
		nanosLe   = "(toUnixTimestamp64Nano(`TimeUnix`) <= ?)"
		literalGe = "(`TimeUnix` >= parseDateTime64BestEffort("
		literalLe = "(`TimeUnix` <= parseDateTime64BestEffort("
	)
	cases := []struct {
		name       string
		start, end time.Time
		present    []string
		absent     []string
		args       []int64
	}{
		{
			name: "both_bounds_before_1900", start: start1800, end: end1850,
			present: []string{nanosGe, nanosLe}, absent: []string{literalGe, literalLe},
			args: []int64{start1800.UnixNano(), end1850.UnixNano()},
		},
		{
			name: "start_before_1900", start: start1800, end: modern,
			present: []string{nanosGe, literalLe}, absent: []string{literalGe, nanosLe},
			args: []int64{start1800.UnixNano()},
		},
		{
			name: "start_at_1900", start: time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC), end: modern,
			present: []string{literalGe, literalLe}, absent: []string{nanosGe, nanosLe},
		},
		{
			name: "bounds_beyond_storage", start: time.Unix(0, math.MinInt64), end: time.Unix(0, math.MaxInt64),
			absent: []string{nanosGe, nanosLe, literalGe, literalLe},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := LowerMetadataRange(context.Background(), expr, schema.DefaultOTelMetrics(), c.start, c.end)
			if err != nil {
				t.Fatal(err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.present {
				if !strings.Contains(sql, want) {
					t.Errorf("SQL has no %q:\n%s", want, sql)
				}
			}
			for _, unwanted := range c.absent {
				if strings.Contains(sql, unwanted) {
					t.Errorf("SQL has %q:\n%s", unwanted, sql)
				}
			}
			for _, n := range c.args {
				if !slices.Contains(args, any(n)) {
					t.Errorf("args %v do not bind the nanosecond bound %d", args, n)
				}
			}
		})
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
