//go:build chdb

// Package spec — round-trip SQL preparation seam (no *testing.T).
//
// [RunRoundTrip] (runner_chdb.go) is the assertion path: it seeds an
// ephemeral chDB session, executes the fixture's prepared SQL, and pins
// the result against `expected_rows:`. It is bound to `*testing.T` and
// fails the test on any mismatch.
//
// The nightly corpus-wide perf profiler (cmd/perf-profile via
// internal/perf/profile) needs the SAME seed + SQL-rewrite pipeline —
// `now64(...)` substitution, star-projection expansion, Map-column
// `toJSONString(...)` wrapping, projection-count extraction — but it is
// not a test: it walks every fixture, opens its own chDB session, runs
// `EXPLAIN PLAN actions=1` + a per-subquery `count()` decomposition, and
// reads `peak_memory_usage` from the query log. It must reuse the exact
// rewrite passes so the SQL it profiles is byte-identical to the SQL the
// round-trip assertion executes — otherwise the profiler would measure a
// different query than the one CI actually runs.
//
// This file hoists that pipeline behind an exported, test-free seam.
// [PrepareRoundTrip] returns the prepared seed + query + bound args for
// a loaded [Case], or (nil, false) when the fixture has not opted into
// round-trip execution. The rewrite helpers it calls
// (substituteNow64 / expandStarProjection / rewriteMapProjections /
// extractProjectionCount) stay unexported and shared with RunRoundTrip
// — there is exactly one rewrite pipeline, consumed two ways.
package spec

import "strings"

// PreparedRoundTrip is the seed + rewritten SQL + bound args for an
// executable round-trip fixture, produced by [PrepareRoundTrip]. It
// carries everything a non-test caller needs to seed a chDB session and
// run the fixture's query through the same rewrite pipeline the
// round-trip assertion uses — without depending on `*testing.T`.
type PreparedRoundTrip struct {
	// Seed is the raw `seed:` DDL+INSERT script. Callers split it on
	// top-level semicolons (see [SplitSeedStatements]) and promote bare
	// `CREATE TABLE` to `CREATE OR REPLACE TABLE` (see
	// [PromoteCreateTable]) for cross-fixture idempotency, exactly as
	// the round-trip runner's applySeed does.
	Seed string

	// Query is the fully-rewritten SQL: now64(...) substituted to the
	// deterministic anchor, star projections expanded, Map columns
	// wrapped in toJSONString(...). Byte-identical to the SQL
	// RunRoundTrip executes.
	Query string

	// Args is the bound []any matching the `?` placeholders remaining in
	// Query after now64(?) slots were consumed by the substitution pass.
	Args []any

	// ColumnCount is the outer SELECT projection count, or 0 when the
	// projection is a wildcard the count cannot be read off statically
	// (the caller falls back to rows.Columns() after execution).
	ColumnCount int
}

// PrepareRoundTrip loads the round-trip sections off c and runs the
// shared SQL-rewrite pipeline, returning the prepared seed + query +
// args. It returns (nil, false) when the fixture has not opted into
// round-trip execution (no `seed:` + `expected_rows:`) or carries an
// empty `sql:` section — the same gate RunRoundTrip applies before it
// touches chDB. An error is returned only when the fixture's sections
// are malformed (bad `expected_rows:` JSON or `args:` lines).
func PrepareRoundTrip(c *Case) (*PreparedRoundTrip, bool, error) {
	rt, err := LoadRoundTrip(c)
	if err != nil {
		return nil, false, err
	}
	if !rt.IsRoundTrip() || strings.TrimSpace(rt.SQL) == "" {
		return nil, false, nil
	}

	// Mirror RunRoundTrip's rewrite ordering exactly: substituteNow64
	// first (it consumes the now64(?) arg slots), then star expansion,
	// then Map-column wrapping, then the ORDER BY-over-Map nest guard,
	// then projection-count extraction. nestMapOrderBy must run after the
	// Map-wrap passes (it keys off the wrapped projection) and is required
	// for the sort_by_label / sort_by_label_desc shape — without it the
	// outer `ORDER BY Attributes['k']` binds to the toJSONString String
	// alias and chDB rejects arrayElement-over-String. Keeping it here (not
	// only in RunRoundTrip) is what makes Query byte-identical to the SQL
	// the round-trip assertion executes — the perf profiler reads this
	// prepared Query directly.
	query, queryArgs := substituteNow64(rt.SQL, rt.Args)
	query = expandStarProjection(query)
	query = rewriteMapProjections(query)
	query = nestMapOrderBy(query)
	colCount := extractProjectionCount(query)

	return &PreparedRoundTrip{
		Seed:        strings.Join(backfillResourceAttributes(splitStatements(rt.Seed)), ";\n"),
		Query:       query,
		Args:        queryArgs,
		ColumnCount: colCount,
	}, true, nil
}

// SplitSeedStatements and PromoteCreateTable now live in the build-tag-free
// roundtrip_prep.go so the perf profiler (chdb-tagged), the chDB round-trip
// runner, and the integration-tagged strict-scan differential all share one
// copy. They remain reachable from this package under any build tag.
