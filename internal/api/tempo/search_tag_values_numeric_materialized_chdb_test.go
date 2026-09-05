//go:build chdb

// chDB-backed correctness proof for cerberus issue #2869: typing
// http.status_code's materialized column as Nullable(Int32) instead of
// LowCardinality(String). Three questions the issue names as needing a
// real ClickHouse (or chDB, matching #2776/#2870's own established
// verification lane) to settle, each pinned by its own test below:
//
//  1. TestToInt32OrNullDefault_GracefulNullOnMalformedOrMissing — the DDL
//     shape itself: `Nullable(Int32) DEFAULT
//     toInt32OrNull(SpanAttributes['http.status_code'])` must resolve to
//     NULL for an absent key or a non-numeric value, never abort the
//     query, exactly like the String-typed materialized columns'
//     DEFAULT never aborts on an absent key.
//  2. TestTraceQLNumericMaterializedColumn_MatchesMapOnlyResult — the
//     coercion-skip in internal/traceql/lower.go's coerceNumericFieldAccess
//     (FieldAccess.MaterializedColumnNumeric) must be result-identical to
//     the map-only path's toFloat64OrNull coercion, INCLUDING the
//     malformed-value row, which must drop from both.
//  3. TestBuildAutoScopeUnionAttributeValuesSQL_NumericArmUnionsWithMapFallbackArm
//     — the #2870 auto-scope UNION ALL, with one arm reading the new
//     Nullable(Int32) column and the other arm (a DIFFERENT map, same key
//     name, unmaterialized there) falling back to the String map
//     subscript, must execute without a ClickHouse type error and return
//     the correct unioned/deduped string set — attrValueArmFrag's
//     toString(...) cast on every materialized arm (search_tag_values.go)
//     is exactly what makes this type-safe; see that function's doc.
package tempo_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// numericMatWindowBase anchors every seeded row well away from wall-clock
// drift, mirroring autoScopeWindowBase's convention in this package's
// sibling chdb test.
var numericMatWindowBase = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

// numericMatStatusCodeColumn is the materialized column name every case
// below routes http.status_code to — the same name
// internal/schema.DefaultMaterializedSpanAttributeColumns would produce.
const numericMatStatusCodeColumn = "__cerberus_materialized_http.status_code"

// numericMatSeedTable declares otel_traces with http.status_code's
// materialized column in its REAL production shape (cerberus issue
// #2869): Nullable(Int32) DEFAULT toInt32OrNull(SpanAttributes[...]),
// never MATERIALIZED — matching
// internal/schema/ddl.renderAddMaterializedAttrColumns' numeric branch
// exactly. MergeTree (not Memory) so the TraceQL-lowered queries below,
// which read from a table the emitter expects to be time-ordered, behave
// the same as production.
const numericMatSeedTable = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String),
    ` + "`" + numericMatStatusCodeColumn + "`" + ` Nullable(Int32) DEFAULT toInt32OrNull(SpanAttributes['http.status_code'])
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// numericMatSeedRow is one seeded span: a TraceId plus the raw
// http.status_code STRING value to write into SpanAttributes (or "" to
// omit the key entirely, the absent-key case).
type numericMatSeedRow struct {
	traceID   string
	statusVal string // "" => key omitted from the map entirely
	omit      bool
}

// numericMatSeedRows covers every value shape toInt32OrNull must handle
// gracefully: a normal 2xx/4xx/5xx numeric value, a value AT the
// predicate boundary, a non-numeric string (the malformed case the issue
// names explicitly), and a span that never carries the key at all (the
// pre-existing "absent from the map" case #2776 already established).
func numericMatSeedRows() []numericMatSeedRow {
	return []numericMatSeedRow{
		{traceID: "trace-200", statusVal: "200"},
		{traceID: "trace-404", statusVal: "404"},
		{traceID: "trace-500", statusVal: "500"},
		{traceID: "trace-boundary", statusVal: "400"},
		{traceID: "trace-malformed", statusVal: "not-a-number"},
		{traceID: "trace-absent", omit: true},
	}
}

// seedNumericMatClient seeds otel_traces with numericMatSeedRows, one
// trace (single span) per second from numericMatWindowBase, and returns
// the chDB client plus the [start, end) window bracketing every seeded
// row.
func seedNumericMatClient(t *testing.T) (*chclienttest.Client, time.Time, time.Time) {
	t.Helper()
	const tsFmt = "2006-01-02 15:04:05.000"

	rows := numericMatSeedRows()
	seed := numericMatSeedTable
	for i, r := range rows {
		ts := numericMatWindowBase.Add(time.Duration(i) * time.Second).Format(tsFmt)
		attrs := "map()"
		if !r.omit {
			attrs = fmt.Sprintf("map('http.status_code', '%s')", r.statusVal)
		}
		seed += fmt.Sprintf(
			"\nINSERT INTO otel_traces (TraceId, SpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode, SpanAttributes)"+
				" VALUES ('%s', '1', 'op', 'Server', 100, toDateTime64('%s', 9), 'Unset', %s);",
			r.traceID, ts, attrs,
		)
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	start := numericMatWindowBase.Add(-time.Minute)
	end := numericMatWindowBase.Add(time.Duration(len(rows))*time.Second + time.Minute)
	return c, start, end
}

// TestToInt32OrNullDefault_GracefulNullOnMalformedOrMissing is the
// central DDL-shape proof the issue's "Verification needed" section
// calls for: the ADD COLUMN's DEFAULT expression must resolve every row
// without erroring the query, mapping a non-numeric value AND an absent
// key to NULL exactly like the pre-existing (String) materialized
// columns map an absent key to the empty string, never abort the read.
func TestToInt32OrNullDefault_GracefulNullOnMalformedOrMissing(t *testing.T) {
	c, _, _ := seedNumericMatClient(t)
	ctx := context.Background()

	// One string column, "<TraceId>:<materialized value or NULL>",
	// keeps this a single-column QueryStrings read (chclienttest's
	// generic string decoder) while still pinning both the TraceId and
	// the column's resolved value per row.
	sqlStr := "SELECT concat(TraceId, ':', ifNull(toString(`" + numericMatStatusCodeColumn + "`), 'NULL')) " +
		"FROM otel_traces ORDER BY TraceId"
	got, err := c.QueryStrings(ctx, sqlStr)
	if err != nil {
		t.Fatalf("query malformed/absent http.status_code rows: %v (the query itself must never error — that is exactly what this test guards)", err)
	}

	want := []string{
		"trace-200:200",
		"trace-404:404",
		"trace-500:500",
		"trace-absent:NULL",
		"trace-boundary:400",
		"trace-malformed:NULL",
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("materialized column values = %v, want %v", got, want)
	}
}

// execTraceQLTraceIDsChDB lowers and executes query against s using the
// chDB client, returning the distinct TraceId values of the matched
// spans — the chDB-client counterpart of internal/schema/ddl's
// execTraceQLTraceIDs (which drives a real ClickHouse driver.Conn).
func execTraceQLTraceIDsChDB(ctx context.Context, t *testing.T, c *chclienttest.Client, query string, s schema.Traces) []string {
	t.Helper()
	expr, err := ast.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	got, err := c.QueryStrings(ctx, fmt.Sprintf("SELECT DISTINCT TraceId FROM (%s)", sqlStr), args...)
	if err != nil {
		t.Fatalf("query %q: %v\nSQL: %s", query, err, sqlStr)
	}
	sort.Strings(got)
	return got
}

// TestTraceQLNumericMaterializedColumn_MatchesMapOnlyResult is cerberus
// issue #2869's central coercion-skip correctness proof, mirroring
// internal/schema/ddl's real-CH
// TestLowerTraceQL_MaterializedColumnRoutingRealCH for #2776: the SAME
// TraceQL query, lowered once against the numeric-materialized schema
// (FieldAccess.MaterializedColumnNumeric skips toFloat64OrNull) and once
// against schema.DefaultOTelTraces() (map-only, still wrapped in
// toFloat64OrNull), must return IDENTICAL matched-trace sets — INCLUDING
// dropping trace-malformed in both, the NULL-drops-row parity the issue's
// "Verification needed" section calls out by name.
func TestTraceQLNumericMaterializedColumn_MatchesMapOnlyResult(t *testing.T) {
	c, _, _ := seedNumericMatClient(t)
	ctx := context.Background()

	materialized := schema.DefaultOTelTraces()
	materialized.SpansTable = "otel_traces"
	materialized.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": numericMatStatusCodeColumn}
	mapOnly := schema.DefaultOTelTraces()
	mapOnly.SpansTable = "otel_traces"

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"eq_string_literal", `{ span.http.status_code = "500" }`, []string{"trace-500"}},
		{"ge_numeric_500", `{ span.http.status_code >= 500 }`, []string{"trace-500"}},
		{"gt_numeric_400_boundary_excluded", `{ span.http.status_code > 400 }`, []string{"trace-404", "trace-500"}},
		{"ge_numeric_400_boundary_included", `{ span.http.status_code >= 400 }`, []string{"trace-404", "trace-500", "trace-boundary"}},
		{"lt_numeric_500_malformed_and_absent_drop", `{ span.http.status_code < 500 }`, []string{"trace-200", "trace-404", "trace-boundary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matGot := execTraceQLTraceIDsChDB(ctx, t, c, tc.query, materialized)
			mapGot := execTraceQLTraceIDsChDB(ctx, t, c, tc.query, mapOnly)
			// First pin against the case's own known-good expectation, so
			// a failure names the actual wrong trace set...
			want := sortedCopy(tc.want)
			if fmt.Sprint(matGot) != fmt.Sprint(want) {
				t.Errorf("materialized-column result = %v, want %v", matGot, want)
			}
			// ...then the load-bearing #2869 assertion: materialized and
			// map-only must agree exactly, proving the coercion-skip
			// changes nothing about the ANSWER, only how it is computed.
			if fmt.Sprint(matGot) != fmt.Sprint(mapGot) {
				t.Errorf("materialized-column result diverges from map-only result:\nmaterialized: %v\nmap-only:     %v", matGot, mapGot)
			}
		})
	}

	t.Run("malformed_and_absent_never_match_any_numeric_predicate", func(t *testing.T) {
		for _, q := range []string{
			`{ span.http.status_code >= 0 }`,
			`{ span.http.status_code < 1000 }`,
			`{ span.http.status_code != 200 }`,
		} {
			got := execTraceQLTraceIDsChDB(ctx, t, c, q, materialized)
			for _, bad := range []string{"trace-malformed", "trace-absent"} {
				for _, g := range got {
					if g == bad {
						t.Errorf("%s matched %s, but a malformed/absent value must NULL-drop under NULL-drops-row semantics; got %v", q, bad, got)
					}
				}
			}
		}
	})
}

// TestBuildAutoScopeUnionAttributeValuesSQL_NumericArmUnionsWithMapFallbackArm
// is the empirical UNION-ALL type-compatibility check the task requires:
// http.status_code materialized ONLY in the SPAN map (Nullable(Int32)),
// while the SAME key name also appears as a plain, unmaterialized
// RESOURCE attribute on some rows — exercising exactly the
// numeric-materialized-arm + map-fallback-arm combination
// buildAutoScopeUnionAttributeValuesSQL must reconcile (cerberus issue
// #2870's UNION ALL). Confirms the query executes without ClickHouse
// rejecting it (NO_COMMON_TYPE would 5xx the request) and returns the
// correct deduped string set from both arms.
func TestBuildAutoScopeUnionAttributeValuesSQL_NumericArmUnionsWithMapFallbackArm(t *testing.T) {
	const seedTable = `CREATE TABLE otel_traces (
    Timestamp DateTime64(9),
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String),
    ` + "`" + numericMatStatusCodeColumn + "`" + ` Nullable(Int32) DEFAULT toInt32OrNull(SpanAttributes['http.status_code'])
) ENGINE = Memory;`

	windowBase := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	type row struct{ spanMapSQL, resourceMapSQL string }
	rows := []row{
		// Span-only, numeric-materialized arm: 200, 500.
		{spanMapSQL: "map('http.status_code','200')", resourceMapSQL: "map()"},
		{spanMapSQL: "map('http.status_code','500')", resourceMapSQL: "map()"},
		// Resource-only, map-fallback arm (no materialized column on
		// this side for this key at all): 999.
		{spanMapSQL: "map()", resourceMapSQL: "map('http.status_code','999')"},
		// BOTH sides carry the key on the SAME row, different values —
		// the numeric arm's "200" and the map arm's "777" must both
		// survive the union, deduped only where the STRING values
		// actually coincide.
		{spanMapSQL: "map('http.status_code','200')", resourceMapSQL: "map('http.status_code','777')"},
		// A malformed span-side value: must contribute NOTHING from the
		// numeric arm (NULL, filtered by materializedColumnPresenceFrag)
		// while its resource-side sibling, if any, still contributes via
		// the map arm — no cross-arm interference.
		{spanMapSQL: "map('http.status_code','abc')", resourceMapSQL: "map('http.status_code','321')"},
	}

	const tsFmt = "2006-01-02 15:04:05.000"
	seed := seedTable
	for i, r := range rows {
		ts := windowBase.Add(time.Duration(i) * time.Second).Format(tsFmt)
		seed += fmt.Sprintf(
			"\nINSERT INTO otel_traces (Timestamp, SpanAttributes, ResourceAttributes) VALUES (toDateTime64('%s', 9), %s, %s);",
			ts, r.spanMapSQL, r.resourceMapSQL,
		)
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	ctx := context.Background()
	start := windowBase.Add(-time.Minute)
	end := windowBase.Add(time.Duration(len(rows))*time.Second + time.Minute)

	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": numericMatStatusCodeColumn}
	// Deliberately NOT setting MaterializedResourceAttributeColumns for
	// this key: the resource side must fall back to the map subscript,
	// which is the map-fallback arm this test targets.

	sqlStr, args := tempo.BuildAttributeValuesSQLForTest(s, "http.status_code", tempo.AttrMapScopeAnyForTest, nil, start, end, nil)
	got, err := c.QueryStrings(ctx, sqlStr, args...)
	if err != nil {
		t.Fatalf("UNION ALL of numeric materialized arm + map-fallback arm failed (likely a ClickHouse NO_COMMON_TYPE / arm-type mismatch): %v\nSQL: %s", err, sqlStr)
	}
	sort.Strings(got)
	want := []string{"200", "321", "500", "777", "999"}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("union result = %v, want %v\nSQL: %s", got, want, sqlStr)
	}
}
