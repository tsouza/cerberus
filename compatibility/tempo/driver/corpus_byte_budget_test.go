package main

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// minByteBudgetCeilingMargin is how far below the Tempo wide-projection
// byte ceiling (chclient.NewTempoSpanDrainBudget) this corpus's real charge
// must sit. It is deliberately loose (the compat fixture is a small,
// deterministic dataset, not a stress test) — the point of this test is
// not to prove the ceiling is tight, but that this repo's own compat
// corpus, the one thing #1540(c) named as unmeasured, is nowhere near it.
const minByteBudgetCeilingMargin = 500

// TestFixtureCorpus_WideProjectionByteBudget_WellBelowCeiling is the
// compatibility/tempo corpus-floor check #1540(c) named as absent: nothing
// under compatibility/tempo/ established that a legitimate Matched set
// stays clear of the 256 MiB wide-projection byte ceiling
// (chclient.DrainByteBudget), so the risk the budget exists to bound — a
// real query 422ing rather than a runaway one — was unmeasured.
//
// This charges chclient.SumWideProjectionBytes (the exact production
// labelMapBytes formula rowsCursor/columnarCursor use) over every span this
// harness's own seeder fixture (buildFixture) generates — the same data
// both /api/search and /api/traces/{id} drain in the live compose lane —
// using each span's ResourceAttributes ∪ SpanAttributes merged into one map
// (span wins on key collision, matching how a more-specific span-level
// attribute is expected to shadow a resource-level one). Per-span charging
// rather than per-trace is the conservative direction for THIS assertion:
// it is an upper bound on /api/search (which interns/shares repeated
// resource maps across spans, so the real charge is smaller), and a lower
// bound on /api/traces/{id} (which drains one whole trace's spans in one
// response) — so if the per-span sum already clears the ceiling by a wide
// margin, both real drain shapes do too.
//
// buildFixture is pure/in-memory (no ClickHouse, no Tempo, no network) —
// this runs as a plain `go test`, no Docker compose required.
func TestFixtureCorpus_WideProjectionByteBudget_WellBelowCeiling(t *testing.T) {
	anchor := time.Unix(0, 0).UTC()
	traces := buildFixture(anchor)
	if len(traces) == 0 {
		t.Fatal("buildFixture produced no traces — nothing to measure")
	}

	var labelSets []map[string]string
	totalSpans := 0
	for _, tr := range traces {
		for _, sp := range tr.spans {
			totalSpans++
			merged := make(map[string]string, len(tr.resAttrs)+len(sp.Attributes))
			for k, v := range tr.resAttrs {
				merged[k] = v
			}
			for k, v := range keyValuesToMap(sp.Attributes) {
				merged[k] = v
			}
			labelSets = append(labelSets, merged)
		}
	}

	peak := chclient.SumWideProjectionBytes(labelSets)
	ceiling := chclient.NewTempoSpanDrainBudget().Limit()

	if peak <= 0 {
		t.Fatal("measured zero wide-projection bytes — the fixture's attribute maps did not get charged")
	}
	margin := float64(ceiling) / float64(peak)
	t.Logf("corpus (%d traces / %d spans): peak=%d B (%.2f KiB) — ceiling=%d B (%d MiB), margin=%.0fx",
		len(traces), totalSpans, peak, float64(peak)/(1<<10), ceiling, ceiling>>20, margin)

	if peak*minByteBudgetCeilingMargin > ceiling {
		t.Errorf("compat fixture's real wide-projection charge %d B is within %dx of the %d B ceiling — want at least %dx margin (got %.0fx)",
			peak, minByteBudgetCeilingMargin, ceiling, minByteBudgetCeilingMargin, margin)
	}
}
