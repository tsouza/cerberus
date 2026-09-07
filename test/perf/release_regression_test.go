//go:build chdb

// TestReleasePerfRegression is the RELEASE gate: it re-profiles the current
// TXTAR corpus and asserts no shared fixture regressed against the frozen
// baseline of the LAST ACTUAL TAGGED RELEASE — not against the rolling
// per-PR baseline TestCardinalityRatchet uses.
//
// # The gap this closes (cerberus issue #3150)
//
// TestCardinalityRatchet's baseline is deliberately rolling: any PR that
// legitimately changes a fixture's structural cost re-baselines it via
// `just update-cardinality-baseline`, and the ratchet only ever compares
// against whatever was last committed. That is the correct design for a
// merge-blocking per-PR gate (a static baseline would either freeze for an
// entire release cycle or need a second mechanism), but it is structurally
// blind to SLOW DRIFT: a dozen individually-reviewed, individually-justified
// re-baselines across a release cycle can each look fine in isolation while
// summing to a real regression against what actually shipped last time —
// and nothing checked that sum.
//
// This test checks it, at release time, using the SAME structural
// comparison TestCardinalityRatchet already applies
// (compareCardinalityEntry, cardinality_ratchet_test.go) — fan_factor may
// not grow, scan_rows/has_array_join must match exactly, a CROSS JOIN or
// WITH RECURSIVE may not newly appear, recursion depth may not grow — just
// against a DIFFERENT, FROZEN baseline: test/perf/release-baseline/<version>/
// cardinality/, written once per release by
// .github/scripts/capture-release-perf-baseline.mjs (via
// `just capture-release-perf-baseline`, wired into `just release-prep`
// itself so every future release freezes its own reference for the next
// one — see that script's own doc) and never touched by
// `update-cardinality-baseline`.
//
// # What "the last release" means here
//
// test/perf/release-baseline/ holds one subdirectory per released version.
// releaseBaselineVersion picks the SEMVER-HIGHEST one — not "whichever was
// captured most recently by wall-clock", which would be wrong the moment a
// backport baseline for an OLDER line is captured after a newer mainline
// release already has one. Comparing against anything other than the
// highest released version would silently stop catching drift the moment a
// second release baseline existed.
//
// # Why only the matched set, and why silently
//
// A fixture with no counterpart in the release baseline is new since that
// release — there is nothing to regress FROM, so (unlike
// TestCardinalityRatchet's rolling "added" check) it is not an error, only
// a t.Logf note. Likewise a fixture the release baseline has that the
// current corpus does not is one that existed at that release and no
// longer does — also not a regression to report here. The rolling ratchet
// already owns keeping the corpus roster and the CURRENT baseline in exact
// sync (TestCardinalityBaselineCoversTheCorpus); this test's only job is
// the regression comparison over whatever fixtures both baselines can
// actually agree existed at both points in time.
package perf

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/perf/profile"
)

// releaseBaselineRoot is where every release's frozen cardinality snapshot
// lives, one subdirectory per version (see capture-release-perf-baseline.mjs).
const releaseBaselineRoot = "release-baseline"

// releaseRegen is what compareCardinalityEntry's messages interpolate as the
// fix. Every one of those messages reads as "regenerate with `%s`" or
// "run `%s`", so this has to read as a command, not a parenthetical aside —
// deliberately NOT cardinalityRegen ("just update-cardinality-baseline"):
// that recipe writes the ROLLING baseline this test does not read, and
// running it would silently do nothing to fix a release-gate failure. There
// is no command that fixes this: a release-gate regression means the query
// genuinely got more expensive since the last release, which needs a
// root-cause fix (or, if the cost increase is deliberate, a release-notes
// callout) — not a regeneration. The frozen baseline itself is only ever
// written by `just capture-release-perf-baseline`, once, at the NEXT release.
const releaseRegen = "(nothing — see release_regression_test.go's own doc: root-cause the regression, do not regenerate)"

// releaseBaselineVersion returns the semver-highest subdirectory name under
// test/perf/release-baseline/. This repo's own commit history never has a
// state with zero captured release baselines once this test exists — the
// v1.19.0 backfill lands in the same change that adds it, and
// `release-prep` captures every version after that — so an empty or
// missing root is a real misconfiguration (a corrupted checkout, a
// baseline accidentally deleted) and returns an error rather than a
// tolerated "nothing to gate against" state (CLAUDE.md invariant 6: a gap
// like that is asserted loudly, never silently passed).
func releaseBaselineVersion(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read %s: %w (has `just capture-release-perf-baseline` ever run?)", root, err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("%s holds no version subdirectories — the release perf gate has nothing to compare against", root)
	}
	sort.Slice(versions, func(i, j int) bool { return semverLess(versions[i], versions[j]) })
	return versions[len(versions)-1], nil
}

// semverLess compares two "X.Y.Z"-shaped version strings numerically per
// component (never lexically — "1.9.0" < "1.10.0" would sort backwards as
// plain strings). A component that fails to parse as a non-negative integer
// sorts as 0, which only matters for a malformed directory name; every
// directory this repo's own tooling creates is a clean release version.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			return na < nb
		}
	}
	return false
}

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

	for _, id := range matched {
		compareCardinalityEntry(t, id, current[id], release[id], releaseRegen)
	}
	t.Logf("release perf regression gate: %d fixture(s) checked against v%s", len(matched), version)
}
