package rejectionparity

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// This file is the two mechanisms that keep class=divergence from decaying
// into the allow-list this package forbids (see doc.go): a monotonic count
// ceiling and a per-entry age cap. Both are enforced by
// divergence_ratchet_test.go; this file holds the pure, independently
// testable logic.

// divergenceDateLayout is the on-disk format of Entry.Since — a bare date,
// because a divergence's age is measured in days, not seconds.
const divergenceDateLayout = "2006-01-02"

// divergenceCeilingPath is the checked-in ratchet artifact path (relative to
// this package's directory, mirroring cataloguePath).
const divergenceCeilingPath = "divergence-ceiling.json"

// divergenceStaleAfter bounds how long an entry may carry class=divergence
// before TestDivergenceEntriesRespectAgeCap forces a conscious re-decision.
// 90 days is long enough to cover a normal fix-it-properly cycle (file,
// design, implement, land — the kind of change #1768's label_replace
// duplicate-capture-group divergence needs, since closing it means moving
// regex expansion from ClickHouse's extractGroups to Go) but short enough
// that an entry cannot quietly become permanent: at the 90-day mark a human
// has to look at it again and choose fix, won't-fix, or a deliberate,
// acknowledged ceiling bump — never a silent extension.
const divergenceStaleAfter = 90 * 24 * time.Hour

// DivergenceCeiling is the checked-in ratchet artifact
// (test/rejection-parity/divergence-ceiling.json): the maximum number of
// class=divergence entries the catalogue may carry.
type DivergenceCeiling struct {
	MaxEntries int `json:"max_entries"`
}

// LoadDivergenceCeiling reads + parses the checked-in ceiling file.
func LoadDivergenceCeiling(path string) (*DivergenceCeiling, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
	if err != nil {
		return nil, err
	}
	var c DivergenceCeiling
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// MarshalDivergenceCeiling renders the canonical on-disk JSON form (2-space
// indent + trailing newline), mirroring ShardCatalogue's per-shard
// rendering.
func MarshalDivergenceCeiling(c *DivergenceCeiling) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CountDivergenceEntries counts the class=divergence entries in cat.
func CountDivergenceEntries(cat *Catalogue) int {
	n := 0
	for _, e := range cat.Entries {
		if e.Class == ClassDivergence {
			n++
		}
	}
	return n
}

// CheckDivergenceCeiling reports an error when count exceeds ceiling. This
// is the enforcement half of the ratchet: it fires whether the excess came
// from a new divergence entry landing without a hand-edited ceiling bump, or
// (defensively) from a ceiling file update trying to raise the number on its
// own — NextDivergenceCeiling never returns a value this check would reject.
func CheckDivergenceCeiling(count, ceiling int) error {
	if count > ceiling {
		return fmt.Errorf(
			"catalogue carries %d class=divergence entries, exceeding the committed ceiling of %d in %s — "+
				"the count may never increase silently: bump max_entries by hand to %d (or higher) as a "+
				"deliberate, reviewable line in this PR's diff, acknowledging the new divergence rather than "+
				"letting the ratchet absorb it",
			count, ceiling, divergenceCeilingPath, count,
		)
	}
	return nil
}

// NextDivergenceCeiling computes the ratcheted ceiling for the
// CERBERUS_UPDATE_INVENTORY regen path: it tightens the ceiling down to
// match count for free, but REFUSES to raise it — count > existing is
// returned as an error rather than a larger number, because an increase
// must be a hand-edited line a reviewer sees, never a tool's silent output
// (the same asymmetry coverage-summary.mjs's nextFloors enforces in the
// opposite direction for a floor).
func NextDivergenceCeiling(count, existing int) (int, error) {
	if count > existing {
		return 0, fmt.Errorf(
			"catalogue carries %d class=divergence entries, exceeding the committed ceiling of %d in %s — "+
				"regeneration cannot fix that by raising the ceiling: edit max_entries to %d by hand so the "+
				"increase is a reviewable line in the diff",
			count, existing, divergenceCeilingPath, count,
		)
	}
	return count, nil
}

// CheckDivergenceAge reports an error when e (class=divergence) has sat open
// longer than divergenceStaleAfter as of now, or when its Since date is
// unparseable. The message spells out the three legitimate remedies — fix
// it, close it won't-fix with recorded reasoning, or a deliberate,
// acknowledged ceiling bump — because the wrong remedy (quietly extending
// Since) is exactly the allow-list decay doc.go warns against. A divergence
// can be real, correct, and expensive to close (see #1768: ClickHouse's
// extractGroups cannot distinguish "group did not participate" from
// "participated but matched empty", the exact semantic Go's ExpandString
// relies on — closing it means moving the computation to Go); the cap does
// not dispute that, it only forces the decision to stay conscious.
func CheckDivergenceAge(e Entry, now time.Time) error {
	since, err := time.Parse(divergenceDateLayout, e.Since)
	if err != nil {
		return fmt.Errorf("divergence entry %s has an unparseable since date %q (want %s): %w",
			e.Site.Site, e.Since, divergenceDateLayout, err)
	}
	age := now.Sub(since)
	if age > divergenceStaleAfter {
		return fmt.Errorf(
			"divergence entry %s (tracking #%d) has sat open %s since %s, past the %s age cap — a divergence "+
				"does not get to decay into a permanent allow-list entry silently. Pick one, in this PR: fix "+
				"the underlying rejection so the entry can be deleted; close #%d as won't-fix with the reasoning "+
				"recorded there and delete the entry; or make a deliberate, acknowledged bump (a hand-edited "+
				"Since date with fresh justification, or a ceiling increase) so the extension is a reviewable "+
				"line in the diff. Never quietly push the deadline out",
			e.Site.Site, e.TrackingIssue, age.Round(time.Hour), e.Since, divergenceStaleAfter, e.TrackingIssue,
		)
	}
	return nil
}
