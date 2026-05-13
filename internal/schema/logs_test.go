package schema

import "testing"

// TestDefaultOTelLogsPinsUpstreamColumns pins every column name
// DefaultOTelLogs() returns against the verbatim string the upstream
// `clickhouseexporter` writes in its `logs_table.sql` template.
func TestDefaultOTelLogsPinsUpstreamColumns(t *testing.T) {
	t.Parallel()

	l := DefaultOTelLogs()

	if l.LogsTable != "otel_logs" {
		t.Errorf("LogsTable: got %q, want %q", l.LogsTable, "otel_logs")
	}

	cases := map[string]string{
		"Body":               l.BodyColumn,
		"SeverityText":       l.SeverityColumn,
		"SeverityNumber":     l.SeverityNumberColumn,
		"LogAttributes":      l.AttributesColumn,
		"ResourceAttributes": l.ResourceAttributesColumn,
		"ScopeName":          l.ScopeNameColumn,
		"ScopeVersion":       l.ScopeVersionColumn,
		"ScopeAttributes":    l.ScopeAttributesColumn,
		"Timestamp":          l.TimestampColumn,
		"TraceId":            l.TraceIDColumn,
		"SpanId":             l.SpanIDColumn,
		"TraceFlags":         l.TraceFlagsColumn,
		"ServiceName":        l.ServiceNameColumn,
		"EventName":          l.EventNameColumn,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("logs column %q: got %q, want %q (mismatch against upstream OTel CH Exporter template)", want, got, want)
		}
	}
}
