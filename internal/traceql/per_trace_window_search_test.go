package traceql

import (
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// leafWindow is the request-window bound one leaf Filter(Scan) of a lowered
// search plan carries — the nanosecond instants of its Timestamp bounds, in
// the order they render — and whether the leaf reads for a per-trace subquery
// (a TraceLookupFilter lookup or a trace-cohort membership subquery) rather
// than for the span rows the search matches.
type leafWindow struct {
	instants []int64
	perTrace bool
}

// collectLeafWindows records a leafWindow for every Filter(Scan) leaf of n,
// descending the trace-cohort subqueries behind Expr slots and the lookup
// relations of a TraceLookupFilter, which read for a per-trace subquery.
func collectLeafWindows(n chplan.Node, perTrace bool, tsCol string, out *[]leafWindow) {
	cohorts := func(e chplan.Expr) {
		for _, in := range chplan.CohortSubqueries(e) {
			collectLeafWindows(in.Subquery, true, tsCol, out)
		}
	}
	switch v := n.(type) {
	case nil:
		return
	case *chplan.Filter:
		cohorts(v.Predicate)
		if _, ok := v.Input.(*chplan.Scan); ok {
			*out = append(*out, leafWindow{instants: windowInstants(v.Predicate, tsCol), perTrace: perTrace})
			return
		}
		collectLeafWindows(v.Input, perTrace, tsCol, out)
		return
	case *chplan.TraceLookupFilter:
		cohorts(v.Predicate)
		for _, lookup := range v.Lookups {
			collectLeafWindows(lookup, true, tsCol, out)
		}
		collectLeafWindows(v.Input, perTrace, tsCol, out)
		return
	}
	for _, c := range n.Children() {
		collectLeafWindows(c, perTrace, tsCol, out)
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
// plain top-N shape and nested under a spanset operation; and every span row
// the search matches stays inside [start, end].
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
			var perTraceLeaves, rowLeaves int
			for i, leaf := range leaves {
				want := rows
				if leaf.perTrace {
					want = perTrace
					perTraceLeaves++
				} else {
					rowLeaves++
				}
				if !slices.Equal(leaf.instants, want) {
					t.Errorf("leaf %d (per-trace %v) window instants %v, want %v", i, leaf.perTrace, leaf.instants, want)
				}
			}
			if perTraceLeaves == 0 || rowLeaves == 0 {
				t.Fatalf("%d per-trace and %d row leaves; the query must exercise both", perTraceLeaves, rowLeaves)
			}
		})
	}
}

// TestSearchPerTraceWindowZeroLookback pins the setting's zero: with no
// maximum trace duration, a per-trace subquery reads the request window
// itself.
func TestSearchPerTraceWindowZeroLookback(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = 0
	start, end := time.Unix(1782571392, 0).UTC(), time.Unix(1782573192, 0).UTC()
	if got, want := perTraceWindow(start, end, s), andWindow(nil, start, end, s.TimestampColumn); !got.Equal(want) {
		t.Fatalf("perTraceWindow with no lookback = %#v, want the request window %#v", got, want)
	}
}
