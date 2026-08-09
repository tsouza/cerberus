//go:build !chdb

// Package spec — parity stub when the `chdb` build tag is unset.
//
// The default `just test` lane stays pure Go / CGO_ENABLED=0 and must not
// pull in chdb-go (which links libchdb.so via cgo), nor the reference
// query engine. This file provides a no-op RunParity so the per-head
// lower_test.go files can call spec.RunParity unconditionally; the
// `parity:` section is then inert.
//
// The section's GRAMMAR is still checked in this lane —
// test/regression's parity contract test calls LoadParity, which is
// build-tag-free — so a malformed `parity:` body fails fast on every
// commit rather than only under the chdb-tagged lane.
package spec

import (
	"testing"
	"time"
)

// RunParity is a no-op when the `chdb` build tag is not set. The real
// implementation lives in parity_chdb.go.
func RunParity(t *testing.T, c *Case, evalStart, evalEnd time.Time, step time.Duration) {
	t.Helper()
	_, _, _, _, _ = c, evalStart, evalEnd, step, t
}
