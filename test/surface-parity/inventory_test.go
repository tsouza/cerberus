package surfaceparity

import (
	"os"
	"sort"
	"strings"
	"testing"
)

const inventoryPath = "inventory.json"

// TestInventoryIsRegenerable re-probes the three parser symbol tables,
// re-runs the cerberus + reference verdicts, and diffs the regenerated
// inventory byte-for-byte against the checked-in JSON. Set
// CERBERUS_UPDATE_INVENTORY=1 to rewrite the artifact (the same
// update-via-env convention as test/rejection-parity + test/inventory).
// Because every field is mechanically derived, any drift — a parser
// symbol added/removed upstream, a cerberus lowering that started or
// stopped accepting a symbol — moves the artifact, and that move is a
// reviewable diff.
func TestInventoryIsRegenerable(t *testing.T) {
	t.Parallel()

	inv, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := MarshalInventory(inv)
	if err != nil {
		t.Fatalf("MarshalInventory: %v", err)
	}

	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		if err := os.WriteFile(inventoryPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", inventoryPath, err)
		}
		t.Logf("rewrote %s (%d entries)", inventoryPath, len(inv.Entries))
		return
	}

	got, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatalf("read %s (rerun with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", inventoryPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale relative to the parser symbol tables / cerberus lowering — "+
			"rerun with CERBERUS_UPDATE_INVENTORY=1, review the diff, and commit it.\n"+
			"--- want %d bytes, got %d bytes", inventoryPath, len(want), len(got))
	}
}

// TestWrongRejectionsAreRatcheted pins the current wrong-reject set per
// head. The checked-in inventory IS the pin: TestInventoryIsRegenerable
// already fails on any drift, so this test asserts the higher-level
// invariant the ratchet protects — the wrong-reject set may only
// SHRINK relative to the committed ledger, never grow. A regression
// (a symbol that flips from accept to wrong-reject) and a fix (a symbol
// that leaves the wrong-reject set) both surface here against the
// checked-in inventory, forcing a deliberate regeneration + review.
func TestWrongRejectionsAreRatcheted(t *testing.T) {
	t.Parallel()
	assertRatchet(t, ClassWrongReject)
}

// TestWrongAcceptsAreRatcheted does the same for wrong-accepts — symbols
// cerberus accepts that the reference rejects, a correctness risk.
func TestWrongAcceptsAreRatcheted(t *testing.T) {
	t.Parallel()
	assertRatchet(t, ClassWrongAccept)
}

// assertRatchet re-probes live and compares the live set of `class`
// symbols against the committed inventory's set, per head. New symbols
// in the live set that aren't pinned fail (a regression or a new
// upstream grammar symbol cerberus doesn't handle); pinned symbols
// missing from the live set fail too (a fix that wasn't regenerated).
// Either direction demands CERBERUS_UPDATE_INVENTORY=1 + review.
func assertRatchet(t *testing.T, class Classification) {
	t.Helper()

	pinned, err := LoadInventory(inventoryPath)
	if err != nil {
		t.Fatalf("load %s (rerun TestInventoryIsRegenerable with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", inventoryPath, err)
	}
	live, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, head := range []string{"promql", "logql", "traceql"} {
		pinnedSet := classSet(pinned, head, class)
		liveSet := classSet(live, head, class)

		var appeared, healed []string
		for sym := range liveSet {
			if !pinnedSet[sym] {
				appeared = append(appeared, sym)
			}
		}
		for sym := range pinnedSet {
			if !liveSet[sym] {
				healed = append(healed, sym)
			}
		}
		sort.Strings(appeared)
		sort.Strings(healed)

		if len(appeared) > 0 {
			t.Errorf("%s %s: NEW symbols not in the committed inventory: %s\n"+
				"a symbol regressed into %s — fix the cerberus lowering, or (if intended) "+
				"rerun with CERBERUS_UPDATE_INVENTORY=1 and justify the diff in review",
				head, class, strings.Join(appeared, ", "), class)
		}
		if len(healed) > 0 {
			t.Errorf("%s %s: committed symbols no longer present live: %s\n"+
				"a burndown fixed these — rerun with CERBERUS_UPDATE_INVENTORY=1 to re-pin the ledger",
				head, class, strings.Join(healed, ", "))
		}
	}
}

func classSet(inv *Inventory, head string, class Classification) map[string]bool {
	out := map[string]bool{}
	for _, e := range inv.Entries {
		if e.Head == head && e.Class == class && !e.ReferenceUnresolved {
			out[e.Symbol] = true
		}
	}
	return out
}

// TestInventoryShapeInvariants pins structural invariants that hold
// regardless of which symbols are wrong-rejected: every entry carries a
// non-empty probe + a valid head/verdict/class, no duplicate (head,
// symbol) keys, and the class is consistent with the two verdicts.
func TestInventoryShapeInvariants(t *testing.T) {
	t.Parallel()

	inv, err := LoadInventory(inventoryPath)
	if err != nil {
		t.Fatalf("load %s: %v", inventoryPath, err)
	}
	if len(inv.Entries) == 0 {
		t.Fatalf("%s is empty", inventoryPath)
	}
	seen := map[string]bool{}
	for _, e := range inv.Entries {
		key := e.Head + "/" + e.Symbol
		if seen[key] {
			t.Errorf("duplicate entry %s", key)
		}
		seen[key] = true

		switch e.Head {
		case "promql", "logql", "traceql":
		default:
			t.Errorf("entry %s: unknown head %q", key, e.Head)
		}
		if strings.TrimSpace(e.Probe) == "" {
			t.Errorf("entry %s: empty probe", key)
		}
		if e.Cerberus != VerdictAccept && e.Cerberus != VerdictReject {
			t.Errorf("entry %s: invalid cerberus verdict %q", key, e.Cerberus)
		}
		if e.Reference != VerdictAccept && e.Reference != VerdictReject {
			t.Errorf("entry %s: invalid reference verdict %q", key, e.Reference)
		}
		if want := classify(e.Cerberus, e.Reference); e.Class != want {
			t.Errorf("entry %s: class %q inconsistent with verdicts (cerberus=%s reference=%s → %s)",
				key, e.Class, e.Cerberus, e.Reference, want)
		}
		// A wrong-reject entry must carry the cerberus error it
		// rejected with — that's the concrete failure a burndown fixes.
		if e.Class == ClassWrongReject && strings.TrimSpace(e.CerberusError) == "" {
			t.Errorf("entry %s: wrong-reject carries no cerberus_error", key)
		}
	}
}
