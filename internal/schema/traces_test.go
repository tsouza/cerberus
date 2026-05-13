package schema

import "testing"

// TestDefaultOTelTracesPinsUpstreamColumns pins every column name
// DefaultOTelTraces() returns against the verbatim string the upstream
// `clickhouseexporter` writes in its `traces_table.sql` template.
//
// Note: the upstream traces DDL does not declare a `ScopeAttributes`
// column; cerberus carries the field but defaults it to the empty
// string so emitters can skip it for the upstream layout.
func TestDefaultOTelTracesPinsUpstreamColumns(t *testing.T) {
	t.Parallel()

	tr := DefaultOTelTraces()

	if tr.SpansTable != "otel_traces" {
		t.Errorf("SpansTable: got %q, want %q", tr.SpansTable, "otel_traces")
	}

	cases := map[string]string{
		"TraceId":            tr.TraceIDColumn,
		"SpanId":             tr.SpanIDColumn,
		"ParentSpanId":       tr.ParentSpanIDColumn,
		"TraceState":         tr.TraceStateColumn,
		"SpanName":           tr.SpanNameColumn,
		"SpanKind":           tr.SpanKindColumn,
		"ServiceName":        tr.ServiceNameColumn,
		"Duration":           tr.DurationColumn,
		"StatusCode":         tr.StatusCodeColumn,
		"StatusMessage":      tr.StatusMessageColumn,
		"SpanAttributes":     tr.AttributesColumn,
		"ResourceAttributes": tr.ResourceAttributesColumn,
		"ScopeName":          tr.ScopeNameColumn,
		"ScopeVersion":       tr.ScopeVersionColumn,
		"Events":             tr.EventsColumn,
		"Links":              tr.LinksColumn,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("traces column %q: got %q, want %q (mismatch against upstream OTel CH Exporter template)", want, got, want)
		}
	}

	// Synthetic / cerberus-local fields.
	if tr.StartTimeColumn != "Timestamp" {
		t.Errorf("StartTimeColumn: got %q, want %q (OTel-CH stores duration; start == Timestamp)", tr.StartTimeColumn, "Timestamp")
	}
	if tr.EndTimeColumn != "Timestamp" {
		t.Errorf("EndTimeColumn: got %q, want %q (synthetic — emitter derives end as Timestamp + Duration when EndTimeColumn == StartTimeColumn)", tr.EndTimeColumn, "Timestamp")
	}
	if tr.TimestampColumn != "Timestamp" {
		t.Errorf("TimestampColumn: got %q, want %q", tr.TimestampColumn, "Timestamp")
	}

	// ScopeAttributesColumn is intentionally empty: upstream traces DDL
	// does not declare it. Custom-schema users may set it explicitly.
	if tr.ScopeAttributesColumn != "" {
		t.Errorf("ScopeAttributesColumn: got %q, want %q (upstream traces_table.sql has no ScopeAttributes column)", tr.ScopeAttributesColumn, "")
	}
}
