package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestExpHistogramWindowStagesChainsEachStageOntoThePrevious pins
// expHistogramWindowStages' whole reason for existing: every stage must
// receive the PREVIOUS stage's output, never the original base.
//
// The bug it guards (cerberus issue #3178) is invisible with one stage —
// the first stage's input IS the base either way — so this asserts with
// two, where reading the base twice produces a plan that is missing the
// first stage entirely rather than one that merely nests differently.
func TestExpHistogramWindowStagesChainsEachStageOntoThePrevious(t *testing.T) {
	base := &chplan.Scan{Table: "base_table"}
	wrap := func(alias string) func(chplan.Node) chplan.Node {
		return func(n chplan.Node) chplan.Node {
			return &chplan.Project{
				Input:       n,
				Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: alias}, Alias: alias}},
			}
		}
	}

	got := expHistogramWindowStages(base, wrap("inner"), wrap("outer"))

	outer, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("outermost node = %T, want *chplan.Project", got)
	}
	if outer.Projections[0].Alias != "outer" {
		t.Fatalf("outermost stage = %q, want the LAST stage %q", outer.Projections[0].Alias, "outer")
	}
	inner, ok := outer.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("second node = %T, want *chplan.Project — the first stage was dropped, "+
			"which is what re-reading base instead of the running node does", outer.Input)
	}
	if inner.Projections[0].Alias != "inner" {
		t.Fatalf("second stage = %q, want the FIRST stage %q", inner.Projections[0].Alias, "inner")
	}
	if inner.Input != chplan.Node(base) {
		t.Fatalf("bottom node = %#v, want the base handed in", inner.Input)
	}
}

// TestExpHistogramWindowStagesWithNoStagesIsTheBase pins the degenerate
// case the reshape relies on for a non-counter window: no reset mask, so
// no stages, so the grouping itself is what the factor stage sees.
func TestExpHistogramWindowStagesWithNoStagesIsTheBase(t *testing.T) {
	base := &chplan.Scan{Table: "base_table"}
	if got := expHistogramWindowStages(base); got != chplan.Node(base) {
		t.Fatalf("expHistogramWindowStages(base) = %#v, want base unchanged", got)
	}
}
