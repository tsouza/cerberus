package rejectionparity

import (
	"os"
	"testing"
	"time"
)

// TestDivergenceCeilingRatchet is the monotonic-count leg: the catalogue's
// class=divergence entries may never exceed the committed ceiling in
// divergence-ceiling.json without a hand-edited bump to that file in the
// same diff. CERBERUS_UPDATE_INVENTORY=1 (the same env-var convention
// TestCatalogueIsRegenerable uses, already wired into `just
// update-parity-ledgers` / `just update-golden parity`) tightens the ceiling down
// to match a decrease for free; it refuses to raise the ceiling to match an
// increase, because that must be a reviewable line a human wrote, never a
// tool's silent output.
func TestDivergenceCeilingRatchet(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	count := CountDivergenceEntries(cat)
	ceiling, err := LoadDivergenceCeiling(divergenceCeilingPath)
	if err != nil {
		t.Fatalf("load %s: %v", divergenceCeilingPath, err)
	}

	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		next, err := NextDivergenceCeiling(count, ceiling.MaxEntries)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if next == ceiling.MaxEntries {
			t.Logf("%s already matches (%d)", divergenceCeilingPath, next)
			return
		}
		want, err := MarshalDivergenceCeiling(&DivergenceCeiling{MaxEntries: next})
		if err != nil {
			t.Fatalf("MarshalDivergenceCeiling: %v", err)
		}
		if err := os.WriteFile(divergenceCeilingPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", divergenceCeilingPath, err)
		}
		t.Logf("tightened %s: %d -> %d", divergenceCeilingPath, ceiling.MaxEntries, next)
		return
	}

	if err := CheckDivergenceCeiling(count, ceiling.MaxEntries); err != nil {
		t.Fatal(err)
	}
}

// TestDivergenceCeilingRatchetCatchesViolatingInput proves the ceiling gate
// is not hollow: it must reject a count over the ceiling and refuse to raise
// the ceiling automatically, while still passing the happy paths (at the
// ceiling, and with slack below it) that CI runs every day.
func TestDivergenceCeilingRatchetCatchesViolatingInput(t *testing.T) {
	t.Parallel()

	if err := CheckDivergenceCeiling(4, 3); err == nil {
		t.Fatal("CheckDivergenceCeiling(4, 3): expected an error, count exceeds the ceiling")
	}
	if err := CheckDivergenceCeiling(3, 3); err != nil {
		t.Fatalf("CheckDivergenceCeiling(3, 3): expected the at-ceiling happy path to pass, got %v", err)
	}
	if err := CheckDivergenceCeiling(2, 3); err != nil {
		t.Fatalf("CheckDivergenceCeiling(2, 3): expected slack below the ceiling to pass, got %v", err)
	}

	if _, err := NextDivergenceCeiling(4, 3); err == nil {
		t.Fatal("NextDivergenceCeiling(4, 3): expected an error, regeneration must not raise the ceiling automatically")
	}
	if next, err := NextDivergenceCeiling(2, 3); err != nil || next != 2 {
		t.Fatalf("NextDivergenceCeiling(2, 3): expected (2, nil) — a decrease tightens for free, got (%d, %v)", next, err)
	}
	if next, err := NextDivergenceCeiling(3, 3); err != nil || next != 3 {
		t.Fatalf("NextDivergenceCeiling(3, 3): expected (3, nil) — no movement when already exact, got (%d, %v)", next, err)
	}
}

// TestDivergenceEntriesRespectAgeCap is the age-cap leg: no class=divergence
// entry in the checked-in catalogue may silently outlive divergenceStaleAfter.
// This test is wall-clock-driven by design — see divergence_ratchet.go's
// CheckDivergenceAge doc comment and doc.go: an entry aging past the cap
// (including one that is real, correct, and expensive to close, like
// #1768's label_replace duplicate-capture-group divergence) is meant to
// force a conscious re-decision, not to be treated as flake.
func TestDivergenceEntriesRespectAgeCap(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	now := time.Now()
	for _, e := range cat.Entries {
		if e.Class != ClassDivergence {
			continue
		}
		if err := CheckDivergenceAge(e, now); err != nil {
			t.Error(err)
		}
	}
}

// TestDivergenceAgeCapCatchesViolatingInput proves the age gate is not
// hollow: it must reject an entry past the threshold and an unparseable
// Since date, while passing a fresh entry and one sitting exactly at the
// boundary.
func TestDivergenceAgeCapCatchesViolatingInput(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)

	stale := Entry{
		Site:          Site{Site: "synthetic/stale"},
		Class:         ClassDivergence,
		Since:         now.Add(-divergenceStaleAfter - 24*time.Hour).Format(divergenceDateLayout),
		TrackingIssue: 1,
	}
	if err := CheckDivergenceAge(stale, now); err == nil {
		t.Fatal("CheckDivergenceAge(stale): expected an error, entry is past the age cap")
	}

	fresh := Entry{
		Site:          Site{Site: "synthetic/fresh"},
		Class:         ClassDivergence,
		Since:         now.Add(-24 * time.Hour).Format(divergenceDateLayout),
		TrackingIssue: 1,
	}
	if err := CheckDivergenceAge(fresh, now); err != nil {
		t.Fatalf("CheckDivergenceAge(fresh): expected the happy path to pass, got %v", err)
	}

	boundary := Entry{
		Site:          Site{Site: "synthetic/boundary"},
		Class:         ClassDivergence,
		Since:         now.Add(-divergenceStaleAfter).Format(divergenceDateLayout),
		TrackingIssue: 1,
	}
	if err := CheckDivergenceAge(boundary, now); err != nil {
		t.Fatalf("CheckDivergenceAge(boundary): expected an entry exactly at the cap to pass, got %v", err)
	}

	unparseable := Entry{
		Site:          Site{Site: "synthetic/unparseable"},
		Class:         ClassDivergence,
		Since:         "not-a-date",
		TrackingIssue: 1,
	}
	if err := CheckDivergenceAge(unparseable, now); err == nil {
		t.Fatal("CheckDivergenceAge(unparseable): expected an error, since date does not parse")
	}
}
