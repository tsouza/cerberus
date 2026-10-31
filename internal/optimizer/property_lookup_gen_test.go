//go:build chdb

// TraceLookupFilter generation for TestPropertyOptimizerSemanticEquivalence.
//
// The TraceQL lowering builds a TraceLookupFilter for a trace-scoped
// intrinsic compared with a per-span operand (`{ rootName = name }`): the
// spans LEFT JOINed to a one-row-per-trace lookup, filtered by a row
// predicate over both. ProjectionPushdown narrows the span input of a
// Project over it, so the generated shape is that Project directly over the
// node, whose span input is a Scan or a Filter(Scan) — the adjacency
// applyProjectTraceLookupFilter matches. A narrowing that drops a column the
// joined predicate or the join key reads fails the round-trip with
// UNKNOWN_IDENTIFIER; one that keeps a lookup column on the span Scan does
// too, because the spans table has no such column.
package optimizer_test

import (
	"math/rand"

	"github.com/tsouza/cerberus/internal/chplan"
)

// propertyRootNameColumn is the value column of the generated root-name
// lookup, named as the TraceQL lowering names it.
const propertyRootNameColumn = "_cerb_root_name"

// generateTraceLookupFilter draws Project(TraceLookupFilter(<span input>))
// over the spans seed, comparing each span's name with its trace's root-span
// name. Trace t3's orphan chain keeps the rootless side of the join live: o2
// and o3 have a root (o1), but a span whose trace had none would read the
// LEFT JOIN default, so the predicate defaults the lookup value the way the
// lowering does.
func generateTraceLookupFilter(rng *rand.Rand) chplan.Node {
	spans := &chplan.Scan{
		Table: propertySpansTable,
		Roles: []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}},
	}
	var input chplan.Node = spans
	if rng.Intn(2) == 0 {
		input = &chplan.Filter{Input: spans, Predicate: spanKindPredicate(rng)}
	}
	rootName := &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: propertySpansTable},
			Predicate: &chplan.Binary{
				Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ParentSpanId"}, Right: &chplan.LitString{V: ""},
			},
		},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{chplan.TraceLookupKeyColumn},
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnArgMin,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}, &chplan.ColumnRef{Name: "Timestamp"}},
			Alias: propertyRootNameColumn,
		}},
	}
	op := chplan.OpEq
	if rng.Intn(2) == 0 {
		op = chplan.OpNe
	}
	pred := chplan.Expr(&chplan.Binary{
		Op: op,
		Left: &chplan.FuncCall{Fn: chplan.FnIfNull, Args: []chplan.Expr{
			&chplan.ColumnRef{Name: propertyRootNameColumn}, &chplan.LitString{V: ""},
		}},
		Right: &chplan.ColumnRef{Name: "SpanName"},
	})
	if rng.Intn(2) == 0 {
		// A span column only the joined predicate reads: the narrowed span
		// Scan must still carry it.
		pred = &chplan.Binary{Op: chplan.OpOr, Left: pred, Right: spanServicePredicate(rng)}
	}
	return &chplan.Project{
		Input: &chplan.TraceLookupFilter{Input: input, Lookups: []chplan.Node{rootName}, Predicate: pred},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "SpanId"}},
			{Expr: &chplan.ColumnRef{Name: "SpanName"}},
		},
	}
}
