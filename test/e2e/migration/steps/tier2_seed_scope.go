package steps

import (
	"fmt"
	"strings"
	"time"
)

// Seed-scope identity for the Tier-2 scenarios.
//
// Every Tier-2 scenario that seeds the recording rule's source series writes
// the SAME metric name (`node_cpu_seconds_total`, because that is what
// tiers/tier2-ruler/grafana/alerting/rules.yaml's `expr` selects) into ONE
// long-lived ClickHouse instance. Before this file existed they also wrote the
// same label set, over overlapping wall-clock windows, at different counter
// rates — MIG-09 at rulerCPUIdleRatePerSecond, MIG-13/MIG-19 at their own —
// so the stored series was a single identity whose value went up, then down,
// then up again as the two seeds interleaved. `rate()` reads every one of
// those downward steps as a counter reset, and the recording rule's output
// stops meaning anything: MIG-19's first live run reported a landed sample of
// 0.05 (MIG-09's `1 - 0.95`) against a live re-evaluation of -27.8, a NEGATIVE
// CPU utilisation that no correct evaluation of the expression can produce.
//
// The fix is identity, not tolerance: each scenario seeds under its own
// `seed_scope` label, so two seeds can never be the same stored series and no
// scenario's data can perturb another's. The recording rule's own
// `avg without (cpu)` keeps `seed_scope` as a grouping key, so each scope also
// gets its OWN recorded output series — which is what lets MIG-13/MIG-19 read
// back exactly the samples their own seed produced.
//
// This is the same shape as the Tier-1 fix in "keep the tier-1 archetypes
// apart by identity, not by window": overlapping windows are fine, colliding
// identities are not.
const tier2SeedScopeLabel = "seed_scope"

// tier2SeedScopeFallback names a scenario whose title is empty, so the scope
// is never a bare nonce with a leading separator.
const tier2SeedScopeFallback = "scenario"

// tier2RunNonce distinguishes two runs of the SAME scenario against ONE
// long-lived stack. `just migration-tier2-up` brings the stack up once and
// `just migration-tier2-run` can be re-run against it any number of times;
// without the nonce the second run would re-seed the identity the first run
// already wrote, re-creating exactly the interleaving this file exists to
// prevent. Process start time is enough: two runs are two processes.
var tier2RunNonce = fmt.Sprintf("%x", time.Now().UnixNano())

// tier2SeedScopeFor derives a scenario's seed scope from its title. Scenario
// titles are unique within the suite (godog reports duplicates), so the slug
// plus the per-process nonce is unique per scenario per run — and it reads
// back in a failure message as the scenario that wrote it, which a bare
// random token would not.
func tier2SeedScopeFor(scenarioName string) string {
	slug := tier2Slug(scenarioName)
	if slug == "" {
		slug = tier2SeedScopeFallback
	}
	return slug + "-" + tier2RunNonce
}

// tier2Slug lowercases scenarioName and collapses every run of characters
// outside [a-z0-9] into a single separator, with no leading or trailing one.
// The result goes into a PromQL label matcher and a ClickHouse Map key lookup,
// both of which would otherwise have to quote whatever punctuation a scenario
// title happens to carry.
func tier2Slug(s string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if pendingSep && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingSep = false
			b.WriteRune(r)
		default:
			pendingSep = true
		}
	}
	return b.String()
}
