package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TraceQL micro-benchmarks (Layer 12). Three representative shapes,
// mirroring the dispatch in lowerRoot: simple attribute matcher,
// structural chain (`<` / `>` parent-child join), and metrics pipeline.

func parseTraceQL(b *testing.B, query string) *tempo.RootExpr {
	b.Helper()
	expr, err := tempo.Parse(query)
	if err != nil {
		b.Fatalf("Parse(%q): %v", query, err)
	}
	return expr
}

func BenchmarkLower_AttributeMatcher(b *testing.B) {
	expr := parseTraceQL(b, `{ duration > 100ms }`)
	s := schema.DefaultOTelTraces()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_StructuralChain(b *testing.B) {
	expr := parseTraceQL(b, `{ resource.service.name = "api" } < { resource.service.name = "frontend" }`)
	s := schema.DefaultOTelTraces()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_MetricsPipeline(b *testing.B) {
	expr := parseTraceQL(b, `{} | rate()`)
	s := schema.DefaultOTelTraces()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

// TestAllocs_Lower pins per-query allocation ceilings for the three
// representative TraceQL shapes. Generous slack — see PromQL companion.
func TestAllocs_Lower(t *testing.T) {
	// AllocsPerRun forbids parallel execution; this test is serial.

	cases := []struct {
		name   string
		query  string
		maxAvg float64
	}{
		// Baselines: 13 / 23 / 13 — ceilings = baseline × ~3.
		{"attribute_matcher", `{ duration > 100ms }`, 40},
		{"structural_chain", `{ resource.service.name = "api" } < { resource.service.name = "frontend" }`, 70},
		{"metrics_pipeline", `{} | rate()`, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			s := schema.DefaultOTelTraces()
			got := testing.AllocsPerRun(100, func() {
				_, _ = traceql.Lower(context.Background(), expr, s)
			})
			if got > tc.maxAvg {
				t.Errorf("Lower(%q) avg allocs = %.1f; want <= %.1f", tc.query, got, tc.maxAvg)
			}
			t.Logf("Lower(%q) avg allocs = %.1f (ceiling %.1f)", tc.query, got, tc.maxAvg)
		})
	}
}
