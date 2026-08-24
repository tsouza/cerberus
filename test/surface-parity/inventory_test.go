package surfaceparity

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestInventoryIsRegenerable re-probes the three parser symbol tables,
// re-runs the cerberus + reference verdicts, and diffs the regenerated
// inventory byte-for-byte against the checked-in shard directory
// (test/surface-parity/inventory/, see inventory_shard.go). Set
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

	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		if err := WriteInventoryDir(inventoryDir, inv); err != nil {
			t.Fatalf("write %s: %v", inventoryDir, err)
		}
		t.Logf("rewrote %s (%d entries)", inventoryDir, len(inv.Entries))
		return
	}

	want, err := ShardInventory(inv)
	if err != nil {
		t.Fatalf("ShardInventory: %v", err)
	}
	diffs, err := DiffInventoryShards(inventoryDir, want)
	if err != nil {
		t.Fatalf("DiffInventoryShards: %v", err)
	}
	if len(diffs) > 0 {
		t.Fatalf("%s is stale relative to the parser symbol tables / cerberus lowering — "+
			"rerun with CERBERUS_UPDATE_INVENTORY=1, review the diff, and commit it.\n%s",
			inventoryDir, strings.Join(diffs, "\n"))
	}
}

// TestInventoryShardNamesRoundTrip proves inventoryShardName is injective
// and reversible over the live inventory's own (head, symbol) pairs —
// mirroring test/rejection-parity/catalogue_test.go's
// TestShardNamesRoundTrip for the sibling ledger.
func TestInventoryShardNamesRoundTrip(t *testing.T) {
	t.Parallel()

	inv, err := LoadInventoryDir(inventoryDir)
	if err != nil {
		t.Fatalf("LoadInventoryDir(%s): %v", inventoryDir, err)
	}
	owners := map[string]string{}
	for _, e := range inv.Entries {
		name, err := inventoryShardName(e.Head, e.Symbol)
		if err != nil {
			t.Fatalf("inventoryShardName(%q, %q): %v", e.Head, e.Symbol, err)
		}
		key := e.Head + ":" + e.Symbol
		if prev, ok := owners[name]; ok && prev != key {
			t.Fatalf("shard %s is claimed by both %s and %s — the mapping is not injective", name, prev, key)
		}
		owners[name] = key
		head, symbol, err := inventoryShardHeadSymbol(name)
		if err != nil {
			t.Fatalf("inventoryShardHeadSymbol(%q): %v", name, err)
		}
		if head != e.Head || symbol != e.Symbol {
			t.Fatalf("shard %s reverses to (%s, %s), want (%s, %s)", name, head, symbol, e.Head, e.Symbol)
		}
	}
	if len(owners) == 0 {
		t.Fatal("no shard names derived — the inventory lost its entries")
	}
}

// TestWriteInventoryDirPrunesEmptiedShards is the pruning pin: when a
// symbol drops out of the live inventory, its shard must be REMOVED, not
// left on disk. A lingering shard is a silent correctness hole —
// LoadInventoryDir merges whatever it finds, so the stale verdict would go
// on being asserted long after the symbol it names stopped existing.
// Mirrors catalogue_test.go's TestWriteCataloguePrunesEmptiedShards.
func TestWriteInventoryDirPrunesEmptiedShards(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	before := &Inventory{Entries: []Entry{
		{Head: "promql", Symbol: "fn:a", Kind: "function", Probe: "a()", Cerberus: VerdictAccept, Reference: VerdictAccept, Class: ClassParityAccept},
		{Head: "promql", Symbol: "fn:b", Kind: "function", Probe: "b()", Cerberus: VerdictAccept, Reference: VerdictAccept, Class: ClassParityAccept},
	}}
	if err := WriteInventoryDir(dir, before); err != nil {
		t.Fatalf("WriteInventoryDir(before): %v", err)
	}
	goneName, err := inventoryShardName("promql", "fn:b")
	if err != nil {
		t.Fatalf("inventoryShardName: %v", err)
	}
	gone := filepath.Join(dir, goneName)
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("shard for promql/fn:b was not written: %v", err)
	}

	after := &Inventory{Entries: before.Entries[:1]}
	if err := WriteInventoryDir(dir, after); err != nil {
		t.Fatalf("WriteInventoryDir(after): %v", err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want a not-exist error — a shard for a symbol dropped from the "+
			"inventory must be pruned, or LoadInventoryDir keeps merging a verdict nobody regenerates "+
			"any more", gone, err)
	}

	reloaded, err := LoadInventoryDir(dir)
	if err != nil {
		t.Fatalf("LoadInventoryDir: %v", err)
	}
	if len(reloaded.Entries) != len(after.Entries) {
		t.Fatalf("reloaded %d entries, want %d — the pruned shard is still contributing", len(reloaded.Entries), len(after.Entries))
	}

	// The same emptied-shard state must be a FAILURE for the
	// regenerate-and-diff test, not merely repaired by regeneration.
	if err := os.WriteFile(gone, []byte(`{"entries":[]}`+"\n"), inventoryShardFileMode); err != nil {
		t.Fatalf("re-plant stale shard: %v", err)
	}
	want, err := ShardInventory(after)
	if err != nil {
		t.Fatalf("ShardInventory: %v", err)
	}
	diffs, err := DiffInventoryShards(dir, want)
	if err != nil {
		t.Fatalf("DiffInventoryShards: %v", err)
	}
	if len(diffs) != 1 || !strings.Contains(diffs[0], "stale shard") {
		t.Fatalf("DiffInventoryShards reported %v, want exactly one stale-shard difference", diffs)
	}
}

// TestLoadInventoryDirMergesShardsInOrder pins the invariant every
// downstream consumer rests on: the merged value is sorted (head, then
// symbol) regardless of which shard each entry came from.
func TestLoadInventoryDirMergesShardsInOrder(t *testing.T) {
	t.Parallel()

	inv, err := LoadInventoryDir(inventoryDir)
	if err != nil {
		t.Fatalf("LoadInventoryDir(%s): %v", inventoryDir, err)
	}
	for i := 1; i < len(inv.Entries); i++ {
		a, b := inv.Entries[i-1], inv.Entries[i]
		if headOrder(a.Head) > headOrder(b.Head) || (a.Head == b.Head && a.Symbol >= b.Symbol) {
			t.Fatalf("entries out of order at index %d: %s/%s then %s/%s", i, a.Head, a.Symbol, b.Head, b.Symbol)
		}
	}
	shards, err := listInventoryShards(inventoryDir)
	if err != nil {
		t.Fatalf("listInventoryShards: %v", err)
	}
	if len(shards) < 2 {
		t.Fatalf("found %d shard(s) in %s — the merge path is not being exercised", len(shards), inventoryDir)
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

	pinned, err := LoadInventoryDir(inventoryDir)
	if err != nil {
		t.Fatalf("load %s (rerun TestInventoryIsRegenerable with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", inventoryDir, err)
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

	inv, err := LoadInventoryDir(inventoryDir)
	if err != nil {
		t.Fatalf("load %s: %v", inventoryDir, err)
	}
	if len(inv.Entries) == 0 {
		t.Fatalf("%s is empty", inventoryDir)
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
