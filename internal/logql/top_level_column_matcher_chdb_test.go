//go:build chdb

package logql

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestStreamMatcherOnEveryTopLevelColumn drives a stream matcher against
// EVERY top-level OTel-CH scalar column [resourceFallbackColumn] hoists,
// over a real engine, and asserts each one selects the seeded row.
//
// The set is closed on purpose. Two of the nine columns are numeric
// (SeverityNumber is Int32, TraceFlags is UInt8) and the fallback
// expression NULLed the column out against the STRING literal `”`, so
// `{SeverityNumber="9"}` and `{TraceFlags="0"}` reached ClickHouse as
// `Code: 32 … while converting ” to Int32` — a 502 where reference Loki
// answers with the matching streams. Nothing tested the numeric two: the
// seven string columns work either way, and an emitted-SQL assertion
// cannot see a type error at all. Only executing every column can.
func TestStreamMatcherOnEveryTopLevelColumn(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The OTel-CH logs layout, with the two numeric columns at their
	// real declared types.
	if _, err := db.Exec("CREATE TABLE otel_logs (" +
		"Timestamp DateTime64(9), Body String, " +
		"SeverityText LowCardinality(String) DEFAULT '', SeverityNumber Int32 DEFAULT 0, " +
		"ScopeName String DEFAULT '', ScopeVersion String DEFAULT '', " +
		"TraceId String DEFAULT '', SpanId String DEFAULT '', TraceFlags UInt8 DEFAULT 0, " +
		"ServiceName String DEFAULT '', " +
		"ResourceAttributes Map(String,String), LogAttributes Map(String,String)" +
		") ENGINE = Memory"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO otel_logs " +
		"(Timestamp, Body, SeverityText, SeverityNumber, ScopeName, ScopeVersion, " +
		" TraceId, SpanId, TraceFlags, ServiceName, ResourceAttributes) VALUES " +
		"(now64(9), 'a', 'INFO', 9, 'scope', 'v1', 'abc', 'def', 1, 'api', map())"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := schema.DefaultOTelLogs()
	// One matcher per hoisted column, keyed by the label a client
	// spells. `service_name` is the underscore alias
	// [resourceFallbackColumn] maps onto ServiceName; the other eight
	// are spelled with the column name.
	cases := []struct{ label, value string }{
		{s.SeverityColumn, "INFO"},
		{s.SeverityNumberColumn, "9"},
		{s.ScopeNameColumn, "scope"},
		{s.ScopeVersionColumn, "v1"},
		{s.TraceIDColumn, "abc"},
		{s.SpanIDColumn, "def"},
		{s.TraceFlagsColumn, "1"},
		{"service_name", "api"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			if col := resourceFallbackColumn(s, tc.label); col == "" {
				t.Fatalf("%q is not a hoisted top-level column on the default schema — "+
					"the case list has drifted from resourceFallbackColumn", tc.label)
			}
			query := `{` + tc.label + `="` + tc.value + `"}`
			expr, err := syntax.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			var n int
			if err := db.QueryRow("SELECT count() FROM ("+sqlStr+")", args...).Scan(&n); err != nil {
				t.Fatalf("%s: ClickHouse refused the emitted matcher (reference Loki answers it): %v", query, err)
			}
			if n != 1 {
				t.Fatalf("%s matched %d rows, want 1 — the seeded row carries %s=%q",
					query, n, tc.label, tc.value)
			}
		})
	}
}
