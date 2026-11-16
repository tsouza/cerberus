package logql

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// projectedNames lists the output column names of p, in order.
func projectedNames(p *chplan.Project) []string {
	names := make([]string, 0, len(p.Projections))
	for _, projection := range p.Projections {
		names = append(names, chplan.ProjectionOutputName(projection))
	}
	return names
}

func lowerOf(column string) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{&chplan.ColumnRef{Name: column}}}
}

// TestRowLayersSeal pins how [rowLayers.seal] shapes the stack under a
// consumer:
//
//   - a layer whose value nothing above reads is spliced out;
//   - a kept layer passes through exactly the columns read above it,
//     sorted, followed by its own value — never its own alias;
//   - a filter between two layers adds the columns its predicate reads
//     to what the layers below it carry, so a value defined below the
//     filter and read only by the filter survives;
//   - the walk stops at the stream scan.
func TestRowLayersSeal(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_logs"}
	rows := newRowLayers(scan, true)
	a := rows.define("_a", lowerOf("X"))
	layerA := rows.node
	rows.filter(&chplan.Binary{Op: chplan.OpEq, Left: a, Right: &chplan.LitString{V: "x"}})
	filter := rows.node
	b := rows.define("_b", lowerOf("Y"))
	layerB := rows.node
	rows.define("_unread", lowerOf("Z"))

	consumer := &chplan.Project{
		Input: rows.node,
		Projections: []chplan.Projection{
			{Expr: b, Alias: "out"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
	}
	rows.seal(consumer)

	if consumer.Input != layerB {
		t.Fatalf("consumer input = %T, want the _b layer — the unread _unread layer must be spliced out", consumer.Input)
	}
	if got, want := projectedNames(layerB.(*chplan.Project)), []string{"Timestamp", "_b"}; !slices.Equal(got, want) {
		t.Errorf("_b layer projects %v, want %v", got, want)
	}
	if layerB.(*chplan.Project).Input != filter {
		t.Fatalf("_b layer input = %T, want the filter", layerB.(*chplan.Project).Input)
	}
	if filter.(*chplan.Filter).Input != layerA {
		t.Fatalf("filter input = %T, want the _a layer — a value only the filter reads must survive", filter.(*chplan.Filter).Input)
	}
	// Y is read by the _b layer above the filter and _a by the filter;
	// the _a layer passes the former through and defines the latter.
	if got, want := projectedNames(layerA.(*chplan.Project)), []string{"Timestamp", "Y", "_a"}; !slices.Equal(got, want) {
		t.Errorf("_a layer projects %v, want %v", got, want)
	}
	if layerA.(*chplan.Project).Input != scan {
		t.Errorf("_a layer input = %T, want the stream scan", layerA.(*chplan.Project).Input)
	}
}

// TestRowLayersSealCarry pins that a carried column passes through every
// kept layer although the consumer does not read it.
func TestRowLayersSealCarry(t *testing.T) {
	t.Parallel()

	rows := newRowLayers(&chplan.Scan{Table: "otel_logs"}, true)
	a := rows.define("_a", lowerOf("X"))
	layer := rows.node
	consumer := &chplan.Project{Input: rows.node, Projections: []chplan.Projection{{Expr: a, Alias: "out"}}}
	rows.seal(consumer, "Body")

	if got, want := projectedNames(layer.(*chplan.Project)), []string{"Body", "_a"}; !slices.Equal(got, want) {
		t.Errorf("layer projects %v, want %v", got, want)
	}
}

// TestRowLayersInline pins that a stack that does not materialise
// builds no layer: define returns the expression itself and filters
// fold onto the scan.
func TestRowLayersInline(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_logs"}
	rows := newRowLayers(scan, false)
	value := lowerOf("X")
	if got := rows.define("_a", value); got != value {
		t.Errorf("define = %#v, want the expression itself", got)
	}
	rows.filter(&chplan.Binary{Op: chplan.OpEq, Left: value, Right: &chplan.LitString{V: "x"}})
	if f, ok := rows.node.(*chplan.Filter); !ok || f.Input != scan {
		t.Errorf("row stage = %#v, want one filter over the scan", rows.node)
	}
}
