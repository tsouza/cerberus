package traceql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestNestedSetAnnotatePreservesClosedChildIdentities(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	child := &chplan.Project{
		Input: &chplan.Scan{Table: "derived_spans"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}, Alias: "derived_trace"},
			{Expr: &chplan.ColumnRef{Name: s.SpanIDColumn}, Alias: "derived_span"},
		},
		Roles: []chplan.Column{
			{Name: "derived_trace", Role: chplan.RoleTraceID},
			{Name: "derived_span", Role: chplan.RoleSpanID},
		},
	}

	plan := nestedSetAnnotate(child, s)
	if plan.Input != child {
		t.Fatal("nested-set constructor replaced an already-closed child")
	}
	got := plan.Input.RowType()
	traceID, traceOK := got.Find(chplan.RoleTraceID)
	spanID, spanOK := got.Find(chplan.RoleSpanID)
	if !traceOK || traceID.Name != "derived_trace" || !spanOK || spanID.Name != "derived_span" {
		t.Fatalf("child identities = (%#v, %#v), want derived trace/span roles", traceID, spanID)
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"m.`derived_trace` = ns.`" + s.TraceIDColumn + "`",
		"m.`derived_span` = ns.`" + s.SpanIDColumn + "`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing child-to-lookup identity join %q:\n%s", want, sql)
		}
	}
}

func TestNestedSetAnnotateClosesOpenInputWithConfiguredIdentities(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	open := spanScan(s)
	plan := nestedSetAnnotate(open, s)
	if plan.Input == open {
		t.Fatal("nested-set constructor did not close an open lowering input")
	}
	got := plan.Input.RowType()
	if got.Open {
		t.Fatal("nested-set constructor left lowering input schema open")
	}
	traceID, traceOK := got.Find(chplan.RoleTraceID)
	spanID, spanOK := got.Find(chplan.RoleSpanID)
	if !traceOK || traceID.Name != s.TraceIDColumn || !spanOK || spanID.Name != s.SpanIDColumn {
		t.Fatalf("closed identities = (%#v, %#v), want configured trace/span roles", traceID, spanID)
	}
}
