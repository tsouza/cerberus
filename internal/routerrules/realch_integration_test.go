//go:build integration

// realch_integration_test.go — the router-rules corpus WRITE + READ paths
// exercised against a REAL ClickHouse through the production clickhouse-go/v2
// driver.
//
// # WHY THIS LANE EXISTS
//
// The router-calibration corpus has two CH-touching seams that run, in
// production, against a real ClickHouse over the native protocol via
// clickhouse-go/v2:
//
//   - the WRITE path — optcorpus.CHTableSink: it issues the corpus CREATE TABLE
//     DDL and streams reconciled rows through clickhouse-go's columnar batch
//     API (PrepareBatch + Append + Send);
//   - the READ path — routerrules.chCorpusSource: the offline go/no-go
//     analysis scans that table, decoding aggregate columns positionally into
//     Go destinations (*float64 / *int64) via clickhouse-go's STRICT Scan.
//
// Until this lane, BOTH seams were only ever tested two ways, and neither sees
// what production sees:
//
//  1. unit tests with a fake CHExecer / in-memory rows (chtable_test.go,
//     source_ch_test.go) — they never touch a real driver or a real server, so
//     a wire-type / Scan-type mismatch is invisible to them;
//  2. the cross-backend parity tests (parity_chdb_test.go) — they DO execute
//     the chCorpusSource SQL, but against chDB (libchdb behind chdb-go's
//     Parquet database/sql driver), which leniently COERCES result-column types
//     into the Go destination a Scan supplies. A UInt64 count() or an
//     integer-typed quantileExact lands happily in a *float64 under chDB.
//
// clickhouse-go/v2's Scan is the opposite: STRICT. A column whose CH type does
// not match the Go destination is a hard error ("converting ... to ...",
// code 47), which the operator surfaces as a 502. This is the exact class of
// bug #1064 was: the chCorpusSource aggregate expressions returned integer CH
// types (count → UInt64; sum/max/min/quantileExact over UInt* columns →
// integer) but the cursor scanned them into *float64 / *int64. Every chDB
// parity test passed; the read path 502'd against real ClickHouse. The fix
// wrapped every integer-returning aggregate in toFloat64(...) / toInt64(...)
// (source_ch.go) so the wire type matches the Scan destination exactly.
//
// The chDB parity lane is structurally BLIND to that class — it only ever
// observes the COERCED value. compose-smoke / e2e never exercise the offline
// corpus paths at all (they drive the data plane, not the calibration
// reconciler). So nothing in CI ran the WRITE + READ corpus seams through the
// strict driver against a real server. This test closes that gap.
//
// # WHAT THIS TEST DOES
//
// It spins ONE real clickhouse/clickhouse-server via testcontainers (the exact
// pattern internal/chclient/{client,columnar}_integration_test.go, the
// schema-integration lane, and the strict-scan lane already use), connects via
// chclient (the production driver wrapper), then:
//
//  1. WRITE: builds optcorpus.NewCHTableSink over the live driver.Conn — this
//     runs the real CREATE TABLE DDL — and calls sink.Write on the
//     effectiveness corpus rows, streaming them through the real columnar batch
//     (so a column-order / batch-Append type mismatch fails here);
//  2. READ: runs the embedded catalog through routerrules.NewCHCorpusSource +
//     NewEvaluator.Evaluate over the SAME live connection — every aggregate
//     SELECT is strict-scanned exactly as in production;
//  3. asserts the evaluation returns NO scan/type error and produces sane,
//     non-empty findings whose support counts are positive.
//
// Step 2 is the load-bearing one: with #1064's toFloat64/toInt64 wraps reverted
// in source_ch.go, the Scan calls in scalarAggregate / scalarOrPartition /
// countRatio / EvalRule fail against this real ClickHouse exactly as they did
// in production — the failure the chDB parity lane swallows.
//
// Gated by the `integration` build tag (Docker required); INFORMATIONAL — wired
// on pull_request + push but NOT (yet) a required status check. See
// .github/workflows/strict-scan.yml (this test joins that lane).
package routerrules

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/optcorpus"
)

// realCHStartTimeout bounds container pull + start + the full write/read cycle.
const realCHStartTimeout = 5 * time.Minute

// realCHImage pins the ClickHouse server image, matching the chclient + strict-
// scan integration lanes so all real-CH lanes exercise the same server version.
const realCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// TestCorpusWriteReadRealClickHouse is the real-CH end-to-end guard for the
// router-corpus WRITE (optcorpus.CHTableSink) and READ (chCorpusSource) paths.
// It is the production-driver counterpart of the chDB-based parity tests, which
// are blind to the strict-scan class of bug #1064 (see file header).
func TestCorpusWriteReadRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), realCHStartTimeout)
	defer cancel()

	// client.Conn() returns the production clickhouse-go/v2 driver.Conn, which
	// satisfies both optcorpus.CHExecer (Exec + PrepareBatch, the WRITE surface)
	// and routerrules.CHConn (Query, the READ surface).
	conn := startRealCH(ctx, t).Conn()

	// --- WRITE path: real CREATE TABLE DDL + columnar batch INSERT. ---
	//
	// This is the write-side analog of #1064 with LESS protection than #1064
	// got: chtable.go binds the route / exit_status Enum8 columns as raw int8
	// (routeEnumValue / exitEnumValue) and streams them through clickhouse-go's
	// columnar Append, which is STRICT about Enum8 binding + column order.
	// chdb-go is lenient, and the only existing chtable_test uses a fakeBatch
	// whose Append is a no-op — so an Enum8/int8 binding or DDL column-order bug
	// would pass CI green and 502 at the first prod batch.Send(). Running the
	// real sink against real ClickHouse and reading the rows back asserts the
	// columnar Append landed every column — enums included — correctly.
	rows := loadCorpusRows(t, "testdata/effectiveness.jsonl")
	sink, err := optcorpus.NewCHTableSink(ctx, conn)
	if err != nil {
		t.Fatalf("create CH table sink (real CREATE DDL): %v", err)
	}
	if err := sink.Write(rows); err != nil {
		t.Fatalf("write corpus rows through real columnar batch: %v", err)
	}

	// WRITE read-back: the rows must have actually landed, and the Enum8
	// columns (route / exit_status) must decode to the named values the int8
	// binding mapped — proof the strict columnar Append bound the enums and the
	// column order correctly, not just that Send() returned nil.
	assertCorpusLanded(ctx, t, conn, rows)

	// --- READ path: strict-scan every aggregate the catalog resolves. ---
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := effConfig()
	opts := EvalOptions{IncludeExperimental: true}

	// Resolve the corpus-derived params first: this drives chCorpusSource's
	// scalar/partitioned Aggregate (quantileExact/sum/max/min/countRatio) — the
	// exact Scan-into-*float64 sites #1064 fixed. A strict-driver type mismatch
	// surfaces here.
	env, err := NewParamResolver(cfg, NewCHCorpusSource(conn, 0)).Resolve(ctx, cat)
	if err != nil {
		t.Fatalf("resolve corpus params (strict-scan aggregate): %v", err)
	}
	if len(env) == 0 {
		t.Fatalf("expected resolved corpus params, got empty env")
	}

	// Then the full evaluation: this drives chCorpusSource.EvalRule, whose
	// SELECT scans count()→*int64 and evidence aggregates→*float64 — the other
	// strict-scan sites #1064 fixed.
	report, err := NewEvaluator(cat, cfg, NewCHCorpusSource(conn, 0)).Evaluate(ctx, opts)
	if err != nil {
		t.Fatalf("evaluate against real ClickHouse (strict-scan rule groups): %v", err)
	}

	// Sane findings: the effectiveness corpus exercises every rule's fire path,
	// so a green strict-scan must produce non-empty findings with positive
	// support — proof the read path decoded real rows, not just an empty table.
	if len(report.Findings) == 0 {
		t.Fatalf("expected non-empty findings from effectiveness corpus, got none")
	}
	for i, f := range report.Findings {
		if f.RuleID == "" {
			t.Errorf("finding[%d] has empty RuleID: %+v", i, f)
		}
		if f.Support <= 0 {
			t.Errorf("finding[%d] (%s) has non-positive support %d", i, f.RuleID, f.Support)
		}
	}
	t.Logf("real-CH evaluation: %d resolved params, %d findings", len(env), len(report.Findings))
}

// TestQueryLogReadRealClickHouse gives the corpus WRITE-side's upstream — the
// system.query_log reconciler read (optcorpus.CHQueryLogSource.FinishedByQueryID)
// — the same defensive real-CH coverage. It is type-correct today (scans into
// uint64 / int32 / string, matching the column types) but fake-only, so it is
// one edit from a silent strict-scan regression like #1064. Here it runs the
// bounded query_log SELECT against a real server's real system.query_log: a
// query-id match is not required (the table may be mid-flush), the assertion is
// that the SELECT + its conservative settings are accepted and every returned
// row strict-scans into the SourceRow destinations without a type error.
func TestQueryLogReadRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), realCHStartTimeout)
	defer cancel()

	conn := startRealCH(ctx, t).Conn()

	// Run a query under a known query_id, then flush logs so the reconciler's
	// bounded SELECT has a real terminal row to strict-scan (rather than only
	// proving an empty result decodes).
	const seedQueryID = "11111111-1111-1111-1111-111111111111"
	seedCtx := clickhouse.Context(ctx, clickhouse.WithQueryID(seedQueryID))
	if err := conn.Exec(seedCtx, "SELECT 1"); err != nil {
		t.Fatalf("seed query for query_log: %v", err)
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}

	src := optcorpus.NewCHQueryLogSource(conn, queryLogReadTimeout, optcorpus.QueryLogWindow(0))
	// The seeded id drives the IN(?) bind and should match the flushed terminal
	// row; the SELECT must execute and every returned row must strict-scan into
	// the SourceRow destinations (uint64 / int32 / string) without a type error.
	out, err := src.FinishedByQueryID(ctx, []string{seedQueryID})
	if err != nil {
		t.Fatalf("FinishedByQueryID against real system.query_log: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected the seeded query_id to appear as a terminal query_log row, got none")
	}
	if out[0].ExitStatus != optcorpus.ExitOK {
		t.Errorf("seeded QueryFinish should map to ExitOK, got %v", out[0].ExitStatus)
	}
	t.Logf("query_log read: %d terminal rows strict-scanned", len(out))
}

// queryLogReadTimeout bounds the query_log read in the integration test.
const queryLogReadTimeout = 30 * time.Second

// assertCorpusLanded reads the written corpus back and asserts (a) the row
// count matches what was written, and (b) the route / exit_status Enum8 columns
// decode to the same named-value distribution the int8 binding mapped — proof
// the strict columnar Append bound enums + column order correctly.
func assertCorpusLanded(ctx context.Context, t *testing.T, conn driver.Conn, want []optcorpus.Row) {
	t.Helper()

	var gotCount uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+optcorpus.CorpusTableName).Scan(&gotCount); err != nil {
		t.Fatalf("read back corpus count: %v", err)
	}
	if int(gotCount) != len(want) {
		t.Fatalf("corpus row count: wrote %d, read back %d", len(want), gotCount)
	}

	// The enum read-back: count rows per route name and per exit_status name,
	// compared against the expected distribution computed from the source Rows.
	// toString() on the Enum8 yields the named value (e.g. 'A' / 'oom'); a wrong
	// int8→Enum8 binding would shift these counts.
	wantRoute := map[string]int{}
	wantExit := map[string]int{}
	for _, r := range want {
		wantRoute[normRoute(r.Route)]++
		wantExit[normExit(r.ExitStatus)]++
	}
	gotRoute := scanEnumCounts(ctx, t, conn,
		"SELECT toString(route), count() FROM "+optcorpus.CorpusTableName+" GROUP BY route")
	gotExit := scanEnumCounts(ctx, t, conn,
		"SELECT toString(exit_status), count() FROM "+optcorpus.CorpusTableName+" GROUP BY exit_status")
	assertCountsEqual(t, "route", wantRoute, gotRoute)
	assertCountsEqual(t, "exit_status", wantExit, gotExit)
}

// scanEnumCounts runs a `SELECT toString(enum), count() ... GROUP BY enum`
// read-back and returns name→count. count() is UInt64; it is scanned into a
// uint64 (the correct strict-driver destination) to keep the read-back itself
// honest about CH wire types.
func scanEnumCounts(ctx context.Context, t *testing.T, conn driver.Conn, sql string) map[string]int {
	t.Helper()
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatalf("enum read-back query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			name string
			n    uint64
		)
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatalf("enum read-back scan: %v", err)
		}
		out[name] = int(n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enum read-back rows: %v", err)
	}
	return out
}

// normRoute / normExit mirror routeEnumValue / exitEnumValue: an empty / unknown
// route is stored as 'A', an empty / unknown exit_status as 'ok'.
func normRoute(route string) string {
	if route == "B" {
		return "B"
	}
	return "A"
}

func normExit(status string) string {
	switch status {
	case "oom", "timeout", "sample_budget", "breaker", "rejected":
		return status
	default:
		return "ok"
	}
}

func assertCountsEqual(t *testing.T, label string, want, got map[string]int) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s value set differs: want %v, got %v", label, want, got)
	}
	for k, w := range want {
		if g := got[k]; g != w {
			t.Errorf("%s[%q]: wrote %d, read back %d", label, k, w, g)
		}
	}
}

// startRealCH spins one ClickHouse server via testcontainers, connects through
// the production chclient wrapper, and returns the live driver.Conn — which
// satisfies both optcorpus.CHExecer (write) and routerrules.CHConn (read). The
// container + client are torn down via t.Cleanup.
func startRealCH(ctx context.Context, t *testing.T) *chclient.Client {
	t.Helper()

	const (
		chUser = "cerberus"
		chPass = "cerberus"
		chDB   = "otel"
	)
	container, err := tcclickhouse.Run(
		ctx,
		realCHImage,
		tcclickhouse.WithUsername(chUser),
		tcclickhouse.WithPassword(chPass),
		tcclickhouse.WithDatabase(chDB),
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
		Addr:     host + ":" + port.Port(),
		Database: chDB,
		Username: chUser,
		Password: chPass,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// loadCorpusRows decodes a JSONL corpus fixture (the optcorpus JSONL-sink
// format, whose JSON tags are optcorpus.Row's) into []optcorpus.Row for
// re-insertion through the real CH-table write path. event_time is ignored on
// read — the sink stamps it at write time — and the source scans with since=0,
// so every row is in-window.
func loadCorpusRows(t *testing.T, fixture string) []optcorpus.Row {
	t.Helper()
	f, err := os.Open(fixture) //nolint:gosec // test fixture path under testdata/
	if err != nil {
		t.Fatalf("open corpus fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []optcorpus.Row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r optcorpus.Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode corpus line: %v", err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus fixture: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("corpus fixture %s decoded to zero rows", fixture)
	}
	return out
}
