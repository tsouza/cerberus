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

// chaosSleepWrap wraps an already-rendered (sql, args) pair in an outer
// `SELECT * FROM (<inner>) WHERE <blocking-sleep> >= 0` when the request
// ctx carries a positive chaos sleep duration; otherwise it returns the
// pair unchanged. The sleep is composed entirely from typed chsql Frags —
// no raw SQL strings — and runs SERVER-SIDE so ClickHouse genuinely
// blocks for the configured duration.
//
// The blocking predicate is a correlated-free scalar subquery:
//
//	WHERE (SELECT sum(sleepEachRow(1)) FROM numbers(<seconds>)) >= 0
//
// ClickHouse evaluates this scalar subquery exactly once, BEFORE streaming
// the outer rows, independent of how many rows the inner plan returns (so
// the block is guaranteed even for a 0-row inner result — e.g. a series
// matcher with no data). `numbers(n)` yields n rows; `sleepEachRow(1)`
// sleeps 1s per row, so the subquery blocks for `seconds` seconds total.
// `sleepEachRow` (per-row) sidesteps the single-`sleep()` 3s cap, letting
// the block exceed any wall-clock timeout cleanly. The `>= 0` comparison
// is always true (the sum is non-negative), so no outer row is filtered:
// the query's RESULT SET is unchanged, only its wall-clock latency grows.
func chaosSleepWrap(ctx context.Context, sql string, args []any) (string, []any) {
	seconds := chaosSleepSecondsFromContext(ctx)
	if seconds <= 0 {
		return sql, args
	}

	const sleepSecondsPerRow int64 = 1

	// (SELECT sum(sleepEachRow(1)) FROM numbers(<seconds>))
	sleepSubquery := NewQuery().
		Select(Call("sum", Call("sleepEachRow", InlineLit(sleepSecondsPerRow)))).
		From(Call("numbers", InlineLit(int64(seconds))))

	// SELECT * FROM (<inner>) WHERE (<sleepSubquery>) >= 0
	wrapped := NewQuery().
		Select(Star()).
		From(Subquery(PreRenderedSQL{SQL: sql, Args: args})).
		Where(Gte(Subquery(sleepSubquery), InlineLit(int64(0))))

	return wrapped.Build()
}
