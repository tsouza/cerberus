//go:build chdb

// Package promql_spec_test — chDB-backed round-trip pass over the
// PromQL TXTAR fixtures.
//
// This test is gated behind the `chdb` build tag. It walks every
// *.txtar fixture under test/spec/promql/ and, for fixtures that
// declare both `seed:` and `expected_rows:`, executes the stored
// `sql:` + `args:` against an ephemeral in-process chDB session and
// asserts the resulting rows match `expected_rows:`.
//
// Fixtures without those sections fall through to text-equality only
// (the default check in internal/promql/lower_test.go still gates them).
package promql_spec_test

import (
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// TestRoundTripChDB walks the slice of the PromQL corpus this process owns.
//
// With SPEC_SHARD_INDEX / SPEC_SHARD_COUNT unset — a hand-typed
// `go test -tags chdb ./test/spec/promql/`, or CI's `roundtrip (promql)` job —
// that slice is the whole corpus and this behaves exactly as it did before the
// partition existed. `just update-golden promql` sets the pair and fans the
// generator out across several processes instead
// (.github/scripts/lib/golden-shards.mjs), because this walk is the single
// longest step in that recipe and it cannot be parallelised in-process: the
// subtests share chdb-go's package-level session singleton, which
// spec.resetChDBSession deliberately tears down and rebuilds between fixtures
// for cross-fixture isolation (#1987), so a t.Parallel() here would race two
// goroutines against that teardown. A separate process has its own singleton.
func TestRoundTripChDB(t *testing.T) {
	t.Parallel()

	shard, err := spec.ShardFromEnv()
	if err != nil {
		t.Fatalf("round-trip corpus shard: %v", err)
	}

	dir := filepath.Join(".")
	spec.WalkShard(t, dir, shard, func(t *testing.T, c *spec.Case) {
		// LoadRoundTrip + IsRoundTrip is the opt-in gate; fixtures
		// without seed/expected_rows are silent no-ops.
		spec.RunRoundTrip(t, c)
	})
}
