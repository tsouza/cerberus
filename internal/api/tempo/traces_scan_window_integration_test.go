//go:build integration

// traces_scan_window_integration_test.go — real-ClickHouse guard for the
// spans-scan WINDOW invariant (the traces-drilldown OOM follow-up to PR #1154).
//
// # WHY THIS LANE EXISTS
//
// The resource-bound invariant (#1154) bounds every otel_traces scan by a
// finite TraceId set or the recursion depth cap. But `TraceId IN (<seed>)`
// membership prunes NO partitions: otel_traces is `PARTITION BY
// toDate(Timestamp)`, so only a Timestamp predicate sitting DIRECTLY on a
// physical scan prunes the partitions. Without the request-window predicate the
// recursive numbering / structural closures and the compare root-lookup read
// FULL RETENTION behind the inert IN — the prod OOM.
//
// This test drives the REAL production handlers (/api/search, /api/metrics/
// query_range) end-to-end against a real ClickHouse seeded with a dataset that
// is SMALL inside the request window but LARGE across out-of-window
// toDate(Timestamp) partitions, under a session `max_memory_usage` cap. It
// asserts each drilldown query (i) completes (no ClickHouse memory error 241)
// and (ii) reads strictly fewer rows than the whole table — partition pruning
// fired. An anti-vacuous control proves the seed is non-trivial: a raw
// un-windowed leaf scan reads every partition, while the windowed leaf reads
// one, so the bound is not vacuously satisfied by a tiny dataset.
//
// Gated behind the `integration` build tag (Docker required); wired into the
// strict-scan CI lane via `just traces-scan-window-integration` (#1509 —
// this test was fully written but ran in no CI lane before that fix).
// strict-scan is itself a required status check on main, so this step gates
// PRs the same way its sibling traces-scan-bound-integration step does.

package tempo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// scanWinCHImage pins the same ClickHouse server image the other real-CH lanes
// use.
const scanWinCHImage = "clickhouse/clickhouse-server:25.8-alpine"

const scanWinDB = "default"

// Seed shape: scanWinDays daily partitions of scanWinTracesPerDay 5-span trace
// trees. The request window covers exactly ONE day-partition, so the in-window
// span count is one partition's worth and the table holds scanWinDays of them.
const (
	scanWinDays          = 30
	scanWinTracesPerDay  = 200
	scanWinSpansPerTrace = 5
	// inWindowDayIndex is the day-partition (0-based) the request window
	// covers — mid-table so out-of-window partitions exist on both sides.
	scanWinInWindowDayIndex = 15
	// scanWinMemoryCapBytes caps ClickHouse's per-query memory. Low enough that
	// a regression that un-prunes the recursive walk (reading every partition
	// repeatedly) trends toward OOM (code 241) instead of silently succeeding,
	// while every correctly-pruned drilldown query completes well under it.
	scanWinMemoryCapBytes = 256 * 1024 * 1024
)

// seed-time constants derived from the shape above.
const (
	scanWinTotalTraces    = scanWinDays * scanWinTracesPerDay
	scanWinTotalSpans     = scanWinTotalTraces * scanWinSpansPerTrace
	scanWinPartitionSpans = scanWinTracesPerDay * scanWinSpansPerTrace
)

// scanWinBase is the first seeded partition's date (00:00:00 UTC). The window
// covers the whole inWindowDayIndex-th day.
var scanWinBase = time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)

func scanWinWindow() (time.Time, time.Time) {
	start := scanWinBase.AddDate(0, 0, scanWinInWindowDayIndex)
	return start, start.AddDate(0, 0, 1)
}

// loggedCappedQuerier wraps a *chclient.Client (which carries the
// max_memory_usage cap from its Config) and stamps a per-request `log_comment`
// onto every query it issues, so the test can sum read_rows back out of
// system.query_log for exactly the handler request under test.
type loggedCappedQuerier struct {
	inner  *chclient.Client
	marker string
}

func (q *loggedCappedQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	return q.inner.Query(chclient.WithQuerySetting(ctx, "log_comment", q.marker), sql, args...)
}

func (q *loggedCappedQuerier) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	return q.inner.QueryStrings(chclient.WithQuerySetting(ctx, "log_comment", q.marker), sql, args...)
}

func TestTracesScanWindowRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := startScanWinCH(ctx, t)
	conn := client.Conn()

	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply traces DDL: %v", err)
	}
	seedScanWinTraces(ctx, t, conn)

	// Anti-vacuous seed sanity: the table really spans many partitions, so a
	// windowless read touches far more than one window's worth.
	parts := scanWinScalar(ctx, t, conn, "SELECT count() FROM system.parts WHERE table='otel_traces' AND active")
	if parts < 2 {
		t.Fatalf("seed produced %d active parts; need a multi-partition table for the pruning assertion to be meaningful", parts)
	}
	total := scanWinScalar(ctx, t, conn, "SELECT count() FROM otel_traces")
	if total != scanWinTotalSpans {
		t.Fatalf("seed produced %d spans; want %d", total, scanWinTotalSpans)
	}

	s := schema.DefaultOTelTraces()
	q := &loggedCappedQuerier{inner: client}
	h := New(q, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)

	start, end := scanWinWindow()
	startNano, endNano := start.UnixNano(), end.UnixNano()

	// --- Anti-vacuous control: prove the leaf window prunes partitions. A raw
	// un-windowed Server-kind scan reads every partition; the windowed one reads
	// only the in-window partition. If the window did NOT prune, these match. ---
	fullLeaf := scanWinRawReadRows(ctx, t, conn, "scanwin_ctrl_full",
		"SELECT TraceId, Timestamp FROM otel_traces WHERE SpanKind = 'Server'")
	windowLeaf := scanWinRawReadRows(ctx, t, conn, "scanwin_ctrl_window", fmt.Sprintf(
		"SELECT TraceId, Timestamp FROM otel_traces WHERE SpanKind = 'Server' "+
			"AND Timestamp >= fromUnixTimestamp64Nano(%d) AND Timestamp <= fromUnixTimestamp64Nano(%d)",
		startNano, endNano,
	))
	if windowLeaf >= fullLeaf {
		t.Fatalf("control: window did not prune at the leaf (windowed=%d full=%d) — seed/window not anti-vacuous",
			windowLeaf, fullLeaf)
	}
	if fullLeaf < scanWinTotalTraces {
		t.Fatalf("control: un-windowed leaf read %d rows; expected to touch all ~%d root spans", fullLeaf, scanWinTotalTraces)
	}

	cases := []struct {
		name   string
		path   string
		params url.Values
		// readRowsFactor sets this case's per-request ceiling as a multiple of
		// ONE partition's span count (scanWinPartitionSpans). It is per-case,
		// not shared, because the cases below issue genuinely different
		// numbers of windowed sub-scans within their one combined SQL
		// statement: compare_metrics_range is a single root-lookup GROUP BY,
		// while structure_tab_search's union-structural-arm + nested-set
		// numbering shape textually repeats the same windowed top-N
		// trace-id-selection subquery across ~7 branches (two structural
		// closures — canonical and inverse — each with their own anchor +
		// recursive step, the plain-root union arm, and the nested-set CTE's
		// own anchor + recursive step), so its properly-windowed total is a
		// much larger — but still bounded — multiple of one partition. Every
		// factor here was set from a REAL measured run (see the PR that wired
		// this test into CI, #1509) with ~1.3x headroom over the measured
		// value; a regression that drops the window entirely multiplies every
		// case's read_rows by ~scanWinDays (30x), an easily-distinguished
		// jump these bounds catch with a wide margin either way.
		readRowsFactor uint64
	}{
		{
			// Grafana Traces Drilldown "Structure" tab: union-structural arm OR'd
			// with the plain root filter, piped into select() over the nested-set
			// intrinsics — the recursive numbering CTE OOM shape. Measured 46500
			// (46.5x partition-spans) on a correctly-windowed run.
			name: "structure_tab_search",
			path: "/api/search",
			params: url.Values{
				"q":     {`({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`},
				"limit": {"20"},
			},
			readRowsFactor: 60,
		},
		{
			// Grafana Traces Drilldown "Comparison" tab: compare() matrix, whose
			// per-trace root-lookup GROUP BY must be window-pruned below the group.
			// Measured 10000 (10x partition-spans) on a correctly-windowed run.
			name: "compare_metrics_range",
			path: "/api/metrics/query_range",
			params: url.Values{
				"q": {`{ } | compare({ status = error })`},
			},
			readRowsFactor: 12,
		},
		{
			// Bare structural descendant closure (`A >> B`) — the recursive step
			// `t` scan must be window-pruned. Measured 13800 (13.8x partition-spans)
			// on a correctly-windowed run: the top-N trace-id-selection subquery
			// appears twice (anchor restriction + R-side restriction) alongside the
			// recursive step's own multi-iteration window-only scan.
			name: "structural_descendant_search",
			path: "/api/search",
			params: url.Values{
				"q":     {`{ kind = server } >> { kind = client }`},
				"limit": {"20"},
			},
			readRowsFactor: 18,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := "scanwin_" + tc.name
			params := cloneValues(tc.params)
			params.Set("start", fmt.Sprintf("%d", startNano))
			params.Set("end", fmt.Sprintf("%d", endNano))

			code := scanWinDoGet(t, mux, q, marker, tc.path, params)
			if code != http.StatusOK {
				// A ClickHouse memory error (code 241) maps to 500: the cap fired
				// because the query was not pruned.
				t.Fatalf("%s returned HTTP %d (want 200) — likely an un-pruned scan tripping the %d-byte memory cap (code 241)",
					tc.name, code, scanWinMemoryCapBytes)
			}

			readRows := scanWinRequestReadRows(ctx, t, conn, marker)
			if readRows == 0 {
				t.Fatalf("%s: no QueryFinish rows recorded for log_comment %q — read_rows correlation broke", tc.name, marker)
			}
			// readRowsBound is THIS case's per-request ceiling: a windowed
			// drilldown must stay within its declared multiple of ONE partition.
			// A no-prune regression (every windowed sub-scan degrading to a full
			// scanWinDays-day scan) inflates read_rows by ~scanWinDays (30x),
			// which blows past every case's bound with a wide margin — see the
			// per-case readRowsFactor comments in the cases table above for why
			// the bound is not a single shared constant. (A blanket
			// "read_rows >= whole table" check does NOT hold here: a case that
			// legitimately issues several separate, correctly-windowed
			// sub-scans within one combined SQL statement — e.g.
			// structure_tab_search's two structural closures + nested-set CTE —
			// can sum to more than one table's worth of rows while every
			// individual sub-scan stays properly pruned to its own partition.)
			readRowsBound := uint64(scanWinPartitionSpans) * tc.readRowsFactor
			// Sanity-check the bound itself, independent of the measured
			// readRows: a factor set so large the bound could never meaningfully
			// fail (e.g. a future typo) is caught here rather than silently
			// passing vacuously. scanWinTotalSpans*scanWinBoundSlackMultiplier
			// sits comfortably above every case's real measured bound (60000 at
			// most) while staying well below what even a partial no-prune
			// regression would read.
			const scanWinBoundSlackMultiplier = 3
			if readRowsBound >= scanWinTotalSpans*scanWinBoundSlackMultiplier {
				t.Fatalf("misconfigured bound: %s readRowsBound=%d must stay under %d to remain a meaningful regression guard",
					tc.name, readRowsBound, scanWinTotalSpans*scanWinBoundSlackMultiplier)
			}
			if readRows > readRowsBound {
				t.Errorf("%s read %d rows > bound %d (%d partition-spans * %d) — the request window did not reach a recursive/grouped spans scan",
					tc.name, readRows, readRowsBound, scanWinPartitionSpans, tc.readRowsFactor)
			}
		})
	}
}

// cloneValues deep-copies a url.Values so per-case mutation (start/end) doesn't
// leak across table rows.
func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, v := range in {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// scanWinDoGet points the querier's log_comment at marker, issues the GET
// through the mounted production handler, and returns the HTTP status code.
func scanWinDoGet(t *testing.T, mux *http.ServeMux, q *loggedCappedQuerier, marker, path string, params url.Values) int {
	t.Helper()
	q.marker = marker
	req := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// scanWinRequestReadRows flushes the query log and sums read_rows across every
// QueryFinish carrying marker — the genuine row count the production request
// touched across all its CH queries (search + any missing-root resolve).
func scanWinRequestReadRows(ctx context.Context, t *testing.T, conn driver.Conn, marker string) uint64 {
	t.Helper()
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	return scanWinScalar(ctx, t, conn, fmt.Sprintf(
		"SELECT toUInt64(sum(read_rows)) FROM system.query_log "+
			"WHERE type = 'QueryFinish' AND log_comment = '%s' AND query NOT LIKE '%%query_log%%'",
		marker,
	))
}

// scanWinRawReadRows runs a raw query under a unique log_comment and returns its
// read_rows — used for the leaf-pruning control measured directly on the driver.
func scanWinRawReadRows(ctx context.Context, t *testing.T, conn driver.Conn, marker, sql string) uint64 {
	t.Helper()
	runCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": marker}))
	if err := scanWinDrain(runCtx, conn, sql); err != nil {
		t.Fatalf("raw control query (%s): %v\n%s", marker, err, sql)
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	return scanWinScalar(ctx, t, conn, fmt.Sprintf(
		"SELECT toUInt64(read_rows) FROM system.query_log "+
			"WHERE type = 'QueryFinish' AND log_comment = '%s' AND query NOT LIKE '%%query_log%%' "+
			"ORDER BY event_time_microseconds DESC LIMIT 1",
		marker,
	))
}

func scanWinDrain(ctx context.Context, conn driver.Conn, sql string) error {
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	return rows.Err()
}

func scanWinScalar(ctx context.Context, t *testing.T, conn driver.Conn, query string) uint64 {
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

// seedScanWinTraces inserts scanWinDays daily partitions of root-rooted 5-span
// trace trees server-side via INSERT … SELECT FROM numbers(), byte-deterministic
// and needing no external generator. Each tree: root(Server) → auth(Internal) →
// cache(Client); root → checkout(Server,Error) → db(Client). Traces spread
// uniformly across each day so the one-day window captures exactly one partition.
func seedScanWinTraces(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()
	secPerTrace := 86400 / scanWinTracesPerDay
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
    map('server.address', 'h1', 'url.path', '/checkout') AS SpanAttributes,
    1000 AS Duration,
    if(sp = 2, 'Error', 'Unset') AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM (
    SELECT
        number AS tr,
        intDiv(number, %d) AS day,
        number %% %d AS wd,
        concat('t', leftPad(toString(number), 8, '0')) AS traceid,
        toDateTime64('%s', 9) + toIntervalDay(day) + toIntervalSecond(wd * %d) AS base_ts
    FROM numbers(%d)
) AS tr
ARRAY JOIN [0, 1, 2, 3, 4] AS sp`,
		scanWinTracesPerDay, scanWinTracesPerDay,
		scanWinBase.Format("2006-01-02 15:04:05.000000000"), secPerTrace, scanWinTotalTraces)

	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

func startScanWinCH(ctx context.Context, t *testing.T) *chclient.Client {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		scanWinCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(scanWinDB),
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
		Addr:                host + ":" + port.Port(),
		Database:            scanWinDB,
		Username:            "cerberus",
		Password:            "cerberus",
		BreakerDisabled:     true,
		MaxQueryMemoryBytes: scanWinMemoryCapBytes,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
