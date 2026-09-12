package traceql

import (
	"sort"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func spanScan(s schema.Traces) *chplan.Scan {
	return &chplan.Scan{Table: s.SpansTable, Roles: []chplan.Column{
		{Name: s.TraceIDColumn, Role: chplan.RoleTraceID},
		{Name: s.SpanIDColumn, Role: chplan.RoleSpanID},
		{Name: s.ParentSpanIDColumn, Role: chplan.RoleParentSpanID},
		{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
	}}
}

// closeNestedSetInput establishes the exact child-row contract consumed by
// NestedSetAnnotate without changing the independent SpansTable schema used by
// its recursive numbering walk. Attribute predicates remain below this
// projection and can therefore still read arbitrary storage columns.
func closeNestedSetInput(input chplan.Node, s schema.Traces) chplan.Node {
	// A derived child already owns its physical identity names. Preserve that
	// closed contract verbatim; configured names below describe SpansTable and
	// are only the fallback needed to close an open storage-backed rowset.
	if child := input.RowType(); !child.Open {
		return input
	}
	names := []string{
		s.TraceIDColumn, s.SpanIDColumn, s.ParentSpanIDColumn,
		s.TraceStateColumn, s.SpanNameColumn, s.SpanKindColumn,
		s.ServiceNameColumn, s.DurationColumn, s.StartTimeColumn,
		s.StatusCodeColumn, s.StatusMessageColumn, s.AttributesColumn,
		s.ResourceAttributesColumn, s.ScopeNameColumn, s.ScopeVersionColumn,
		s.ScopeAttributesColumn, s.EventsColumn, s.LinksColumn, s.TimestampColumn,
	}
	for _, columns := range []map[string]string{
		s.MaterializedSpanAttributeColumns,
		s.MaterializedResourceAttributeColumns,
	} {
		materialized := make([]string, 0, len(columns))
		for _, name := range columns {
			materialized = append(materialized, name)
		}
		sort.Strings(materialized)
		names = append(names, materialized...)
	}
	seen := make(map[string]struct{}, len(names))
	projections := make([]chplan.Projection, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}})
	}
	return &chplan.Project{
		Input:       input,
		Projections: projections,
		Roles: []chplan.Column{
			{Name: s.TraceIDColumn, Role: chplan.RoleTraceID},
			{Name: s.SpanIDColumn, Role: chplan.RoleSpanID},
		},
	}
}

func nestedSetAnnotate(input chplan.Node, s schema.Traces) *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              closeNestedSetInput(input, s),
		SpansTable:         s.SpansTable,
		TraceIDColumn:      s.TraceIDColumn,
		SpanIDColumn:       s.SpanIDColumn,
		ParentSpanIDColumn: s.ParentSpanIDColumn,
		TimestampColumn:    s.TimestampColumn,
	}
}
