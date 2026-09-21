package chplan

import "testing"

func TestRewriteChildrenHistogramQuantiles_NilHistogramInputsAreLeaves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		node Node
	}{
		{"classic-nil-histogram", &HistogramQuantiles{}},
		{"classic-nil-input", &HistogramQuantiles{Histogram: &HistogramQuantile{}}},
		{"native-nil-histogram", &HistogramQuantilesNative{}},
		{"native-nil-input", &HistogramQuantilesNative{Histogram: &HistogramQuantileNative{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, changed := RewriteChildren(tc.node, func(Node) (Node, bool) {
				calls++
				return &Scan{}, true
			})
			if calls != 0 {
				t.Fatalf("rewrite callback calls = %d, want 0", calls)
			}
			if changed {
				t.Fatal("nil histogram input reported a child rewrite")
			}
			if got != tc.node {
				t.Fatalf("nil histogram input cloned: got %p, want %p", got, tc.node)
			}
		})
	}
}

func TestCloneHistogramQuantiles_PreservesNilShapeWithoutAliasing(t *testing.T) {
	t.Parallel()

	classicNil := &HistogramQuantiles{}
	classicNilClone := CloneNode(classicNil).(*HistogramQuantiles)
	if classicNilClone == classicNil || classicNilClone.Histogram != nil {
		t.Fatalf("classic nil clone = %#v, want distinct node with nil histogram", classicNilClone)
	}

	classicEmpty := &HistogramQuantiles{Histogram: &HistogramQuantile{}}
	classicEmptyClone := CloneNode(classicEmpty).(*HistogramQuantiles)
	if classicEmptyClone.Histogram == classicEmpty.Histogram || classicEmptyClone.Histogram.Input != nil {
		t.Fatalf("classic empty clone = %#v, want distinct empty histogram", classicEmptyClone)
	}

	nativeNil := &HistogramQuantilesNative{}
	nativeNilClone := CloneNode(nativeNil).(*HistogramQuantilesNative)
	if nativeNilClone == nativeNil || nativeNilClone.Histogram != nil {
		t.Fatalf("native nil clone = %#v, want distinct node with nil histogram", nativeNilClone)
	}

	nativeEmpty := &HistogramQuantilesNative{Histogram: &HistogramQuantileNative{}}
	nativeEmptyClone := CloneNode(nativeEmpty).(*HistogramQuantilesNative)
	if nativeEmptyClone.Histogram == nativeEmpty.Histogram || nativeEmptyClone.Histogram.Input != nil {
		t.Fatalf("native empty clone = %#v, want distinct empty histogram", nativeEmptyClone)
	}
}
