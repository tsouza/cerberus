//go:build !chdb_agpl_oracle

// Package spec — the Tempo parity runner is absent from this build.
//
// The Tempo oracle links grafana/tempo's AGPL pkg/traceql and its vparquet4
// span implementation, so it compiles only under the synthetic
// `chdb_agpl_oracle` tag CI sets alongside `chdb` and `agpl_oracle`. In
// every other build this stub stands in, exactly as parity_nochdb.go's
// RunParity does when the `chdb` tag is unset.
//
// Inertness here is not a silent skip: the parity contract test in
// test/regression parses every `parity:` section on every commit
// (build-tag-free), and the chdb workflow's traceql leg runs this check
// with all three tags set, which is where a tempo-enrolled fixture is
// actually compared.
package spec

import "testing"

// runTempoParity is a no-op when the `chdb_agpl_oracle` tag is not set.
// The real implementation lives in parity_tempo_chdb_agpl_oracle.go.
func runTempoParity(t *testing.T, c *Case, p *Parity, rt *RoundTripSections) {
	t.Helper()
	_, _, _, _ = t, c, p, rt
}
