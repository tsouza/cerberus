package traceql

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerRootAggregate lowers query without a search limit and returns its root
// Aggregate, the exact shape the bounded-search recognisers match.
func lowerRootAggregate(t *testing.T, query string, s schema.Traces) *chplan.Aggregate {
	t.Helper()
	agg, ok := lowerWithSchema(t, context.Background(), query, s).(*chplan.Aggregate)
	if !ok {
		t.Fatalf("%q lowers to a %T root, want an Aggregate", query, agg)
	}
	return agg
}

// aggregateNearMiss is a copy of an exact-match aggregate with one recogniser
// condition broken.
type aggregateNearMiss struct {
	name  string
	apply func(a *chplan.Aggregate)
}

// assertRecogniserNearMisses pins that recognise accepts exact and rejects
// each near miss, every near miss built on its own deep copy of exact.
func assertRecogniserNearMisses(t *testing.T, recognise func(*chplan.Aggregate, schema.Traces) bool,
	exact *chplan.Aggregate, s schema.Traces, misses []aggregateNearMiss,
) {
	t.Helper()
	if !recognise(exact, s) {
		t.Fatalf("exact-match aggregate %#v rejected", exact)
	}
	for _, m := range misses {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			miss, ok := chplan.CloneNode(exact).(*chplan.Aggregate)
			if !ok {
				t.Fatalf("CloneNode returned %T", miss)
			}
			m.apply(miss)
			if recognise(miss, s) {
				t.Errorf("near miss %#v accepted, want rejected", miss)
			}
		})
	}
}

// TestIsCoalesceAggregate_NearMisses gives isCoalesceAggregate the aggregate
// `{ } | coalesce()` lowers to, which it accepts, and one copy per condition
// with only that condition broken, which it rejects: a group key or alias
// added or dropped, and, at each of the two key positions, a key that is no
// column, a qualified key, a key over another column, and another alias.
func TestIsCoalesceAggregate_NearMisses(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	exact := lowerRootAggregate(t, `{ } | coalesce()`, s)
	misses := []aggregateNearMiss{
		{"extra group key", func(a *chplan.Aggregate) {
			a.GroupBy = append(a.GroupBy, &chplan.ColumnRef{Name: s.SpanNameColumn})
		}},
		{"extra alias", func(a *chplan.Aggregate) {
			a.GroupByAliases = append(a.GroupByAliases, s.SpanNameColumn)
		}},
		{"missing group key", func(a *chplan.Aggregate) { a.GroupBy = a.GroupBy[:1] }},
		{"missing alias", func(a *chplan.Aggregate) { a.GroupByAliases = a.GroupByAliases[:1] }},
	}
	for i, pos := range []string{"trace key", "span key"} {
		misses = append(
			misses,
			aggregateNearMiss{pos + " not a column", func(a *chplan.Aggregate) {
				a.GroupBy[i] = &chplan.LitString{V: a.GroupByAliases[i]}
			}},
			aggregateNearMiss{pos + " qualified", func(a *chplan.Aggregate) {
				a.GroupBy[i] = &chplan.ColumnRef{Qualifier: "t", Name: a.GroupByAliases[i]}
			}},
			aggregateNearMiss{pos + " over another column", func(a *chplan.Aggregate) {
				a.GroupBy[i] = &chplan.ColumnRef{Name: s.ParentSpanIDColumn}
			}},
			aggregateNearMiss{pos + " under another alias", func(a *chplan.Aggregate) {
				a.GroupByAliases[i] = s.ParentSpanIDColumn
			}},
		)
	}
	assertRecogniserNearMisses(t, isCoalesceAggregate, exact, s, misses)
	if isGroupAggregate(exact, s) {
		t.Error("the coalesce aggregate is accepted as a by() aggregate")
	}
}

// TestIsGroupAggregate_NearMisses gives isGroupAggregate the aggregate
// `{ } | by(name)` lowers to, which it accepts, and one copy per condition
// with only that condition broken, which it rejects: a group key or alias
// added, no aggregate at all, a TraceId key that is no column, qualified or
// over another column, either alias renamed, and a first aggregate with
// another function, another alias, no or two arguments, or an argument that
// is no column, qualified or over another column.
func TestIsGroupAggregate_NearMisses(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	exact := lowerRootAggregate(t, `{ } | by(name)`, s)
	spanArg := func(a *chplan.Aggregate, e chplan.Expr) { a.AggFuncs[0].Args = []chplan.Expr{e} }
	assertRecogniserNearMisses(t, isGroupAggregate, exact, s, []aggregateNearMiss{
		{"extra group key", func(a *chplan.Aggregate) {
			a.GroupBy = append(a.GroupBy, &chplan.ColumnRef{Name: s.SpanNameColumn})
		}},
		{"extra alias", func(a *chplan.Aggregate) {
			a.GroupByAliases = append(a.GroupByAliases, s.SpanNameColumn)
		}},
		{"no aggregate", func(a *chplan.Aggregate) { a.AggFuncs = nil }},
		{"trace key not a column", func(a *chplan.Aggregate) {
			a.GroupBy[0] = &chplan.LitString{V: s.TraceIDColumn}
		}},
		{"trace key qualified", func(a *chplan.Aggregate) {
			a.GroupBy[0] = &chplan.ColumnRef{Qualifier: "t", Name: s.TraceIDColumn}
		}},
		{"trace key over another column", func(a *chplan.Aggregate) {
			a.GroupBy[0] = &chplan.ColumnRef{Name: s.SpanIDColumn}
		}},
		{"trace alias renamed", func(a *chplan.Aggregate) { a.GroupByAliases[0] = s.SpanIDColumn }},
		{"group key alias renamed", func(a *chplan.Aggregate) { a.GroupByAliases[1] = s.SpanNameColumn }},
		{"first aggregate another function", func(a *chplan.Aggregate) { a.AggFuncs[0].Fn = chplan.FnMin }},
		{"first aggregate another alias", func(a *chplan.Aggregate) { a.AggFuncs[0].Alias = s.ParentSpanIDColumn }},
		{"first aggregate no argument", func(a *chplan.Aggregate) { a.AggFuncs[0].Args = nil }},
		{"first aggregate two arguments", func(a *chplan.Aggregate) {
			a.AggFuncs[0].Args = append(a.AggFuncs[0].Args, &chplan.ColumnRef{Name: s.SpanIDColumn})
		}},
		{"span argument not a column", func(a *chplan.Aggregate) { spanArg(a, &chplan.LitString{V: s.SpanIDColumn}) }},
		{"span argument qualified", func(a *chplan.Aggregate) {
			spanArg(a, &chplan.ColumnRef{Qualifier: "t", Name: s.SpanIDColumn})
		}},
		{"span argument over another column", func(a *chplan.Aggregate) {
			spanArg(a, &chplan.ColumnRef{Name: s.ParentSpanIDColumn})
		}},
	})
	if isCoalesceAggregate(exact, s) {
		t.Error("the by() aggregate is accepted as a coalesce aggregate")
	}
}

// TestIsTraceScoped_Shapes pins which predicates isTraceScoped reports as
// depending on the trace alone: an unqualified TraceId membership, and an AND
// or OR of such predicates at any depth. A qualified membership, a
// membership over another column or over no column, any other binary
// operator over two memberships, and an AND / OR with a span comparison on
// either side are not.
func TestIsTraceScoped_Shapes(t *testing.T) {
	t.Parallel()
	member := func(left chplan.Expr) chplan.Expr {
		return &chplan.InSubquery{Left: left, Subquery: &chplan.Scan{Table: "otel_traces"}}
	}
	m := member(&chplan.ColumnRef{Name: "TraceId"})
	span := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: "a"}}
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}
	for _, tc := range []struct {
		name string
		e    chplan.Expr
		want bool
	}{
		{"membership", m, true},
		{"and of memberships", bin(chplan.OpAnd, m, m), true},
		{"or of memberships", bin(chplan.OpOr, m, m), true},
		{"or of an and of memberships", bin(chplan.OpOr, bin(chplan.OpAnd, m, m), m), true},
		{"and of an or of memberships", bin(chplan.OpAnd, m, bin(chplan.OpOr, m, m)), true},
		{"qualified membership", member(&chplan.ColumnRef{Qualifier: "t", Name: "TraceId"}), false},
		{"membership over another column", member(&chplan.ColumnRef{Name: "SpanId"}), false},
		{"membership over no column", member(&chplan.LitString{V: "TraceId"}), false},
		{"equality of memberships", bin(chplan.OpEq, m, m), false},
		{"span comparison", span, false},
		{"and with a span comparison", bin(chplan.OpAnd, m, span), false},
		{"or with a span comparison first", bin(chplan.OpOr, span, m), false},
		{"or of an and with a span comparison", bin(chplan.OpOr, bin(chplan.OpAnd, m, span), m), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTraceScoped(tc.e, "TraceId"); got != tc.want {
				t.Errorf("isTraceScoped(%#v) = %v, want %v", tc.e, got, tc.want)
			}
		})
	}
}

// TestDropTraceScopedConjuncts_MixedAndOr pins the drop over a span predicate
// whose conjuncts mix the shapes: an OR of an AND of memberships, a bare
// membership, a qualified membership and a span comparison. Only the span
// comparison and the qualified membership stay, in their order.
func TestDropTraceScopedConjuncts_MixedAndOr(t *testing.T) {
	t.Parallel()
	member := func(q string) chplan.Expr {
		return &chplan.InSubquery{Left: &chplan.ColumnRef{Qualifier: q, Name: "TraceId"}, Subquery: &chplan.Scan{Table: "otel_traces"}}
	}
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}
	span := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: "a"}}
	pred := conjoinAll([]chplan.Expr{
		bin(chplan.OpOr, bin(chplan.OpAnd, member(""), member("")), member("")),
		span,
		member(""),
		member("t"),
	})
	scan := &chplan.Scan{Table: "otel_traces"}
	got, ok := dropTraceScopedConjuncts(&chplan.Filter{Input: scan, Predicate: pred}, "TraceId").(*chplan.Filter)
	if !ok {
		t.Fatalf("dropTraceScopedConjuncts returned %T, want a Filter", got)
	}
	if want := conjoin(span, member("t")); !got.Predicate.Equal(want) {
		t.Errorf("kept predicate %#v, want %#v", got.Predicate, want)
	}
	if got.Input != chplan.Node(scan) {
		t.Errorf("filter input %T, want the original Scan", got.Input)
	}
}

// TestBoundPerTraceCohorts pins that a membership subquery in a lifted span
// predicate reads the request window widened by the maximum trace duration
// on each side, and that a request with no window leaves it unbounded.
func TestBoundPerTraceCohorts(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = 10 * time.Minute
	newPred := func() (chplan.Expr, *chplan.InSubquery, *chplan.Scan) {
		scan := &chplan.Scan{Table: "otel_traces"}
		in := &chplan.InSubquery{Left: &chplan.ColumnRef{Name: s.TraceIDColumn}, Subquery: scan}
		span := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: s.SpanNameColumn}, Right: &chplan.LitString{V: "a"}}
		return conjoin(span, in), in, scan
	}

	pred, in, scan := newPred()
	BoundPerTraceCohorts(pred, winStart, winEnd, s)
	f, ok := in.Subquery.(*chplan.Filter)
	if !ok || f.Input != chplan.Node(scan) {
		t.Fatalf("membership subquery is %#v, want a Filter over its Scan", in.Subquery)
	}
	want := conjoin(
		chplan.TimestampBound(chplan.OpGe, winStart.Add(-s.MaxTraceDuration), s.TimestampColumn),
		chplan.TimestampBound(chplan.OpLe, winEnd.Add(s.MaxTraceDuration), s.TimestampColumn),
	)
	if !f.Predicate.Equal(want) {
		t.Errorf("membership subquery reads %#v, want %#v", f.Predicate, want)
	}

	_, in, scan = newPred()
	BoundPerTraceCohorts(in, time.Time{}, time.Time{}, s)
	if in.Subquery != chplan.Node(scan) {
		t.Errorf("windowless request rewrote the membership subquery to %#v, want the bare Scan", in.Subquery)
	}
}

// TestSearchDrainOutside_Windows pins the predicate a drained span outside
// the request window satisfies: before its start or after its end, each side
// only when set, inside the drain window. A request with no window or a zero
// maximum trace duration drains nothing and gets nil.
func TestSearchDrainOutside_Windows(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = 10 * time.Minute
	ts := s.TimestampColumn
	before := chplan.TimestampBound(chplan.OpLt, winStart, ts)
	after := chplan.TimestampBound(chplan.OpGt, winEnd, ts)
	drainLo := chplan.TimestampBound(chplan.OpGe, winStart.Add(-s.MaxTraceDuration), ts)
	drainHi := chplan.TimestampBound(chplan.OpLe, winEnd.Add(s.MaxTraceDuration), ts)
	zero := s
	zero.MaxTraceDuration = 0
	for _, tc := range []struct {
		name       string
		start, end time.Time
		s          schema.Traces
		want       chplan.Expr
	}{
		{"both sides", winStart, winEnd, s, conjoin(conjoin(&chplan.Binary{Op: chplan.OpOr, Left: before, Right: after}, drainLo), drainHi)},
		{"start only", winStart, time.Time{}, s, conjoin(before, drainLo)},
		{"end only", time.Time{}, winEnd, s, conjoin(after, drainHi)},
		{"no window", time.Time{}, time.Time{}, s, nil},
		{"zero maximum trace duration", winStart, winEnd, zero, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SearchDrainOutside(WithSearchWindow(context.Background(), tc.start, tc.end), tc.s)
			if tc.want == nil {
				if got != nil {
					t.Errorf("SearchDrainOutside = %#v, want nil", got)
				}
				return
			}
			if got == nil || !got.Equal(tc.want) {
				t.Errorf("SearchDrainOutside = %#v, want %#v", got, tc.want)
			}
		})
	}
}
