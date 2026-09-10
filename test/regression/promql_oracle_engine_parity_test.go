package regression

import (
	"os"
	"regexp"
	"testing"

	parityoraclepromql "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// TestPromQLOracleEngineParity pins #3271: cerberus grades PromQL against
// TWO independent reference surfaces, and they must run the reference
// engine's semantics-affecting options identically, or "matches the
// reference" has no single meaning.
//
//   - test/spec/parityoracle/promql/oracle.go builds its own promql.Engine
//     in-process for the spec lane, where most PromQL fixtures live and
//     where a change is graded first.
//   - compatibility/prometheus/ diffs against a real, separately started
//     `prom/prometheus` server, the higher-confidence surface and, per
//     invariant 7 (CLAUDE.md), the one whose behaviour is authoritative.
//
// #3271 found the two disagreeing on EnableDelayedNameRemoval: the spec
// oracle hardcoded it ON (promqltest.NewTestEngine's own default) while
// the compat server's docker-compose.yml enables only
// `promql-experimental-functions`, leaving delayed name removal at
// Prometheus's own default of OFF. On most shapes the two settings agree,
// but a name-dropping fold over a colliding histogram/float `or` does
// not: OFF raises "vector cannot contain metrics with the same labelset",
// ON silently answers one histogram-valued series. See
// docs/compatibility.md § "Two PromQL reference engines, one
// configuration" for the full decision and internal/promql/
// histogram_native_mixed_or_aggregate.go's combineMixedFoldBranches doc
// for the corrected mechanism.
//
// This test does not re-measure that divergence — TestEqualValuesRejectsRealDivergence-style
// re-derivation belongs to the property/compat lanes, which need a live
// engine or chDB. It pins the narrower, mechanically checkable half: that
// the ONE feature flag distinguishing the two engine configurations is
// set the SAME way on both sides. Flip either side without the other and
// this goes red instead of the two surfaces silently disagreeing again.
func TestPromQLOracleEngineParity(t *testing.T) {
	t.Parallel()

	compose, err := os.ReadFile("../../compatibility/prometheus/docker-compose.yml")
	if err != nil {
		t.Fatalf("read compatibility/prometheus/docker-compose.yml: %v", err)
	}

	serverEnablesDelayedNameRemoval := delayedNameRemovalFeatureRE.Match(compose)
	oracleEnablesDelayedNameRemoval := parityoraclepromql.EnableDelayedNameRemoval

	if serverEnablesDelayedNameRemoval != oracleEnablesDelayedNameRemoval {
		t.Fatalf(
			"PromQL reference-engine skew (cerberus issue #3271): "+
				"compatibility/prometheus/docker-compose.yml enables "+
				"promql-delayed-name-removal=%v on the real Prometheus server, "+
				"but test/spec/parityoracle/promql.EnableDelayedNameRemoval=%v. "+
				"The spec lane's oracle and the compat lane's real server must run "+
				"this flag identically — it changes ANSWERS (a name-dropping fold "+
				"over a colliding histogram/float `or`), not merely acceptance — or "+
				"'matches the reference' has two different meanings for the same "+
				"query. Align test/spec/parityoracle/promql/oracle.go's "+
				"EnableDelayedNameRemoval constant with the compose file's "+
				"--enable-feature list (see docs/compatibility.md § \"Two PromQL "+
				"reference engines, one configuration\"), then regenerate the "+
				"promql golden shard.",
			serverEnablesDelayedNameRemoval, oracleEnablesDelayedNameRemoval,
		)
	}
}

// delayedNameRemovalFeatureRE matches the reference Prometheus server's
// --enable-feature flag carrying promql-delayed-name-removal, tolerating
// it appearing alongside other comma-separated feature names in either
// position.
var delayedNameRemovalFeatureRE = regexp.MustCompile(
	`--enable-feature=\S*\bpromql-delayed-name-removal\b\S*`,
)
