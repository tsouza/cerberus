package traceql_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// selectedAttributeReads lowers query and returns, for every Project in the
// plan whose input is of type inputType (a %T spelling such as
// "*chplan.Filter"), the MaterializedColumn of each FieldAccess its
// projections read ("" for a map read), in projection order.
func selectedAttributeReads(t *testing.T, query string, s schema.Traces) map[string][]string {
	t.Helper()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	out := map[string][]string{}
	chplan.Walk(plan, func(n chplan.Node) bool {
		p, ok := n.(*chplan.Project)
		if !ok {
			return true
		}
		key := typeName(p.Input)
		for _, projection := range p.Projections {
			chplan.InspectExpr(projection.Expr, func(e chplan.Expr) bool {
				if f, ok := e.(*chplan.FieldAccess); ok {
					out[key] = append(out[key], f.MaterializedColumn)
				}
				return true
			})
		}
		return true
	})
	return out
}

func typeName(n chplan.Node) string {
	switch n.(type) {
	case *chplan.Filter:
		return "Filter"
	case *chplan.StructuralJoin:
		return "StructuralJoin"
	case *chplan.SetOperation:
		return "SetOperation"
	}
	return "other"
}

// TestLowerSelect_MaterializedAttributeReads pins which select() reads go
// through a materialized attribute column. The column stands in for the
// map cell only where it is value-identical to the cell (String kind) and
// the select() input's rows carry it (the span table under a Filter). A
// numeric-kind column, the structural-join envelope, and a set operation's
// aligned arms read the attribute map instead.
func TestLowerSelect_MaterializedAttributeReads(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = schema.DefaultMaterializedSpanAttributeColumns()
	s.MaterializedResourceAttributeColumns = schema.DefaultMaterializedResourceAttributeColumns()
	const (
		rpcMethod = "__cerberus_materialized_rpc.method"
		namespace = "__cerberus_materialized_k8s.namespace.name"
	)

	cases := []struct {
		query string
		want  map[string][]string
	}{
		{`{ kind = server } | select(span.rpc.method)`, map[string][]string{"Filter": {rpcMethod}}},
		{`{ kind = server } | select(resource.k8s.namespace.name)`, map[string][]string{"Filter": {namespace}}},
		{`{ kind = server } | select(span.http.status_code)`, map[string][]string{"Filter": {""}}},
		{`{ kind = server } > { kind = client } | select(span.rpc.method)`, map[string][]string{"StructuralJoin": {""}}},
		{`{ kind = server } >> { kind = client } | select(resource.k8s.namespace.name)`, map[string][]string{"StructuralJoin": {""}}},
		{`{ kind = server } && { kind = client } | select(span.rpc.method)`, map[string][]string{"SetOperation": {""}}},
		// A select() operand of a set operation keeps the column over its own
		// Filter; the copy projected over the structural arm reads the map.
		{
			`({ kind = server } | select(span.rpc.method)) || ({ kind = client } > { kind = client })`,
			map[string][]string{"Filter": {rpcMethod}, "StructuralJoin": {""}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			if got := selectedAttributeReads(t, tc.query, s); !maps.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Errorf("select() reads = %q, want %q", got, tc.want)
			}
		})
	}
}
