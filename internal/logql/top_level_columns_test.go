package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestTopLevelLogColumnFor_DefaultOTel pins the recognised set for the
// default OTel-CH logs schema. The bug task #218 reported (every Log
// volume / Log rate `by(SeverityText)` panel collapsing to one series)
// stems from a missing entry here: any top-level OTel-CH scalar column
// the schema names must surface so the inner identity wrap and outer
// MapAccess agree on which labels resolve from the column vs from
// ResourceAttributes.
func TestTopLevelLogColumnFor_DefaultOTel(t *testing.T) {
	s := schema.DefaultOTelLogs()
	cases := []struct {
		label string
		want  string
		match bool
	}{
		// Recognised: every scalar (non-Map) top-level column on the
		// default OTel-CH schema. Each entry doubles as a guard
		// against a schema-field rename silently dropping a column
		// out of the recognised set.
		{"SeverityText", "SeverityText", true},
		{"SeverityNumber", "SeverityNumber", true},
		{"ServiceName", "ServiceName", true},
		{"ScopeName", "ScopeName", true},
		{"ScopeVersion", "ScopeVersion", true},
		{"EventName", "EventName", true},
		{"TraceId", "TraceId", true},
		{"SpanId", "SpanId", true},
		{"TraceFlags", "TraceFlags", true},

		// Not recognised: Map-typed columns (users group by keys
		// inside those maps, never by the map as a whole) and the
		// Body / Timestamp / table-name slots (never legitimate
		// group-by targets).
		{"ResourceAttributes", "", false},
		{"LogAttributes", "", false},
		{"ScopeAttributes", "", false},
		{"Body", "", false},
		{"Timestamp", "", false},

		// Stream attribute keys (live inside ResourceAttributes) fall
		// through to the attribute-map lookup.
		{"job", "", false},
		{"namespace", "", false},

		// Empty label name short-circuits to a no-match so a
		// custom-schema user who blanks out a column doesn't snag an
		// empty-label query.
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := topLevelLogColumnFor(c.label, s)
		if ok != c.match || got != c.want {
			t.Errorf("topLevelLogColumnFor(%q) = (%q, %v); want (%q, %v)",
				c.label, got, ok, c.want, c.match)
		}
	}
}

// TestTopLevelLogColumnFor_CustomSchema verifies the helper reads from
// the schema fields rather than a static name allow-list — a custom
// schema that renames SeverityColumn (e.g. an ingestion pipeline that
// writes severity to a `level_text` column instead) automatically gets
// the new name recognised here, with the old default name no longer
// matching.
func TestTopLevelLogColumnFor_CustomSchema(t *testing.T) {
	s := schema.DefaultOTelLogs()
	s.SeverityColumn = "level_text"

	if got, ok := topLevelLogColumnFor("level_text", s); !ok || got != "level_text" {
		t.Errorf("topLevelLogColumnFor(level_text) on custom schema = (%q, %v); want (level_text, true)", got, ok)
	}
	if got, ok := topLevelLogColumnFor("SeverityText", s); ok {
		t.Errorf("topLevelLogColumnFor(SeverityText) on custom schema = (%q, %v); want (\"\", false) — schema field renamed away from default", got, ok)
	}
}

// TestTopLevelLogColumnFor_BlankedFieldNoMatch pins the "empty schema
// field doesn't snag an empty-label query" invariant — a custom schema
// that blanks out EventNameColumn must not collapse `by("")` lookups
// into a spurious EventName match.
func TestTopLevelLogColumnFor_BlankedFieldNoMatch(t *testing.T) {
	s := schema.DefaultOTelLogs()
	s.EventNameColumn = ""

	if got, ok := topLevelLogColumnFor("", s); ok {
		t.Errorf("topLevelLogColumnFor(\"\") with blanked EventName = (%q, %v); want (\"\", false)", got, ok)
	}
}

// TestTopLevelColumnsReferencedBy pins the order-preserving,
// dedup-on-column-name behaviour the inner identity wrap depends on
// for a deterministic map-literal shape.
func TestTopLevelColumnsReferencedBy(t *testing.T) {
	s := schema.DefaultOTelLogs()

	got := topLevelColumnsReferencedBy([]string{"SeverityText", "job", "ServiceName", "SeverityText", "namespace"}, s)
	want := []string{"SeverityText", "ServiceName"}

	if len(got) != len(want) {
		t.Fatalf("topLevelColumnsReferencedBy len = %d (%v); want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("topLevelColumnsReferencedBy[%d] = %q; want %q", i, got[i], want[i])
		}
	}

	if got := topLevelColumnsReferencedBy(nil, s); got != nil {
		t.Errorf("topLevelColumnsReferencedBy(nil) = %v; want nil", got)
	}
	if got := topLevelColumnsReferencedBy([]string{"job", "namespace"}, s); len(got) != 0 {
		t.Errorf("topLevelColumnsReferencedBy(no-top-level) = %v; want empty", got)
	}
}
