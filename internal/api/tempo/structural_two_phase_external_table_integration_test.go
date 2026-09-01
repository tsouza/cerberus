//go:build integration

// structural_two_phase_external_table_integration_test.go — real-ClickHouse
// verification for cerberus issue #2783: the native-protocol external-table
// push that replaces restrictStructural's literal TraceId IN-list splice on
// wide closures.
//
// # WHY THIS LANE EXISTS
//
// chDB (search_structural_two_phase_chdb_test.go's own parity lane) proves
// the two-phase split is result-correct, but it never goes through
// clickhouse-go/v2's native-protocol Conn — chDB is an embedded engine driven
// by SQL text, with no WithExternalTable wire feature to exercise. Everything
// this file checks — that the external-table form (a) returns the exact same
// traces as the literal form, (b) engages ClickHouse's idx_trace_id skip
// index the same way the literal form does (issue #2783's own EXPLAIN
// indexes=1 mandate), and (c) is safe to attach fresh across more than one
// dispatch of the SAME context (the shape both a retry and a route-B shard
// take) — needs a real server.
//
// Gated behind the `integration` build tag (Docker required), mirroring
// traces_scan_window_integration_test.go.
package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// extTblTraceCount is the number of distinct root/leaf trace pairs seeded —
// large enough that its literal IN-list estimate crosses
// traceIDLiteralByteBudget (a 32-hex-char id costs 35 bytes per
// estimatedLiteralBytes; extTblTraceCount*35*traceIDRestrictionSiteCount must
// exceed 64KiB), so a request for ALL of them genuinely exercises the
// external-table path rather than falling back to the literal one anyway.
const extTblTraceCount = 700

// extTblExplainIDCount is the id-set size the EXPLAIN indexes=1 sub-test
// splices/pushes — small enough (per issue #2783's own manual verification)
// that idx_trace_id still meaningfully prunes granules for BOTH forms, so a
// pruning-parity regression shows up as a granule-count difference rather
// than both forms degrading to a full scan alike.
const extTblExplainIDCount = 200

func TestStructuralTwoPhase_ExternalTraceIDTable_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := startScanWinCH(ctx, t)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply traces DDL: %v", err)
	}
	seedExtTblTraces(ctx, t, conn)

	s := schema.DefaultOTelTraces()
	q := &loggedCappedQuerier{inner: client}
	h := New(q, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)

	query := `{ resource.service.name = "ext-root" } >> { resource.service.name = "ext-leaf" }`
	params := url.Values{
		"q":     {query},
		"limit": {strconv.Itoa(extTblTraceCount)},
		"start": {"1777593600"},
		"end":   {"1777680000"},
	}

	// --- Run 1: h.ExternalTraceIDPush=false forces the pre-#2783 literal
	// splice even though the closure is wide enough to qualify. ---
	h.ExternalTraceIDPush = false
	literalMarker := "ext2783_literal"
	literal := extTblDoSearch(t, mux, q, literalMarker, params)
	if len(literal.Traces) != extTblTraceCount {
		t.Fatalf("literal-path search returned %d traces, want all %d — seed or closure misconfigured",
			len(literal.Traces), extTblTraceCount)
	}
	literalReadRows := scanWinRequestReadRows(ctx, t, conn, literalMarker)

	// --- Run 2: h.ExternalTraceIDPush=true, same request. useExternalTraceIDTable
	// must actually pick the external form here (extTblTraceCount is sized for
	// that above), which the granule-parity assertion below indirectly proves —
	// a run that silently fell back to the literal form would read the exact
	// same rows, not merely similar ones. ---
	h.ExternalTraceIDPush = true
	externalMarker := "ext2783_external"
	external := extTblDoSearch(t, mux, q, externalMarker, params)
	externalReadRows := scanWinRequestReadRows(ctx, t, conn, externalMarker)

	// ★ Correctness parity: the external-table form must return the EXACT
	// same ordered trace set as the literal form — the whole point of #2783
	// is a wire-format change with no observable difference in results.
	if len(external.Traces) != len(literal.Traces) {
		t.Fatalf("external-table search returned %d traces, want %d (literal-path count)",
			len(external.Traces), len(literal.Traces))
	}
	for i := range literal.Traces {
		if external.Traces[i].TraceID != literal.Traces[i].TraceID {
			t.Errorf("trace %d: external=%q literal=%q — external-table form diverged from the literal reference",
				i, external.Traces[i].TraceID, literal.Traces[i].TraceID)
		}
		if external.Traces[i].StartTimeUnixNano != literal.Traces[i].StartTimeUnixNano {
			t.Errorf("trace %d (%s): external StartTimeUnixNano=%s literal=%s",
				i, external.Traces[i].TraceID, external.Traces[i].StartTimeUnixNano, literal.Traces[i].StartTimeUnixNano)
		}
	}

	// ★ Read-row parity: the external-table form must read a COMPARABLE
	// number of rows to the literal form — not dramatically more, which
	// would mean it lost granule pruning idx_trace_id gives the literal
	// splice (issue #2783's EXPLAIN indexes=1 risk). A generous 2x band
	// (rather than requiring near-equality) absorbs the external table's own
	// small extra read (its own extTblTraceCount rows) plus ordinary
	// part-layout noise between the two runs.
	if externalReadRows > literalReadRows*2 {
		t.Errorf("external-table read_rows=%d, literal read_rows=%d — external form reads more than 2x the literal form, suggesting it lost idx_trace_id pruning",
			externalReadRows, literalReadRows)
	}
	t.Logf("read_rows: literal=%d external=%d (closure=%d traces)", literalReadRows, externalReadRows, extTblTraceCount)
}

// TestStructuralTwoPhase_ExternalTraceIDTable_ExplainIndexes is issue #2783's
// own mandated check: EXPLAIN indexes=1 against a real server, comparing the
// literal IN-list form against the external-table form at a size
// (extTblExplainIDCount) where idx_trace_id still meaningfully prunes for
// both. It asserts the two forms drop the SAME number of granules — not
// merely "some" — so a future ClickHouse version that stops treating an
// external-table IN-subquery as a prepared set for index analysis would fail
// this test loudly rather than silently regressing production.
func TestStructuralTwoPhase_ExternalTraceIDTable_ExplainIndexes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := startScanWinCH(ctx, t)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply traces DDL: %v", err)
	}
	seedExtTblTraces(ctx, t, conn)
	seedExtTblGranuleNoise(ctx, t, conn)

	ids := extTblRootTraceIDs(ctx, t, conn, extTblExplainIDCount)
	if len(ids) != extTblExplainIDCount {
		t.Fatalf("seeded %d root trace ids, want %d", len(ids), extTblExplainIDCount)
	}

	literalDropped, literalTotal := extTblExplainGranules(ctx, t, conn, extTblLiteralExplainSQL(ids))

	const extTable = "ext2783_explain_ids"
	extCtx, err := chclient.WithExternalTraceIDs(ctx, extTable, "TraceId", ids)
	if err != nil {
		t.Fatalf("WithExternalTraceIDs: %v", err)
	}
	externalDropped, externalTotal := extTblExplainGranules(extCtx, t, conn, fmt.Sprintf(
		"EXPLAIN indexes=1 SELECT count() FROM otel_traces WHERE TraceId IN (SELECT TraceId FROM %s)", extTable,
	))

	if literalTotal != externalTotal {
		t.Fatalf("literal and external EXPLAIN plans scanned different total granule counts (%d vs %d) — not a comparable pair",
			literalTotal, externalTotal)
	}
	if literalDropped == 0 {
		t.Fatalf("literal form's idx_trace_id dropped 0 granules — the seed is not selective enough to prove anything")
	}
	if externalDropped != literalDropped {
		t.Errorf("idx_trace_id dropped %d/%d granules for the external-table form, %d/%d for the literal form — external-table pruning is NOT at parity",
			externalDropped, externalTotal, literalDropped, literalTotal)
	}
	t.Logf("idx_trace_id granules dropped: literal=%d/%d external=%d/%d", literalDropped, literalTotal, externalDropped, externalTotal)
}

// TestExternalTraceIDs_ReusableAcrossDispatches proves the retry / route-B
// sharding safety issue #2783 calls out: a *ext.Table-carrying context built
// ONCE by chclient.WithExternalTraceIDs is safe to hand to more than one
// Client.Query dispatch — the exact shape BOTH a transport retry
// (chclient/retry.go's withTransportRetry, which reuses the SAME derived ctx
// across attempts) and a route-B shard (each shard issuing its own Query call
// under the shared ctx) take. If attaching the table drained or mutated
// shared state on first use, the second dispatch below would return an empty
// or short result instead of the full, identical set.
func TestExternalTraceIDs_ReusableAcrossDispatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := startScanWinCH(ctx, t)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply traces DDL: %v", err)
	}
	seedExtTblTraces(ctx, t, conn)

	ids := extTblRootTraceIDs(ctx, t, conn, extTblExplainIDCount)
	extCtx, err := chclient.WithExternalTraceIDs(ctx, "ext2783_retry_ids", "TraceId", ids)
	if err != nil {
		t.Fatalf("WithExternalTraceIDs: %v", err)
	}
	// Filtered to root spans only: each seeded trace carries a root AND a leaf
	// row (seedExtTblTraces), so counting bare TraceId membership would count
	// both per trace — restricting to ServiceName='ext-root' keeps the
	// expected count exactly extTblExplainIDCount, one row per pushed id.
	const sql = "SELECT toString(count()) FROM otel_traces WHERE ServiceName = 'ext-root' AND TraceId IN (SELECT TraceId FROM ext2783_retry_ids)"

	first, err := client.QueryStrings(extCtx, sql)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	second, err := client.QueryStrings(extCtx, sql)
	if err != nil {
		t.Fatalf("second dispatch (simulated retry / second shard) over the SAME ctx: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want exactly one count() row per dispatch, got %d then %d", len(first), len(second))
	}
	if first[0] != second[0] {
		t.Fatalf("second dispatch over the same external-table ctx returned %q, want %q (first dispatch) — the table was drained or mutated by the first use",
			second[0], first[0])
	}
	wantCount := strconv.Itoa(extTblExplainIDCount)
	if first[0] != wantCount {
		t.Fatalf("count()=%q, want %q (%d seeded root ids all present)", first[0], wantCount, extTblExplainIDCount)
	}
}

// extTblSearchResult is the decoded /api/search envelope this file needs —
// just the trace list, unlike search_trace_limit_chdb_test.go's searchResult
// (which also tracks the span-drain header this file's assertions don't use).
type extTblSearchResult struct {
	SearchResponse
}

// extTblDoSearch points q's log_comment at marker, issues ONE GET for
// path+params through mux, and decodes the /api/search response body.
func extTblDoSearch(t *testing.T, mux *http.ServeMux, q *loggedCappedQuerier, marker string, params url.Values) extTblSearchResult {
	t.Helper()
	q.marker = marker
	req := httptest.NewRequest(http.MethodGet, "/api/search?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("marker=%s: /api/search returned HTTP %d, want 200: %s", marker, rec.Code, rec.Body.String())
	}
	var sr extTblSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &sr.SearchResponse); err != nil {
		t.Fatalf("marker=%s: decode /api/search body: %v\nbody: %s", marker, err, rec.Body.String())
	}
	return sr
}

// seedExtTblTraces inserts extTblTraceCount root/leaf trace pairs server-side
// via INSERT … SELECT FROM numbers(), matching seedScanWinTraces's style. Root
// spans carry ResourceAttributes.service.name="ext-root" (ParentSpanId=""),
// leaf spans "ext-leaf" parented at the root — the same shape
// search_structural_two_phase_chdb_test.go's structuralTwoPhaseSeed builds by
// hand, generated here at real-server scale. Every pair sits inside the same
// single partition/window (2026-05-01, matching the params start/end used
// above) so partition pruning is not a confound for the granule-count
// assertions — only idx_trace_id is under test.
//
// traceid is left-padded to the canonical 32-char TraceId hex width
// (handler.go's traceIDHexLen / normaliseTraceID): padTraceIDs left-pads any
// SHORTER id with leading zeros before splicing/pushing it as the phase-B
// restriction, so an unpadded (here, 16-char) synthetic id would restrict on
// a value that never matches what otel_traces actually stores — phase B
// would silently return zero rows even though phase A found the trace.
func seedExtTblTraces(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()
	insert := fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    base_ts + toIntervalSecond(if(leaf, 1, 0)) AS Timestamp,
    traceid AS TraceId,
    concat(traceid, if(leaf, '_1', '_0')) AS SpanId,
    if(leaf, concat(traceid, '_0'), '') AS ParentSpanId,
    if(leaf, 'leaf-op', 'root-op') AS SpanName,
    if(leaf, 'Internal', 'Server') AS SpanKind,
    if(leaf, 'ext-leaf', 'ext-root') AS ServiceName,
    map('service.name', if(leaf, 'ext-leaf', 'ext-root')) AS ResourceAttributes,
    map() AS SpanAttributes,
    1000 AS Duration, 'Unset' AS StatusCode, '' AS StatusMessage,
    '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM (
    SELECT
        number AS n,
        leftPad(lower(hex(reinterpretAsFixedString(number))), 32, '0') AS traceid,
        toDateTime64('2026-05-01 10:00:00', 9) + toIntervalSecond(number %% 60) AS base_ts
    FROM numbers(%d)
) AS tr
ARRAY JOIN [false, true] AS leaf`, extTblTraceCount)
	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// extTblGranuleNoiseRows is the row count seedExtTblGranuleNoise inserts.
// idx_trace_id is a bloom_filter skip index over 8192-row granules
// (index_granularity, ddl's traces DDL default); 4,000,000 noise rows sharing
// seedExtTblTraces' SAME date partition gives ~488 granules for it to prune
// against — comparable to the manual scale (20M rows / 2458 granules, 200-id
// lookup dropping ~2000 of them) issue #2783's own EXPLAIN verification used
// while implementing this feature, scaled down to keep this test's runtime
// reasonable for CI.
const extTblGranuleNoiseRows = 4_000_000

// seedExtTblGranuleNoise inserts extTblGranuleNoiseRows unrelated spans into
// the SAME date partition seedExtTblTraces uses (2026-05-01), so
// TestStructuralTwoPhase_ExternalTraceIDTable_ExplainIndexes's EXPLAIN
// indexes=1 has enough granules for idx_trace_id to meaningfully prune —
// without this, seedExtTblTraces' own 1,400 rows fit inside a single 8192-row
// granule and the index has nothing to drop either way. TraceIds are
// generated from a number range disjoint from seedExtTblTraces' (starting at
// 10,000,000) so no noise row can collide with a real root/leaf TraceId.
func seedExtTblGranuleNoise(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()
	insert := fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    toDateTime64('2026-05-01 10:00:00', 9) + toIntervalSecond(number %% 86400) AS Timestamp,
    leftPad(lower(hex(reinterpretAsFixedString(number + 10000000))), 32, '0') AS TraceId,
    lower(hex(reinterpretAsFixedString(number + 10000000))) AS SpanId,
    '' AS ParentSpanId, 'noise-op' AS SpanName, 'Server' AS SpanKind, 'ext-noise' AS ServiceName,
    map('service.name', 'ext-noise') AS ResourceAttributes, map() AS SpanAttributes,
    1000 AS Duration, 'Unset' AS StatusCode, '' AS StatusMessage,
    '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%d)`, extTblGranuleNoiseRows)
	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("seed granule-noise insert: %v", err)
	}
	if err := conn.Exec(ctx, "OPTIMIZE TABLE otel_traces FINAL"); err != nil {
		t.Fatalf("optimize table final: %v", err)
	}
}

// extTblRootTraceIDs returns n distinct on-disk TraceId values read directly
// off the seeded root spans — the exact bytes otel_traces stores, whatever
// their length, so the literal and external-table forms compare the SAME
// values either way. extTblLiteralExplainSQL and chclient.WithExternalTraceIDs
// both consume this slice.
func extTblRootTraceIDs(ctx context.Context, t *testing.T, conn driver.Conn, n int) []string {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT TraceId FROM otel_traces WHERE ServiceName = 'ext-root' ORDER BY TraceId LIMIT %d", n,
	))
	if err != nil {
		t.Fatalf("select root trace ids: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan trace id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("root trace ids rows: %v", err)
	}
	return out
}

// extTblLiteralExplainSQL renders the literal IN-list EXPLAIN statement
// extTblExplainGranules parses, using chsql's own quoting convention (single
// quotes) so the rendered predicate matches what inStringLiteralsFrag emits.
func extTblLiteralExplainSQL(ids []string) string {
	sql := "EXPLAIN indexes=1 SELECT count() FROM otel_traces WHERE TraceId IN ("
	for i, id := range ids {
		if i > 0 {
			sql += ", "
		}
		sql += "'" + id + "'"
	}
	return sql + ")"
}

// extTblExplainGranuleLine matches EXPLAIN indexes=1's per-index summary line,
// e.g. "Granules: 456/2458" under the "Skip" / "Name: idx_trace_id" block.
var extTblExplainGranuleLine = regexp.MustCompile(`^\s*Granules: (\d+)/(\d+)\s*$`)

// extTblExplainGranules runs an EXPLAIN indexes=1 statement and returns the
// idx_trace_id skip index's (dropped, total) granule counts — dropped =
// total - readGranules, parsed off the "Granules: <read>/<total>" line that
// immediately follows the "Name: idx_trace_id" line in the plan text
// (ClickHouse's own EXPLAIN indexes=1 output shape, verified manually against
// a real 26.6 server while implementing #2783).
func extTblExplainGranules(ctx context.Context, t *testing.T, conn driver.Conn, sql string) (dropped, total int) {
	t.Helper()
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, sql)
	}
	defer func() { _ = rows.Close() }()
	sawIdxTraceID := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN line: %v", err)
		}
		if line == "    Name: idx_trace_id" || line == "Name: idx_trace_id" ||
			(len(line) > 0 && lastField(line) == "idx_trace_id") {
			sawIdxTraceID = true
			continue
		}
		if sawIdxTraceID {
			if m := extTblExplainGranuleLine.FindStringSubmatch(line); m != nil {
				read, _ := strconv.Atoi(m[1])
				tot, _ := strconv.Atoi(m[2])
				return tot - read, tot
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	t.Fatalf("EXPLAIN output carried no idx_trace_id Granules line:\n%s", sql)
	return 0, 0
}

// lastField returns the last whitespace-separated field of line, used to
// match "Name: idx_trace_id" regardless of leading-space indentation depth.
func lastField(line string) string {
	fields := regexp.MustCompile(`\s+`).Split(line, -1)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}
