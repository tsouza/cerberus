package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestLowerSharedHistogramQuantileKernels_ReportsNestedReplacement(t *testing.T) {
	t.Parallel()
	input := &chplan.Project{Input: &chplan.HistogramQuantile{Input: &chplan.Scan{}}}

	got, replacements := lowerSharedHistogramQuantileKernels(
		input,
		"quantile",
		[]chplan.HistogramQuantileLevel{{Phi: 0.5, Label: "0.5"}},
	)
	if replacements != 1 {
		t.Fatalf("replacements = %d, want 1", replacements)
	}
	project, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("rewritten node = %T, want *chplan.Project", got)
	}
	if _, ok := project.Input.(*chplan.HistogramQuantiles); !ok {
		t.Fatalf("rewritten child = %T, want *chplan.HistogramQuantiles", project.Input)
	}
}

func TestLowerSharedHistogramQuantileKernels_AccumulatesSiblingReplacements(t *testing.T) {
	t.Parallel()
	input := &chplan.UnionAll{Inputs: []chplan.Node{
		&chplan.HistogramQuantile{Input: &chplan.Scan{}},
		&chplan.HistogramQuantileNative{Input: &chplan.Scan{}},
	}}

	got, replacements := lowerSharedHistogramQuantileKernels(
		input,
		"quantile",
		[]chplan.HistogramQuantileLevel{{Phi: 0.5, Label: "0.5"}},
	)
	if replacements != len(input.Inputs) {
		t.Fatalf("replacements = %d, want %d", replacements, len(input.Inputs))
	}
	union, ok := got.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("rewritten node = %T, want *chplan.UnionAll", got)
	}
	if _, ok := union.Inputs[0].(*chplan.HistogramQuantiles); !ok {
		t.Fatalf("rewritten classic child = %T, want *chplan.HistogramQuantiles", union.Inputs[0])
	}
	if _, ok := union.Inputs[1].(*chplan.HistogramQuantilesNative); !ok {
		t.Fatalf("rewritten native child = %T, want *chplan.HistogramQuantilesNative", union.Inputs[1])
	}
}

func TestLowerSharedHistogramQuantileKernels_PreservesUnchangedTree(t *testing.T) {
	t.Parallel()
	input := &chplan.Project{Input: &chplan.Scan{}}

	got, replacements := lowerSharedHistogramQuantileKernels(
		input,
		"quantile",
		[]chplan.HistogramQuantileLevel{{Phi: 0.5, Label: "0.5"}},
	)
	if replacements != 0 {
		t.Fatalf("replacements = %d, want 0", replacements)
	}
	if got != input {
		t.Fatalf("unchanged tree was cloned: got %p, want %p", got, input)
	}
}
