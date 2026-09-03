//go:build chdb

// chDB-backed correctness roundtrip for cerberus issue #3010's
// attrMapScopeInstrumentation routing. buildAttributeValuesSQL's flat-Map
// shape for this scope is asserted at the Frag level by the
// non-chdb-tagged tests in search_tag_values_test.go; this file proves
// the query, executed against a real (chDB) ClickHouse engine, both:
//
//  1. finds a value that lives ONLY in the instrumentation-scope column
//     (ScopeAttributes) — the case #2850/#3010 exist to fix, since the
//     pre-fix attrMapScopeAny routing never reads that column at all;
//  2. does NOT surface a value planted under the SAME key name in
//     SpanAttributes/ResourceAttributes but absent from ScopeAttributes
//     — the discriminating proof that the query is scope-ISOLATED, not
//     merely scope-labeled: a query that accidentally still unioned all
//     three maps would pass case 1 but fail this one.
//
// Mirrors search_tag_values_autoscope_chdb_test.go's seed/window
// conventions (a Memory-engine otel_traces projection, one row per
// second from a fixed UTC base, chclienttest.NewChDB).
package tempo_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// instrumentationScopeWindowBase anchors every seeded row well away from
// wall-clock drift, mirroring autoScopeWindowBase.
var instrumentationScopeWindowBase = time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)

// instrumentationScopeSeedTable carries all three dynamic-attribute maps
// so a routing regression (falling back to attrMapScopeAny, which reads
// only Span/ResourceAttributes) is distinguishable from correct
// attrMapScopeInstrumentation routing (which reads only ScopeAttributes)
// against the SAME rows.
const instrumentationScopeSeedTable = "CREATE TABLE otel_traces (\n" +
	"    Timestamp DateTime64(9),\n" +
	"    ScopeAttributes Map(String, String),\n" +
	"    SpanAttributes Map(String, String),\n" +
	"    ResourceAttributes Map(String, String)\n" +
	") ENGINE = Memory;"

// TestBuildAttributeValuesSQL_ChDB_InstrumentationScope_IsolatedFromSpanResource
// seeds four rows for the key `otel.scope.name`:
//   - r0/r1: the value lives in ScopeAttributes ONLY — the values a
//     correct instrumentation-scope query must return.
//   - r2: the SAME key name carries a DIFFERENT value in SpanAttributes,
//     with ScopeAttributes empty on that row — must NOT surface.
//   - r3: same key name, DIFFERENT value in ResourceAttributes, with
//     ScopeAttributes empty on that row — must NOT surface either.
func TestBuildAttributeValuesSQL_ChDB_InstrumentationScope_IsolatedFromSpanResource(t *testing.T) {
	const tsFmt = "2006-01-02 15:04:05.000"
	rows := []struct{ scopeMapSQL, spanMapSQL, resourceMapSQL string }{
		{scopeMapSQL: "map('otel.scope.name','otelcol-contrib')", spanMapSQL: "map()", resourceMapSQL: "map()"},
		{scopeMapSQL: "map('otel.scope.name','otelcol-collector')", spanMapSQL: "map()", resourceMapSQL: "map()"},
		{scopeMapSQL: "map()", spanMapSQL: "map('otel.scope.name','span-leaked-value')", resourceMapSQL: "map()"},
		{scopeMapSQL: "map()", spanMapSQL: "map()", resourceMapSQL: "map('otel.scope.name','resource-leaked-value')"},
	}
	seed := instrumentationScopeSeedTable
	for i, r := range rows {
		ts := instrumentationScopeWindowBase.Add(time.Duration(i) * time.Second).Format(tsFmt)
		seed += "\nINSERT INTO otel_traces (Timestamp, ScopeAttributes, SpanAttributes, ResourceAttributes) VALUES" +
			" (toDateTime64('" + ts + "', 9), " + r.scopeMapSQL + ", " + r.spanMapSQL + ", " + r.resourceMapSQL + ");"
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	start := instrumentationScopeWindowBase.Add(-time.Minute)
	end := instrumentationScopeWindowBase.Add(time.Duration(len(rows))*time.Second + time.Minute)

	s := schema.DefaultOTelTraces()
	s.ScopeAttributesColumn = "ScopeAttributes"

	sqlStr, args := tempo.BuildAttributeValuesSQLForTest(
		s, "otel.scope.name", tempo.AttrMapScopeInstrumentationForTest, nil, start, end,
	)

	got, err := c.QueryStrings(context.Background(), sqlStr, args...)
	if err != nil {
		t.Fatalf("query failed: %v\nSQL: %s", err, sqlStr)
	}
	sort.Strings(got)

	want := []string{"otelcol-collector", "otelcol-contrib"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (SQL: %s)", got, want, sqlStr)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v (SQL: %s)", got, want, sqlStr)
			break
		}
	}
	for _, leaked := range []string{"span-leaked-value", "resource-leaked-value"} {
		for _, v := range got {
			if v == leaked {
				t.Errorf("instrumentation-scope query must not surface a Span/ResourceAttributes-only value, got %v", got)
			}
		}
	}
}
