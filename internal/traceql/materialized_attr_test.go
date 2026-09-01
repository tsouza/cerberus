package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// emitTraceQL parses, lowers, and emits query against schema s, failing the
// test on any error along the way.
func emitTraceQL(t *testing.T, query string, s schema.Traces) string {
	t.Helper()
	sqlStr, _ := emitTraceQLWithArgs(t, query, s)
	return sqlStr
}

// emitTraceQLWithArgs is emitTraceQL plus the bound positional args — a
// materialized-column ColumnRef binds no arg for the key (it is a bare
// identifier), while a map subscript binds the key as a `?` positional
// arg, so the two shapes need different assertions.
func emitTraceQLWithArgs(t *testing.T, query string, s schema.Traces) (string, []any) {
	t.Helper()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr, args
}

// TestLowerAttribute_MaterializedColumnRouting pins cerberus issue #2776's
// central routing contract:
//
//   - OFF (schema.DefaultOTelTraces(), no operator opt-in): a span/resource
//     attribute reference emits the plain `<Map>[<key>]` subscript,
//     byte-for-byte unchanged from before the feature landed.
//   - ON (MaterializedSpanAttributeColumns / MaterializedResourceAttributeColumns
//     configured for a key): the SAME query emits a bare top-level
//     ColumnRef instead — the read the materialized column was
//     provisioned for — with NO map subscript / decompression left in the
//     SQL.
//   - A key NOT in the configured map keeps reading the map even when the
//     registry is non-nil — the per-key opt-in, not an all-or-nothing
//     schema switch.
func TestLowerAttribute_MaterializedColumnRouting(t *testing.T) {
	t.Parallel()

	off := schema.DefaultOTelTraces()

	on := schema.DefaultOTelTraces()
	on.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"}
	on.MaterializedResourceAttributeColumns = map[string]string{"k8s.namespace.name": "__cerberus_materialized_k8s.namespace.name"}

	cases := []struct {
		name        string
		query       string
		schema      schema.Traces
		wantMapRead bool   // true: `<Map>`[?] bound-arg subscript; false: bare materialized ColumnRef
		wantMapCol  string // the map column the subscript reads (mapRead cases only)
		wantKeyArg  string // the key bound as a positional arg (mapRead cases only)
		wantColRef  string // the bare column identifier (materialized cases only)
	}{
		{
			name:        "span_key_off_reads_map",
			query:       `{ span.http.status_code = "200" }`,
			schema:      off,
			wantMapRead: true,
			wantMapCol:  "SpanAttributes",
			wantKeyArg:  "http.status_code",
		},
		{
			name:       "span_key_on_reads_column",
			query:      `{ span.http.status_code = "200" }`,
			schema:     on,
			wantColRef: "__cerberus_materialized_http.status_code",
		},
		{
			name:       "resource_key_on_reads_column",
			query:      `{ resource.k8s.namespace.name = "checkout" }`,
			schema:     on,
			wantColRef: "__cerberus_materialized_k8s.namespace.name",
		},
		{
			name:        "unconfigured_key_still_reads_map_even_when_registry_on",
			query:       `{ span.rpc.method = "Get" }`,
			schema:      on,
			wantMapRead: true,
			wantMapCol:  "SpanAttributes",
			wantKeyArg:  "rpc.method",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr, args := emitTraceQLWithArgs(t, tc.query, tc.schema)
			if strings.Contains(sqlStr, "__cerberus_materialized") != !tc.wantMapRead {
				t.Errorf("materialized-column presence mismatch (want present=%v); got SQL: %s", !tc.wantMapRead, sqlStr)
			}
			if tc.wantMapRead {
				wantSubstr := "`" + tc.wantMapCol + "`[?]"
				if !strings.Contains(sqlStr, wantSubstr) {
					t.Errorf("SQL missing map subscript %q; got: %s", wantSubstr, sqlStr)
				}
				if !containsArg(args, tc.wantKeyArg) {
					t.Errorf("bound args missing key %q; got: %v", tc.wantKeyArg, args)
				}
				return
			}
			wantSubstr := "`" + tc.wantColRef + "`"
			if !strings.Contains(sqlStr, wantSubstr) {
				t.Errorf("SQL missing materialized column ref %q; got: %s", wantSubstr, sqlStr)
			}
			if containsArg(args, "http.status_code") || containsArg(args, "k8s.namespace.name") {
				t.Errorf("materialized-column path unexpectedly bound the key as a positional arg: %v", args)
			}
		})
	}
}

// containsArg reports whether args contains a string value equal to want.
func containsArg(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestLowerAttribute_MaterializedColumnNumericCoercion pins that a
// materialized attribute reference stays wrapped in the SAME
// toFloat64OrNull numeric coercion an unmaterialized FieldAccess gets
// (internal/traceql/lower.go's coerceNumericFieldAccess) — proving the
// design choice to carry MaterializedColumn AS a chplan.FieldAccess field
// (rather than swap in a bare chplan.ColumnRef at lowering time) keeps
// every existing FieldAccess-aware pass working unchanged. Without this,
// `span.http.status_code > 400` would compare a LowCardinality(String)
// column directly against an integer literal — a type mismatch ClickHouse
// rejects.
func TestLowerAttribute_MaterializedColumnNumericCoercion(t *testing.T) {
	t.Parallel()

	on := schema.DefaultOTelTraces()
	on.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": "__cerberus_materialized_http.status_code"}

	sqlStr := emitTraceQL(t, `{ span.http.status_code > 400 }`, on)
	want := "toFloat64OrNull(`__cerberus_materialized_http.status_code`)"
	if !strings.Contains(sqlStr, want) {
		t.Errorf("SQL missing numeric coercion around the materialized column (want %q); got: %s", want, sqlStr)
	}
}
