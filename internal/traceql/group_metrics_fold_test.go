package traceql

import (
	"context"
	"slices"
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
// Timestamp-stripping node that, fed into a metrics rate grid, produces
// ClickHouse code 47 SQL.
func hasGroupKeyAggregate(n chplan.Node) bool {
	if n == nil {
		return false
	}
	if a, ok := n.(*chplan.Aggregate); ok {
		if slices.Contains(a.GroupByAliases, groupKeyAlias) {
			return true
		}
	}
	return slices.ContainsFunc(n.Children(), hasGroupKeyAggregate)
}

// TestDropGroupByBeforeMetrics pins that the by() stages right before a
// metrics function are dropped: reference Tempo's metrics evaluator observes
// every span of every spanset the pipeline returns and labels its series only
// from the metrics function's own by-clause, so `{ } | by(name) | rate()`
// answers the one unlabelled series `{ } | rate()` answers. Each pair must
// lower to the same plan, with no GroupKey aggregate left in it.
func TestDropGroupByBeforeMetrics(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	pairs := [][2]string{
		{`{} | by(name) | rate()`, `{} | rate()`},
		{`by(name) | rate()`, `{} | rate()`},
		{`{ resource.service.name = "a" } | by(resource.service.name) | count_over_time()`, `{ resource.service.name = "a" } | count_over_time()`},
		{`{ nestedSetParent < 0 } | by(nestedSetParent) | rate()`, `{ nestedSetParent < 0 } | rate()`},
		{`{} | by(name) | avg_over_time(duration)`, `{} | avg_over_time(duration)`},
		{`{} | by(name) | count_over_time() by (kind)`, `{} | count_over_time() by (kind)`},
		{`{} | by(name) | by(kind) | rate()`, `{} | rate()`},
		{`{} | by(span.a + 1) | rate()`, `{} | rate()`},
		{`{} | by(name) | compare({ status = error })`, `{} | compare({ status = error })`},
	}
	for _, p := range pairs {
		got := lowerPlain(t, p[0], s)
		want := lowerPlain(t, p[1], s)
		if !got.Equal(want) {
			t.Errorf("%q did not lower equivalently to %q", p[0], p[1])
		}
		if hasGroupKeyAggregate(got) {
			t.Errorf("%q still lowers to a Timestamp-stripping GroupKey aggregate", p[0])
		}
	}
}

// TestDropGroupByBeforeMetricsKeepsGroupByUnderAnotherStage pins the other
// edge of the drop: a by() stage with another stage between it and the
// metrics function decides which spans that stage passes, so it stays.
func TestDropGroupByBeforeMetricsKeepsGroupByUnderAnotherStage(t *testing.T) {
	t.Parallel()
	expr, err := tempoql.Parse(`{} | by(name) | count() > 1 | rate()`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := dropGroupByBeforeMetrics(expr.Pipeline)
	if len(got.Elements) != len(expr.Pipeline.Elements) {
		t.Fatalf("dropGroupByBeforeMetrics kept %d of %d elements, want all of them", len(got.Elements), len(expr.Pipeline.Elements))
	}
}

// TestDropGroupByBeforeMetricsAllGroupOpsNoUnderflow pins the `keep > 0`
// loop bound in dropGroupByBeforeMetrics. A pipeline lowerRoot hands it
// always starts with a spanset expression, which breaks the loop before keep
// can reach 0. Feeding a pipeline whose every element IS a group op drives
// keep all the way down to 0, so the bound is exercised directly: `keep > 0`
// must stop the loop at 0, leaving an empty pipeline. The mutant `keep >= 0`
// would re-enter the body at keep=0 and index els[-1], panicking.
func TestDropGroupByBeforeMetricsAllGroupOpsNoUnderflow(t *testing.T) {
	t.Parallel()

	expr, err := tempoql.Parse(`{} | by(name) | rate()`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(expr.Pipeline.Elements) < 2 {
		t.Fatalf("expected [spanset filter, group op], got %d elements", len(expr.Pipeline.Elements))
	}
	groupOp := expr.Pipeline.Elements[1]
	if _, ok := asGroupOperation(groupOp); !ok {
		t.Fatalf("Elements[1] = %T, want a GroupOperation", groupOp)
	}

	// Two group ops and nothing else: keep decrements 2 -> 1 -> 0.
	pipe := tempoql.Pipeline{Elements: []tempoql.PipelineElement{groupOp, groupOp}}
	if got := dropGroupByBeforeMetrics(pipe); len(got.Elements) != 0 {
		t.Fatalf("every group op must be dropped (keep -> 0), got %d elements remaining", len(got.Elements))
	}
}

// TestStandaloneGroupByWithoutMetricsUnchanged verifies the drop is scoped to
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
