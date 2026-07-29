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

// TestCanonicalizeSeriesKeyExprs_WrapsARawKeyAfterANonRawOne pins that a key
// the walk cannot prove raw only makes THAT key pass through — the scan must
// carry on to the keys behind it. A `break` in place of the `continue` stops
// at the first ordinary key (a bare value column, which every real key list
// carries alongside the attribute Map) and leaves the raw Map behind it
// unwrapped, which is the silent series-split the whole file exists to stop.
func TestCanonicalizeSeriesKeyExprs_WrapsARawKeyAfterANonRawOne(t *testing.T) {
	inputs := []chplan.Node{gaugeScan()}
	keys := []chplan.Expr{
		&chplan.ColumnRef{Name: "Value"},      // not an attribute Map: skipped
		&chplan.ColumnRef{Name: "Attributes"}, // raw Map: must still be wrapped
	}

	out := chplan.CanonicalizeSeriesKeyExprs(keys, inputs, attrCols())

	if len(out) != 2 {
		t.Fatalf("want 2 keys back, got %d", len(out))
	}
	if !out[0].Equal(keys[0]) {
		t.Errorf("the non-raw key must pass through untouched; got %#v", out[0])
	}
	call, ok := out[1].(*chplan.FuncCall)
	if !ok || call.Name != chplan.CanonicalMapFunc {
		t.Fatalf("the raw key behind it must be wrapped in %s; got %#v", chplan.CanonicalMapFunc, out[1])
	}
}

// TestCanonicalizeSeriesIdentityKeys_WrapsARawKeyAfterANonRawOne is the
// whole-plan counterpart: an Aggregate keyed on an ordinary column AND the
// raw attribute Map still gets the repair. The column scan behind the node's
// key list has to survive a key it cannot prove raw.
func TestCanonicalizeSeriesIdentityKeys_WrapsARawKeyAfterANonRawOne(t *testing.T) {
	in := &chplan.Aggregate{
		Input: gaugeScan(),
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "Value"},
			&chplan.ColumnRef{Name: "Attributes"},
		},
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	reps := projectReplacements(t, out)
	if len(reps) != 1 || reps[0].Alias != "Attributes" {
		t.Fatalf("want the Attributes column canonicalised beneath the Aggregate; got %#v", reps)
	}
}

// TestCanonicalizeSeriesIdentityKeys_WrapsThroughAMapValuedCall pins that the
// idempotence check tests for the CANONICAL function specifically. A key like
// `mapConcat(Attributes)` is a Map-returning call sitting bare in a key list —
// the binding site — so the walk has to descend into its arguments. Treating
// every named call as already canonical skips the descent and leaves the
// positional Map comparison unrepaired.
func TestCanonicalizeSeriesIdentityKeys_WrapsThroughAMapValuedCall(t *testing.T) {
	in := &chplan.Aggregate{
		Input: gaugeScan(),
		GroupBy: []chplan.Expr{&chplan.FuncCall{
			Name: "mapConcat",
			Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}},
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	reps := projectReplacements(t, out)
	if len(reps) != 1 || reps[0].Alias != "Attributes" {
		t.Fatalf("want the Attributes column canonicalised beneath the Aggregate; got %#v", reps)
	}
}

// TestCanonicalizeSeriesIdentityKeys_ResolvesTheProjectionThatBindsTheName
// pins that resolving a column through an explicit projection list matches on
// the alias that actually binds the name. Answering with the WRONG projection
// reads a sibling's expression, so a plan whose projection already
// canonicalises the Map is judged raw and gets a second, redundant repair
// layer inserted beneath it.
func TestCanonicalizeSeriesIdentityKeys_ResolvesTheProjectionThatBindsTheName(t *testing.T) {
	canonicalising := &chplan.Project{
		Input: gaugeScan(),
		Projections: []chplan.Projection{
			// A sibling that reads the RAW Map, bound under a different name.
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "RawCopy"},
			// The projection that actually binds "Attributes", already canonical.
			{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}), Alias: "Attributes"},
		},
	}
	in := &chplan.Aggregate{
		Input:    canonicalising,
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	if out != chplan.Node(in) {
		t.Fatalf("a key already canonicalised by the projection that binds it needs no repair; got %#v", out)
	}
}

// TestCanonicalizeSeriesIdentityKeys_ScansPastANonBindingProjection is the
// mirror of the test above: the projection that binds the key is again not the
// first one, but this time it hands through the RAW Map, so the key does need
// repair. Abandoning the projection list at the first alias that does not match
// leaves the binding unresolved, and an unresolved binding is treated as "not
// raw" — the plan then compares Maps positionally and answers with the wrong
// series identity.
func TestCanonicalizeSeriesIdentityKeys_ScansPastANonBindingProjection(t *testing.T) {
	binding := &chplan.Project{
		Input: gaugeScan(),
		Projections: []chplan.Projection{
			// A non-matching alias the scan has to walk past.
			{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}), Alias: "Canonical"},
			// The projection that actually binds "Attributes", still raw.
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
		},
	}
	in := &chplan.Aggregate{
		Input:    binding,
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}

	out := chplan.CanonicalizeSeriesIdentityKeys(in, attrCols())

	reps := projectReplacements(t, out)
	if len(reps) != 1 || reps[0].Alias != "Attributes" {
		t.Fatalf("want the Attributes column canonicalised beneath the Aggregate; got %#v", reps)
	}
}
