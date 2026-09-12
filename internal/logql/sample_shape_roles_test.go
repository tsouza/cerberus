package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

func customRoleSamplePlan() chplan.Node {
	roles := []chplan.Column{
		{Name: "metric_physical", Role: chplan.RoleMetricName},
		{Name: "attrs_physical", Role: chplan.RoleAttributes},
		{Name: "time_physical", Role: chplan.RoleTimestamp},
		{Name: "value_physical", Role: chplan.RoleValue},
	}
	project := &chplan.Project{
		Input: &chplan.OneRow{},
		Roles: roles,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: roles[0].Name},
			{Expr: &chplan.FuncCall{Fn: chplan.FnMap}, Alias: roles[1].Name},
			{Expr: chplan.NowNano(), Alias: roles[2].Name},
			{Expr: &chplan.LitFloat{V: 1}, Alias: roles[3].Name},
		},
	}
	// Limit preserves RowType and was deliberately absent from the old
	// concrete-node recursion. Role consumers must need no wrapper arm.
	return &chplan.Limit{Input: project, Count: 1}
}

func TestLogSampleRolesSurviveUnlistedWrapper(t *testing.T) {
	t.Parallel()
	plan := customRoleSamplePlan()
	cols, err := logSampleColumns(plan, schema.DefaultOTelLogs())
	if err != nil {
		t.Fatalf("logSampleColumns: %v", err)
	}
	if cols.attrsCol != "attrs_physical" || cols.valueCol != "value_physical" {
		t.Fatalf("resolved columns = attrs %q value %q", cols.attrsCol, cols.valueCol)
	}

	projected, err := (&Lang{Schema: schema.DefaultOTelLogs()}).ProjectSamples(plan, engine.Meta{IsMetric: true})
	if err != nil {
		t.Fatalf("ProjectSamples: %v", err)
	}
	p := projected.(*chplan.Project)
	for i, want := range []string{"metric_physical", "attrs_physical", "time_physical", "value_physical"} {
		ref, ok := p.Projections[i].Expr.(*chplan.ColumnRef)
		if !ok || ref.Name != want {
			t.Fatalf("projection %d = %#v, want ColumnRef(%q)", i, p.Projections[i].Expr, want)
		}
	}

	if !isVariantPlan(&chplan.UnionAll{Inputs: []chplan.Node{plan}}) {
		t.Fatal("variant arm behind schema-preserving wrapper not recognised")
	}
}

func TestLogSampleRolesTakePrecedenceOverMatrixFallback(t *testing.T) {
	t.Parallel()
	plan := customRoleSamplePlan()
	project := plan.(*chplan.Limit).Input.(*chplan.Project)
	project.Input = &chplan.RangeWindow{
		Input:      &chplan.OneRow{},
		OuterRange: 1,
	}

	cols, err := logSampleColumns(plan, schema.DefaultOTelLogs())
	if err != nil {
		t.Fatalf("logSampleColumns: %v", err)
	}
	metric, ok := cols.metricName.(*chplan.ColumnRef)
	if !ok || metric.Name != "metric_physical" {
		t.Fatalf("metric column = %#v, want metric_physical", cols.metricName)
	}
	timestamp, ok := cols.timeExpr.(*chplan.ColumnRef)
	if !ok || timestamp.Name != "time_physical" {
		t.Fatalf("timestamp column = %#v, want time_physical", cols.timeExpr)
	}
	if cols.attrsCol != "attrs_physical" || cols.valueCol != "value_physical" {
		t.Fatalf("resolved columns = attrs %q value %q", cols.attrsCol, cols.valueCol)
	}
}

func TestResolveLogSampleShapeRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()
	valid := []chplan.Column{
		{Name: "m", Role: chplan.RoleMetricName},
		{Name: "a", Role: chplan.RoleAttributes},
		{Name: "t", Role: chplan.RoleTimestamp},
		{Name: "v", Role: chplan.RoleValue},
	}
	tests := map[string]chplan.Schema{
		"open":      {Columns: valid, Open: true},
		"missing":   {Columns: valid[:3]},
		"duplicate": {Columns: append(append([]chplan.Column{}, valid...), chplan.Column{Name: "v2", Role: chplan.RoleValue})},
		"unnamed":   {Columns: append(append([]chplan.Column{}, valid[:3]...), chplan.Column{Role: chplan.RoleValue})},
	}
	for name, row := range tests {
		row := row
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := resolveLogSampleShape(row); err == nil {
				t.Fatal("resolveLogSampleShape accepted malformed schema")
			}
		})
	}
}
