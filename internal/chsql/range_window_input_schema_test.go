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
