//go:build integration

// executor_realch_integration_test.go — proves the solver's mandatory
// per-shard memory apportionment (Executor.Execute / runShard, see
// executor.go §"MANDATORY PER-SHARD MEMORY APPORTIONMENT") is enforced by a
// REAL ClickHouse server, not merely computed correctly in Go.
//
// # WHY THIS LANE EXISTS
//
// executor_memory_apportion_test.go already pins the ARITHMETIC: given a
// route-A cap and an effective shard concurrency kEff, Execute must stamp
// exactly cap/kEff as the `max_memory_usage` setting on every shard's
// context, via chclient.WithQuerySetting. That test proves the Go-side
// wiring is correct using a fakeQuerier that never talks to ClickHouse at
// all — it inspects the settings MAP the context carries, not what a real
// server does with it.
//
// That leaves an unverified seam: does the stamped value actually reach the
// wire and get enforced? Two things could silently defeat it and no fake
// would ever notice:
//
//  1. a stamped `max_memory_usage` value that never makes it into the
//     driver's per-query Settings (e.g. a future refactor that starts a
//     second, non-merging clickhouse.WithSettings wrap — the exact
//     footgun querySettings's own doc comment in client.go warns against,
//     since a second wrap REPLACES the settings map instead of unioning
//     into it);
//  2. the chDB-based property/round-trip lanes elsewhere in this repo
//     giving a false sense of coverage for this exact setting. chDB
//     (libchdb) is a single embedded query engine, not the same
//     multi-threaded server process with its own per-query MemoryTracker
//     and OvercommitTracker that a real `clickhouse-server` runs — nothing
//     structurally guarantees an embedded engine enforces a per-query
//     `max_memory_usage` setting with the same fidelity (or at all) as the
//     real server does. This is the SAME class of chDB-vs-prod-CH blind
//     spot test/spec/strictscan_integration_test.go documents for Scan
//     type strictness (chDB leniently coerces column types that the real
//     clickhouse-go/v2 driver strict-scans); here the axis is a query
//     SETTING instead of a destination type, but the underlying reason is
//     identical — chDB and a real clickhouse-server are different engines,
//     and only one of them is what production talks to.
//
// Neither failure mode is visible from Go-side inspection or from a chDB
// round-trip; both are only visible by asking a REAL ClickHouse server "did
// you actually reject this" and "did you actually accept that".
//
// # WHAT THIS TEST DOES
//
// It spins ONE real `clickhouse/clickhouse-server` via testcontainers (the
// exact pattern test/spec/strictscan_integration_test.go and
// internal/routerrules/realch_integration_test.go already use), seeds one
// small synthetic Memory-engine table whose GROUP BY aggregation memory is
// deterministically controllable by two knobs — row count and GROUP BY
// cardinality (see the memProbe* constants below) — then runs the IDENTICAL
// query through a real *Executor twice, differing only in the route-A cap
// each *chclient.Client is configured with:
//
//   - a deliberately tiny cap, apportioned down to a per-shard budget far
//     below the query's deterministic peak memory: the real server must
//     abort with its native MEMORY_LIMIT_EXCEEDED (code 241);
//   - a deliberately generous cap, apportioned down to a per-shard budget
//     far above that same peak: the identical query must succeed.
//
// Both runs go through the SAME Executor.Execute -> runShard code path that
// production uses (emit, apportion, chclient.WithQuerySetting, QueryCursor,
// drain) — this test only swaps which *chclient.Client (and therefore which
// configured cap) the Executor dispatches through.
//
// Gated behind the `integration` build tag (Docker required); joins the
// INFORMATIONAL real-CH lane test/spec/strictscan_integration_test.go and
// internal/routerrules/realch_integration_test.go already established —
// run locally with:
//
//	go test -race -tags=integration -run TestExecutor_PerShardMaxMemoryUsage_RealClickHouse ./internal/solver/...
package solver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// memProbeCHImage pins the ClickHouse server image, matching the other
// real-CH integration lanes (strict-scan, router-rules, chclient) so every
// real-CH lane exercises the same server version.
const memProbeCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// memProbeStartTimeout bounds container pull + start + both Executor runs.
const memProbeStartTimeout = 5 * time.Minute

// memProbeExecuteTimeout is the Executor's own Config.Timeout for each
// Execute call — generous relative to the two runs' expected duration (the
// tiny-budget run aborts almost immediately; the generous-budget run scans
// a purely in-memory table) so neither run can be mistaken for a Go-side
// wall-clock timeout instead of a genuine server-side memory rejection.
const memProbeExecuteTimeout = 30 * time.Second

// The three knobs that make the probe query's peak memory deterministic —
// "row count x cardinality", per the mechanism this test exists to exercise
// (see the file header). groupArray retains one copy of every row's pad
// string in its aggregate state, so the query's peak memory is a simple
// product of memProbeRowCount and memProbePadBytes; memProbeGroupCardinality
// is a second, independent axis that spreads those rows across multiple
// GROUP BY buckets (exercising the aggregation hash-table dimension) rather
// than collapsing everything into a single group.
const (
	// memProbeRowCount is the row count of the synthetic mem_probe table —
	// the primary memory-scaling knob.
	memProbeRowCount = 400_000

	// memProbeGroupCardinality is the number of distinct GROUP BY buckets
	// (grp = number % memProbeGroupCardinality).
	memProbeGroupCardinality = 50

	// memProbePadBytes is the per-row payload size groupArray retains for
	// every row.
	memProbePadBytes = 200
)

// memProbeApproxBytes is the deterministic peak-memory estimate the two
// budgets below are sized against: memProbeRowCount rows, each retained as
// a memProbePadBytes-byte string element inside one shared groupArray
// aggregate state (≈76 MiB). It deliberately UNDERESTIMATES the true figure
// (Array element/offset overhead, GROUP BY hash-table bookkeeping), which is
// exactly why the tiny/generous per-shard budgets below leave an
// order-of-magnitude margin on both sides rather than bracketing this
// number tightly — the assertion only needs "far below" and "far above", not
// a precise threshold.
const memProbeApproxBytes = memProbeRowCount * memProbePadBytes

const (
	// tinyPerShardMemoryBytes is the per-shard budget the tiny-cap run
	// expects Execute to stamp: far below memProbeApproxBytes, so the real
	// server must abort with MEMORY_LIMIT_EXCEEDED long before the
	// aggregation state finishes filling.
	tinyPerShardMemoryBytes = 4 << 20 // 4 MiB, ~18x below memProbeApproxBytes

	// generousPerShardMemoryBytes is the per-shard budget the generous-cap
	// run expects Execute to stamp: far above memProbeApproxBytes, so the
	// IDENTICAL query must succeed under the identical code path.
	generousPerShardMemoryBytes = 512 << 20 // 512 MiB, ~7x above memProbeApproxBytes
)

// memProbeShardCount is K (== kEff, since Cfg.Parallel is set to the same
// value and no gate is configured — see newMemProbeExecutor): the Decision
// fans the identical probe query out over this many shards, so Execute's
// apportionment divides each *chclient.Client's configured route-A cap by
// memProbeShardCount to derive the per-shard budget actually stamped.
const memProbeShardCount = 2

// memProbeTable / memProbeCreateSQL / memProbeInsertSQL seed one small,
// purely synthetic Memory-engine table: two columns, no MergeTree part
// machinery, no on-disk footprint beyond the process's own memory. The pad
// column is a fixed repeated character rather than random data — the
// mechanism under test is peak QUERY-TIME memory (the decompressed,
// materialised groupArray state), not on-disk table size, so there is no
// reason to fight ClickHouse's compression for a synthetic fixture.
const memProbeTable = "mem_probe"

var memProbeCreateSQL = fmt.Sprintf(
	"CREATE TABLE %s (grp UInt32, pad String) ENGINE = Memory", memProbeTable,
)

var memProbeInsertSQL = fmt.Sprintf(
	"INSERT INTO %s SELECT number %% %d AS grp, repeat('x', %d) AS pad FROM numbers(%d)",
	memProbeTable, memProbeGroupCardinality, memProbePadBytes, memProbeRowCount,
)

// memProbeAggregateSQL is the query both Executor runs dispatch, verbatim.
// It projects the four-column (String, Map(String,String), DateTime,
// Float64) shape chclient's rowsCursor scans positionally
// (MetricName/Attributes/TimeUnix/Value), so a successful run genuinely
// decodes every row through the same driver Scan production uses — not just
// "the query didn't error". groupArray(pad) is the memory-scaling operator:
// ClickHouse must materialise one aggregate-state array per GROUP BY bucket
// holding every matching row's pad string before it can return the first
// output row, so the query's peak memory is paid in full regardless of how
// small the final projected result is.
const memProbeAggregateSQL = `SELECT
	toString(grp) AS MetricName,
	map('grp', toString(grp)) AS Attributes,
	now() AS TimeUnix,
	toFloat64(length(groupArray(pad))) AS Value
FROM ` + memProbeTable + `
GROUP BY grp
ORDER BY grp`

// memProbeEmitter emits the SAME memProbeAggregateSQL for every shard,
// ignoring the Slice.Plan entirely — mirroring fakeEmitter's contract
// (exec_fakes_test.go). This test exercises Execute's per-shard memory
// apportionment against a real server, not plan lowering, so every shard
// deliberately runs the identical deterministic-memory query.
type memProbeEmitter struct{}

func (memProbeEmitter) Emit(_ context.Context, _ chplan.Node) (string, []any, error) {
	return memProbeAggregateSQL, nil, nil
}

// TestExecutor_PerShardMaxMemoryUsage_RealClickHouse is the real-CH
// end-to-end guard for the solver's mandatory per-shard memory
// apportionment (see file header). It is the production-driver counterpart
// of executor_memory_apportion_test.go, which is blind to whether a real
// ClickHouse server actually enforces the stamped setting.
func TestExecutor_PerShardMaxMemoryUsage_RealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), memProbeStartTimeout)
	defer cancel()

	addr, chDB, chUser, chPass := startMemProbeCH(ctx, t)
	seedMemProbeTable(ctx, t, addr, chDB, chUser, chPass)

	// The tiny-cap run: apportioned per-shard budget (tinyPerShardMemoryBytes)
	// sits far below memProbeApproxBytes, so both shards' identical
	// aggregation must be aborted server-side.
	t.Run("tiny per-shard budget is rejected by real ClickHouse", func(t *testing.T) {
		tinyClientCap := int64(tinyPerShardMemoryBytes * memProbeShardCount)
		x := newMemProbeExecutor(t, addr, chDB, chUser, chPass, tinyClientCap)

		cur, _, err := x.Execute(ctx, "promql", makeDecision(memProbeShardCount), nil)
		if err != nil {
			// A memory rejection at open time (before any block streams) is
			// an equally valid proof of enforcement as a mid-drain one — see
			// TestQueryCursor_OpenTimeMemoryLimit in internal/chclient.
			assertRealMemoryLimitError(t, err)
			return
		}
		defer func() { _ = cur.Close() }()

		n, drainErr := drainAll(cur)
		if drainErr == nil {
			t.Fatalf("tiny per-shard budget (%d bytes/shard) drained %d rows with no error; "+
				"want ClickHouse to reject a query whose deterministic peak memory (~%d bytes) "+
				"vastly exceeds the stamped max_memory_usage", tinyPerShardMemoryBytes, n, memProbeApproxBytes)
		}
		assertRealMemoryLimitError(t, drainErr)
	})

	// The generous-cap run: apportioned per-shard budget
	// (generousPerShardMemoryBytes) sits far above memProbeApproxBytes, so
	// the IDENTICAL query, through the IDENTICAL Executor/runShard code
	// path, must succeed and every row must strict-scan cleanly.
	t.Run("generous per-shard budget succeeds against real ClickHouse", func(t *testing.T) {
		generousClientCap := int64(generousPerShardMemoryBytes * memProbeShardCount)
		x := newMemProbeExecutor(t, addr, chDB, chUser, chPass, generousClientCap)

		cur, info, err := x.Execute(ctx, "promql", makeDecision(memProbeShardCount), nil)
		if err != nil {
			t.Fatalf("Execute with a generous per-shard budget (%d bytes/shard, ~7x the query's "+
				"deterministic peak of ~%d bytes) failed to even open: %v",
				generousPerShardMemoryBytes, memProbeApproxBytes, err)
		}
		defer func() { _ = cur.Close() }()

		n, drainErr := drainAll(cur)
		if drainErr != nil {
			var memLimit *chclient.MemoryLimitError
			if errors.As(drainErr, &memLimit) {
				t.Fatalf("generous per-shard budget (%d bytes/shard) still hit MEMORY_LIMIT_EXCEEDED "+
					"(cap=%d): %v — the budget is not actually generous relative to the query's real "+
					"peak memory; widen generousPerShardMemoryBytes or shrink the memProbe* fixture knobs",
					generousPerShardMemoryBytes, memLimit.Limit, drainErr)
			}
			t.Fatalf("drain under generous per-shard budget: %v", drainErr)
		}

		// Both shards run the identical full-table aggregation and their
		// outputs concatenate (docs §"Result composition"): every GROUP BY
		// bucket, once per shard.
		wantRows := memProbeGroupCardinality * memProbeShardCount
		if n != wantRows {
			t.Fatalf("drained %d rows, want %d (%d GROUP BY buckets x %d shards) — "+
				"the generous run must decode the full result, not merely avoid erroring",
				n, wantRows, memProbeGroupCardinality, memProbeShardCount)
		}
		if info.Parallelism != memProbeShardCount {
			t.Errorf("info.Parallelism = %d, want %d (kEff should equal the shard count here: "+
				"no gate, Cfg.Parallel == memProbeShardCount)", info.Parallelism, memProbeShardCount)
		}
	})
}

// assertRealMemoryLimitError asserts err is a genuine ClickHouse
// MEMORY_LIMIT_EXCEEDED rejection reaching the caller as
// *chclient.MemoryLimitError — never a Go-side wall-clock timeout
// (errSolverTimeout / context.DeadlineExceeded) and never any other
// unrelated failure. A wall-clock timeout would masquerade as "the tiny
// budget rejected the query" without proving the SETTING was what stopped
// it, which is exactly the false-positive this assertion rules out.
func assertRealMemoryLimitError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("assertRealMemoryLimitError: err is nil")
	}
	if errors.Is(err, errSolverTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got a Go-side wall-clock timeout, not a ClickHouse memory rejection: %v", err)
	}
	var memLimit *chclient.MemoryLimitError
	if !errors.As(err, &memLimit) {
		t.Fatalf("want *chclient.MemoryLimitError in the error chain (a genuine ClickHouse "+
			"MEMORY_LIMIT_EXCEEDED), got %T: %v", err, err)
	}
	if !errors.Is(err, chclient.ErrMemoryLimitExceeded) {
		t.Fatalf("errors.Is(err, chclient.ErrMemoryLimitExceeded) = false; want true (err=%v)", err)
	}
}

// newMemProbeExecutor dials a fresh *chclient.Client against the already-
// running container (configured with routeACapBytes as its route-A
// max_memory_usage cap — the value Execute apportions by kEff) and wires it
// into a real *Executor alongside memProbeEmitter. No Gate, no Breaker, no
// Admit: kEff collapses to min(K, Cfg.Parallel) == memProbeShardCount, which
// is exactly the divisor this test wants to reason about.
func newMemProbeExecutor(t *testing.T, addr, database, username, password string, routeACapBytes int64) *Executor {
	t.Helper()
	client, err := chclient.New(chclient.Config{
		Addr:                addr,
		Database:            database,
		Username:            username,
		Password:            password,
		MaxQueryMemoryBytes: routeACapBytes,
	})
	if err != nil {
		t.Fatalf("connect chclient (cap=%d): %v", routeACapBytes, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	cfg := DefaultConfig()
	cfg.Mode = ModeSharded
	cfg.Parallel = memProbeShardCount
	cfg.Timeout = memProbeExecuteTimeout
	cfg.MaxOutputRows = 1_000_000 // far above the handful of GROUP BY buckets this fixture drains

	return &Executor{
		Client:  client,
		Emitter: memProbeEmitter{},
		Cfg:     cfg,
	}
}

// startMemProbeCH spins one ClickHouse server via testcontainers and returns
// its connection coordinates. The container + its teardown are registered
// via t.Cleanup; callers dial their own *chclient.Client(s) against the
// returned coordinates (this test needs two, one per configured cap).
func startMemProbeCH(ctx context.Context, t *testing.T) (addr, database, username, password string) {
	t.Helper()

	// internal/solver's package-wide TestMain (stress_test.go) wraps every
	// test in this package in goleak.VerifyTestMain, so it can prove the
	// Executor's own producer goroutines always terminate. testcontainers'
	// Ryuk reaper is a SEPARATE, session-scoped background connection that
	// outlives any single container's Terminate() call by design (it reaps
	// every container the whole test process starts, not just this one), so
	// goleak — correctly, from its own package's perspective — would flag it
	// as an unexpected goroutine. It is not a leak in the code under test:
	// this test already tears its container down deterministically via the
	// t.Cleanup below, so the safety net Ryuk provides is redundant here.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	const (
		chUser = "cerberus"
		chPass = "cerberus"
		chDB   = "otel"
	)
	container, err := tcclickhouse.Run(
		ctx,
		memProbeCHImage,
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
	return host + ":" + port.Port(), chDB, chUser, chPass
}

// seedMemProbeTable creates and populates the synthetic mem_probe table
// through a plain, uncapped *chclient.Client (DDL/DML through Exec is exempt
// from MaxQueryMemoryBytes — see Config.MaxQueryMemoryBytes's doc comment —
// so seeding never risks tripping the very setting this test is about to
// probe).
func seedMemProbeTable(ctx context.Context, t *testing.T, addr, database, username, password string) {
	t.Helper()

	seedClient, err := chclient.New(chclient.Config{
		Addr:     addr,
		Database: database,
		Username: username,
		Password: password,
	})
	if err != nil {
		t.Fatalf("connect seed client: %v", err)
	}
	defer func() { _ = seedClient.Close() }()

	if err := seedClient.Exec(ctx, memProbeCreateSQL); err != nil {
		t.Fatalf("create mem_probe table: %v", err)
	}
	if err := seedClient.Exec(ctx, memProbeInsertSQL); err != nil {
		t.Fatalf("seed mem_probe table: %v", err)
	}
}
