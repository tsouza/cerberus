//go:build chdb

// Package logql_spec_test — chDB-backed round-trip pass over the
// LogQL TXTAR fixtures.
//
// This test is gated behind the `chdb` build tag. It walks every
// *.txtar fixture under test/spec/logql/ and, for fixtures that
// declare both `seed:` and `expected_rows:`, executes the stored
// `sql:` + `args:` against an ephemeral in-process chDB session and
// asserts the resulting rows match `expected_rows:`.
//
// Fixtures without those sections fall through to text-equality only
// (the default check in internal/logql/lower_test.go still gates them).
package logql_spec_test

import (
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

func TestRoundTripChDB(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(".")
	spec.Walk(t, dir, func(t *testing.T, c *spec.Case) {
		// LoadRoundTrip + IsRoundTrip is the opt-in gate; fixtures
		// without seed/expected_rows are silent no-ops.
		spec.RunRoundTrip(t, c)
	})
}
