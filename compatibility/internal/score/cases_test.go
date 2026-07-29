package score

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The parity ratchet gates on the roster this file emits, so its
// invariants are load-bearing: an unsorted or short roster fails the
// gate on the next run, and a silently-dropped duplicate would shrink
// what is gated without anyone noticing.

func TestNormaliseCasesSortsByID(t *testing.T) {
	out, err := normaliseCases([]Case{
		{ID: "zeta", Passed: true},
		{ID: "alpha", Passed: false},
		{ID: "mu", Passed: true},
	})
	if err != nil {
		t.Fatalf("normaliseCases: %v", err)
	}
	want := []Case{
		{ID: "alpha", Passed: false},
		{ID: "mu", Passed: true},
		{ID: "zeta", Passed: true},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d cases, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("case %d = %+v, want %+v", i, out[i], want[i])
		}
	}
}

func TestNormaliseCasesKeepsRepeatedIDsDistinct(t *testing.T) {
	// A corpus may legitimately carry the same query twice. Both must
	// survive into the roster: collapsing them would drop a case the
	// ratchet is supposed to gate on, and the counts would then
	// disagree with compat-score.json.
	out, err := normaliseCases([]Case{
		{ID: "dup", Passed: true},
		{ID: "dup", Passed: false},
		{ID: "dup", Passed: true},
	})
	if err != nil {
		t.Fatalf("normaliseCases: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d cases, want 3 — a repeated ID must not collapse", len(out))
	}
	ids := map[string]bool{}
	for _, c := range out {
		if ids[c.ID] {
			t.Fatalf("duplicate ID %q survived normalisation", c.ID)
		}
		ids[c.ID] = true
	}
	for _, want := range []string{"dup", "dup #2", "dup #3"} {
		if !ids[want] {
			t.Errorf("missing expected ID %q; got %v", want, ids)
		}
	}
}

func TestNormaliseCasesSuffixFollowsCorpusOrderNotSortOrder(t *testing.T) {
	// The suffix is assigned as the corpus is walked, so inserting an
	// unrelated case ahead of a repeated one must not renumber it —
	// otherwise every baseline diff would churn.
	first, err := normaliseCases([]Case{
		{ID: "q", Passed: true},
		{ID: "q", Passed: true},
	})
	if err != nil {
		t.Fatalf("normaliseCases: %v", err)
	}
	second, err := normaliseCases([]Case{
		{ID: "a-new-case", Passed: true},
		{ID: "q", Passed: true},
		{ID: "q", Passed: true},
	})
	if err != nil {
		t.Fatalf("normaliseCases: %v", err)
	}
	if first[0].ID != "q" || first[1].ID != "q #2" {
		t.Fatalf("got %q,%q; want q,q #2", first[0].ID, first[1].ID)
	}
	if second[1].ID != "q" || second[2].ID != "q #2" {
		t.Errorf("inserting a case renumbered an unrelated one: got %q,%q", second[1].ID, second[2].ID)
	}
}

func TestNormaliseCasesRejectsEmptyID(t *testing.T) {
	// An empty ID cannot identify a case across runs, so the ratchet
	// could never match it. Failing here beats emitting a roster entry
	// that is guaranteed to look VANISHED next run.
	if _, err := normaliseCases([]Case{{ID: "  ", Passed: true}}); err == nil {
		t.Fatal("expected an error for a blank case ID")
	}
}

func TestNormaliseCasesRejectsCollisionWithAnOccurrenceSuffix(t *testing.T) {
	// A corpus case whose own ID already ends in another case's
	// occurrence suffix would collide after disambiguation.
	if _, err := normaliseCases([]Case{
		{ID: "q", Passed: true},
		{ID: "q", Passed: true},
		{ID: "q #2", Passed: true},
	}); err == nil {
		t.Fatal("expected an error for an ID colliding with a generated occurrence suffix")
	}
}

func TestWriteCasesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "compat-cases.json")
	if err := WriteCases(path, "prometheus", []Case{
		{ID: "beta", Passed: false},
		{ID: "alpha", Passed: true},
	}); err != nil {
		t.Fatalf("WriteCases: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got CaseSet
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Head != "prometheus" {
		t.Errorf("head = %q, want prometheus", got.Head)
	}
	if len(got.Cases) != 2 || got.Cases[0].ID != "alpha" || got.Cases[1].ID != "beta" {
		t.Fatalf("cases = %+v, want alpha then beta", got.Cases)
	}
	if !got.Cases[0].Passed || got.Cases[1].Passed {
		t.Errorf("pass flags did not survive the round trip: %+v", got.Cases)
	}
}

func TestWriteCasesRequiresHead(t *testing.T) {
	// The ratchet rejects a cases file whose head does not match the job
	// it is gating; an empty head would make that check vacuous.
	path := filepath.Join(t.TempDir(), "compat-cases.json")
	if err := WriteCases(path, "", []Case{{ID: "a", Passed: true}}); err == nil {
		t.Fatal("expected an error when head is empty")
	}
}
