//go:build !chdb_agpl_oracle

// Package spec — the Tempo parity runner is absent from this build.
//
// The Tempo oracle links grafana/tempo's AGPL pkg/traceql and its vparquet4
// span implementation, so it compiles only under the synthetic
// `chdb_agpl_oracle` tag CI sets alongside `chdb` and `agpl_oracle`. In
// every other build this stub stands in, exactly as parity_nochdb.go's
// RunParity does when the `chdb` tag is unset.
//
// Invoking the absent seam is a hard failure. Parsing a `parity:` section is
// not evidence that the reference implementation actually ran, so a lane with
// the wrong tag set must fail rather than report a hollow green.
package spec

import "testing"

// runTempoParity fails when the `chdb_agpl_oracle` tag is not set. The real
// implementation lives in parity_tempo_chdb_agpl_oracle.go.
func runTempoParity(t *testing.T, c *Case, p *Parity, rt *RoundTripSections) {
	t.Helper()
	_, _ = p, rt
	t.Fatalf(
		"fixture %s is enrolled against the %q oracle, but this lane was built without "+
			"the `chdb_agpl_oracle` build tag, so the Tempo oracle is compiled out. "+
			"Run this package with `-tags chdb,agpl_oracle,chdb_agpl_oracle`.",
		c.Name, OracleTempo,
	)
}
