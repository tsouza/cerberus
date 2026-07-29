//go:build integration

// queryexit_realch_integration_test.go — the corpus reconciler's
// system.query_log terminal-row predicate and exit classification exercised
// against a REAL ClickHouse.
//
// # WHY THIS LANE EXISTS
//
// system.query_log.type is an Enum8 whose members ClickHouse resolves at
// analysis time. Comparing that column against a string that is NOT a member
// is not an error in the IN-list form: ClickHouse coerces the comparison to
// String and the term matches nothing, silently. A predicate that names a
// non-member therefore looks healthy — the query runs, rows come back for the
// members it did spell correctly — while every exception exit is invisible.
//
// No pure-Go test can catch that: a table test asserting the classifier's
// behaviour for a given type string agrees with whatever spelling the code
// uses, and both can be wrong together. Only a real server holds the enum's
// member list. This lane seeds one query per terminal outcome ClickHouse can
// produce, reads them back through the production CHQueryLogSource, and
// asserts each id lands with the exit status its ClickHouse error code means.
//
// Gated by the `integration` build tag (Docker required); run by
// `just router-corpus-integration`, which globs ./internal/optcorpus/... with
// -run 'RealClickHouse'.
package optcorpus

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chsql"
)

// queryExitCHImage pins the ClickHouse server image, matching every other
// real-CH lane so all of them exercise the same server version.
const queryExitCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// queryExitCHStartTimeout bounds container pull + start + the seed/read cycle.
const queryExitCHStartTimeout = 5 * time.Minute

// queryExitReadTimeout bounds one corpus SELECT in this lane.
const queryExitReadTimeout = 30 * time.Second

// Seed query ids, one per terminal outcome ClickHouse can produce for a
// dispatched query. They are the join keys the corpus reconciler uses.
const (
	seedIDOK       = "corpus-exit-ok"
	seedIDTimeout  = "corpus-exit-timeout"
	seedIDOOM      = "corpus-exit-oom"
	seedIDError    = "corpus-exit-error"
	seedIDPreStart = "corpus-exit-prestart"
)

// Seed queries, one per terminal outcome. The resource caps ride in the query
// text rather than as driver-level settings so the server applies them to this
// statement and no other, and so the seed's outcome does not depend on how the
// driver encodes a settings map.
const (
	// seedQueryOK finishes cleanly: type QueryFinish, exception_code 0.
	seedQueryOK = "SELECT 1"
	// seedQueryTimeout scans far past a one-second deadline. The WHERE keeps
	// the scan real — a bare count() over numbers() folds to a constant and
	// would never reach the deadline — and max_threads pins it to one core.
	seedQueryTimeout = "SELECT count() FROM numbers(100000000000) WHERE sipHash64(number) = 0 " +
		"SETTINGS max_execution_time = 1, timeout_overflow_mode = 'throw', max_threads = 1"
	// seedQueryOOM builds aggregation state past a memory limit set below the
	// smallest chunk the aggregator allocates.
	seedQueryOOM = "SELECT count(DISTINCT number) FROM numbers(100000000) SETTINGS max_memory_usage = 1000000"
	// seedQueryError raises a plain server-side exception mid-execution: a
	// fault that is neither a memory limit nor a deadline.
	seedQueryError = "SELECT throwIf(1, 'x')"
	// seedQueryPreStart fails during analysis, before execution starts, so
	// ClickHouse logs it as ExceptionBeforeStart with no cost columns.
	seedQueryPreStart = "SELECT * FROM no_such_table_for_corpus"
)

// TestQueryLogExitStatusRealClickHouse seeds one query per terminal
// system.query_log outcome and asserts the production CHQueryLogSource returns
// every EXCEPTION exit alongside the clean finish, each classified by its
// ClickHouse error code.
//
// ExceptionBeforeStart is deliberately absent from the result: those rows
// carry no cost (the query never started, so read_rows / memory_usage are 0 by
// construction), and folding zero-cost rows into a corpus whose sole purpose
// is a cost distribution would deflate every watermark learnt from it. The
// cerberus-side pre-dispatch rejections that matter are recorded at the engine
// seam instead. This test pins that exclusion behaviourally.
func TestQueryLogExitStatusRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), queryExitCHStartTimeout)
	defer cancel()

	conn := startQueryExitCH(ctx, t)

	seedOK(ctx, t, conn, seedIDOK, seedQueryOK)
	seedFailing(ctx, t, conn, seedIDTimeout, seedQueryTimeout)
	seedFailing(ctx, t, conn, seedIDOOM, seedQueryOOM)
	seedFailing(ctx, t, conn, seedIDError, seedQueryError)
	seedFailing(ctx, t, conn, seedIDPreStart, seedQueryPreStart)

	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}

	src := NewCHQueryLogSource(conn, queryExitReadTimeout, QueryLogWindow(0))
	out, err := src.FinishedByQueryID(ctx, []string{
		seedIDOK, seedIDTimeout, seedIDOOM, seedIDError, seedIDPreStart,
	})
	if err != nil {
		t.Fatalf("FinishedByQueryID against real system.query_log: %v", err)
	}

	got := map[string]ExitStatus{}
	for _, row := range out {
		got[row.QueryID] = row.ExitStatus
	}
	want := map[string]ExitStatus{
		seedIDOK:      ExitOK,
		seedIDTimeout: ExitTimeout,
		seedIDOOM:     ExitOOM,
		seedIDError:   ExitError,
	}
	if len(got) != len(want) {
		t.Errorf("terminal rows returned = %d (%v); want %d (%v)", len(got), got, len(want), want)
	}
	for id, wantStatus := range want {
		gotStatus, ok := got[id]
		if !ok {
			t.Errorf("query_id %q missing from the terminal rows; want %v", id, wantStatus)
			continue
		}
		if gotStatus != wantStatus {
			t.Errorf("query_id %q exit status = %v; want %v", id, gotStatus, wantStatus)
		}
	}
	if status, ok := got[seedIDPreStart]; ok {
		t.Errorf("ExceptionBeforeStart id %q was ingested as %v; it carries no cost and must be excluded",
			seedIDPreStart, status)
	}
}

// TestQueryLogTerminalReductionRealClickHouse runs the PRODUCTION terminal
// reduction over a synthetic two-row group whose values carry the real
// system.query_log.type Enum8, on a real server.
//
// A distributed query_log carries one row per participating node for the same
// query_id, so the reconciler groups by query_id and reduces the terminal
// type. The reduction must classify the group as FAILED whenever any
// constituent row is an exception, independently of how the enum orders its
// members: a failed query whose remote shard finished must not be reclassified
// as a clean exit.
//
// One node is all testcontainers spins, so this exercises the reduction's
// ordering semantics — the defect — over real enum-typed values rather than a
// real multi-node group. That the distributed case emits the two rows in the
// first place is documented ClickHouse behaviour the reconciler's grouping
// already assumes.
func TestQueryLogTerminalReductionRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), queryExitCHStartTimeout)
	defer cancel()

	conn := startQueryExitCH(ctx, t)

	var enumType string
	if err := conn.QueryRow(
		ctx,
		"SELECT type FROM system.columns WHERE database = 'system' AND table = 'query_log' AND name = 'type'",
	).Scan(&enumType); err != nil {
		t.Fatalf("read system.query_log.type declared type: %v", err)
	}

	// Each synthetic row carries a value of the REAL enum type, read from the
	// server above, so the reduction sees exactly what a query_log group holds.
	row := func(member string) chsql.Frag {
		return chsql.NewQuery().
			SelectAs(chsql.Call("CAST", chsql.InlineLit(member), chsql.InlineLit(enumType)),
				queryLogTypeColumn).
			Frag()
	}
	sql, args := chsql.NewQuery().
		Select(terminalFailedFrag()).
		From(chsql.Paren(chsql.UnionAll(
			row(queryLogFinishMember),
			row(queryLogExceptionMember),
		))).
		Build()
	if len(args) != 0 {
		t.Fatalf("reduction probe bound %d args; the members are inline literals", len(args))
	}

	var failed uint8
	if err := conn.QueryRow(ctx, sql).Scan(&failed); err != nil {
		t.Fatalf("run terminal reduction %q: %v", sql, err)
	}
	if failed != 1 {
		t.Errorf("terminal reduction over {%s, %s} = %d; the exception must dominate (want 1)",
			queryLogFinishMember, queryLogExceptionMember, failed)
	}
}

// seedOK runs a query that must SUCCEED under the given query id.
func seedOK(ctx context.Context, t *testing.T, conn driver.Conn, id, query string) {
	t.Helper()
	if err := conn.Exec(clickhouse.Context(ctx, clickhouse.WithQueryID(id)), query); err != nil {
		t.Fatalf("seed %q (%s): %v", id, query, err)
	}
}

// seedFailing runs a query that must FAIL under the given query id. A seed
// that unexpectedly succeeds fails the test: the terminal row it would produce
// is not the one the assertions are written against.
func seedFailing(ctx context.Context, t *testing.T, conn driver.Conn, id, query string) {
	t.Helper()
	err := conn.Exec(clickhouse.Context(ctx, clickhouse.WithQueryID(id)), query)
	if err == nil {
		t.Fatalf("seed %q (%s) unexpectedly succeeded; it must produce an exception exit", id, query)
	}
	t.Logf("seed %q failed as required: %v", id, err)
}

// startQueryExitCH spins one ClickHouse server via testcontainers and returns a
// live driver.Conn. The helper is duplicated rather than shared with
// internal/routerrules' equivalent so optcorpus's test binary does not import
// a sibling package purely for a container fixture.
func startQueryExitCH(ctx context.Context, t *testing.T) driver.Conn {
	t.Helper()

	const (
		chUser = "cerberus"
		chPass = "cerberus"
		chDB   = "otel"
	)
	container, err := tcclickhouse.Run(
		ctx,
		queryExitCHImage,
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
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{Database: chDB, Username: chUser, Password: chPass},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
