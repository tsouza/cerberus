//go:build chaos_sleep

// This file is compiled ONLY when the binary is built with the
// `chaos_sleep` build tag (the chaos e2e lane's cerberus image — see
// Dockerfile.local's GO_BUILD_TAGS arg). It is absent from every other
// build: production, the compose-smoke image, and all CI lanes link the
// no-op sibling (chaos_sleep_stub.go) instead, so the sleep injection
// cannot reach a production query plan.
//
// Purpose: make the chaos `ch-slow-query-timeout` scenario DETERMINISTIC
// across substrates (compose vs k3d, which have different data volume
// and CPU). Timing calibration of a "naturally slow" query failed — the
// same expression ran ~650ms on compose but <250ms on k3d, so no static
// wall-clock cap separates it from a trivial query everywhere. A genuine
// server-side ClickHouse sleep is substrate-independent: it blocks for a
// fixed duration no matter the backend.

package chsql

import "context"

// chaosSleepKeyType is the unexported context-key type for the per-request
// chaos sleep duration. A dedicated struct type (not a string) keeps the
// key unforgeable and collision-free, matching chclient.WithQueryTimeout.
type chaosSleepKeyType struct{}

var chaosSleepKey = chaosSleepKeyType{}

// WithChaosSleepSeconds returns a ctx carrying a request-scoped server-side
// sleep duration (seconds). The prom chaos handler stamps it from the
// undocumented X-Cerberus-Chaos-Sleep-Seconds request header; Emit reads
// it back to splice a blocking ClickHouse sleep into the emitted SQL. A
// non-positive value is inert (no sleep is spliced).
//
// Exported so the api/prom chaos-tagged handler can set it without a
// shared third package — api/prom already depends on chsql in the
// architecture graph.
func WithChaosSleepSeconds(ctx context.Context, seconds int) context.Context {
	return context.WithValue(ctx, chaosSleepKey, seconds)
}

// chaosSleepSecondsFromContext returns the per-request sleep duration in
// seconds, or 0 when none was stamped.
func chaosSleepSecondsFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	s, _ := ctx.Value(chaosSleepKey).(int)
	return s
}

// ClickHouse hard-caps a SINGLE sleep / sleepEachRow evaluation at
// max_sleep_in_seconds (default 3s). The cap is checked PER BLOCK as
// (per-row seconds × rows-in-block): a sleepEachRow(s) over an N-row
// block requests s·N seconds for that block and is rejected UP FRONT with
//
//	code 160 — "The maximum sleep time is 3000000 microseconds. Requested:
//	            <s·N×1e6> microseconds per block"
//
// before any sleeping happens (so it can never time out — it 502s as a
// plain internal error). The old splice emitted sleepEachRow(1) over
// numbers(<seconds>); for seconds=10 that is a single 10-row block, so
// the per-block request was 1·10 = 10s > 3s → code 160, NOT the intended
// max_execution_time abort. See PR #915 / chaos run 27507299606.
const (
	// chMaxSleepSeconds is ClickHouse's per-block sleep cap
	// (max_sleep_in_seconds, default 3s). Each individual sleepEachRow
	// evaluation must request strictly less than this.
	chMaxSleepSeconds int64 = 3

	// chaosSleepPerCallSeconds is the per-ROW sleep each block requests.
	// It must be < chMaxSleepSeconds so a single per-row block
	// (guaranteed by the max_block_size=1 chaos setting, see
	// internal/api/prom/chaos_sleep.go) stays under CH's per-block cap and
	// is NOT rejected with code 160.
	chaosSleepPerCallSeconds int64 = 2

	// chaosSleepRows is the row count of the numbers() source. With
	// max_block_size=1 each row is its own block sleeping
	// chaosSleepPerCallSeconds; CH re-checks max_execution_time BETWEEN
	// blocks, so the cumulative block time (chaosSleepPerCallSeconds ×
	// chaosSleepRows) must comfortably exceed the chaos build's
	// max_execution_time (chaosCHExecutionCap, 3s) for CH to abort with
	// code 159 (TIMEOUT_EXCEEDED → breaker-neutral → 503 errorType=timeout)
	// part-way through. 2s × 10 rows = up to 20s cumulative, aborted at ~3s.
	chaosSleepRows int64 = 10
)

// Compile-time guard: the per-call sleep must stay strictly under CH's
// per-block cap, else every block trips code 160 (502) instead of letting
// max_execution_time abort with code 159 (503). The blank-array index
// fails to compile if the invariant ever regresses.
var _ = [1]struct{}{}[chaosSleepPerCallSeconds/chMaxSleepSeconds]

// chaosSleepWrap wraps an already-rendered (sql, args) pair in an outer
// `SELECT * FROM (<inner>) WHERE <blocking-sleep> >= 0` when the request
// ctx carries a positive chaos sleep duration; otherwise it returns the
// pair unchanged. The sleep is composed entirely from typed chsql Frags —
// no raw SQL strings — and runs SERVER-SIDE so ClickHouse genuinely
// blocks until its own max_execution_time aborts the query.
//
// The blocking predicate is a correlated-free scalar subquery:
//
//	WHERE (SELECT sum(sleepEachRow(2)) FROM numbers(10)) >= 0
//
// ClickHouse evaluates this scalar subquery exactly once, BEFORE streaming
// the outer rows, independent of how many rows the inner plan returns (so
// the block is guaranteed even for a 0-row inner result — e.g. a series
// matcher with no data). Paired with the chaos build's max_block_size=1
// setting, numbers(10) is read as 10 single-row blocks, each requesting
// only chaosSleepPerCallSeconds (2s) — strictly under CH's 3s per-block
// max_sleep_in_seconds cap, so no block is rejected with code 160. CH
// re-checks max_execution_time (3s on the chaos build) between blocks, so
// the query is aborted ~3s in with code 159 (TIMEOUT_EXCEEDED) — the path
// the scenario asserts (503 errorType=timeout, breaker stays CLOSED). The
// `>= 0` comparison is always true (the sum is non-negative), so no outer
// row is filtered: the query's RESULT SET is unchanged, only its
// wall-clock latency grows — until CH aborts it.
//
// The ctx-carried seconds value is no longer used as the per-call sleep
// magnitude (that was the code-160 bug): the cumulative block time is
// fixed at chaosSleepPerCallSeconds × chaosSleepRows, which any chaos
// max_execution_time below it will abort. A positive ctx value still
// gates whether the splice happens at all.
func chaosSleepWrap(ctx context.Context, sql string, args []any) (string, []any) {
	if chaosSleepSecondsFromContext(ctx) <= 0 {
		return sql, args
	}

	// (SELECT sum(sleepEachRow(<perCall>)) FROM numbers(<rows>))
	sleepSubquery := NewQuery().
		Select(Call("sum", Call("sleepEachRow", InlineLit(chaosSleepPerCallSeconds)))).
		From(Call("numbers", InlineLit(chaosSleepRows)))

	// SELECT * FROM (<inner>) WHERE (<sleepSubquery>) >= 0
	wrapped := NewQuery().
		Select(Star()).
		From(Subquery(PreRenderedSQL{SQL: sql, Args: args})).
		Where(Gte(Subquery(sleepSubquery), InlineLit(int64(0))))

	return wrapped.Build()
}
