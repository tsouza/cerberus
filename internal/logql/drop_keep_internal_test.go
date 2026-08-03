package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestNarrowIdentityByProjection_EmptyKeepIsIdentity pins upstream's
// rule that a zero-entry `| keep` keeps EVERYTHING rather than nothing.
// The grammar has no source spelling for it, so the stage is built
// directly — the contract still has to hold if one ever reaches the
// lowering, and the inverted reading (filter everything away) would
// silently empty every series.
func TestNarrowIdentityByProjection_EmptyKeepIsIdentity(t *testing.T) {
	t.Parallel()

	base := &chplan.ColumnRef{Name: "ResourceAttributes"}
	got := narrowIdentityByProjection(base, &syntax.PipelineExpr{
		MultiStages: []syntax.StageExpr{&syntax.KeepLabelsExpr{}},
	})
	if got != chplan.Expr(base) {
		t.Errorf("empty keep list rewrote the identity to %#v; want it untouched", got)
	}
}

// TestNarrowIdentityByProjection_NoPipelineIsIdentity pins that a bare
// selector (no pipeline at all) is left alone, which is what keeps every
// pre-existing golden byte-identical.
func TestNarrowIdentityByProjection_NoPipelineIsIdentity(t *testing.T) {
	t.Parallel()

	base := &chplan.ColumnRef{Name: "ResourceAttributes"}
	got := narrowIdentityByProjection(base, &syntax.MatchersExpr{})
	if got != chplan.Expr(base) {
		t.Errorf("bare selector rewrote the identity to %#v; want it untouched", got)
	}
}

// TestNarrowIdentityByProjection_StagesComposeInOrder pins that a
// pipeline carrying two projection stages wraps twice — a single-wrap
// implementation would silently honour only the last stage.
func TestNarrowIdentityByProjection_StagesComposeInOrder(t *testing.T) {
	t.Parallel()

	drop, err := syntax.ParseExpr(`{job="api"} | drop env | drop pod`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	sel, ok := drop.(syntax.LogSelectorExpr)
	if !ok {
		t.Fatalf("parsed %T is not a LogSelectorExpr", drop)
	}

	got := narrowIdentityByProjection(&chplan.ColumnRef{Name: "ResourceAttributes"}, sel)
	outer, ok := got.(*chplan.FuncCall)
	if !ok || outer.Name != "mapFilter" {
		t.Fatalf("outer node is %#v; want a mapFilter FuncCall", got)
	}
	inner, ok := outer.Args[1].(*chplan.FuncCall)
	if !ok || inner.Name != "mapFilter" {
		t.Fatalf("inner node is %#v; want a second mapFilter FuncCall", outer.Args[1])
	}
	if _, ok := inner.Args[1].(*chplan.ColumnRef); !ok {
		t.Errorf("innermost source is %#v; want the base ColumnRef", inner.Args[1])
	}
}
