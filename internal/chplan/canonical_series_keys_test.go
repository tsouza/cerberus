package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func attrCols() chplan.AttributeMapColumns {
	return chplan.NewAttributeMapColumns("Attributes", "ResourceAttributes")
}

func gaugeScan() *chplan.Scan {
	return &chplan.Scan{Table: "otel_metrics_gauge", Columns: []string{"Attributes", "Value"}}
}

// projectReplacements returns the REPLACE list of the Project directly beneath
// n's first child, or nil when that child is not a replacing Project.
func projectReplacements(t *testing.T, n chplan.Node) []chplan.Projection {
	t.Helper()
	kids := n.Children()
	if len(kids) == 0 {
		t.Fatalf("node %T has no children", n)
	}
	p, ok := kids[0].(*chplan.Project)
	if !ok {
		return nil
	}
	return p.Replacements
}

func TestCanonicalizeSeriesIdentityKeys_WrapsRawAttributeMapKey(t *testing.T) {
	in := &chplan.Aggregate{
		Input:    gaugeScan(),
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	reps := projectReplacements(t, out)
	if len(reps) != 1 {
		t.Fatalf("want 1 replacement beneath the Aggregate, got %d (%T)", len(reps), out.Children()[0])
	}
	if reps[0].Alias != "Attributes" {
		t.Fatalf("replacement must keep the column NAME so every reference above it still resolves; got alias %q", reps[0].Alias)
	}
	call, ok := reps[0].Expr.(*chplan.FuncCall)
	if !ok || call.Name != chplan.CanonicalMapFunc {
		t.Fatalf("replacement expr = %#v, want a %s call", reps[0].Expr, chplan.CanonicalMapFunc)
	}
	if len(call.Args) != 1 {
		t.Fatalf("%s call takes exactly the column, got %d args", chplan.CanonicalMapFunc, len(call.Args))
	}
	col, ok := call.Args[0].(*chplan.ColumnRef)
	if !ok || col.Name != "Attributes" {
		t.Fatalf("%s argument = %#v, want ColumnRef{Attributes}", chplan.CanonicalMapFunc, call.Args[0])
	}
	if !out.Children()[0].Children()[0].Equal(gaugeScan()) {
		t.Fatalf("the scan beneath the inserted Project must be untouched, got %#v", out.Children()[0].Children()[0])
	}
}

func TestCanonicalizeSeriesIdentityKeys_LeavesCanonicalPlanUntouched(t *testing.T) {
	// The shape every head's lowering produces: the identity projection is
	// already mapSort-rooted and the key references it by alias.
	in := &chplan.Aggregate{
		Input: &chplan.Project{
			Input: gaugeScan(),
			Projections: []chplan.Projection{
				{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}), Alias: "Attributes"},
				{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			},
		},
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(chplan.CloneNode(in), attrCols())

	if !out.Equal(in) {
		t.Fatalf("a plan that already canonicalises must come back identical; got %#v", out)
	}
}

func TestCanonicalizeSeriesIdentityKeys_IsIdempotent(t *testing.T) {
	in := &chplan.Aggregate{
		Input:    gaugeScan(),
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}

	once := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())
	twice := chplan.CanonicalizeSeriesIdentityKeys(once, attrCols())

	if !twice.Equal(once) {
		t.Fatalf("second pass must be a no-op, otherwise the repair stacks mapSort layers; got %#v", twice)
	}
}

func TestCanonicalizeSeriesIdentityKeys_WrapsThroughMapWithoutKeys(t *testing.T) {
	// PromQL `without(...)` keys on a key-order-PRESERVING wrapper, so the
	// invariant binds on the map it reads, not on the wrapper.
	in := &chplan.Aggregate{
		Input: gaugeScan(),
		GroupBy: []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: "Attributes"},
			Keys: []string{"instance"},
		}},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	if reps := projectReplacements(t, out); len(reps) != 1 || reps[0].Alias != "Attributes" {
		t.Fatalf("a without(...) key must still canonicalise the map beneath it; replacements=%#v", reps)
	}
}

func TestCanonicalizeSeriesIdentityKeys_IgnoresNonMapKey(t *testing.T) {
	in := &chplan.Aggregate{
		Input:    &chplan.Scan{Table: "otel_metrics_gauge", Columns: []string{"MetricName", "Value"}},
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(chplan.CloneNode(in), attrCols())

	if !out.Equal(in) {
		t.Fatalf("a scalar key is not a Map and must not be rewritten; got %#v", out)
	}
}

func TestCanonicalizeSeriesKeyExprs_WrapsOnlyRawKeys(t *testing.T) {
	inputs := []chplan.Node{gaugeScan()}
	raw := &chplan.ColumnRef{Name: "Attributes"}
	scalar := &chplan.ColumnRef{Name: "Value"}

	out := chplan.CanonicalizeSeriesKeyExprs([]chplan.Expr{raw, scalar}, inputs, attrCols())

	if len(out) != 2 {
		t.Fatalf("key count must be preserved, got %d", len(out))
	}
	call, ok := out[0].(*chplan.FuncCall)
	if !ok || call.Name != chplan.CanonicalMapFunc {
		t.Fatalf("raw attribute key = %#v, want a %s call", out[0], chplan.CanonicalMapFunc)
	}
	if out[1] != scalar {
		t.Fatalf("a non-Map key must pass through by identity, got %#v", out[1])
	}
}

func TestCanonicalizeSeriesKeyExprs_LeavesCanonicalKeysAlone(t *testing.T) {
	inputs := []chplan.Node{gaugeScan()}
	keys := []chplan.Expr{chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"})}

	out := chplan.CanonicalizeSeriesKeyExprs(keys, inputs, attrCols())

	if len(out) != 1 || !out[0].Equal(keys[0]) {
		t.Fatalf("an already-canonical key must not be double-wrapped; got %#v", out)
	}
}
