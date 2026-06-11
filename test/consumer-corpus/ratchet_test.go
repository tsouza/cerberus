package consumercorpus

import "testing"

// Corpus floors. These are RAISE-ONLY: when entries are added the
// floors move up to match; lowering one (or deleting entries without
// replacing them) is a corpus shrink, which this meta-test exists to
// forbid. The corpus is the project's record of what Grafana actually
// sends — shrinking it silently un-pins a consumer contract.
const (
	minTotalEntries = 18
	minTempoEntries = 9
	minLokiEntries  = 4
	minPromEntries  = 5
)

// TestConsumerCorpus_Ratchet pins the corpus census and the per-entry
// schema discipline:
//
//   - total + per-datasource entry counts may only grow,
//   - every entry names a decoder, stub fixture, and predicates the
//     harness actually implements (a typo'd name would otherwise rot
//     as a never-evaluated string),
//   - every entry carries provenance + the Grafana-side request
//     (enforced by Load via Entry.validate).
func TestConsumerCorpus_Ratchet(t *testing.T) {
	t.Parallel()

	entries, err := Load(".")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}

	byDS := map[string]int{}
	for _, e := range entries {
		byDS[e.Datasource]++

		if !KnownDecoder(e.Expect.Decode) {
			t.Errorf("%s/%s: decoder %q is not registered in the harness", e.Version, e.Name, e.Expect.Decode)
		}
		if !KnownStubFixture(e.Stub) {
			t.Errorf("%s/%s: stub fixture %q is not registered in the harness", e.Version, e.Name, e.Stub)
		}
		for _, p := range append(append([]string{}, e.Expect.Wire...), e.Expect.Data...) {
			name, _, err := splitPredicate(p)
			if err != nil {
				t.Errorf("%s/%s: %v", e.Version, e.Name, err)
				continue
			}
			if !KnownPredicate(name) {
				t.Errorf("%s/%s: predicate %q is not registered in the harness", e.Version, e.Name, name)
			}
		}
		if len(e.Expect.Wire)+len(e.Expect.Data) == 0 {
			t.Errorf("%s/%s: entry declares no predicates beyond decode — add at least one shape predicate", e.Version, e.Name)
		}
	}

	if got := len(entries); got < minTotalEntries {
		t.Errorf("corpus shrank: %d entries, floor is %d", got, minTotalEntries)
	}
	floors := map[string]int{"tempo": minTempoEntries, "loki": minLokiEntries, "prom": minPromEntries}
	for ds, floor := range floors {
		if byDS[ds] < floor {
			t.Errorf("corpus shrank for %s: %d entries, floor is %d", ds, byDS[ds], floor)
		}
	}
}
