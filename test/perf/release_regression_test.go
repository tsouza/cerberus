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

// releaseRegen is the command baselineShards quotes when the FROZEN tree
// itself cannot be read — a missing directory, a corrupt shard file, a record
// filed under a name that is not its own. That is the one release-baseline
// failure a command does fix, and this is the command: the capture script
// rewrites the tree from the rolling baseline it was cut from.
//
// It is deliberately NOT cardinalityRegen ("just update-cardinality-baseline"):
// that recipe writes the ROLLING tree this test does not read, and running it
// would silently do nothing here.
//
// It is equally deliberately NOT what a fixture-level DIFFERENCE reports. Those
// take releaseRemedy instead, because re-cutting a frozen release reference is
// never the answer to a fixture that moved since that release — see its doc.
const releaseRegen = "just capture-release-perf-baseline <version>"

// releaseRemedy is what the release gate appends to a per-fixture difference a
// maintainer could legitimately record — the slot the rolling ratchet fills
// with `just update-cardinality-baseline`.
//
// There is no such command here, and the honest reason depends on WHICH of two
// situations produced the difference. The gate could not previously tell them
// apart, so it said "a real regression" for both (cerberus issue #3244):
//
//   - RECORDED SINCE THE RELEASE. The fixture's current measurement is exactly
//     what the committed ROLLING baseline holds, so some PR between that
//     release and now measured this value and re-recorded the row. #3214's
//     LogQL `[start, end)` fix is the worked example: a half-open entry window
//     legitimately stops scanning the endpoint sample, so logql/range_filter
//     went from 2 scan rows to 1. What that establishes is that the value was
//     RECORDED, not that the release-over-release delta is fine — this gate
//     exists precisely because a cycle's worth of individually-justified
//     re-baselines can sum to something none of them shows (see
//     `just capture-release-perf-baseline`'s own doc and issue #3150). So the
//     remedy sends the reader to that PR's justification, measured against the
//     release value, rather than to a defect hunt that will find nothing.
//   - LIVE DRIFT. The current measurement is not what the rolling baseline
//     holds either, so nothing has reviewed this value at all —
//     TestCardinalityRatchet is failing on this fixture too whenever the
//     difference runs in its direction. That is the root-cause case.
//
// Neither branch is an escape hatch: both still FAIL, and neither regenerates
// anything. What changes is that the reader is told which one they are looking
// at instead of being sent to root-cause a value some PR already recorded on
// purpose — the dead end issue #3244 hit.
//
// The discriminator is cardinalityEntryMatches, an EQUALITY, deliberately not
// "cardinalityEntryProblems is empty". That function is a one-sided ratchet: it
// stays silent on a fan_factor that fell, a recursion that got shallower and a
// fixture that became measurable, so a current value the rolling baseline does
// NOT hold would read as recorded. That is the wrong verdict on exactly the
// unreviewed drift this gate is for.
//
// rolling is the committed rolling baseline row for this fixture and ok says
// whether one exists. It always should: TestCardinalityBaselineCoversTheCorpus
// pins the rolling tree against the same corpus roster `current` is profiled
// from. When it does not, that absence is reported rather than folded into
// either verdict.
func releaseRemedy(version, id string, cur, rolling baselineEntry, ok bool) string {
	rollingPath := filepath.Join(baselinePath, id+shardExt)
	if !ok {
		return fmt.Sprintf("The rolling baseline has no row for this fixture at all (%s is missing), so "+
			"whether this difference was ever reviewed cannot be established here — "+
			"`%s` records it, and TestCardinalityBaselineCoversTheCorpus is the gate that should have "+
			"caught the gap.", rollingPath, cardinalityRegen)
	}
	if cardinalityEntryMatches(cur, rolling) {
		return fmt.Sprintf("This is a CHANGE SINCE v%s: the current measurement is exactly what the "+
			"committed rolling baseline records (%s), so some PR between v%s and now measured this "+
			"value and re-recorded the row. Read that PR's justification against v%s's value — this "+
			"gate's whole job is the SUM of a cycle's individually-justified re-baselines, which no "+
			"single one of them shows. Nothing is regenerated here either way: a frozen release "+
			"reference moves only at a release cut, where `just release-prep` freezes today's rolling "+
			"baseline as the next release's reference (docs/operations.md, \"The release ritual\").",
			version, rollingPath, version, version)
	}
	return fmt.Sprintf("This is LIVE DRIFT, not a change recorded since v%s: the current measurement is "+
		"not what the committed rolling baseline records either (%s), so nothing has reviewed this "+
		"value at all. Root-cause it; re-cutting the frozen v%s reference is never the fix.",
		version, rollingPath, version)
}

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

// TestReleaseRemedy_DistinguishesRatifiedChangeFromLiveDrift pins the whole
// point of cerberus issue #3244: the release gate has to be able to say which
// of two things a difference against the frozen reference IS.
//
// Before this, it could say only one — "a real regression" — and offered a
// parenthetical where a command would go. That wording sent the reader of a
// perfectly correct, already-reviewed change (#3214's LogQL `[start, end)`
// fix, which legitimately took logql/range_filter from 2 scan rows to 1) off
// to root-cause a defect that was not there, and then to conclude the trunk
// lane was deadlocked because no command could clear it.
//
// The discriminator is evidence the tree already holds: the ROLLING baseline.
// A value that matches it was measured, recorded and reviewed by some PR since
// the release; a value that does not match it is failing TestCardinalityRatchet
// right now and nobody has looked at it. This test drives both, plus the
// should-never-happen third case, through the real comparison rule.
//
// It deliberately does NOT assert that either verdict passes: both branches are
// still reported failures. The verdict is the wording, not the outcome.
func TestReleaseRemedy_DistinguishesRatifiedChangeFromLiveDrift(t *testing.T) {
	const (
		version = "1.20.0"
		id      = "logql/range_filter"
	)
	// The real #3214 row: the frozen v1.20.0 reference says 2, the corpus and
	// its rolling baseline both say 1.
	current := baselineEntry{Fixture: id, ScanRows: 1, PeakIntermediate: 1}

	recorded := releaseRemedy(version, id, current, baselineEntry{Fixture: id, ScanRows: 1, PeakIntermediate: 1}, true)
	// A rolling row that ALSO disagrees with the measurement, in the direction
	// the ratchet reports.
	live := releaseRemedy(version, id, current, baselineEntry{Fixture: id, ScanRows: 7, PeakIntermediate: 7}, true)
	absent := releaseRemedy(version, id, current, baselineEntry{}, false)
	// The case the discriminator is actually easy to get wrong on: the rolling
	// row disagrees in the direction the RATCHET IGNORES. fan_factor 3.0 against
	// a recorded 5.0 is a decrease, which TestCardinalityRatchet allows without
	// comment — so "the ratchet is silent" would call this recorded, when the
	// rolling baseline plainly holds a different number and nothing measured
	// 3.0 on purpose. Only an equality gets it right.
	fanFactor := func(v float64) *float64 { return &v }
	asymmetric := releaseRemedy(version, id,
		baselineEntry{Fixture: id, FanFactor: fanFactor(3), ScanRows: 1, PeakIntermediate: 3},
		baselineEntry{Fixture: id, FanFactor: fanFactor(5), ScanRows: 1, PeakIntermediate: 5}, true)

	if recorded == live {
		t.Fatalf("a change recorded between releases and live drift produce the SAME remedy, which is "+
			"the whole defect issue #3244 reports:\n%s", recorded)
	}
	for _, tc := range []struct {
		name string
		got  string
		want []string
		deny []string
	}{
		{
			name: "recorded-since-the-release",
			got:  recorded,
			// It must name the reference version, say plainly that the value
			// was recorded, and send the reader at the recording PR's own
			// justification rather than at a defect hunt.
			want: []string{"CHANGE SINCE v" + version, "rolling baseline", "justification", baselinePath},
			// Root-causing is exactly the wrong instruction here.
			deny: []string{"Root-cause"},
		},
		{
			name: "live-drift",
			got:  live,
			want: []string{"LIVE DRIFT", "Root-cause", baselinePath},
			deny: []string{"CHANGE SINCE"},
		},
		{
			// The whole point of the asymmetric fixture: a difference the
			// ratchet would not report is still a difference, so this must
			// land on the SAME side as live drift, never on "recorded".
			name: "rolling-disagrees-in-the-direction-the-ratchet-ignores",
			got:  asymmetric,
			want: []string{"LIVE DRIFT", "Root-cause"},
			deny: []string{"CHANGE SINCE"},
		},
		{
			name: "no-rolling-row",
			got:  absent,
			want: []string{"no row for this fixture", cardinalityRegen},
			deny: []string{"CHANGE SINCE", "LIVE DRIFT"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range tc.want {
				if !strings.Contains(tc.got, w) {
					t.Errorf("remedy does not mention %q:\n%s", w, tc.got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(tc.got, d) {
					t.Errorf("remedy wrongly mentions %q:\n%s", d, tc.got)
				}
			}
		})
	}
}

// TestCardinalityEntryProblems_RemediableSplit pins which differences carry the
// caller's remedy sentence and which do not.
//
// The split is not cosmetic. The remedy slot says how a maintainer records a
// difference that was intended, and a CROSS JOIN or an unbounded closure
// appearing where the baseline had none is never a thing to record — appending
// "regenerate and review the diff" to it would read as an offer to bless it.
// An identical pair must produce no problems at all, or every fixture would
// carry a remedy nobody asked for.
func TestCardinalityEntryProblems_RemediableSplit(t *testing.T) {
	const id = "promql/probe"
	base := baselineEntry{Fixture: id, ScanRows: 2, PeakIntermediate: 2}

	if got := cardinalityEntryProblems(id, base, base); len(got) != 0 {
		t.Errorf("an identical pair produced %d problem(s), want none: %+v", len(got), got)
	}

	drift := cardinalityEntryProblems(id, baselineEntry{Fixture: id, ScanRows: 1, PeakIntermediate: 1}, base)
	if len(drift) != 1 || !drift[0].remediable {
		t.Errorf("a scan_rows drift produced %+v; want exactly one REMEDIABLE problem", drift)
	}

	crossJoin := cardinalityEntryProblems(id,
		baselineEntry{Fixture: id, ScanRows: 2, PeakIntermediate: 2, HasCrossJoin: true}, base)
	if len(crossJoin) != 1 || crossJoin[0].remediable {
		t.Errorf("a newly-appeared CROSS JOIN produced %+v; want exactly one NON-remediable problem", crossJoin)
	}
}
