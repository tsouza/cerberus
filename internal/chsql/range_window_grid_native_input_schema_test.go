package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func nativeMatrixInputProject() *chplan.Project {
	return &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: "series"}, Alias: "raw_labels"},
			{Expr: &chplan.LitInt{V: 1}, Alias: "sample_time"},
			{Expr: &chplan.LitFloat{V: 2}, Alias: "sample_value"},
		},
		Roles: []chplan.Column{
			{Name: "raw_labels", Role: chplan.RoleAttributes},
			{Name: "sample_time", Role: chplan.RoleTimestamp},
			{Name: "sample_value", Role: chplan.RoleValue},
		},
	}
}

func nativeMatrixPlan(input chplan.Node) *chplan.RangeWindowGridNative {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindowGridNative{
		Input: input, Func: "rate", Range: time.Minute, Step: time.Minute,
		Start: start, End: start.Add(time.Minute),
		TimestampColumn: "public_time", ValueColumn: "public_value",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "raw_labels"}},
	}
}

func TestRangeWindowGridNativeResolvesMatrixInputsAndPreservesAliases(t *testing.T) {
	plan := nativeMatrixPlan(nativeMatrixInputProject())
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{"(`sample_time`, `sample_value`)", "AS `public_time`", "AS `public_value`"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL missing %q: %s", want, sql)
		}
	}
	if strings.Contains(sql, "(`public_time`, `public_value`)") {
		t.Fatalf("SQL reads output aliases as physical inputs: %s", sql)
	}
	row := plan.RowType()
	if timestamp, ok := row.Find(chplan.RoleTimestamp); !ok || timestamp.Name != "public_time" {
		t.Fatalf("timestamp output = (%v, %v), want public_time", timestamp, ok)
	}
	if value, ok := row.Find(chplan.RoleValue); !ok || value.Name != "public_value" {
		t.Fatalf("value output = (%v, %v), want public_value", value, ok)
	}
}

func TestRangeWindowGridNativeRecollapseUsesResolvedMatrixInputs(t *testing.T) {
	plan := nativeMatrixPlan(nativeMatrixInputProject())
	plan.Recollapse = []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "raw_labels"}, Alias: "labels"}}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "(`sample_time`, `sample_value`)") || !strings.Contains(sql, "AS `public_value`") {
		t.Fatalf("recollapse SQL lost physical inputs or output alias: %s", sql)
	}
}

func TestRangeWindowGridNativeRejectsMalformedMatrixInputSchema(t *testing.T) {
	closed := func(columns []chplan.Projection, roles []chplan.Column) chplan.Node {
		return &chplan.Project{Input: &chplan.OneRow{}, Projections: columns, Roles: roles}
	}
	for name, input := range map[string]chplan.Node{
		"open":                &chplan.Scan{Roles: []chplan.Column{{Name: "ts", Role: chplan.RoleTimestamp}, {Name: "v", Role: chplan.RoleValue}}},
		"missing timestamp":   closed([]chplan.Projection{{Expr: &chplan.LitFloat{V: 1}, Alias: "v"}}, []chplan.Column{{Name: "v", Role: chplan.RoleValue}}),
		"missing value":       closed([]chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "ts"}}, []chplan.Column{{Name: "ts", Role: chplan.RoleTimestamp}}),
		"duplicate timestamp": closed([]chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "a"}, {Expr: &chplan.LitInt{V: 2}, Alias: "b"}, {Expr: &chplan.LitFloat{V: 3}, Alias: "v"}}, []chplan.Column{{Name: "a", Role: chplan.RoleTimestamp}, {Name: "b", Role: chplan.RoleTimestamp}, {Name: "v", Role: chplan.RoleValue}}),
		"duplicate value":     closed([]chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "ts"}, {Expr: &chplan.LitFloat{V: 2}, Alias: "a"}, {Expr: &chplan.LitFloat{V: 3}, Alias: "b"}}, []chplan.Column{{Name: "ts", Role: chplan.RoleTimestamp}, {Name: "a", Role: chplan.RoleValue}, {Name: "b", Role: chplan.RoleValue}}),
		"conflicting name":    closed([]chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "shared"}, {Expr: &chplan.LitFloat{V: 2}, Alias: "shared"}}, []chplan.Column{{Name: "shared", Role: chplan.RoleTimestamp}, {Name: "shared", Role: chplan.RoleValue}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Emit(context.Background(), nativeMatrixPlan(input)); err == nil {
				t.Fatal("malformed child schema accepted")
			}
		})
	}
}
