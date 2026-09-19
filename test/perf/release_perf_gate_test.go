//go:build chdb && release_perf

// TestReleasePerfRegression is enabled only for release-branch validation.
// Ordinary main CI runs the rolling ratchet; the frozen comparison is expected
// to report reviewed drift between release cuts.

package perf

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/test/perf/profile"
)

func TestReleasePerfRegression(t *testing.T) {
	version, err := releaseBaselineVersion(releaseBaselineRoot)
	if err != nil {
		t.Fatalf("resolve the release baseline version: %v", err)
	}
	t.Logf("release perf regression gate: comparing against v%s (%s)", version, filepath.Join(releaseBaselineRoot, version, "cardinality"))

	// Shares TestCardinalityRatchet's profile of this shard rather than
	// re-running the same chDB pass a second time — see
	// currentCardinalityEntries' own doc (cardinality_ratchet_test.go) for
	// why: a second independent pass is what pushed three CI legs over
	// perf-chdb's 27m timeout the first time this test shipped. Reported
	// here too (not left to TestCardinalityRatchet alone) so an
	// unprofilable fixture is still caught when this test runs by itself.
	shard, current, profErrs := currentCardinalityEntries(t)
	for _, e := range profErrs {
		t.Errorf("fixture failed to profile (was it profilable at baseline time?): %s", e)
	}

	releaseShards := baselineShards[baselineEntry]{
		dir:   filepath.Join(releaseBaselineRoot, version, "cardinality"),
		depth: cardinalityShardDepth,
		keyOf: func(e baselineEntry) string { return e.Fixture },
		regen: releaseRegen,
	}
	release, err := releaseShards.load()
	if err != nil {
		t.Fatalf("load the v%s release baseline: %v", version, err)
	}
	release = profile.FilterShardMap(shard, release)

	var newSince, retiredSince, matched []string
	for id := range current {
		if _, ok := release[id]; ok {
			matched = append(matched, id)
		} else {
			newSince = append(newSince, id)
		}
	}
	for id := range release {
		if _, ok := current[id]; !ok {
			retiredSince = append(retiredSince, id)
		}
	}
	sort.Strings(newSince)
	sort.Strings(retiredSince)
	sort.Strings(matched)

	if len(newSince) > 0 {
		t.Logf("%d fixture(s) new since v%s (nothing to regress from, not gated): %v", len(newSince), version, newSince)
	}
	if len(retiredSince) > 0 {
		t.Logf("%d fixture(s) present at v%s no longer in the corpus (not gated): %v", len(retiredSince), version, retiredSince)
	}

	// The ROLLING baseline is the second reference this gate reads, and it reads
	// it for ONE purpose: to tell a difference some PR already recorded between
	// releases from one nothing has looked at. It is never compared against for
	// the verdict — every difference below is still a failure against the FROZEN
	// reference (cerberus issue #3244).
	//
	// Its integrity is deliberately NOT fatal here, unlike in
	// TestCardinalityRatchet, which owns that tree. This gate's verdict does not
	// depend on it, so an unreadable rolling tree degrades the WORDING of a
	// failure rather than replacing this gate's own answer with a different
	// gate's complaint; releaseRemedy says so in as many words when a row is
	// missing.
	rollingAll, rollingErr := cardinalityShards.load()
	if rollingErr != nil {
		t.Logf("the rolling baseline is unreadable (%v) — every difference below is reported without "+
			"the recorded/live-drift distinction; %s owns that tree", rollingErr, baselinePath)
	}
	rolling := profile.FilterShardMap(shard, rollingAll)

	for _, id := range matched {
		rollingEntry, haveRolling := rolling[id]
		compareCardinalityEntry(t, id, current[id], release[id], func() string {
			return releaseRemedy(version, id, current[id], rollingEntry, haveRolling)
		})
	}
	t.Logf("release perf regression gate: %d fixture(s) checked against v%s", len(matched), version)
}
