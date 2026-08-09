//go:build agpl_oracle

package traceql_test

import (
	"testing"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/traceql"
)

// tree is the shape every structural case below runs on, mirroring the
// corpus's own recursive-descendant seed:
//
//	trace-a: root -> mid -> leaf -> leafer
//	trace-b: two disconnected roots, one of which is also named "leaf"
//
// The second trace is what makes the structural assertions meaningful: a
// lowering that forgot to key its join on TraceId would match trace-b's
// "leaf" root against trace-a's "root" ancestor.
func tree() []oracle.Span {
	svc := func(name string) map[string]string {
		return map[string]string{"service.name": name}
	}
	return []oracle.Span{
		{TraceID: "trace-a", SpanID: "span-1", ParentSpanID: "", ResourceAttrs: svc("root")},
		{TraceID: "trace-a", SpanID: "span-2", ParentSpanID: "span-1", ResourceAttrs: svc("mid")},
		{TraceID: "trace-a", SpanID: "span-3", ParentSpanID: "span-2", ResourceAttrs: svc("leaf")},
		{TraceID: "trace-a", SpanID: "span-4", ParentSpanID: "span-3", ResourceAttrs: svc("leafer")},
		{TraceID: "trace-b", SpanID: "span-5", ParentSpanID: "", ResourceAttrs: svc("leaf")},
		{TraceID: "trace-b", SpanID: "span-6", ParentSpanID: "", ResourceAttrs: svc("root")},
	}
}

func evaluate(t *testing.T, spans []oracle.Span, query string) []oracle.Result {
	t.Helper()
	got, err := oracle.Evaluate(t, spans, query)
	if err != nil {
		t.Fatalf("Evaluate(%q): %v", query, err)
	}
	return got
}

func requireResults(t *testing.T, got []oracle.Result, want ...oracle.Result) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d result(s) %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("result %d = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestDescendantCrossesGenerationsAndNotTraces is the case the fork exists
// for: `>>` is answered by upstream's own nested-set arithmetic over the
// numbering this package computes.
func TestDescendantCrossesGenerationsAndNotTraces(t *testing.T) {
	got := evaluate(t, tree(),
		`{ resource.service.name = "root" } >> { resource.service.name = "leaf" }`)
	requireResults(t, got, oracle.Result{TraceID: "trace-a", SpanID: "span-3"})
}

// TestChildIsOneGenerationOnly separates `>` from `>>`: the leaf is a
// descendant of the root but not its child, so this must return nothing
// while the descendant case above returns a span.
func TestChildIsOneGenerationOnly(t *testing.T) {
	got := evaluate(t, tree(),
		`{ resource.service.name = "root" } > { resource.service.name = "leaf" }`)
	requireResults(t, got)

	got = evaluate(t, tree(),
		`{ resource.service.name = "root" } > { resource.service.name = "mid" }`)
	requireResults(t, got, oracle.Result{TraceID: "trace-a", SpanID: "span-2"})
}

// TestSiblingNeedsASharedParent pins sibling identity, including the
// non-obvious case the root sentinel creates: upstream numbers a root's
// parent -1, and SiblingOf's rule is "same nestedSetParent, neither 0", so
// two ROOTS of one trace ARE siblings. trace-b is exactly that shape.
func TestSiblingNeedsASharedParent(t *testing.T) {
	got := evaluate(t, tree(),
		`{ resource.service.name = "root" } ~ { resource.service.name = "leaf" }`)
	requireResults(t, got, oracle.Result{TraceID: "trace-b", SpanID: "span-5"})

	forked := append(tree(), oracle.Span{
		TraceID: "trace-a", SpanID: "span-7", ParentSpanID: "span-1",
		ResourceAttrs: map[string]string{"service.name": "sibling"},
	})
	got = evaluate(t, forked,
		`{ resource.service.name = "mid" } ~ { resource.service.name = "sibling" }`)
	requireResults(t, got, oracle.Result{TraceID: "trace-a", SpanID: "span-7"})
}

// TestRootParentSentinelIsNegativeOne pins the sentinel itself through the
// query surface that reads it, so a numbering that used 0 for a root would
// fail here rather than only through the sibling case above.
func TestRootParentSentinelIsNegativeOne(t *testing.T) {
	got := evaluate(t, tree(), `{ nestedSetParent < 0 }`)
	requireResults(
		t, got,
		oracle.Result{TraceID: "trace-a", SpanID: "span-1"},
		oracle.Result{TraceID: "trace-b", SpanID: "span-5"},
		oracle.Result{TraceID: "trace-b", SpanID: "span-6"},
	)
}

// TestTraceScopedIntrinsics covers the per-trace facts upstream hangs off
// every span: the root's name and service, and the whole trace's duration
// (max end minus min start), not any single span's.
func TestTraceScopedIntrinsics(t *testing.T) {
	spans := []oracle.Span{
		{
			TraceID: "t", SpanID: "root", Name: "GET /",
			StartUnixNano: 1_000, DurationNanos: 2_000,
			ResourceAttrs: map[string]string{"service.name": "frontend"},
		},
		{
			TraceID: "t", SpanID: "child", ParentSpanID: "root", Name: "SELECT",
			StartUnixNano: 2_000, DurationNanos: 9_000,
			ResourceAttrs: map[string]string{"service.name": "db"},
		},
	}

	all := []oracle.Result{{TraceID: "t", SpanID: "child"}, {TraceID: "t", SpanID: "root"}}
	requireResults(t, evaluate(t, spans, `{ trace:rootName = "GET /" }`), all...)
	requireResults(t, evaluate(t, spans, `{ trace:rootService = "frontend" }`), all...)
	// min start 1000, max end 11000 -> 10000ns, which no single span has.
	requireResults(t, evaluate(t, spans, `{ trace:duration = 10000ns }`), all...)
	requireResults(t, evaluate(t, spans, `{ trace:duration = 9000ns }`))
}

// TestPlainAttributeFilterMatchesEveryTrace covers the non-structural
// baseline, and pins that trace grouping does not accidentally scope a
// plain filter.
func TestPlainAttributeFilterMatchesEveryTrace(t *testing.T) {
	got := evaluate(t, tree(), `{ resource.service.name = "leaf" }`)
	requireResults(
		t, got,
		oracle.Result{TraceID: "trace-a", SpanID: "span-3"},
		oracle.Result{TraceID: "trace-b", SpanID: "span-5"},
	)
}

// TestIntrinsicsDecodeFromTheirColumns covers the storage-encoding
// restatement this package makes: name, duration, status and kind all
// arrive as ClickHouse column text and must reach the engine as the right
// traceql.Static type.
func TestIntrinsicsDecodeFromTheirColumns(t *testing.T) {
	spans := []oracle.Span{
		{
			TraceID: "t", SpanID: "fast", Name: "GET /", Kind: "Client",
			StatusCode: "Ok", DurationNanos: 50_000_000,
		},
		{
			TraceID: "t", SpanID: "slow", Name: "POST /", Kind: "Server",
			StatusCode: "Error", DurationNanos: 150_000_000,
		},
	}

	requireResults(t, evaluate(t, spans, `{ duration > 100ms }`),
		oracle.Result{TraceID: "t", SpanID: "slow"})
	requireResults(t, evaluate(t, spans, `{ status = error }`),
		oracle.Result{TraceID: "t", SpanID: "slow"})
	requireResults(t, evaluate(t, spans, `{ kind = client }`),
		oracle.Result{TraceID: "t", SpanID: "fast"})
	requireResults(t, evaluate(t, spans, `{ name = "GET /" }`),
		oracle.Result{TraceID: "t", SpanID: "fast"})
}

// TestEmptyStatusColumnIsUnset pins the DEFAULT ” reading, which most of
// the corpus's seeds rely on.
func TestEmptyStatusColumnIsUnset(t *testing.T) {
	spans := []oracle.Span{{TraceID: "t", SpanID: "s"}}
	requireResults(t, evaluate(t, spans, `{ status = unset }`),
		oracle.Result{TraceID: "t", SpanID: "s"})
	requireResults(t, evaluate(t, spans, `{ status = error }`))
}

// TestDanglingParentIsLeftUnnumbered pins the second rule copied from
// upstream's writer: a span whose ParentSpanID names nothing in the trace
// is NOT promoted to a root. It keeps 0/0/0, so it still matches plain
// filters but can never satisfy a structural operator — and it is not a
// sibling of the real root either, which promoting it would have made it.
func TestDanglingParentIsLeftUnnumbered(t *testing.T) {
	spans := []oracle.Span{
		{TraceID: "t", SpanID: "a", ParentSpanID: "", ResourceAttrs: map[string]string{"service.name": "root"}},
		{TraceID: "t", SpanID: "b", ParentSpanID: "missing", ResourceAttrs: map[string]string{"service.name": "orphan"}},
	}
	requireResults(t, evaluate(t, spans,
		`{ resource.service.name = "root" } >> { resource.service.name = "orphan" }`))
	requireResults(t, evaluate(t, spans,
		`{ resource.service.name = "root" } ~ { resource.service.name = "orphan" }`))
	requireResults(t, evaluate(t, spans, `{ resource.service.name = "orphan" }`),
		oracle.Result{TraceID: "t", SpanID: "b"})
	requireResults(t, evaluate(t, spans, `{ nestedSetLeft = 0 }`),
		oracle.Result{TraceID: "t", SpanID: "b"})
}

// TestParentCycleLeavesEverySpanUnnumbered pins that a cycle terminates
// and matches upstream's own reading of it. Upstream descends only from
// real roots, so a cycle is simply unreachable and every span in it keeps
// 0/0/0 — present in the spanset, never a structural match.
func TestParentCycleLeavesEverySpanUnnumbered(t *testing.T) {
	spans := []oracle.Span{
		{TraceID: "t", SpanID: "a", ParentSpanID: "b"},
		{TraceID: "t", SpanID: "b", ParentSpanID: "a"},
	}
	requireResults(
		t, evaluate(t, spans, `{ nestedSetLeft = 0 }`),
		oracle.Result{TraceID: "t", SpanID: "a"},
		oracle.Result{TraceID: "t", SpanID: "b"},
	)
	requireResults(t, evaluate(t, spans, `{ true } > { true }`))
}

// TestChainBelowAnAbsentParentIsUnnumbered is the same rule one level
// down, and the shape the corpus actually seeds: a span hangs off a
// ParentSpanId no row carries, and a third span hangs off THAT. Neither is
// reachable from a root, so neither is numbered — promoting the first to a
// root would have invented a subtree upstream never sees.
func TestChainBelowAnAbsentParentIsUnnumbered(t *testing.T) {
	spans := []oracle.Span{
		{TraceID: "t", SpanID: "orphan", ParentSpanID: "absent"},
		{TraceID: "t", SpanID: "below", ParentSpanID: "orphan"},
	}
	requireResults(t, evaluate(t, spans, `{ true } >> { true }`))
	requireResults(
		t, evaluate(t, spans, `{ nestedSetLeft = 0 }`),
		oracle.Result{TraceID: "t", SpanID: "below"},
		oracle.Result{TraceID: "t", SpanID: "orphan"},
	)
}

// TestServiceNameColumnFeedsTheResourceScope covers the dedicated
// OTel-ClickHouse column: a seed that populates only ServiceName must
// still answer resource.service.name and trace:rootService.
func TestServiceNameColumnFeedsTheResourceScope(t *testing.T) {
	spans := []oracle.Span{
		{TraceID: "t", SpanID: "root", ServiceName: "checkout"},
		{TraceID: "t", SpanID: "child", ParentSpanID: "root", ServiceName: "db"},
	}
	requireResults(t, evaluate(t, spans, `{ resource.service.name = "checkout" }`),
		oracle.Result{TraceID: "t", SpanID: "root"})
	requireResults(
		t, evaluate(t, spans, `{ rootServiceName = "checkout" }`),
		oracle.Result{TraceID: "t", SpanID: "child"},
		oracle.Result{TraceID: "t", SpanID: "root"},
	)

	// An explicit map entry is the value the fixture chose; the column
	// must not override it.
	withMap := []oracle.Span{{
		TraceID: "t", SpanID: "root", ServiceName: "column",
		ResourceAttrs: map[string]string{"service.name": "map"},
	}}
	requireResults(t, evaluate(t, withMap, `{ resource.service.name = "map" }`),
		oracle.Result{TraceID: "t", SpanID: "root"})
	requireResults(t, evaluate(t, withMap, `{ resource.service.name = "column" }`))
}

// TestRepeatedSpanIDIsAnError pins that a seed emitting the same span
// twice fails loudly. Such a seed is testing SQL-level de-duplication, not
// TraceQL semantics, and nested-set numbering has no answer for it.
func TestRepeatedSpanIDIsAnError(t *testing.T) {
	spans := []oracle.Span{
		{TraceID: "t", SpanID: "a", ParentSpanID: ""},
		{TraceID: "t", SpanID: "a", ParentSpanID: ""},
	}
	if _, err := oracle.Evaluate(t, spans, `{ true }`); err == nil {
		t.Fatal("a repeated span ID must be an error, not an arbitrary numbering of one of them")
	}
}

// TestUnparseableQueryIsAnError pins that a query upstream rejects comes
// back as an error the caller can attribute, rather than as an empty
// answer that would look like agreement with an equally empty cerberus
// result.
func TestUnparseableQueryIsAnError(t *testing.T) {
	if _, err := oracle.Evaluate(t, tree(), `{ this is not traceql`); err == nil {
		t.Fatal("a query the reference engine cannot parse must be an error, not an empty answer")
	}
}
