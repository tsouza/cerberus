//go:build !chdb

// Package spec — round-trip stub when the `chdb` build tag is unset.
//
// The default `just test` lane stays pure Go / CGO_ENABLED=0 and
// must not pull in chdb-go (which links libchdb.so via cgo). This
// file provides a no-op RunRoundTrip so test files in lower-level
// packages can call spec.RunRoundTrip unconditionally; the seed:
// and expected_rows: sections are then inert.
package spec

import "testing"

// RunRoundTrip is a no-op when the `chdb` build tag is not set. The
// real implementation lives in runner_chdb.go.
func RunRoundTrip(t *testing.T, c *Case) {
	t.Helper()
	// Intentionally empty — seed: / expected_rows: are inert without
	// the chdb build tag.
	_ = c
}
