//go:build chdb

// Component C of the perf-assessment framework: the cardinality / fan-factor
// RATCHET gate.
//
// Component B (`test/perf/profile`, the `perf-profile` nightly lane) profiles
// the WHOLE TXTAR corpus for compute fan-out but is informational — a new
// fan-out shows up as a step-summary outlier, not a merge block. This test
// turns that measurement into a per-PR ratchet: it re-profiles the corpus
// in-process via chDB and diffs every fixture against a committed baseline
// (`cardinality-baseline.json`). It runs inside the `perf-guards` job
// (`just perf-chdb` → `go test -tags chdb ./test/perf/...`), which is a
// REQUIRED status check on `main`, so a regression blocks the merge rather
// than colouring one non-blocking lane red.
//
// # What it ratchets (and what it deliberately does not)
//
// The baseline stores only the DETERMINISTIC, structural fan-out signals —
// fan_factor (peak intermediate rows / leaf scan rows), the leaf scan row
// count, the CROSS-JOIN / ARRAY-JOIN / recursive-CTE operator flags, and the
// recursion depth. Wall-time, peak_bytes_read and result_rows from
// profile.Record are intentionally excluded: they are environment-noisy and
// would make the baseline flap. The retained fields are pure functions of the
// lowered SQL over the seeded fixture data, so they reproduce run-to-run.
//
// A fixture FAILS the ratchet when, versus its baseline row:
//   - fan_factor grows (the headline compute-fan-out regression),
//   - scan_rows differs AT ALL (see "every stored field is compared" below),
//   - has_array_join differs at all (likewise),
//   - has_cross_join flips false→true (a CROSS JOIN appeared where none was),
//   - has_recursive_cte flips false→true (an unbounded closure appeared), or
//   - max_recursion_depth grows (a recursive walk got deeper).
//
// fan_factor DECREASES are always allowed (improvements never block); the
// committed ceiling only tightens when a maintainer re-runs
// `just update-cardinality-baseline`. The same asymmetry applies to the two
// operator flags with a "bad" direction (cross-join, recursive-CTE) and to the
// recursion depth: their appearance is the regression, their disappearance is
// the fix.
//
// # Every stored field is compared
//
// scan_rows and has_array_join have no "better" direction — they are
// structural identity, not a budget — so they are compared for EXACT equality.
// A field that is recorded but never compared is not soft coverage, it is a
// hole: the row can drift from the fixture it claims to describe and nothing
// says so. Both halves of that hole were live and both were found by
// regenerating the baseline rather than by the gate:
//
//   - #1415 (regex metric-name histogram fan-out) added an arrayJoin to three
//     promql fixtures' emitted SQL. has_array_join was documented as
//     "recorded for diff visibility, a flip alone is not a failure", so the
//     three rows kept claiming false against SQL that plainly contains
//     arrayJoin;
//   - #1411 recorded traceql/spanset_pipeline_intersect's row from the
//     PRE-rewrite `-- sql --` (`LIMIT 1 BY TraceId, SpanId`) while committing
//     the rewritten one (`LIMIT 1 BY TraceId`), pinning scan_rows /
//     peak_intermediate for a query that never shipped.
//
// Neither moved fan_factor, so the ratchet stayed green over rows that
// described queries the corpus does not contain. Comparing the fields the
// baseline already stores is what makes the recorded row an assertion about
// the fixture instead of a note about it.
//
// peak_intermediate stays diagnostic-only on purpose rather than by omission:
// with scan_rows pinned exactly, fan_factor = peak_intermediate / scan_rows,
// so the fan_factor ceiling already pins peak_intermediate's bad direction and
// a second rule would only forbid improvements.
//
// # New / removed fixtures force a baseline edit (built-in cost review)
//
// The baseline key-set must match the corpus exactly. A NEW fixture fails with
// a "run just update-cardinality-baseline" hint — which adds its row, so the
// fixture's absolute fan_factor lands in the PR diff and a reviewer sees the
// cost of the construct being introduced. A REMOVED fixture likewise fails
// until the stale row is dropped. This mirrors the `update-golden` discipline:
// any drift in either direction is a deliberate, reviewed baseline update.

package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/test/perf/profile"
)

// specDir is the TXTAR corpus root, relative to this package directory
// (test/perf → test/spec).
const specDir = "../spec"

// baselinePath is the committed ratchet baseline, relative to this package.
const baselinePath = "cardinality-baseline.json"

// updateEnv, when set to "1", regenerates the baseline from the current
// corpus profile instead of asserting against it. Mirrors the repo's
// GOLDEN_UPDATE convention; driven by `just update-cardinality-baseline`.
const updateEnv = "UPDATE_CARDINALITY_BASELINE"

// baselineEntry is the deterministic, structural subset of profile.Record the
// ratchet compares + commits. See the file doc for why the noisy fields are
// dropped.
type baselineEntry struct {
	Fixture           string  `json:"fixture"`
	FanFactor         float64 `json:"fan_factor"`
	ScanRows          int64   `json:"scan_rows"`
	PeakIntermediate  int64   `json:"peak_intermediate"`
	HasCrossJoin      bool    `json:"has_cross_join"`
	HasArrayJoin      bool    `json:"has_array_join"`
	HasRecursiveCTE   bool    `json:"has_recursive_cte"`
	MaxRecursionDepth int64   `json:"max_recursion_depth"`
}

// fanFactorEpsilon absorbs float representation noise in the
// peak_intermediate/scan_rows ratio so an exactly-equal fan_factor never
// trips the ceiling.
const fanFactorEpsilon = 1e-6

func toEntry(r profile.Record) baselineEntry {
	return baselineEntry{
		Fixture:           r.Fixture,
		FanFactor:         r.FanFactor,
		ScanRows:          r.ScanRows,
		PeakIntermediate:  r.PeakIntermediate,
		HasCrossJoin:      r.HasCrossJoin,
		HasArrayJoin:      r.HasArrayJoin,
		HasRecursiveCTE:   r.HasRecursiveCTE,
		MaxRecursionDepth: r.MaxRecursionDepth,
	}
}

func TestCardinalityRatchet(t *testing.T) {
	recs, err := profile.ProfileCorpus(specDir)
	if err != nil {
		t.Fatalf("profile corpus: %v", err)
	}

	// A fixture the profiler could not execute has no meaningful fan-out
	// signal — treat it as a hard regression (it used to be profilable) and
	// keep it out of the baseline.
	current := make(map[string]baselineEntry, len(recs))
	var profErrs []string
	for _, r := range recs {
		if r.Err != "" {
			profErrs = append(profErrs, fmt.Sprintf("%s: %s", r.Fixture, r.Err))
			continue
		}
		current[r.Fixture] = toEntry(r)
	}

	if os.Getenv(updateEnv) == "1" {
		if len(profErrs) > 0 {
			sort.Strings(profErrs)
			t.Fatalf("refusing to write a baseline with %d unprofilable fixture(s):\n  %v",
				len(profErrs), profErrs)
		}
		writeBaseline(t, current)
		t.Logf("wrote %s with %d fixtures", baselinePath, len(current))
		return
	}

	for _, e := range profErrs {
		t.Errorf("fixture failed to profile (was it profilable at baseline time?): %s", e)
	}

	baseline := loadBaseline(t)

	// New fixtures (in corpus, not in baseline) — add the row so the absolute
	// fan_factor surfaces in the diff for cost review.
	var added []string
	for id := range current {
		if _, ok := baseline[id]; !ok {
			added = append(added, id)
		}
	}
	// Removed fixtures (in baseline, not in corpus) — drop the stale row.
	var removed []string
	for id := range baseline {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	for _, id := range added {
		c := current[id]
		t.Errorf("new fixture %q not in cardinality baseline (fan_factor=%.2f, cross_join=%v, "+
			"recursive_cte=%v, max_depth=%d) — run `just update-cardinality-baseline` to record it",
			id, c.FanFactor, c.HasCrossJoin, c.HasRecursiveCTE, c.MaxRecursionDepth)
	}
	for _, id := range removed {
		t.Errorf("baseline fixture %q no longer in the corpus — run `just update-cardinality-baseline` "+
			"to drop the stale row", id)
	}

	// Per-fixture upward-regression ratchet over the matched set.
	matched := make([]string, 0, len(current))
	for id := range current {
		if _, ok := baseline[id]; ok {
			matched = append(matched, id)
		}
	}
	sort.Strings(matched)
	for _, id := range matched {
		cur, base := current[id], baseline[id]
		if cur.FanFactor > base.FanFactor+fanFactorEpsilon {
			t.Errorf("%s: fan_factor regressed UPWARD %.2f → %.2f (peak_intermediate %d→%d at scan_rows %d). "+
				"A query that fanned out N× now fans out more — root-cause the new intermediate blow-up; "+
				"only run `just update-cardinality-baseline` if the increase is genuinely intended.",
				id, base.FanFactor, cur.FanFactor, base.PeakIntermediate, cur.PeakIntermediate, cur.ScanRows)
		}
		if cur.ScanRows != base.ScanRows {
			t.Errorf("%s: scan_rows drifted %d → %d. The leaf scan row count is a pure function of the "+
				"fixture's seed and its recorded `-- sql --`, so a change means the fixture changed and "+
				"its row no longer describes it — regenerate with `just update-cardinality-baseline` and "+
				"review the diff. A row recorded against SQL the same invocation then rewrote is exactly "+
				"how this drifts silently.",
				id, base.ScanRows, cur.ScanRows)
		}
		if cur.HasArrayJoin != base.HasArrayJoin {
			t.Errorf("%s: has_array_join drifted %v → %v. arrayJoin is a bounded operator, so this is not "+
				"a fan-out verdict — it is a structural identity check that the row still describes the "+
				"emitted SQL. Regenerate with `just update-cardinality-baseline` so the operator set the "+
				"fixture actually emits lands in the PR diff.",
				id, base.HasArrayJoin, cur.HasArrayJoin)
		}
		if cur.HasCrossJoin && !base.HasCrossJoin {
			t.Errorf("%s: a CROSS JOIN appeared where the baseline had none — this is the classic "+
				"unbounded compute-fan-out shape; emit a bounded join/array form instead.", id)
		}
		if cur.HasRecursiveCTE && !base.HasRecursiveCTE {
			t.Errorf("%s: a WITH RECURSIVE appeared where the baseline had none — ensure it carries a "+
				"depth cap before introducing it.", id)
		}
		if cur.MaxRecursionDepth > base.MaxRecursionDepth {
			t.Errorf("%s: recursion depth grew %d → %d — a recursive walk got deeper; confirm the "+
				"depth cap still bounds it.", id, base.MaxRecursionDepth, cur.MaxRecursionDepth)
		}
	}
}

// writeBaseline serialises the current profile as a deterministically-ordered
// JSON array (sorted by fixture id) so the committed file diffs cleanly.
func writeBaseline(t *testing.T, entries map[string]baselineEntry) {
	t.Helper()
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]baselineEntry, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, entries[id])
	}
	buf, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(baselinePath, buf, 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

// loadBaseline reads the committed baseline into a fixture-keyed map.
func loadBaseline(t *testing.T) map[string]baselineEntry {
	t.Helper()
	buf, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline %s: %v — run `just update-cardinality-baseline` to create it", baselinePath, err)
	}
	var entries []baselineEntry
	if err := json.Unmarshal(buf, &entries); err != nil {
		t.Fatalf("parse baseline %s: %v", baselinePath, err)
	}
	m := make(map[string]baselineEntry, len(entries))
	for _, e := range entries {
		m[e.Fixture] = e
	}
	return m
}
