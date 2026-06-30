package traceql

import (
	"context"
	"testing"

	tempoql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerPlain parses + lowers a query with a bare context (no search limit /
// window), the metrics-pipeline path.
func lowerPlain(t *testing.T, query string, s schema.Traces) chplan.Node {
	t.Helper()
	expr, err := tempoql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

// hasGroupKeyAggregate reports whether the plan contains the standalone
// spanset-grouping Aggregate (`| by(X)` lowered on its own) — the
// Timestamp-stripping node that, fed into a metrics rate grid, produces the
// Bug 1 code-47 SQL.
func hasGroupKeyAggregate(n chplan.Node) bool {
	if n == nil {
		return false
	}
	if a, ok := n.(*chplan.Aggregate); ok {
		for _, alias := range a.GroupByAliases {
			if alias == groupKeyAlias {
				return true
			}
		}
	}
	for _, c := range n.Children() {
		if hasGroupKeyAggregate(c) {
			return true
		}
	}
	return false
}

// TestFoldStandaloneGroupByIntoMetrics pins Bug 1: a standalone `| by(X)`
// grouping stage that immediately precedes a metrics aggregate folds into the
// aggregate's by-clause, so `{...} | by(X) | rate()` lowers IDENTICALLY to the
// already-valid `{...} | rate() by (X)`. The pre-fix lowering produced a
// Timestamp-stripping GROUP-BY Aggregate feeding the rate grid (ClickHouse
// code 47, a 502); the folded plan must instead match the inline-by form and
// carry no GroupKey aggregate.
func TestFoldStandaloneGroupByIntoMetrics(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	pairs := [][2]string{
		{`{} | by(name) | rate()`, `{} | rate() by (name)`},
		{`{ resource.service.name = "a" } | by(resource.service.name) | count_over_time()`, `{ resource.service.name = "a" } | count_over_time() by (resource.service.name)`},
		{`{ nestedSetParent < 0 } | by(nestedSetParent) | rate()`, `{ nestedSetParent < 0 } | rate() by (nestedSetParent)`},
		{`{} | by(name) | avg_over_time(duration)`, `{} | avg_over_time(duration) by (name)`},
	}
	for _, p := range pairs {
		got := lowerPlain(t, p[0], s)
		want := lowerPlain(t, p[1], s)
		if !got.Equal(want) {
			t.Errorf("%q did not lower equivalently to %q", p[0], p[1])
		}
		if hasGroupKeyAggregate(got) {
			t.Errorf("%q still lowers to a Timestamp-stripping GroupKey aggregate (Bug 1 not fixed)", p[0])
		}
	}
}

// TestStandaloneGroupByWithoutMetricsUnchanged verifies the fold is scoped to
// the metrics path: a standalone `| by(X)` with NO following metrics aggregate
// (a spanset search) still lowers to the GroupKey aggregate, unchanged.
func TestStandaloneGroupByWithoutMetricsUnchanged(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	plan := lowerPlain(t, `{ resource.service.name = "a" } | by(name)`, s)
	if !hasGroupKeyAggregate(plan) {
		t.Fatalf("standalone by() without a metrics aggregate must keep its GroupKey aggregate")
	}
}
