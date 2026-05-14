package chsql

import (
	"testing"
)

// chsql Builder / QueryBuilder / Frag micro-benchmarks (Layer 12). The
// emitter sits in the per-query hot path — every API request lands here
// to render the lowered chplan into SQL. These benchmarks pin the
// per-build cost of a representative deeply-nested QueryBuilder tree
// and a representative deep Frag chain.

// buildDeepQuery composes a QueryBuilder shape similar to what
// range-window emission produces: SELECT with multiple projections,
// nested subquery FROM (via another QueryBuilder.Frag), WHERE +
// PREWHERE filters, GROUP BY, ORDER BY, LIMIT. Mirrors the structural
// depth a real PromQL `sum by (le)(rate(...))` lowering reaches.
func buildDeepQuery() *QueryBuilder {
	inner := NewQuery().
		Select(Col("MetricName"), Col("Attributes"), Col("TimeUnix"), Col("Value")).
		From(Col("otel_metrics_gauge")).
		Where(
			Eq(Col("MetricName"), Lit("up")),
			Gt(Col("Value"), Lit(0.0)),
		).
		Prewhere(Gte(Col("TimeUnix"), Lit(int64(1700000000))))

	mid := NewQuery().
		Select(
			Col("Attributes"),
			Col("TimeUnix"),
			As(Mul(Col("Value"), Lit(2.0)), "scaled_value"),
		).
		From(inner.Frag()).
		Where(Eq(Subscript(Col("Attributes"), Lit("job")), Lit("api")))

	return NewQuery().
		Select(
			Col("Attributes"),
			Call("sum", Col("scaled_value")),
		).
		From(mid.Frag()).
		GroupBy(Col("Attributes")).
		OrderBy(Col("Attributes"), false).
		Limit(1000)
}

// buildDeepFrag composes a deeply-nested boolean Frag. Shape matches
// the predicate trees the predicate-pushdown rule moves around.
func buildDeepFrag() Frag {
	return And(
		Eq(Col("MetricName"), Lit("up")),
		Or(
			Paren(And(
				Eq(Subscript(Col("Attributes"), Lit("job")), Lit("api")),
				Gte(Col("TimeUnix"), Lit(int64(1700000000))),
			)),
			Paren(And(
				Eq(Subscript(Col("Attributes"), Lit("job")), Lit("web")),
				Lt(Col("TimeUnix"), Lit(int64(1700000100))),
			)),
		),
		Like(Subscript(Col("Attributes"), Lit("instance")), Lit("host-%")),
	)
}

func BenchmarkQueryBuilder_Build(b *testing.B) {
	q := buildDeepQuery()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = q.Build()
	}
}

func BenchmarkFrag_Construction(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Re-build each iteration — exercises constructor allocation,
		// the common pattern in emit.go where every emitNode call
		// builds its Frag tree fresh.
		_ = buildDeepFrag()
	}
}

func BenchmarkBuilder_NewAndBuild(b *testing.B) {
	frag := buildDeepFrag()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bld := NewBuilder()
		frag(bld)
		_, _ = bld.Build()
	}
}

// TestAllocs_QueryBuilderBuild pins the allocation count of a typical
// deeply-nested QueryBuilder.Build. The render path appends into a
// strings.Builder + a []any args slice — both grow geometrically, so
// per-build allocs cluster tight.
func TestAllocs_QueryBuilderBuild(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	q := buildDeepQuery()
	got := testing.AllocsPerRun(100, func() {
		_, _ = q.Build()
	})
	// Baseline 12 allocs (strings.Builder + args slice growth).
	// Ceiling = baseline × ~3.
	const ceiling = 36.0
	if got > ceiling {
		t.Errorf("QueryBuilder.Build avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("QueryBuilder.Build avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}

// TestAllocs_FragConstruction pins the per-construction allocs of a
// deeply-nested Frag tree. Each typed Frag constructor returns a
// closure; the goal of the ceiling is to catch a regression where
// somebody slips a slice-copy or map alloc into a wrapper.
func TestAllocs_FragConstruction(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	got := testing.AllocsPerRun(100, func() {
		_ = buildDeepFrag()
	})
	// Baseline 34 allocs (one closure per constructor — the typed
	// Frag API trades alloc count for type safety, and the per-emit
	// cost is amortised). Ceiling = baseline × ~2.
	const ceiling = 70.0
	if got > ceiling {
		t.Errorf("Frag construction avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("Frag construction avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}
