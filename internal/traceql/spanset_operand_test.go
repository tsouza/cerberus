package traceql_test

import (
	"context"
	"testing"
	"time"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// lowerOperandQuery parses + lowers q with no request window, returning
// the plan or failing the test.
func lowerOperandQuery(t *testing.T, ctx context.Context, q string, s schema.Traces) chplan.Node {
	t.Helper()
	expr, err := tempo.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%s): %v", q, err)
	}
	plan, err := traceql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("Lower(%s): %v", q, err)
	}
	return plan
}

// semiJoinCohort asserts n is the span-granular semi-join a spanset
// aggregate operand lowers to — a Filter over span rows whose predicate
// is `<TraceIDColumn> IN (<cohort>)` — and returns the cohort subquery.
func semiJoinCohort(t *testing.T, n chplan.Node, s schema.Traces) chplan.Node {
	t.Helper()
	f, ok := n.(*chplan.Filter)
	if !ok {
		t.Fatalf("arm is %T, want *chplan.Filter (span-granular semi-join)", n)
	}
	in, ok := f.Predicate.(*chplan.InSubquery)
	if !ok {
		t.Fatalf("arm predicate is %T, want *chplan.InSubquery", f.Predicate)
	}
	col, ok := in.Left.(*chplan.ColumnRef)
	if !ok || col.Name != s.TraceIDColumn {
		t.Fatalf("membership key = %#v, want ColumnRef(%q)", in.Left, s.TraceIDColumn)
	}
	// The row source is the aggregate's own input, not the aggregate: the
	// arm must expose SpanId, which a GROUP BY TraceId relation cannot.
	if _, isAgg := f.Input.(*chplan.Aggregate); isAgg {
		t.Fatalf("arm row source is still the trace-granular Aggregate")
	}
	if in.Subquery == nil {
		t.Fatal("cohort subquery is nil")
	}
	return in.Subquery
}

// TestSpansetAggregateOperandIsSpanGranular pins the mixed-granularity
// union fix: a parenthesised sub-pipeline ending in `| count() > N` is a
// spanset-operation OPERAND, and reference Tempo combines SPANS there
// (ast_execute.go: Aggregate.evaluate keeps every span and sets only
// Spanset.Scalar; OpSpansetUnion emits uniqueSpans). Lowering it to the
// per-trace GROUP BY shape gave the two `||` arms different column
// counts, which ClickHouse rejects with code 258.
func TestSpansetAggregateOperandIsSpanGranular(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	ctx := context.Background()

	t.Run("union_mixed_granularity", func(t *testing.T) {
		t.Parallel()
		plan := lowerOperandQuery(t, ctx,
			`({ span.region = "us" } | count() > 1) || ({ span.tier = "gold" })`, s)
		so, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("plan is %T, want *chplan.SetOperation", plan)
		}
		cohort := semiJoinCohort(t, so.Left, s)
		// The cohort keeps the aggregate + its HAVING and projects the
		// group key alone, so the membership test is a single column.
		p, ok := cohort.(*chplan.Project)
		if !ok {
			t.Fatalf("cohort is %T, want *chplan.Project", cohort)
		}
		if len(p.Projections) != 1 {
			t.Fatalf("cohort projects %d columns, want 1", len(p.Projections))
		}
		if _, ok := p.Input.(*chplan.Filter); !ok {
			t.Fatalf("cohort input is %T, want the scalar-filter *chplan.Filter", p.Input)
		}
		// The plain arm is untouched.
		if _, ok := so.Right.(*chplan.Filter); !ok {
			t.Fatalf("right arm is %T, want the plain selector *chplan.Filter", so.Right)
		}
	})

	t.Run("intersect_both_aggregate", func(t *testing.T) {
		t.Parallel()
		plan := lowerOperandQuery(t, ctx,
			`({ span.region = "us" } | count() > 0) && ({ span.tier = "gold" } | count() > 0)`, s)
		so, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("plan is %T, want *chplan.SetOperation", plan)
		}
		semiJoinCohort(t, so.Left, s)
		semiJoinCohort(t, so.Right, s)
	})

	t.Run("top_level_aggregate_stays_per_trace", func(t *testing.T) {
		t.Parallel()
		// Outside operand position the per-trace shape is correct — the
		// /api/search envelope is one summary per trace — so the rewrite
		// must not reach it.
		plan := lowerOperandQuery(t, ctx, `{ span.region = "us" } | count() > 1`, s)
		f, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("plan is %T, want *chplan.Filter", plan)
		}
		if _, ok := f.Input.(*chplan.Aggregate); !ok {
			t.Fatalf("plan input is %T, want *chplan.Aggregate", f.Input)
		}
	})
}

// TestSpansetAggregateOperandCohortIsWindowed pins that the request
// window reaches the cohort subquery's leaf scan. The cohort hangs off an
// Expr slot (chplan.InSubquery), which chplan.RewriteChildren cannot see,
// so without the explicit descent in pushLeafPredicate the aggregation
// would scan full retention behind an inert membership test.
func TestSpansetAggregateOperandCohortIsWindowed(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(time.Hour)
	ctx := traceql.WithSearchWindow(traceql.WithSearchTraceLimit(context.Background(), 20), start, end)

	plan := lowerOperandQuery(t, ctx,
		`({ span.region = "us" } | count() > 1) || ({ span.tier = "gold" })`, s)
	so, ok := plan.(*chplan.SetOperation)
	if !ok {
		t.Fatalf("plan is %T, want *chplan.SetOperation", plan)
	}
	cohort := semiJoinCohort(t, so.Left, s)
	if got := countWindowBounds(cohort, s.TimestampColumn, start, end); got != 2 {
		t.Fatalf("cohort carries %d window bounds, want 2 (start + end)", got)
	}
}

// countWindowBounds counts the `<tsCol> >= start` / `<tsCol> <= end`
// comparisons stampSearchWindow folds into a subtree's leaves.
func countWindowBounds(n chplan.Node, tsCol string, start, end time.Time) int {
	found := 0
	chplan.Walk(n, func(node chplan.Node) bool {
		f, ok := node.(*chplan.Filter)
		if !ok {
			return true
		}
		chplan.InspectExpr(f.Predicate, func(e chplan.Expr) bool {
			b, ok := e.(*chplan.Binary)
			if !ok {
				return true
			}
			col, ok := b.Left.(*chplan.ColumnRef)
			if !ok || col.Name != tsCol {
				return true
			}
			call, ok := b.Right.(*chplan.FuncCall)
			if !ok || len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*chplan.LitInt)
			if !ok {
				return true
			}
			if lit.V == start.UnixNano() || lit.V == end.UnixNano() {
				found++
			}
			return true
		})
		return true
	})
	return found
}
