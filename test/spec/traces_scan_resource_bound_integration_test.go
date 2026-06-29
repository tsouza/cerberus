//go:build integration

// traces_scan_resource_bound_integration_test.go — the real-ClickHouse guard
// for the spans-scan resource-bound invariant (PR #1154).
//
// # WHY THIS LANE EXISTS
//
// The TraceQL whole-trace drilldown legs (the Grafana Traces Drilldown
// "Structure" and "Comparison" tabs) read the otel_traces span table through
// recursive CTEs and matrix metric scans. Two production incidents motivated
// PR #1154:
//
//   - the Structure tab "never loads": its nested-set numbering walk emitted a
//     recursive step `FROM otel_traces AS t INNER JOIN _cerberus_ns_paths AS c
//     ON ... WHERE c._depth < 128` whose `t` scan carried NO trace-id / window
//     predicate, so ClickHouse re-read the full retention on every recursion
//     iteration (the groupArray-the-world OOM). The fix (delta N1) bounds that
//     step with `AND t.TraceId IN (SELECT TraceId FROM <non-recursive scope>)`;
//   - the Comparison tab "flakes under OOM": its matrix inner scan was pushed a
//     request-window predicate by a helper that SILENTLY no-ops on a zero
//     window, so a windowless request scanned full retention. The fix fails
//     closed (ErrUnboundedSpansScan) instead.
//
// # WHY chDB CANNOT VALIDATE THIS
//
// The fix's delta-N1 prune embeds a NON-recursive `TraceId IN (SELECT TraceId
// FROM <scope>)` subquery INSIDE a recursive arm. ClickHouse rejects a
// *recursive* subquery nested in a recursive arm with error 49; the fix is
// specifically shaped (the scope is the boundedRootScope / traceScope, never
// the recursive CTE itself) to stay legal. The chDB round-trip lane (libchdb
// behind chdb-go's Parquet driver) does not execute these recursive-CTE
// drilldown shapes, so the error-49 seam is invisible to it. Only a real
// `clickhouse/clickhouse-server` over the native protocol exercises it — this
// lane does.
//
// # WHAT THIS TEST PROVES (all against a real multi-partition otel_traces)
//
//  1. structure_executes_correct — the REAL lowered+emitted Structure query
//     (windowed) runs WITHOUT error 49 and returns the correct nested-set
//     numbering. (the chDB-blind error-49 + correctness seam)
//  2. compare_executes — the REAL lowered+emitted Comparison query (windowed)
//     runs without error and returns rows.
//  3. window_bound_prunes_partitions — a window-bounded leaf scan reads
//     strictly FEWER MergeTree parts than the table holds (EXPLAIN ESTIMATE).
//     The drilldown query itself prunes far fewer parts windowed than
//     un-windowed (query_log SelectedParts). (runtime partition pruning fired)
//  4. structure_recursive_steps_bounded — every otel_traces recursive step in
//     the emitted Structure SQL carries a TraceId / Timestamp bound, so a
//     regression that drops delta N1 (un-prunes a recursive scan) fails here.
//  5. compare_absent_window_fails_closed — emitting the Comparison query with a
//     zero request window returns ErrUnboundedSpansScan rather than a
//     full-retention scan.
//
// Gated by the `integration` build tag (Docker required); wired into the
// strict-scan CI lane via `just traces-scan-bound-integration` (see
// .github/workflows/strict-scan.yml). INFORMATIONAL, not a required PR gate
// (yet) — same promotion playbook as the rest of that lane.

package spec_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// scanBoundCHImage pins the same ClickHouse server image the other real-CH
// lanes (strict-scan, chclient integration) use.
const scanBoundCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// scanBoundDB is the connection's default database. The lowered SQL references
// otel_traces unqualified, so the table must live in the connection's database.
// ddl.Apply's zero-value Config also creates into `default`.
const scanBoundDB = "default"

// Seed shape: 10 daily partitions, tracesPerDay root traces per day (each a
// 5-span tree), so a 1-hour query window falls inside ONE partition while the
// table holds ten. tracesPerDay is kept under the search trace limit's reach in
// the window so the correctness assertion is exact.
const (
	scanBoundDays        = 10
	scanBoundTracesDay   = 300
	scanBoundSpansTrace  = 5
	scanBoundSearchLimit = 20
)

// The seeded window: 2026-05-14 10:00:00..11:00:00 UTC — one hour inside the
// fifth seeded day (2026-05-10 is day 0).
var (
	scanBoundWinStart = time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	scanBoundWinEnd   = scanBoundWinStart.Add(time.Hour)
)

// structureQuery is the exact shape Grafana Traces Drilldown's Structure tab
// sends: a union-structural arm OR'd with a plain root filter, piped into
// select() over the three nested-set intrinsics.
const structureQuery = `({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`

func TestTracesScanResourceBoundRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := startScanBoundCH(ctx, t)
	conn := client.Conn()

	// Real production OTel-CH DDL (PARTITION BY toDate(Timestamp), the
	// bloom-filter idx_trace_id, ORDER BY (ServiceName, SpanName, Timestamp)).
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply traces DDL: %v", err)
	}
	seedScanBoundTraces(ctx, t, conn)

	s := schema.DefaultOTelTraces()

	// Sanity: the seed produced the expected multi-partition layout and a
	// non-empty in-window root set, so every assertion below is non-vacuous.
	totalParts := scalarUInt64(ctx, t, conn,
		"SELECT count() FROM system.parts WHERE table='otel_traces' AND active")
	if totalParts < scanBoundDays {
		t.Fatalf("seed produced %d active parts, want >= %d (multi-partition layout)", totalParts, scanBoundDays)
	}
	inWindowRoots := scalarUInt64(ctx, t, conn, fmt.Sprintf(
		"SELECT count() FROM otel_traces WHERE ParentSpanId='' AND Timestamp >= fromUnixTimestamp64Nano(%d) AND Timestamp <= fromUnixTimestamp64Nano(%d)",
		scanBoundWinStart.UnixNano(), scanBoundWinEnd.UnixNano(),
	))
	if inWindowRoots == 0 {
		t.Fatal("seed produced zero in-window root traces — correctness assertions would be vacuous")
	}
	wantTraces := inWindowRoots
	if wantTraces > scanBoundSearchLimit {
		wantTraces = scanBoundSearchLimit
	}

	// --- 1. Structure query: real lower→emit, executes WITHOUT error 49 and
	//        returns the correct nested-set numbering. ---
	structSQL := emitStructure(t, s, true)
	t.Run("structure_executes_correct", func(t *testing.T) {
		rows := queryNestedSet(ctx, t, conn, structSQL)
		assertNestedSetShape(t, rows, int(wantTraces))
	})

	// --- 2. Comparison query: real lower→emit (windowed), executes. ---
	compareSQL := emitCompare(t, s, scanBoundWinStart, scanBoundWinEnd)
	t.Run("compare_executes", func(t *testing.T) {
		n := execAndCount(ctx, t, conn, compareSQL)
		if n == 0 {
			t.Errorf("windowed compare query returned zero rows over seeded error spans")
		}
	})

	// --- 3. Partition pruning fired at runtime. ---
	t.Run("window_bound_prunes_partitions", func(t *testing.T) {
		// (a) form-a leaf: a window-bounded column scan reads strictly fewer
		//     parts than the table holds. EXPLAIN ESTIMATE is deterministic.
		fullParts, _ := explainEstimateAgg(ctx, t, conn,
			"SELECT TraceId, SpanId FROM otel_traces WHERE SpanKind='Server'")
		windowParts, _ := explainEstimateAgg(ctx, t, conn, fmt.Sprintf(
			"SELECT TraceId, SpanId FROM otel_traces WHERE SpanKind='Server' AND Timestamp >= fromUnixTimestamp64Nano(%d) AND Timestamp <= fromUnixTimestamp64Nano(%d)",
			scanBoundWinStart.UnixNano(), scanBoundWinEnd.UnixNano(),
		))
		if windowParts >= fullParts {
			t.Errorf("form-a window bound did not prune parts: windowed=%d full=%d", windowParts, fullParts)
		}
		if windowParts >= int64(totalParts) {
			t.Errorf("windowed leaf read %d parts, table holds %d — partition pruning did not fire", windowParts, totalParts)
		}

		// (b) the actual drilldown query prunes far fewer parts windowed than
		//     un-windowed (runtime query_log SelectedParts).
		windowedDrill := measureSelectedParts(ctx, t, conn, structSQL, "scanbound_struct_win")
		unwindowedDrill := measureSelectedParts(ctx, t, conn, emitStructure(t, s, false), "scanbound_struct_nowin")
		if windowedDrill >= unwindowedDrill {
			t.Errorf("windowed Structure query did not prune parts vs un-windowed: windowed=%d unwindowed=%d", windowedDrill, unwindowedDrill)
		}
	})

	// --- 4. Delta-N1 ratchet: every otel_traces recursive step in the emitted
	//        Structure SQL carries a resource bound (TraceId / Timestamp). A
	//        regression that drops the prune leaves a bare recursive scan and
	//        fails here. ---
	t.Run("structure_recursive_steps_bounded", func(t *testing.T) {
		assertRecursiveStepsBounded(t, structSQL)
	})

	// --- 5. Compare fail-closed: a zero request window returns
	//        ErrUnboundedSpansScan instead of a silent full-retention scan. ---
	t.Run("compare_absent_window_fails_closed", func(t *testing.T) {
		_, _, err := emitCompareRaw(s, time.Time{}, time.Time{})
		if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
			t.Errorf("absent-window compare emit did not fail closed: err=%v (want ErrUnboundedSpansScan)", err)
		}
	})
}

// --- emit helpers (the real cerberus lower→emit path) ---

// emitStructure lowers and emits the Structure query through the production
// path. windowed selects whether a request window is threaded (the form-a
// bound) — the un-windowed variant still lowers to a bounded plan (the
// BoundedTraceScope trace-id set), so both are valid; the windowed one prunes
// partitions, the un-windowed does not.
func emitStructure(t *testing.T, s schema.Traces, windowed bool) string {
	t.Helper()
	expr, err := ast.Parse(structureQuery)
	if err != nil {
		t.Fatalf("parse structure: %v", err)
	}
	ctx := tql.WithSearchTraceLimit(context.Background(), scanBoundSearchLimit)
	if windowed {
		ctx = tql.WithSearchWindow(ctx, scanBoundWinStart, scanBoundWinEnd)
	}
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower structure: %v", err)
	}
	sqlStr, args, err := chsql.Emit(chsql.WithSpansTable(ctx, s.SpansTable), plan)
	if err != nil {
		t.Fatalf("emit structure: %v", err)
	}
	return inlineScanBoundArgs(t, sqlStr, args)
}

// emitCompare lowers `{ } | compare(...)` and wraps it in the RangeWindow the
// /api/metrics/query_range handler builds, then emits — the real metrics-matrix
// lower→emit path.
func emitCompare(t *testing.T, s schema.Traces, start, end time.Time) string {
	t.Helper()
	sqlStr, args, err := emitCompareRaw(s, start, end)
	if err != nil {
		t.Fatalf("emit compare: %v", err)
	}
	return inlineScanBoundArgs(t, sqlStr, args)
}

func emitCompareRaw(s schema.Traces, start, end time.Time) (string, []any, error) {
	cmpQ := fmt.Sprintf(`{ } | compare({ status = error }, 10, %d, %d)`, scanBoundWinStart.UnixNano(), scanBoundWinEnd.UnixNano())
	cexpr, err := ast.Parse(cmpQ)
	if err != nil {
		return "", nil, err
	}
	cplan, err := tql.Lower(context.Background(), cexpr, s)
	if err != nil {
		return "", nil, err
	}
	step := 5 * time.Minute
	rw := &chplan.RangeWindow{
		Input:           cplan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}
	return chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), chplan.Node(rw))
}

// inlineScanBoundArgs splices positional `?` args back as quoted literals so the
// statement runs without a prepared statement (EXPLAIN ESTIMATE / query_log
// correlation). The drilldown plans carry only string args.
func inlineScanBoundArgs(t *testing.T, query string, args []any) string {
	t.Helper()
	out := query
	for _, a := range args {
		var lit string
		switch v := a.(type) {
		case string:
			lit = "'" + strings.ReplaceAll(v, "'", "\\'") + "'"
		case bool:
			if v {
				lit = "1"
			} else {
				lit = "0"
			}
		default:
			lit = fmt.Sprint(a)
		}
		out = strings.Replace(out, "?", lit, 1)
	}
	return out
}

// --- assertions ---

type nestedSetRow struct {
	traceID string
	parent  int64
	left    int64
	right   int64
}

// assertNestedSetShape pins the Structure query's correctness: each in-window
// trace contributes exactly two spans — its root (parent=-1, the whole-tree
// envelope 1..2*spans) and the matched server-kind descendant (the checkout
// span) — with the nested-set numbering the 5-span seed tree implies.
func assertNestedSetShape(t *testing.T, rows []nestedSetRow, wantTraces int) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("Structure query returned zero rows over seeded in-window traces")
	}
	const span = scanBoundSpansTrace
	// root envelope: left=1, right=2*span; checkout subtree: the third visited
	// span (root=1, auth=2..5, cache=3..4, checkout=6..9, db=7..8).
	rootLeft, rootRight := int64(1), int64(2*span)
	checkoutParent, checkoutLeft, checkoutRight := int64(1), int64(6), int64(9)
	var roots, checkouts int
	for _, r := range rows {
		switch {
		case r.parent == -1 && r.left == rootLeft && r.right == rootRight:
			roots++
		case r.parent == checkoutParent && r.left == checkoutLeft && r.right == checkoutRight:
			checkouts++
		default:
			t.Errorf("unexpected nested-set row: trace=%s parent=%d left=%d right=%d", r.traceID, r.parent, r.left, r.right)
		}
	}
	if roots != wantTraces {
		t.Errorf("got %d root rows, want %d (one per in-window trace)", roots, wantTraces)
	}
	if checkouts != wantTraces {
		t.Errorf("got %d server-descendant rows, want %d (one per in-window trace)", checkouts, wantTraces)
	}
}

// assertRecursiveStepsBounded fails if any otel_traces recursive step in the
// emitted SQL is a bare scan — i.e. a `FROM otel_traces AS t INNER JOIN <cte> AS
// c ON ...` whose step never restricts `t` by TraceId or Timestamp. This is the
// shape delta N1 closed; a regression that drops the prune reintroduces it.
func assertRecursiveStepsBounded(t *testing.T, sqlStr string) {
	t.Helper()
	const marker = "`otel_traces` AS t INNER JOIN"
	steps := strings.Count(sqlStr, marker)
	if steps == 0 {
		t.Fatalf("emitted Structure SQL has no recursive `otel_traces AS t` step — query shape changed, ratchet is vacuous:\n%s", sqlStr)
	}
	// Each recursive step's WHERE must constrain t by trace-id or timestamp.
	// Walk every step occurrence and inspect the clause up to the next closing
	// of its SELECT (the next `)` that balances, approximated by the segment to
	// the next recursive marker or end). A bound is present iff `t.`TraceId`` or
	// a `t.`Timestamp`` predicate appears before the step's depth guard chain
	// ends. We assert the bound token appears in the same step segment.
	segments := strings.Split(sqlStr, marker)
	for i := 1; i < len(segments); i++ {
		seg := segments[i]
		// Limit the inspection window to this step's clause: up to the next
		// `UNION` / end, whichever comes first (the recursive step ends before
		// the closure's UNION-ALL anchor join or the CTE close).
		if cut := strings.Index(seg, " UNION "); cut >= 0 {
			seg = seg[:cut]
		}
		if !strings.Contains(seg, "t.`TraceId` IN") && !strings.Contains(seg, "t.`Timestamp`") {
			t.Errorf("recursive otel_traces step #%d carries no TraceId/Timestamp bound (delta-N1 regression):\n...%s...", i, clip(seg, 320))
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- ClickHouse helpers (raw production driver via client.Conn()) ---

func queryNestedSet(ctx context.Context, t *testing.T, conn driver.Conn, sqlStr string) []nestedSetRow {
	t.Helper()
	rows, err := conn.Query(ctx, sqlStr)
	if err != nil {
		// A recursive-subquery rejection surfaces here as ClickHouse error 49.
		t.Fatalf("Structure query failed on real ClickHouse (error 49 = illegal recursive subquery is the chDB-blind seam):\n--- err ---\n%v\n--- sql ---\n%s", err, sqlStr)
	}
	defer func() { _ = rows.Close() }()
	var out []nestedSetRow
	for rows.Next() {
		var traceID, spanID string
		var ts time.Time
		var parent, left, right int64
		if err := rows.Scan(&traceID, &spanID, &ts, &parent, &left, &right); err != nil {
			t.Fatalf("scan nested-set row: %v", err)
		}
		out = append(out, nestedSetRow{traceID: traceID, parent: parent, left: left, right: right})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain Structure rows: %v", err)
	}
	return out
}

func execAndCount(ctx context.Context, t *testing.T, conn driver.Conn, sqlStr string) int {
	t.Helper()
	rows, err := conn.Query(ctx, sqlStr)
	if err != nil {
		t.Fatalf("query failed on real ClickHouse:\n--- err ---\n%v\n--- sql ---\n%s", err, sqlStr)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain rows: %v", err)
	}
	return n
}

// explainEstimateAgg sums (parts, rows) over the EXPLAIN ESTIMATE rows.
func explainEstimateAgg(ctx context.Context, t *testing.T, conn driver.Conn, query string) (parts, estRows int64) {
	t.Helper()
	rows, err := conn.Query(ctx, "EXPLAIN ESTIMATE "+query)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE: %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var database, table string
		var p, r, marks uint64
		if err := rows.Scan(&database, &table, &p, &r, &marks); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		parts += int64(p)
		estRows += int64(r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return parts, estRows
}

// measureSelectedParts executes the query with a unique log_comment, flushes the
// query log, and reads back the SelectedParts profile event for that run — the
// genuine runtime count of MergeTree parts the query touched across all scan
// sites.
func measureSelectedParts(ctx context.Context, t *testing.T, conn driver.Conn, sqlStr, marker string) int64 {
	t.Helper()
	runCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"log_comment": marker,
	}))
	if err := drain(runCtx, conn, sqlStr); err != nil {
		t.Fatalf("execute (%s) on real ClickHouse:\n--- err ---\n%v\n--- sql ---\n%s", marker, err, sqlStr)
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	return int64(scalarUInt64(ctx, t, conn, fmt.Sprintf(
		"SELECT toUInt64(ProfileEvents['SelectedParts']) FROM system.query_log WHERE type='QueryFinish' AND log_comment='%s' AND query NOT LIKE '%%query_log%%' ORDER BY event_time_microseconds DESC LIMIT 1",
		marker,
	)))
}

func drain(ctx context.Context, conn driver.Conn, sqlStr string) error {
	rows, err := conn.Query(ctx, sqlStr)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	return rows.Err()
}

func scalarUInt64(ctx context.Context, t *testing.T, conn driver.Conn, query string) uint64 {
	t.Helper()
	rows, err := conn.Query(ctx, query)
	if err != nil {
		t.Fatalf("scalar query: %v\n%s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var v uint64
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan scalar: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scalar rows: %v", err)
	}
	return v
}

// seedScanBoundTraces inserts scanBoundDays daily partitions of root-rooted
// 5-span trace trees server-side via INSERT … SELECT FROM numbers(), so the
// seed needs no external generator and is byte-deterministic. Each trace tree:
// root(Server) → auth(Internal) → cache(Client); root → checkout(Server,Error)
// → db(Client). Traces are spread uniformly across each day so a 1-hour window
// captures a small fraction.
func seedScanBoundTraces(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()
	totalTraces := scanBoundDays * scanBoundTracesDay
	secPerTrace := 86400 / scanBoundTracesDay
	insert := fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    base_ts + toIntervalNanosecond(sp) AS Timestamp,
    traceid AS TraceId,
    concat(traceid, '_', toString(sp)) AS SpanId,
    multiIf(sp = 0, '', sp = 1, concat(traceid, '_0'), sp = 2, concat(traceid, '_0'),
            sp = 3, concat(traceid, '_2'), concat(traceid, '_1')) AS ParentSpanId,
    multiIf(sp = 0, 'GET /home', sp = 1, 'auth', sp = 2, 'checkout', sp = 3, 'db', 'cache') AS SpanName,
    multiIf(sp = 0, 'Server', sp = 1, 'Internal', sp = 2, 'Server', sp = 3, 'Client', 'Client') AS SpanKind,
    multiIf(sp = 0, 'frontend', sp = 1, 'auth', sp = 2, 'checkout', sp = 3, 'db', 'cache') AS ServiceName,
    map('service.name', multiIf(sp = 0, 'frontend', sp = 1, 'auth', sp = 2, 'checkout', sp = 3, 'db', 'cache')) AS ResourceAttributes,
    map('server.address', 'h1', 'url.path', '/checkout', 'url.route', '/checkout/:id') AS SpanAttributes,
    1000 AS Duration,
    if(sp = 2, 'Error', 'Unset') AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM (
    SELECT
        number AS tr,
        intDiv(number, %d) AS day,
        number %% %d AS wd,
        concat('t', leftPad(toString(number), 8, '0')) AS traceid,
        toDateTime64('2026-05-10 00:00:00.000000000', 9) + toIntervalDay(day) + toIntervalSecond(wd * %d) AS base_ts
    FROM numbers(%d)
) AS tr
ARRAY JOIN [0, 1, 2, 3, 4] AS sp`,
		scanBoundTracesDay, scanBoundTracesDay, secPerTrace, totalTraces)

	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// --- container boot ---

func startScanBoundCH(ctx context.Context, t *testing.T) *chclient.Client {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		scanBoundCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(scanBoundDB),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	client, err := chclient.New(chclient.Config{
		Addr:            host + ":" + port.Port(),
		Database:        scanBoundDB,
		Username:        "cerberus",
		Password:        "cerberus",
		BreakerDisabled: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
