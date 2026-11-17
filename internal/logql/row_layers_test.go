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
	rows := newRowLayers(scan)
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

	rows := newRowLayers(&chplan.Scan{Table: "otel_logs"})
	a := rows.define("_a", lowerOf("X"))
	layer := rows.node
	consumer := &chplan.Project{Input: rows.node, Projections: []chplan.Projection{{Expr: a, Alias: "out"}}}
	rows.seal(consumer, "Body")

	if got, want := projectedNames(layer.(*chplan.Project)), []string{"Body", "_a"}; !slices.Equal(got, want) {
		t.Errorf("layer projects %v, want %v", got, want)
	}
}

// TestRowLayersHoist pins how [rowLayers.hoist] splits the stack at a log
// query's ORDER BY … LIMIT:
//
//   - a value a filter reads is defined below the limit, and a value only
//     the consumer reads is defined above it alone;
//   - a wide value both read is defined once on each side, never carried
//     through the sort, while any other value both read is defined
//     below and carried;
//   - between the filter and the sort a Project passes on only the
//     columns read above it.
func TestRowLayersHoist(t *testing.T) {
	t.Parallel()

	rows := newRowLayers(&chplan.Scan{Table: "otel_logs"})
	both := rows.defineWide("_both", lowerOf("X"))
	filterOnly := rows.define("_filter_only", lowerOf("Y"))
	consumerOnly := rows.define("_consumer_only", lowerOf("Z"))
	carried := rows.define("_carried", lowerOf("W"))
	rows.filter(&chplan.Binary{
		Op:   chplan.OpAnd,
		Left: &chplan.Binary{Op: chplan.OpEq, Left: both, Right: &chplan.LitString{V: "x"}},
		Right: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: filterOnly, Right: &chplan.LitString{V: "y"}},
			Right: &chplan.Binary{Op: chplan.OpEq, Left: carried, Right: &chplan.LitString{V: "w"}},
		},
	})
	order := &chplan.OrderBy{Keys: []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}}}
	limit := &chplan.Limit{Count: 100}
	rows.hoist(order, limit)
	consumer := &chplan.Project{
		Input: rows.node,
		Projections: []chplan.Projection{
			{Expr: both, Alias: "out_both"},
			{Expr: consumerOnly, Alias: "out_consumer"},
			{Expr: carried, Alias: "out_carried"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
	}
	rows.seal(consumer)

	var above, below []string
	side := &above
	var narrow *chplan.Project
	for n := consumer.Input; n != nil; n = inputOf(n) {
		switch v := n.(type) {
		case *chplan.Limit:
			side = &below
		case *chplan.Project:
			if slices.Contains(rows.passes, n) {
				narrow = v
				continue
			}
			*side = append(*side, v.Projections[len(v.Projections)-1].Alias)
		}
	}
	if want := []string{"_consumer_only", "_both"}; !slices.Equal(above, want) {
		t.Errorf("values defined above the limit, top first = %v, want %v", above, want)
	}
	if want := []string{"_carried", "_filter_only", "_both"}; !slices.Equal(below, want) {
		t.Errorf("values defined below the limit, top first = %v, want %v", below, want)
	}
	if narrow == nil {
		t.Fatal("no Project between the filter and the sort")
	}
	if _, ok := narrow.Input.(*chplan.Filter); !ok {
		t.Fatalf("the narrowing Project reads %T, want the filter", narrow.Input)
	}
	if order.Input != narrow {
		t.Fatalf("the sort reads %T, want the narrowing Project", order.Input)
	}
	// Above the limit _both reads X and _consumer_only Z; the consumer
	// reads Timestamp, which the sort orders by too, and _carried.
	if got, want := projectedNames(narrow), []string{"Timestamp", "X", "Z", "_carried"}; !slices.Equal(got, want) {
		t.Errorf("the sort carries %v, want %v: no wide value, and the narrow one the consumer reads", got, want)
	}
}

// TestRowLayersGatherAtScan pins how [rowLayers.gatherAtScan] splits a
// range aggregation's stack at the stream scan:
//
//   - a filter conjunct that reads no value joins the scan's own filter;
//   - a value that reads only the table's columns, and one a moved filter
//     reads, move into the scan's scope with that filter, in their order;
//   - a value whose reads of another value would take the scope past
//     [maxScanScopeExpansion] stays above, with every filter that reads it;
//   - a value that reads another value and that no moved filter reads goes
//     back to its place above;
//   - seal marks the Project directly above the moved part.
func TestRowLayersGatherAtScan(t *testing.T) {
	t.Parallel()

	eq := func(left chplan.Expr, v string) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: left, Right: &chplan.LitString{V: v}}
	}
	// deep is an 8-node value of the table's column X.
	deep := chplan.Expr(&chplan.ColumnRef{Name: "X"})
	for range 7 {
		deep = &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{deep}}
	}

	scanFilter := &chplan.Filter{Input: &chplan.Scan{Table: "otel_logs"}, Predicate: eq(&chplan.ColumnRef{Name: "Job"}, "api")}
	rows := newRowLayers(scanFilter)
	source := rows.define("_source", deep)
	reads := make([]chplan.Expr, 8)
	for i := range reads {
		reads[i] = source
	}
	wide := rows.define("_wide", &chplan.FuncCall{Fn: chplan.FnConcat, Args: reads})
	rows.filter(eq(&chplan.ColumnRef{Name: "Body"}, "line"))
	parsed := rows.define("_parsed", lowerOf("Y"))
	pick := rows.define("_pick", &chplan.Binary{Op: chplan.OpOr, Left: eq(parsed, "a"), Right: eq(parsed, "b")})
	rows.filter(eq(pick, "1"))
	unfiltered := rows.define("_unfiltered", &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{parsed, source}})
	rows.filter(eq(wide, "w"))
	rows.gatherAtScan(nil)

	consumer := &chplan.Project{
		Input: rows.node,
		Projections: []chplan.Projection{
			{Expr: unfiltered, Alias: "out"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
	}
	rows.seal(consumer)

	var stack []string
	var marked []string
	var bottom chplan.Node
	for n := consumer.Input; n != nil; n = inputOf(n) {
		switch v := n.(type) {
		case *chplan.Project:
			alias := v.Projections[len(v.Projections)-1].Alias
			stack = append(stack, alias)
			if v.OneScopeRows {
				marked = append(marked, alias)
			}
		case *chplan.Filter:
			stack = append(stack, "filter")
		}
		bottom = n
	}
	want := []string{"filter", "_unfiltered", "_wide", "filter", "_pick", "_parsed", "_source", "filter"}
	if !slices.Equal(stack, want) {
		t.Errorf("stack, top first = %v, want %v", stack, want)
	}
	if want := []string{"_wide"}; !slices.Equal(marked, want) {
		t.Errorf("marked OneScopeRows = %v, want %v", marked, want)
	}
	if bottom != scanFilter.Input {
		t.Fatalf("the stack bottoms out at %T, want the scan", bottom)
	}
	got := scanFilter.Predicate
	if f, ok := rows.atScan[0].(*chplan.Project).Input.(*chplan.Filter); ok {
		got = f.Predicate
	}
	if want := (&chplan.Binary{Op: chplan.OpAnd, Left: eq(&chplan.ColumnRef{Name: "Job"}, "api"), Right: eq(&chplan.ColumnRef{Name: "Body"}, "line")}); !got.Equal(want) {
		t.Errorf("the scan's filter = %v, want the selector's predicate AND the line filter", got)
	}
}

// TestRowLayersGatherAtScanLeavesWhatItMustNotMove pins what gatherAtScan
// leaves above the scan's scope: a value that reads a value left above it
// — wherever in its expression that read sits — with the filter on it,
// and, over a map the stage reads as a whole, a filter that reads the map
// whole and a value that reads a key of it.
func TestRowLayersGatherAtScanLeavesWhatItMustNotMove(t *testing.T) {
	t.Parallel()

	eq := func(left chplan.Expr, v string) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: left, Right: &chplan.LitString{V: v}}
	}
	mapCol := &chplan.ColumnRef{Name: "M"}
	key := &chplan.MapAccess{Map: mapCol, Key: &chplan.LitString{V: "k"}}
	hasKey := &chplan.FuncCall{Fn: chplan.FnMapContainsKey, Args: []chplan.Expr{mapCol, &chplan.LitString{V: "k"}}}

	for _, tc := range []struct {
		name string
		// build defines the stack and returns what the consumer reads.
		build func(rows *rowLayers) []chplan.Expr
		maps  []string
		// moved are the aliases the scan's scope must define, bottom
		// first, and "filter" for the filter it must run; folded is
		// whether the scan's own filter gains a conjunct.
		moved  []string
		folded bool
	}{
		{
			name: "a value reading an outside value before a moved one",
			build: func(rows *rowLayers) []chplan.Expr {
				// deep is an 8-node value of the table's column X.
				deep := chplan.Expr(&chplan.ColumnRef{Name: "X"})
				for range 7 {
					deep = &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{deep}}
				}
				source := rows.define("_source", deep)
				reads := make([]chplan.Expr, 8)
				for i := range reads {
					reads[i] = source
				}
				// _wide multiplies _source past the bound and stays above.
				wide := rows.define("_wide", &chplan.FuncCall{Fn: chplan.FnConcat, Args: reads})
				part := rows.define("_part", lowerOf("Y"))
				mix := rows.define("_mix", &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{wide, part}})
				rows.filter(eq(mix, "m"))
				return []chplan.Expr{&chplan.ColumnRef{Name: "Timestamp"}}
			},
			moved: []string{"_source", "_part"},
		},
		{
			name: "a map read whole by the consumer",
			build: func(rows *rowLayers) []chplan.Expr {
				rows.define("_key", key)
				rows.filter(hasKey)
				return []chplan.Expr{mapCol}
			},
			maps: []string{"M"},
		},
		{
			name: "a map read whole by a filter",
			build: func(rows *rowLayers) []chplan.Expr {
				rows.define("_key", key)
				rows.filter(hasKey)
				return []chplan.Expr{&chplan.ColumnRef{Name: "Timestamp"}}
			},
			maps: []string{"M"},
		},
		{
			name: "a filter reading a value and a map whole",
			build: func(rows *rowLayers) []chplan.Expr {
				value := rows.define("_value", lowerOf("Y"))
				rows.filter(&chplan.Binary{Op: chplan.OpOr, Left: eq(value, "v"), Right: hasKey})
				return []chplan.Expr{&chplan.ColumnRef{Name: "Timestamp"}}
			},
			maps:  []string{"M"},
			moved: []string{"_value"},
		},
		{
			name: "a filter reading a value",
			build: func(rows *rowLayers) []chplan.Expr {
				value := rows.define("_value", lowerOf("Y"))
				rows.filter(eq(value, "v"))
				return []chplan.Expr{&chplan.ColumnRef{Name: "Timestamp"}}
			},
			maps:  []string{"M"},
			moved: []string{"_value", "filter"},
		},
		{
			name: "a map read by key alone",
			build: func(rows *rowLayers) []chplan.Expr {
				rows.define("_key", key)
				rows.filter(eq(key, "v"))
				return []chplan.Expr{&chplan.ColumnRef{Name: "Timestamp"}}
			},
			maps:   []string{"M"},
			moved:  []string{"_key"},
			folded: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scanFilter := &chplan.Filter{Input: &chplan.Scan{Table: "otel_logs"}, Predicate: eq(&chplan.ColumnRef{Name: "Job"}, "api")}
			rows := newRowLayers(scanFilter)
			reads := tc.build(rows)
			rows.gatherAtScan(tc.maps, reads...)

			var moved []string
			var scan *chplan.Filter
			for _, n := range rows.atScan {
				switch v := n.(type) {
				case *chplan.Project:
					moved = append(moved, v.Projections[len(v.Projections)-1].Alias)
				case *chplan.Filter:
					moved = append(moved, "filter")
				}
			}
			for n := rows.node; n != nil; n = inputOf(n) {
				if f, ok := n.(*chplan.Filter); ok {
					if _, onScan := f.Input.(*chplan.Scan); onScan {
						scan = f
					}
				}
			}
			if !slices.Equal(moved, tc.moved) {
				t.Errorf("moved into the scan's scope = %v, want %v", moved, tc.moved)
			}
			if scan == nil {
				t.Fatal("no filter over the scan")
			}
			if folded := !scan.Predicate.Equal(scanFilter.Predicate); folded != tc.folded {
				t.Errorf("the scan's filter gained a conjunct = %v, want %v: %v", folded, tc.folded, scan.Predicate)
			}
		})
	}
}
