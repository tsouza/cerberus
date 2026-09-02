package logql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// Both fold loops in drop_keep.go accumulate ACROSS entries: `| keep` ORs one
// value predicate per entry naming the key, and the projection predicate ORs
// one key predicate per entry. Each loop's `continue` therefore carries the
// whole accumulation — abandoning the loop instead of skipping one entry
// silently narrows the predicate to whatever had been folded so far, which
// drops labels the query asked to keep (or keeps labels it asked to drop).
//
// Both `continue`s survived mutation on `phase4-logql-other-b` (cerberus issue
// #2913): every fixture that reaches these loops carries a SINGLE entry, and a
// one-entry loop cannot tell "skip this entry" from "stop here". The cases
// below are the multi-entry shapes that can, and each asserts the exact folded
// expression rather than merely that something non-nil came back — a truncated
// fold is non-nil too.

// dropKeepValueColumn is the stand-in for the synthetic key's value
// expression, which the production callers supply as a closure.
const dropKeepValueColumn = "SyntheticValue"

// dropKeepNewValue is the newValue closure the folds call once per entry.
func dropKeepNewValue() chplan.Expr { return &chplan.ColumnRef{Name: dropKeepValueColumn} }

// dropKeepMatcherEntry builds a `| keep foo="bar"`-shaped entry.
func dropKeepMatcherEntry(t *testing.T, name, value string) syntax.NamedLabelMatcher {
	t.Helper()
	m, err := labels.NewMatcher(labels.MatchEqual, name, value)
	if err != nil {
		t.Fatalf("labels.NewMatcher(%q, %q): %v", name, value, err)
	}
	return syntax.NewNamedLabelMatcher(m, "")
}

// TestKeepPredicateForKey_FoldsEveryEntryNamingTheKey pins that an entry for a
// DIFFERENT label is skipped rather than ending the scan. `| keep other="x",
// app="a"` must still constrain `app`; stopping at the first non-matching
// entry would return no predicate at all, and the caller reads "no predicate"
// as "this stage does not condition the key" — so the label would survive the
// keep unconditionally.
func TestKeepPredicateForKey_FoldsEveryEntryNamingTheKey(t *testing.T) {
	t.Parallel()
	const key = "app"
	entries := []syntax.NamedLabelMatcher{
		dropKeepMatcherEntry(t, "other", "x"),
		dropKeepMatcherEntry(t, key, "a"),
	}

	pred, unconditional := keepPredicateForKey(entries, key, dropKeepNewValue)
	if unconditional {
		t.Fatalf("keepPredicateForKey(%q) reported unconditional; want a value predicate", key)
	}
	want := syntheticValueMatches(entries[1], dropKeepNewValue())
	if pred == nil || !pred.Equal(want) {
		t.Errorf("keepPredicateForKey(%q) = %#v; want %#v — the entry for %q must be skipped, not treated as the end of the list", key, pred, want, "other")
	}
}

// TestKeepPredicateForKey_OrsEveryMatcherOnTheSameKey is the same loop's
// accumulating half: two `| keep` entries on ONE key are alternatives, so the
// key survives if EITHER value matches.
func TestKeepPredicateForKey_OrsEveryMatcherOnTheSameKey(t *testing.T) {
	t.Parallel()
	const key = "app"
	entries := []syntax.NamedLabelMatcher{
		dropKeepMatcherEntry(t, key, "a"),
		dropKeepMatcherEntry(t, key, "b"),
	}

	pred, unconditional := keepPredicateForKey(entries, key, dropKeepNewValue)
	if unconditional {
		t.Fatalf("keepPredicateForKey(%q) reported unconditional; want a value predicate", key)
	}
	want := &chplan.Binary{
		Op:    chplan.OpOr,
		Left:  syntheticValueMatches(entries[0], dropKeepNewValue()),
		Right: syntheticValueMatches(entries[1], dropKeepNewValue()),
	}
	if pred == nil || !pred.Equal(want) {
		t.Errorf("keepPredicateForKey(%q) = %#v; want the OR of both entries (%#v)", key, pred, want)
	}
}

// TestAnyProjectionEntryMatches_OrsEveryEntry pins the projection fold. Three
// bare-name entries must produce the OR of all three key predicates: the
// expression selects the map entries the stage targets, so a fold that stops
// after the first one targets only the first label.
func TestAnyProjectionEntryMatches_OrsEveryEntry(t *testing.T) {
	t.Parallel()
	entries := []syntax.NamedLabelMatcher{
		syntax.NewNamedLabelMatcher(nil, "a"),
		syntax.NewNamedLabelMatcher(nil, "b"),
		syntax.NewNamedLabelMatcher(nil, "c"),
	}

	got := anyProjectionEntryMatches(entries)
	want := &chplan.Binary{
		Op: chplan.OpOr,
		Left: &chplan.Binary{
			Op:    chplan.OpOr,
			Left:  keyEquals("a"),
			Right: keyEquals("b"),
		},
		Right: keyEquals("c"),
	}
	if got == nil || !got.Equal(want) {
		t.Errorf("anyProjectionEntryMatches = %#v; want the left-folded OR of all three entries (%#v)", got, want)
	}
}
