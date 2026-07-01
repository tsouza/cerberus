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

// TestFoldTrailingGroupByAllGroupOpsNoUnderflow pins the `keep > 0` loop
// bound in foldTrailingGroupByIntoMetrics. A real parsed pipeline always
// starts with a non-group spanset filter, which breaks the fold loop
// before keep can reach 0. Feeding a degenerate pipeline whose every
// element IS a foldable group op drives keep all the way down to 0, so
// the bound is exercised directly: `keep > 0` must stop the loop at 0,
// leaving an empty pipeline with both group keys folded into the metrics
// by-clause. The mutant `keep >= 0` would re-enter the body at keep=0 and
// index els[-1], panicking.
func TestFoldTrailingGroupByAllGroupOpsNoUnderflow(t *testing.T) {
	t.Parallel()

	// Parse a valid `{} | by(name) | rate()` to obtain a real group-op
	// element and a metrics aggregate that accepts a leading by-clause.
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
	mp := expr.MetricsPipeline
	if mp == nil {
		t.Fatal("MetricsPipeline = nil, want the rate() aggregate")
	}

	// Two foldable group ops and nothing else: keep decrements 2 -> 1 -> 0.
	pipe := tempoql.Pipeline{Elements: []tempoql.PipelineElement{groupOp, groupOp}}
	gotPipe, gotMerged := foldTrailingGroupByIntoMetrics(pipe, mp)

	if len(gotPipe.Elements) != 0 {
		t.Fatalf("all group ops must fold away (keep -> 0), got %d elements remaining", len(gotPipe.Elements))
	}
	merged, ok := gotMerged.(*tempoql.MetricsAggregate)
	if !ok {
		t.Fatalf("merged first stage = %T, want *MetricsAggregate", gotMerged)
	}
	if got := len(merged.GroupBy()); got != 2 {
		t.Fatalf("merged by-clause = %d attrs, want 2 (both group ops folded, keep stopped exactly at 0)", got)
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
