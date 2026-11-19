package traceql

import (
	"context"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestGroupSearchGroupsOnlyTheLimitedTraces pins the statement a search
// ending in a bare by() over a plain-search source lowers to. The by()
// aggregate groups the spans of the `limit` newest traces only: its input is
// a SearchTraceLimit with that limit over the request window alone, as the
// coalesce aggregate's is. A key on a trace-scoped value joins it onto the
// spans through a predicate-free TraceLookupFilter, which keeps every span,
// so the traces are ranked over its plain input (Rank) and only the drain
// computes the lookups. The drained source's Scan carries every column the
// aggregate reads, its group key among them, including the materialized
// column a key lowered over the open span row reads in its map cell's place.
func TestGroupSearchGroupsOnlyTheLimitedTraces(t *testing.T) {
	t.Parallel()
	mapOnly := schema.DefaultOTelTraces()
	materialized := schema.DefaultOTelTraces()
	materialized.MaterializedSpanAttributeColumns = schema.DefaultMaterializedSpanAttributeColumns()
	materialized.MaterializedResourceAttributeColumns = schema.DefaultMaterializedResourceAttributeColumns()
	ctx := WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd)
	window := []int64{winStart.UnixNano(), winEnd.UnixNano()}
	for _, query := range []string{
		`{ } | by(kind)`,
		`{ } | by(name)`,
		`{ } | by(status)`,
		`{ } | by(resource.service.name)`,
		`{ } | by(span.http.method)`,
		`{ } | by(duration > 1s)`,
		`{ name = "b" } | by(kind)`,
		`{ rootServiceName = "frontend" } | by(kind)`,
		`{ rootName = name } | by(kind)`,
		`{ } | by(kind) | by(name)`,
		`{ } | by(rootName)`,
		`{ } | by(traceDuration)`,
		`{ } | by(event:name)`,
		`{ } | by(.foo)`,
		`{ } | by(span.rpc.method)`,
		`{ kind = client } | by(span.rpc.method)`,
		`{ } | coalesce() | by(span.rpc.method)`,
		`{ } | coalesce() | by(resource.k8s.namespace.name)`,
	} {
		for name, s := range map[string]schema.Traces{"map-only": mapOnly, "materialized": materialized} {
			t.Run(name+"/"+query, func(t *testing.T) {
				t.Parallel()
				assertGroupSearchLimited(t, ctx, query, s, window)
			})
		}
	}
}

// assertGroupSearchLimited lowers query under s and checks the by()
// aggregate drains a SearchTraceLimit whose source carries what it reads.
func assertGroupSearchLimited(t *testing.T, ctx context.Context, query string, s schema.Traces, window []int64) {
	t.Helper()
	agg, ok := lowerWithSchema(t, ctx, query, s).(*chplan.Aggregate)
	if !ok || !isGroupAggregate(agg, s) {
		t.Fatalf("plan root is %T, want the by() aggregate", agg)
	}
	limited, ok := agg.Input.(*chplan.SearchTraceLimit)
	if !ok {
		t.Fatalf("by() aggregate reads %T, want a SearchTraceLimit", agg.Input)
	}
	if limited.TraceLimit != 20 {
		t.Fatalf("SearchTraceLimit.TraceLimit = %d, want 20", limited.TraceLimit)
	}
	lookup, annotates := limited.Input.(*chplan.TraceLookupFilter)
	annotates = annotates && lookup.Predicate == nil
	if annotates != (limited.Rank != nil) {
		t.Fatalf("drained source %T (predicate-free lookup: %v), Rank set: %v; want a Rank exactly under a predicate-free lookup",
			limited.Input, annotates, limited.Rank != nil)
	}
	if limited.Rank != nil && !isPlainSearchShape(limited.Rank) {
		t.Fatalf("Rank is %T, want the lookup's plain-search input", limited.Rank)
	}
	var leaves []leafWindow
	collectLeafWindows(limited.Input, false, s.TimestampColumn, &leaves)
	if limited.Rank != nil {
		collectLeafWindows(limited.Rank, false, s.TimestampColumn, &leaves)
	}
	rows := 0
	for i, leaf := range leaves {
		if leaf.perTrace {
			continue
		}
		rows++
		if !slices.Equal(leaf.instants, window) {
			t.Errorf("span-row leaf %d reads %v, want the request window %v", i, leaf.instants, window)
		}
	}
	if rows == 0 {
		t.Fatal("no span-row leaf under the SearchTraceLimit")
	}
	scanColumns := map[string]bool{}
	chplan.Walk(limited.Input, func(n chplan.Node) bool {
		if scan, ok := n.(*chplan.Scan); ok {
			for _, c := range scan.Columns {
				scanColumns[c] = true
			}
			return false
		}
		_, lookup := n.(*chplan.TraceLookupFilter)
		return !lookup || n == limited.Input
	})
	chplan.InspectNodeExprs(agg, func(e chplan.Expr) {
		chplan.InspectExpr(e, func(x chplan.Expr) bool {
			read := ""
			switch v := x.(type) {
			case *chplan.ColumnRef:
				if v.Qualifier == "" && !isTraceScopedLookupColumn(v.Name) {
					read = v.Name
				}
			case *chplan.FieldAccess:
				read = v.MaterializedColumn
			}
			if read != "" && !scanColumns[read] {
				t.Errorf("the by() aggregate reads %s, which the limited Scan does not carry (%v)", read, scanColumns)
			}
			return true
		})
	})
}

// TestGroupSearchLimitLeavesOtherShapesUnbounded pins the shapes the by()
// bound must not reach: a by() followed by a scalar filter answers the groups
// the filter keeps, which need not be the newest traces' ones, and nothing is
// bounded without a search limit.
func TestGroupSearchLimitLeavesOtherShapesUnbounded(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	limited := WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd)
	unlimited := WithSearchWindow(context.Background(), winStart, winEnd)
	for _, tc := range []struct {
		ctx   context.Context
		query string
	}{
		{limited, `{ } | by(kind) | count() > 1`},
		{limited, `{ } | by(kind) | avg(duration) > 1s`},
		{unlimited, `{ } | by(kind)`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			if n := countSearchTraceLimits(lowerWithSchema(t, tc.ctx, tc.query, s)); n != 0 {
				t.Errorf("plan carries %d SearchTraceLimit nodes, want none", n)
			}
		})
	}
}
