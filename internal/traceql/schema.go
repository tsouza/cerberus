package traceql

import (
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
