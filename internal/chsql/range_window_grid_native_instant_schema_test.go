package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestRangeWindowGridNativeInstantResolvesPhysicalValueAndPreservesOutputAlias(t *testing.T) {
	t.Parallel()
	input := &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: &chplan.FuncCall{Fn: chplan.FnMap}, Alias: "labels"},
			{Expr: chplan.NowNano(), Alias: "sample_time"},
			{Expr: &chplan.LitFloat{V: 1}, Alias: "physical_value"},
		},
		Roles: []chplan.Column{
			{Name: "labels", Role: chplan.RoleAttributes},
			{Name: "sample_time", Role: chplan.RoleTimestamp},
			{Name: "physical_value", Role: chplan.RoleValue},
		},
	}
	plan := &chplan.RangeWindowGridNativeInstant{
		Input:           input,
		Func:            "rate",
		Range:           5 * time.Minute,
		Anchor:          time.Unix(1_700_000_000, 0).UTC(),
		TimestampColumn: "sample_time",
		ValueColumn:     "public_value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "labels"}},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "(`sample_time`, `physical_value`)") {
		t.Fatalf("native aggregate does not read physical child value: %s", sql)
	}
	if !strings.Contains(sql, "AS `public_value`") {
		t.Fatalf("native output does not preserve public value alias: %s", sql)
	}
}

func TestRangeWindowGridNativeInstantRejectsMissingChildValueRole(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindowGridNativeInstant{
		Input: &chplan.Scan{
			Table:   "samples",
			Columns: []string{"sample_time"},
			Roles:   []chplan.Column{{Name: "sample_time", Role: chplan.RoleTimestamp}},
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		Anchor:          time.Unix(1_700_000_000, 0).UTC(),
		TimestampColumn: "sample_time",
		ValueColumn:     "public_value",
	}
	if _, _, err := chsql.Emit(context.Background(), plan); err == nil {
		t.Fatal("Emit accepted child schema without RoleValue")
	}
}
