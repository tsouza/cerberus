package traceql

import (
	"context"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	tempoql "github.com/tsouza/cerberus/internal/traceql/ast"
)

// leafWindow is the request-window bound one leaf Filter(Scan) of a lowered
// search plan carries — the nanosecond instants of its Timestamp bounds, in
// the order they render — whether the leaf reads for a per-trace subquery
// (a TraceLookupFilter lookup or a trace-cohort membership subquery) rather
// than for the span rows the search matches, and whether it reads the span
// rows a SearchTraceLimit drains for its ranked traces (its Input, when it
// ranks over a separate Rank).
type leafWindow struct {
	instants []int64
	perTrace bool
	drain    bool
}

// collectLeafWindows records a leafWindow for every Filter(Scan) leaf of n,
// descending the trace-cohort subqueries behind Expr slots and the lookup
// relations of a TraceLookupFilter, which read for a per-trace subquery, and
// both row sources of a SearchTraceLimit.
func collectLeafWindows(n chplan.Node, perTrace bool, tsCol string, out *[]leafWindow) {
	collectLeafWindowsAs(n, leafWindow{perTrace: perTrace}, tsCol, out)
}

// collectLeafWindowsAs is collectLeafWindows with the leaf kind carried as a
// leafWindow template.
func collectLeafWindowsAs(n chplan.Node, kind leafWindow, tsCol string, out *[]leafWindow) {
	perTraceKind := leafWindow{perTrace: true}
	cohorts := func(e chplan.Expr) {
		for _, in := range chplan.CohortSubqueries(e) {
			collectLeafWindowsAs(in.Subquery, perTraceKind, tsCol, out)
		}
	}
	switch v := n.(type) {
	case nil:
		return
	case *chplan.Filter:
		cohorts(v.Predicate)
		if _, ok := v.Input.(*chplan.Scan); ok {
			leaf := kind
			leaf.instants = windowInstants(v.Predicate, tsCol)
			*out = append(*out, leaf)
			return
		}
		collectLeafWindowsAs(v.Input, kind, tsCol, out)
		return
	case *chplan.TraceLookupFilter:
		cohorts(v.Predicate)
		for _, lookup := range v.Lookups {
			collectLeafWindowsAs(lookup, perTraceKind, tsCol, out)
		}
		collectLeafWindowsAs(v.Input, kind, tsCol, out)
		return
	case *chplan.SearchTraceLimit:
		if v.Rank != nil {
			drain := kind
			drain.drain = true
			collectLeafWindowsAs(v.Input, drain, tsCol, out)
			collectLeafWindowsAs(v.Rank, kind, tsCol, out)
			return
		}
	}
	for _, c := range n.Children() {
		collectLeafWindowsAs(c, kind, tsCol, out)
	}
}

// windowInstants returns the nanosecond instants of the `tsCol >= …` /
// `tsCol <= …` request-window bounds pred carries outside its cohort
// subqueries, in render order.
func windowInstants(pred chplan.Expr, tsCol string) []int64 {
	var out []int64
	var walk func(e chplan.Expr)
	walk = func(e chplan.Expr) {
		b, ok := e.(*chplan.Binary)
		if !ok {
			return
		}
		if b.Op == chplan.OpAnd {
			walk(b.Left)
			walk(b.Right)
			return
		}
		col, isCol := b.Left.(*chplan.ColumnRef)
		call, isCall := b.Right.(*chplan.FuncCall)
		if !isCol || col.Name != tsCol || !isCall || call.Fn != chplan.FnFromUnixNanos || len(call.Args) != 1 {
			return
		}
		if lit, ok := call.Args[0].(*chplan.LitInt); ok && (b.Op == chplan.OpGe || b.Op == chplan.OpLe) {
			out = append(out, lit.V)
		}
	}
	walk(pred)
	return out
}

// TestSearchPerTraceSubqueriesReadWidenedWindow pins the search-path half of
// the per-trace lookback: every per-trace subquery — the lookup behind a
// rootName / rootServiceName / traceDuration span-operand comparison, the
// membership subquery behind a literal comparison, a spanset-aggregate
// operand's cohort — reads its traces' spans inside the request window widened
// by the maximum trace duration on both sides, [start - L, end + L], on the
// plain top-N shape and nested under a spanset operation. The span rows a
// search chooses its traces from stay inside [start, end], and the plain
// shape drains its chosen traces' matching spans over [start - L, end + L].
func TestSearchPerTraceSubqueriesReadWidenedWindow(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	lookback := s.MaxTraceDuration
	if lookback <= 0 {
		t.Fatalf("default maximum trace duration %s: the two windows cannot be told apart", lookback)
	}
	const searchLimit = 20
	rows := []int64{winStart.UnixNano(), winEnd.UnixNano()}
	perTrace := []int64{winStart.Add(-lookback).UnixNano(), winEnd.Add(lookback).UnixNano()}

	for _, q := range []string{
		`{ rootName = name }`,
		`{ rootServiceName = resource.service.name }`,
		`{ traceDuration > duration }`,
		`{ rootName = "checkout" }`,
		`{ rootServiceName != "checkout" }`,
		`{ traceDuration > 2s }`,
		`{ rootName = name || traceDuration > 2s }`,
		`{ rootName = name } && { kind = server }`,
		`{ traceDuration > 1s } || { kind = server }`,
		`{ rootName = name || traceDuration > 2s } && { kind = server }`,
		`({ span.region = "us" } | count() > 1) || ({ span.tier = "gold" })`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			plan := lowerSearchWindowed(t, q, searchLimit, winStart, winEnd)
			var leaves []leafWindow
			collectLeafWindows(plan, false, s.TimestampColumn, &leaves)
			var perTraceLeaves, rowLeaves, drainLeaves int
			for i, leaf := range leaves {
				want := rows
				switch {
				case leaf.perTrace:
					want = perTrace
					perTraceLeaves++
				case leaf.drain:
					want = perTrace
					drainLeaves++
				default:
					rowLeaves++
				}
				if !slices.Equal(leaf.instants, want) {
					t.Errorf("leaf %d (per-trace %v, drain %v) window instants %v, want %v", i, leaf.perTrace, leaf.drain, leaf.instants, want)
				}
			}
			if perTraceLeaves == 0 || rowLeaves == 0 {
				t.Fatalf("%d per-trace and %d row leaves; the query must exercise both", perTraceLeaves, rowLeaves)
			}
			// The plain shape drains in its own plan; a spanset operation
			// drains in the search handler's phase B.
			_, plain := plan.(*chplan.SearchTraceLimit)
			if plain != (drainLeaves > 0) {
				t.Fatalf("plain shape %v with %d drain leaves", plain, drainLeaves)
			}
		})
	}
}

// TestSearchPerTraceWindowZeroLookback pins the setting's zero: with no
// maximum trace duration, every per-trace subquery of a lowered search reads
// the request window itself, as the span rows do.
func TestSearchPerTraceWindowZeroLookback(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = 0
	const searchLimit = 20
	expr, err := tempoql.Parse(`{ rootName = name || traceDuration > 2s } && { kind = server }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := WithSearchWindow(WithSearchTraceLimit(context.Background(), searchLimit), winStart, winEnd)
	plan, err := Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	var leaves []leafWindow
	collectLeafWindows(plan, false, s.TimestampColumn, &leaves)
	var perTraceLeaves int
	want := []int64{winStart.UnixNano(), winEnd.UnixNano()}
	for i, leaf := range leaves {
		if leaf.perTrace {
			perTraceLeaves++
		}
		if !slices.Equal(leaf.instants, want) {
			t.Errorf("leaf %d (per-trace %v) window instants %v, want %v", i, leaf.perTrace, leaf.instants, want)
		}
	}
	if perTraceLeaves == 0 {
		t.Fatal("no per-trace leaf; the query must exercise one")
	}
}

// collectStructuralWindows records the [WindowStartNano, WindowEndNano] of
// every StructuralJoin in n, split by whether it sits inside a trace-cohort
// subquery (behind an Expr slot of a Filter or a TraceLookupFilter predicate).
func collectStructuralWindows(n chplan.Node, inCohort bool, cohort, rows *[][2]int64) {
	descend := func(e chplan.Expr) {
		for _, in := range chplan.CohortSubqueries(e) {
			collectStructuralWindows(in.Subquery, true, cohort, rows)
		}
	}
	switch v := n.(type) {
	case nil:
		return
	case *chplan.StructuralJoin:
		out := rows
		if inCohort {
			out = cohort
		}
		*out = append(*out, [2]int64{v.WindowStartNano, v.WindowEndNano})
	case *chplan.Filter:
		descend(v.Predicate)
	case *chplan.TraceLookupFilter:
		descend(v.Predicate)
	}
	for _, c := range n.Children() {
		collectStructuralWindows(c, inCohort, cohort, rows)
	}
}

// TestSearchCohortStructuralClosureReadsWidenedWindow pins the per-trace
// window on a structural closure's recursive arm: a closure inside a
// spanset-aggregate operand's cohort is part of that per-trace subquery and
// reads [start - L, end + L], while a closure on the span rows the search
// matches reads [start, end].
func TestSearchCohortStructuralClosureReadsWidenedWindow(t *testing.T) {
	t.Parallel()
	lookback := schema.DefaultOTelTraces().MaxTraceDuration
	if lookback <= 0 {
		t.Fatalf("default maximum trace duration %s: the two windows cannot be told apart", lookback)
	}
	const searchLimit = 20
	plan := lowerSearchWindowed(t,
		`({ span.a = "1" } >> { span.b = "2" } | count() > 1) || ({ span.c = "3" } >> { span.d = "4" })`,
		searchLimit, winStart, winEnd)
	var cohort, rows [][2]int64
	collectStructuralWindows(plan, false, &cohort, &rows)
	if len(cohort) == 0 || len(rows) == 0 {
		t.Fatalf("%d cohort and %d row closures; the query must exercise both", len(cohort), len(rows))
	}
	for _, w := range cohort {
		if want := [2]int64{winStart.Add(-lookback).UnixNano(), winEnd.Add(lookback).UnixNano()}; w != want {
			t.Errorf("cohort closure window %v, want %v", w, want)
		}
	}
	for _, w := range rows {
		if want := [2]int64{winStart.UnixNano(), winEnd.UnixNano()}; w != want {
			t.Errorf("row closure window %v, want %v", w, want)
		}
	}
}
