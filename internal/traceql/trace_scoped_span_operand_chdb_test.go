//go:build chdb

package traceql_test

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	tempo "github.com/tsouza/cerberus/internal/traceql/ast"
)

// spanOperandSeed is one trace with a root span and one without. The
// rootless trace's spans carry an empty name and an empty service name, so a
// comparison against its rootName / rootServiceName answers true for them
// exactly when that trace-level value reads as "" — reference Tempo's value
// for a trace with no root span.
var spanOperandSeed = []string{
	`CREATE TABLE otel_traces (
		TraceId String,
		SpanId String,
		ParentSpanId String,
		SpanName String DEFAULT '',
		Duration UInt64 DEFAULT 0,
		Timestamp DateTime64(9),
		ResourceAttributes Map(String, String) DEFAULT map(),
		SpanAttributes Map(String, String) DEFAULT map(),
		ServiceName String DEFAULT '',
		StatusCode String DEFAULT '',
		StatusMessage String DEFAULT '',
		SpanKind String DEFAULT '',
		ScopeName String DEFAULT '',
		ScopeVersion String DEFAULT ''
	) ENGINE = Memory`,
	`INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, Timestamp, SpanName, ServiceName, ResourceAttributes) VALUES
		('t1', 'r1', '',   toDateTime64('2024-01-01 00:00:00', 9), 'checkout', 'frontend', map('service.name', 'frontend')),
		('t1', 'c1', 'r1', toDateTime64('2024-01-01 00:00:01', 9), 'checkout', 'backend',  map('service.name', 'backend')),
		('t1', 'c2', 'r1', toDateTime64('2024-01-01 00:00:01', 9), 'charge',   'frontend', map('service.name', 'frontend')),
		('t2', 'o1', 'gone', toDateTime64('2024-01-01 00:00:00', 9), '',       '',         map('service.name', '')),
		('t2', 'o2', 'o1', toDateTime64('2024-01-01 00:00:01', 9), 'pay',      'svc',      map('service.name', 'svc'))`,
}

// TestTraceScopedSpanOperandIndependentOfJoinUseNulls pins that a
// trace-scoped intrinsic compared with a per-span operand answers a rootless
// trace as if its root value were "" under BOTH settings of ClickHouse's
// join_use_nulls. The per-trace root value reaches each span through a LEFT
// JOIN, and a rootless trace has no row on the right: join_use_nulls=0 (the
// server default) fills the missing value with "", but join_use_nulls=1 — a
// legitimate deployment setting — fills it with NULL, which compares as NULL
// and would silently drop every rootless span from `=` and from `!=` alike.
func TestTraceScopedSpanOperandIndependentOfJoinUseNulls(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, stmt := range spanOperandSeed {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`{ rootName = name }`, []string{"c1", "o1", "r1"}},
		{`{ rootName != name }`, []string{"c2", "o2"}},
		{`{ rootServiceName = resource.service.name }`, []string{"c2", "o1", "r1"}},
		{`{ rootServiceName != resource.service.name }`, []string{"c1", "o2"}},
	} {
		for _, setting := range []string{"join_use_nulls = 0", "join_use_nulls = 1"} {
			got := spanOperandMatches(t, db, tc.query, setting)
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s with %s: matched spans %v, want %v", tc.query, setting, got, tc.want)
			}
		}
	}
}

// spanOperandMatches lowers and emits query, runs it with the given
// query-scoped setting, and returns the matched SpanIds sorted.
func spanOperandMatches(t *testing.T, db *sql.DB, query, setting string) []string {
	t.Helper()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	wrapped := "SELECT SpanId FROM (" + strings.TrimSuffix(strings.TrimSpace(sqlText), ";") + ") SETTINGS " + setting
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query %q with %s: %v\n%s", query, setting, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	slices.Sort(out)
	return out
}
