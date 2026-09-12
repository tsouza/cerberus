package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func rangeWindowTimestampTestScan(table, timestamp string, extra ...string) *chplan.Scan {
	columns := []string{"Attributes", timestamp, "Value"}
	columns = append(columns, extra...)
	return &chplan.Scan{Table: table, Columns: columns, Roles: []chplan.Column{{Name: timestamp, Role: chplan.RoleTimestamp}}}
}

func TestRangeWindowResolvesPhysicalInputTimestampAndPreservesOutputAlias(t *testing.T) {
	input := &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitInt{V: 1}, Alias: "labels"},
			{Expr: &chplan.LitInt{V: 2}, Alias: "physical_time"},
			{Expr: &chplan.LitFloat{V: 3}, Alias: "value"},
		},
		Roles: []chplan.Column{
			{Name: "labels", Role: chplan.RoleAttributes},
			{Name: "physical_time", Role: chplan.RoleTimestamp},
			{Name: "value", Role: chplan.RoleValue},
		},
	}
	plan := &chplan.RangeWindow{
		Input: input, Func: "sum_over_time", TimestampColumn: "public_time", ValueColumn: "value",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "labels"}}, InstantScanBounded: true,
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`physical_time`") {
		t.Fatalf("SQL does not read physical timestamp: %s", sql)
	}
	if strings.Contains(sql, "`public_time`") {
		t.Fatalf("instant SQL reads output-only timestamp alias: %s", sql)
	}

	matrix := *plan
	matrix.Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	matrix.End = matrix.Start.Add(time.Minute)
	matrix.Step = time.Minute
	matrix.OuterRange = time.Minute
	matrixSQL, _, err := Emit(context.Background(), &matrix)
	if err != nil {
		t.Fatalf("Emit matrix: %v", err)
	}
	if !strings.Contains(matrixSQL, "`physical_time`") {
		t.Fatalf("matrix SQL does not read physical timestamp: %s", matrixSQL)
	}
	if !strings.Contains(matrixSQL, "anchor_ts AS `public_time`") {
		t.Fatalf("matrix SQL does not preserve public timestamp alias: %s", matrixSQL)
	}
}

func TestRangeWindowRejectsMalformedInputTimestampSchema(t *testing.T) {
	for name, input := range map[string]chplan.Node{
		"open":      &chplan.Scan{Roles: []chplan.Column{{Name: "ts", Role: chplan.RoleTimestamp}}},
		"missing":   &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "v"}}},
		"duplicate": &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "a"}, {Expr: &chplan.LitInt{V: 2}, Alias: "b"}}, Roles: []chplan.Column{{Name: "a", Role: chplan.RoleTimestamp}, {Name: "b", Role: chplan.RoleTimestamp}}},
		"unnamed":   &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}}}, Roles: []chplan.Column{{Role: chplan.RoleTimestamp}}},
		"conflicting": &chplan.Project{
			Input: &chplan.OneRow{},
			Projections: []chplan.Projection{
				{Expr: &chplan.LitInt{V: 1}, Alias: "shared"},
				{Expr: &chplan.LitInt{V: 2}, Alias: "shared"},
			},
			Roles: []chplan.Column{{Name: "shared", Role: chplan.RoleTimestamp}, {Name: "shared", Role: chplan.RoleValue}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRangeWindowInputTimestamp(&chplan.RangeWindow{Input: input}); err == nil {
				t.Fatal("malformed timestamp schema accepted")
			}
		})
	}
}

func TestNestedRangeWindowResolvesInnerGridAnchor(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		end       time.Time
		offset    time.Duration
		stepAlign bool
	}{
		{name: "off-grid with offset", end: start.Add(10*time.Minute + 17*time.Second), offset: 3*time.Minute + 7*time.Second, stepAlign: true},
		{name: "on-grid with aligned offset", end: start.Add(12 * time.Minute), offset: 3 * time.Minute, stepAlign: true},
		{name: "request-grid aligned", end: start.Add(12 * time.Minute), offset: 0, stepAlign: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &chplan.RangeWindow{
				Input:           rangeWindowTimestampTestScan("otel_metrics_gauge", "physical_time"),
				Func:            "sum_over_time",
				Range:           time.Minute,
				OuterRange:      9 * time.Minute,
				Step:            90 * time.Second,
				StepAlign:       tc.stepAlign,
				End:             tc.end,
				Offset:          tc.offset,
				TimestampColumn: "inner_public_time",
				ValueColumn:     "Value",
				GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			}
			outer := &chplan.RangeWindow{
				Input:              inner,
				Func:               "max_over_time",
				Range:              3 * time.Minute,
				End:                tc.end,
				TimestampColumn:    "outer_public_time",
				ValueColumn:        "Value",
				GroupBy:            []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				InstantScanBounded: true,
			}

			if got := rangeWindowInputTimestampColumn(outer); got != chplan.RangeWindowAnchorColumn {
				t.Fatalf("nested temporal driver = %q, want %q", got, chplan.RangeWindowAnchorColumn)
			}
			if err := validateRangeWindowInputTimestamp(outer); err != nil {
				t.Fatalf("validate nested RangeWindow: %v", err)
			}
			sql, _, err := Emit(context.Background(), outer)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, "`anchor_ts` >") || !strings.Contains(sql, "`anchor_ts` <=") {
				t.Fatalf("outer window membership does not read the inner grid anchor: %s", sql)
			}
			if !strings.Contains(sql, "AS `inner_public_time`") {
				t.Fatalf("inner public timestamp alias was not preserved: %s", sql)
			}
		})
	}
}

func TestNestedRangeWindowRejectsMissingGridAnchor(t *testing.T) {
	inner := &chplan.RangeWindow{
		Input:           rangeWindowTimestampTestScan("otel_metrics_gauge", "physical_time"),
		Func:            "sum_over_time",
		Range:           time.Minute,
		TimestampColumn: "inner_public_time",
		ValueColumn:     "Value",
	}
	outer := &chplan.RangeWindow{Input: inner}
	if err := validateRangeWindowInputTimestamp(outer); err == nil {
		t.Fatal("nested instant RangeWindow without a grid anchor was accepted")
	}
}
