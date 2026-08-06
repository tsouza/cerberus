package chplan_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// allNodeKinds is one populated instance of every concrete Node type in
// the chplan IR. The count guard at the bottom of TestCloneNodeExhaustive
// fails when a new Node type is added without a CloneNode case, in
// lock-step with the panic-default in clone.go.
func allNodeKinds() []chplan.Node {
	leaf := &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}}
	expr := chplan.Expr(&chplan.ColumnRef{Name: "Attributes"})
	return []chplan.Node{
		&chplan.Scan{Database: "db", Table: "t", UnionTables: []string{"a", "b"}, Columns: []string{"x"}},
		&chplan.Filter{Input: leaf, Predicate: &chplan.LitBool{V: true}},
		&chplan.Project{Input: leaf, Projections: []chplan.Projection{{Expr: expr, Alias: "A"}}},
		&chplan.Aggregate{
			Input:          leaf,
			GroupBy:        []chplan.Expr{expr},
			GroupByAliases: []string{"g0"},
			AggFuncs:       []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"}},
			// Having carries the duplicate-labelset / conflicting-label aborts,
			// and a zero-valued fixture field is equal on both sides of a
			// dropped copy — so leaving it nil made the Equal assertion below
			// blind to exactly the bug that shipped.
			Having: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.FuncCall{Name: "throwIf", Args: []chplan.Expr{&chplan.LitBool{V: false}, &chplan.LitString{V: "boom"}}},
				Right: &chplan.LitInt{V: 0},
			},
		},
		&chplan.RangeWindow{
			Input: leaf, Func: "rate", Range: 5 * time.Minute, Step: time.Minute,
			OuterRange: time.Hour, Start: time.Unix(1000, 0).UTC(), End: time.Unix(4600, 0).UTC(),
			GroupBy: []chplan.Expr{expr}, Scalars: []float64{1}, ScalarExprs: []chplan.Expr{&chplan.LitFloat{V: 2}},
		},
		&chplan.RangeWindowNative{
			Input: leaf, Func: "rate", Range: 5 * time.Minute, Step: time.Minute,
			Start: time.Unix(1000, 0).UTC(), End: time.Unix(4600, 0).UTC(),
			TimestampColumn: "TimeUnix", ValueColumn: "Value", GroupBy: []chplan.Expr{expr},
			// Populated so the clone contract is exercised on the
			// deferred-shaping shape too — a nil slice clones correctly by
			// accident, so a two-level fixture asserts nothing about it.
			Recollapse: []chplan.Projection{{
				Expr:  &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{expr}},
				Alias: "Attributes",
			}},
		},
		&chplan.RangeLWR{Input: leaf, Lookback: 5 * time.Minute, ValueCol: "Value"},
		&chplan.RangeBucketFanout{
			Input: leaf, GroupBy: []chplan.Expr{expr}, GroupByAliases: []string{"g0"},
			AggFuncs:    []chplan.AggFunc{{Name: "sumForEach", Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}, Alias: "BucketCounts"}},
			AnchorAlias: "anchor_ts", TimestampCol: "TimeUnix",
		},
		&chplan.StepGrid{Start: time.Unix(1, 0).UTC(), End: time.Unix(2, 0).UTC(), Step: time.Second},
		&chplan.AbsentOverTime{Input: leaf, SynthLabels: []chplan.SynthLabel{{Key: "k", Value: "v"}}, Range: 5 * time.Minute},
		&chplan.TopK{Input: leaf, K: 3, By: []chplan.Expr{expr}, SortExpr: &chplan.ColumnRef{Name: "Value"}, Columns: []string{"Value"}},
		&chplan.Limit{Input: leaf, Count: 10},
		&chplan.OrderBy{Input: leaf, Keys: []chplan.OrderKey{{Expr: expr, Desc: true}}},
		&chplan.OneRow{},
		&chplan.UnionAll{Inputs: []chplan.Node{leaf, &chplan.Scan{Table: "u"}}},
		&chplan.CrossJoin{Left: leaf, Right: &chplan.Scan{Table: "r"}},
		&chplan.SetOperation{Left: leaf, Right: &chplan.Scan{Table: "r"}, TraceIDColumn: "TraceId"},
		&chplan.StructuralJoin{Left: leaf, Right: &chplan.Scan{Table: "r"}, TraceIDColumn: "TraceId", ExtraProjectionColumns: []string{"c"}},
		&chplan.VectorJoin{
			Left: leaf, Right: &chplan.Scan{Table: "r"}, Match: chplan.VectorMatch{Labels: []string{"job"}, On: true},
			Include: []string{"inst"}, ValueColumn: "Value",
		},
		&chplan.VectorSetOp{Left: leaf, Right: &chplan.Scan{Table: "r"}, Match: chplan.VectorMatch{Labels: []string{"job"}}, ValueColumn: "Value"},
		&chplan.NaryVectorSetOp{
			Arms: []chplan.Node{leaf, &chplan.Scan{Table: "r"}, &chplan.Scan{Table: "s"}},
			Op:   chplan.VectorSetOr, Match: chplan.VectorMatch{Labels: []string{"job"}}, ValueColumn: "Value",
		},
		&chplan.HistogramQuantile{Input: leaf, Phi: 0.9, GroupBy: []chplan.Expr{expr}, GroupByAliases: []string{"g0"}, BucketCountsColumn: "BucketCounts"},
		&chplan.HistogramQuantileNative{Input: leaf, Phi: 0.9, GroupBy: []chplan.Expr{expr}, GroupByAliases: []string{"g0"}, ScaleColumn: "Scale"},
		&chplan.MetricsAggregate{Attr: expr, GroupBy: []chplan.Expr{expr}, GroupByAliases: []string{"g0"}, GroupByDisplayNames: []string{"g"}, Quantiles: []float64{0.5}, Inner: leaf},
		&chplan.MetricsCompare{Selection: expr, Pairs: expr, RootLookup: leaf, Inner: leaf, TraceIDColumn: "TraceId"},
		&chplan.MetricsHistogramOverTime{Attr: expr, GroupBy: []chplan.Expr{expr}, GroupByAliases: []string{"g0"}, GroupByDisplayNames: []string{"g"}, Inner: leaf},
		&chplan.MetricsSecondStage{Input: leaf, K: 5, PartitionBy: []string{"p"}, ValueAlias: "v"},
		&chplan.NestedSetAnnotate{Input: leaf, SpansTable: "spans", TraceIDColumn: "TraceId"},
		&chplan.InfoJoin{
			Input: leaf, Info: &chplan.Scan{Table: "info"},
			IdentityLabels: []string{"instance", "job"}, DataLabels: []string{"version"},
			MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
			TimestampColumn: "TimeUnix", ValueColumn: "Value",
		},
		&chplan.RangeWindowResample{
			Input: leaf, Start: time.Unix(1000, 0).UTC(), End: time.Unix(4600, 0).UTC(),
			Step: time.Minute, Lookback: 5 * time.Minute, Offset: time.Minute,
			MetricNameCol: "MetricName", AttributesCol: "Attributes",
			TimestampCol: "TimeUnix", ValueCol: "Value",
		},
		&chplan.SearchTraceLimit{Input: leaf, TraceIDColumn: "TraceId", TimestampColumn: "TimeUnix", TraceLimit: 20},
	}
}

// TestCloneNodeExhaustive asserts CloneNode handles every Node kind,
// produces a structurally-Equal copy, and never aliases the root pointer.
func TestCloneNodeExhaustive(t *testing.T) {
	t.Parallel()

	nodes := allNodeKinds()

	seen := map[reflect.Type]bool{}
	for _, n := range nodes {
		rt := reflect.TypeOf(n)
		if seen[rt] {
			t.Errorf("duplicate Node type in exhaustiveness set: %v", rt)
		}
		seen[rt] = true

		clone := chplan.CloneNode(n)
		if reflect.TypeOf(clone) != rt {
			t.Errorf("CloneNode(%T) returned %T", n, clone)
		}
		// Pointer-distinctness is a proxy for "deep copy", but zero-size types
		// (e.g. *OneRow, backed by struct{}) cannot satisfy it: Go returns the
		// single runtime.zerobase address for every zero-size allocation, so
		// clone == n is unavoidable. Sharing is safe because such nodes are
		// immutable — no fields to alias — so the deep-copy contract holds
		// vacuously. Skip the proxy only for them.
		if rt.Elem().Size() != 0 && clone == n {
			t.Errorf("CloneNode(%T) returned the same pointer — not a copy", n)
		}
		if !n.Equal(clone) {
			t.Errorf("CloneNode(%T) is not Equal to the original", n)
		}
		if !clone.Equal(n) {
			t.Errorf("CloneNode(%T): Equal is not symmetric", n)
		}
	}

	// Lock-step guard: every concrete planNode() implementer in chplan must
	// appear here. When this count drifts, a Node type was added — extend
	// allNodeKinds AND the CloneNode switch in clone.go.
	const wantNodeTypes = 31
	if len(nodes) != wantNodeTypes {
		t.Fatalf("expected %d Node types, listed %d — a Node type was added: "+
			"extend allNodeKinds + CloneNode", wantNodeTypes, len(nodes))
	}
}

// TestCloneNodeDeepCopyIsolation mutates fields on a clone and asserts the
// original is untouched, exercising the slice/expr/child sharing hazards.
func TestCloneNodeDeepCopyIsolation(t *testing.T) {
	t.Parallel()

	orig := &chplan.RangeWindow{
		Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: "metrics", Columns: []string{"Value"}},
			Predicate: &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitFloat{V: 1}},
		},
		Func:       "rate",
		Range:      5 * time.Minute,
		Step:       time.Minute,
		OuterRange: time.Hour,
		Start:      time.Unix(1000, 0).UTC(),
		End:        time.Unix(4600, 0).UTC(),
		GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Scalars:    []float64{0.5},
	}
	// The reference values are read off orig BEFORE the clone is mutated. A
	// second CloneNode call would compare the function under test against
	// itself: drop the per-slice copies and `c := *v` hands orig, the
	// reference AND the clone one shared backing array, so every mutation
	// below lands in all three and the comparison still holds.
	wantFunc, wantRange, wantStart := orig.Func, orig.Range, orig.Start
	wantGroupBy, wantScalar := orig.GroupBy[0], orig.Scalars[0]
	wantPredicate := orig.Input.(*chplan.Filter).Predicate

	clone, ok := chplan.CloneNode(orig).(*chplan.RangeWindow)
	if !ok {
		t.Fatal("clone is not *RangeWindow")
	}

	// Aliasing is the failure the value checks below can only observe
	// indirectly, so assert it head-on: a shared backing array or a shared
	// child pointer is already the bug, mutation or not.
	if &clone.GroupBy[0] == &orig.GroupBy[0] {
		t.Fatal("GroupBy backing array aliased between clone and original")
	}
	if &clone.Scalars[0] == &orig.Scalars[0] {
		t.Fatal("Scalars backing array aliased between clone and original")
	}
	if clone.Input == orig.Input {
		t.Fatal("Input child aliased between clone and original")
	}

	// Mutate scalar fields, slice elements, and a nested expr on the clone.
	clone.Func = "increase"
	clone.Range = 99 * time.Hour
	clone.Start = time.Unix(7777, 0).UTC()
	clone.GroupBy[0] = &chplan.ColumnRef{Name: "MUTATED"}
	clone.Scalars[0] = -1
	clone.Input.(*chplan.Filter).Predicate = &chplan.LitBool{V: false}

	if orig.Func != wantFunc || orig.Range != wantRange || !orig.Start.Equal(wantStart) {
		t.Fatal("mutating the clone's scalar fields changed the original — deep copy leaked")
	}
	if orig.GroupBy[0] != wantGroupBy {
		t.Fatal("mutating the clone's GroupBy element changed the original — deep copy leaked")
	}
	if orig.Scalars[0] != wantScalar {
		t.Fatal("mutating the clone's Scalars element changed the original — deep copy leaked")
	}
	if orig.Input.(*chplan.Filter).Predicate != wantPredicate {
		t.Fatal("mutating the clone's nested Filter predicate changed the original — deep copy leaked")
	}
}

// TestCloneNodeRecollapseIsolation pins the deep copy of
// RangeWindowNative.Recollapse. A `c := *v` shallow copy hands the clone the
// SAME backing array, so an optimizer rule that retargets a deferred shaping
// expression on its clone silently rewrites the plan it was cloned from — the
// shaping tower is the node's output series identity, so the corruption
// presents as a wrong label set rather than as an error.
func TestCloneNodeRecollapseIsolation(t *testing.T) {
	t.Parallel()

	orig := &chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		Start:           time.Unix(1000, 0).UTC(),
		End:             time.Unix(4600, 0).UTC(),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Recollapse: []chplan.Projection{{
			Expr:  &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}},
			Alias: "Attributes",
		}},
	}
	// Reference values read off orig before mutating — see the same note in
	// TestCloneNodeDeepCopyIsolation: a CloneNode-derived snapshot shares the
	// mutated backing array with orig whenever the copy is missing, so it can
	// never witness the leak.
	wantAlias, wantExpr := orig.Recollapse[0].Alias, orig.Recollapse[0].Expr

	clone, ok := chplan.CloneNode(orig).(*chplan.RangeWindowNative)
	if !ok {
		t.Fatal("clone is not *RangeWindowNative")
	}
	if &clone.Recollapse[0] == &orig.Recollapse[0] {
		t.Fatal("Recollapse backing array aliased between clone and original")
	}
	clone.Recollapse[0].Alias = "MUTATED"
	clone.Recollapse[0].Expr = &chplan.ColumnRef{Name: "MUTATED"}

	if orig.Recollapse[0].Alias != wantAlias || orig.Recollapse[0].Expr != wantExpr {
		t.Fatal("mutating the clone's Recollapse changed the original tree — deep copy leaked")
	}
}

// TestCloneNodeScalarSubqueryIsolation pins the explicit ScalarSubquery
// handling: chplan.Walk does not recurse into ScalarSubquery.Input, so a
// node-only copy would alias it. The clone's embedded plan must be a fresh
// subtree.
func TestCloneNodeScalarSubqueryIsolation(t *testing.T) {
	t.Parallel()

	embedded := &chplan.Scan{Table: "inner", Columns: []string{"Value"}}
	orig := &chplan.Filter{
		Input: &chplan.Scan{Table: "outer"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: embedded},
		},
	}
	snapshot := chplan.CloneNode(orig)

	clone := chplan.CloneNode(orig).(*chplan.Filter)
	sub := clone.Predicate.(*chplan.Binary).Right.(*chplan.ScalarSubquery)
	if sub.Input == embedded {
		t.Fatal("CloneNode aliased ScalarSubquery.Input instead of deep-copying it")
	}
	// Mutate the cloned embedded subtree; the original must be untouched.
	sub.Input = &chplan.Scan{Table: "MUTATED"}
	if !orig.Equal(snapshot) {
		t.Fatal("mutating the clone's ScalarSubquery.Input changed the original")
	}
}
