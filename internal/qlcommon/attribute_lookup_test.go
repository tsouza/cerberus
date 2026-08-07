package qlcommon

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestOTelDottedFallbackChainSingleCandidate covers the base case: a
// single candidate has no fallback to chain, so the result is a bare
// MapAccess against that candidate rather than an if/mapContains wrapper.
func TestOTelDottedFallbackChainSingleCandidate(t *testing.T) {
	m := &chplan.ColumnRef{Name: "Attributes"}
	got := OTelDottedFallbackChain(m, []string{"http.method"})

	want := &chplan.MapAccess{
		Map: m,
		Key: &chplan.LitString{V: "http.method"},
	}
	if !got.Equal(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestOTelDottedFallbackChainMultipleCandidates covers the chained case:
// each non-terminal candidate is guarded by mapContains, and the chain is
// right-associative so the leftmost (first) candidate is checked first
// and wins when present, falling through to later candidates and finally
// a bare MapAccess on the last one.
func TestOTelDottedFallbackChainMultipleCandidates(t *testing.T) {
	m := &chplan.ColumnRef{Name: "Attributes"}
	got := OTelDottedFallbackChain(m, []string{"http_method", "http.method"})

	want := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapContains",
				Args: []chplan.Expr{m, &chplan.LitString{V: "http_method"}},
			},
			&chplan.MapAccess{Map: m, Key: &chplan.LitString{V: "http_method"}},
			&chplan.MapAccess{Map: m, Key: &chplan.LitString{V: "http.method"}},
		},
	}
	if !got.Equal(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestOTelDottedFallbackChainThreeCandidates locks down the general
// N>2 case, nesting a second if/mapContains level around the two-
// candidate shape TestOTelDottedFallbackChainMultipleCandidates already
// pins, so the recursive structure (not just its first iteration) is
// exercised.
func TestOTelDottedFallbackChainThreeCandidates(t *testing.T) {
	m := &chplan.ColumnRef{Name: "Attributes"}
	got := OTelDottedFallbackChain(m, []string{"k8s_pod_name", "k8s.pod.name", "k8s_pod_name_dotted"})

	inner := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapContains",
				Args: []chplan.Expr{m, &chplan.LitString{V: "k8s.pod.name"}},
			},
			&chplan.MapAccess{Map: m, Key: &chplan.LitString{V: "k8s.pod.name"}},
			&chplan.MapAccess{Map: m, Key: &chplan.LitString{V: "k8s_pod_name_dotted"}},
		},
	}
	want := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapContains",
				Args: []chplan.Expr{m, &chplan.LitString{V: "k8s_pod_name"}},
			},
			&chplan.MapAccess{Map: m, Key: &chplan.LitString{V: "k8s_pod_name"}},
			inner,
		},
	}
	if !got.Equal(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
