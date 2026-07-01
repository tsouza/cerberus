package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerMultiHopChain confirms that a left-associative chain of `>`
// operators (`a > b > c`) lowers to a nested StructuralJoin whose Left
// is itself a StructuralJoin. The shape falls out of lowerSpansetExpr
// recursing into nested SpansetOperation nodes — this test pins it
// against accidental refactors.
func TestLowerMultiHopChain(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(
		`{ resource.service.name = "a" } > { resource.service.name = "b" } > { resource.service.name = "c" }`,
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	outer, ok := plan.(*chplan.StructuralJoin)
	if !ok {
		t.Fatalf("expected outer *chplan.StructuralJoin, got %T", plan)
	}
	if outer.Op != chplan.StructuralChild {
		t.Errorf("outer Op = %q, want %q", outer.Op, chplan.StructuralChild)
	}

	inner, ok := outer.Left.(*chplan.StructuralJoin)
	if !ok {
		t.Fatalf("expected outer.Left to be *chplan.StructuralJoin, got %T", outer.Left)
	}
	if inner.Op != chplan.StructuralChild {
		t.Errorf("inner Op = %q, want %q", inner.Op, chplan.StructuralChild)
	}
}

// TestLowerRecursiveDescendant pins that `a >> b` lowers to a
// StructuralJoin{Op: StructuralDescendant} with MaxDepth = 0 (the
// unbounded default).
func TestLowerRecursiveDescendant(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(
		`{ resource.service.name = "root" } >> { resource.service.name = "leaf" }`,
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	sj, ok := plan.(*chplan.StructuralJoin)
	if !ok {
		t.Fatalf("expected *chplan.StructuralJoin, got %T", plan)
	}
	if sj.Op != chplan.StructuralDescendant {
		t.Errorf("Op = %q, want %q", sj.Op, chplan.StructuralDescendant)
	}
	if sj.MaxDepth != 0 {
		t.Errorf("MaxDepth = %d, want 0 (unbounded)", sj.MaxDepth)
	}
}

// TestLowerRecursiveAncestor pins that `a << b` lowers to a
// StructuralJoin{Op: StructuralAncestor}.
func TestLowerRecursiveAncestor(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(
		`{ resource.service.name = "leaf" } << { resource.service.name = "root" }`,
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	sj, ok := plan.(*chplan.StructuralJoin)
	if !ok {
		t.Fatalf("expected *chplan.StructuralJoin, got %T", plan)
	}
	if sj.Op != chplan.StructuralAncestor {
		t.Errorf("Op = %q, want %q", sj.Op, chplan.StructuralAncestor)
	}
}

// TestEmitRecursiveDescendant_EndToEnd exercises the full parse →
// lower → emit chain for a `>>` query and confirms the emitted SQL
// surfaces the CH `WITH RECURSIVE` CTE header plus both end-of-chain
// service.name filter args.
func TestEmitRecursiveDescendant_EndToEnd(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(
		`{ resource.service.name = "root" } >> { resource.service.name = "leaf" }`,
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	for _, want := range []string{
		"WITH RECURSIVE _struct_closure_",
		"_seed",
		"UNION ALL",
		// Closure CTE name carries a per-emit sequence suffix; the
		// recursive arm self-joins it aliased `c`.
		"_struct_closure_1 AS c",
		// The recursive arm is bounded by the default safety cap.
		"c._depth < 128",
		// Both sides are cheap selective leaves, so the anchor seed is
		// candidate-prefiltered to traces present on the R side too — the
		// L-intersect-R set — via a DISTINCT-TraceId subquery aliased _cand.
		"IN (SELECT DISTINCT `TraceId` FROM",
		"AS _cand",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q\n  got: %s", want, sql)
		}
	}
	// The recursive step no longer re-embeds a seed-trace-id IN subquery — the
	// step JOIN ON `t.TraceId = c.TraceId` already confines `t` to the seed's
	// traces — so `_seed_ids` must be absent.
	if strings.Contains(sql, "_seed_ids") {
		t.Errorf("emitted SQL must not carry the dropped seed-trace-id pushdown\n  got: %s", sql)
	}

	// 6 string args: the L subquery's two ("service.name" / "root") at the
	// _seed position, then R's two ("service.name" / "leaf") in the candidate-
	// prefilter subquery, then R's two again at the final INNER JOIN.
	if got, expectedLen := len(args), 6; got != expectedLen {
		t.Errorf("args length = %d, want %d (args=%v)", got, expectedLen, args)
	}
}
