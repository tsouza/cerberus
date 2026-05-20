package promql_test

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// PromQL micro-benchmarks (Layer 12 perf gate). Each Benchmark
// times Lower against a pre-parsed expression and reports allocs so a
// regression in the per-stage cost is visible from `just bench`. The
// queries here cover one shape from each major lowering family: one
// instant selector, one range-rate, one binary, one aggregation, one
// subquery.

// parsePromQL is a benchmark-helper. Reuses one parser to avoid
// counting the parser-setup cost across iterations.
func parsePromQL(b *testing.B, query string) parser.Expr {
	b.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		b.Fatalf("ParseExpr(%q): %v", query, err)
	}
	return expr
}

func BenchmarkLower_Instant(b *testing.B) {
	expr := parsePromQL(b, `up`)
	s := schema.DefaultOTelMetrics()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_Range(b *testing.B) {
	expr := parsePromQL(b, `rate(http_requests_total[5m])`)
	s := schema.DefaultOTelMetrics()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_Binary(b *testing.B) {
	// Scalar/vector arithmetic — the supported BinaryExpr shape in
	// the seed lowering. Vector/vector cases hit the dedicated
	// vector-join slice and are exercised via the aggregation +
	// subquery benches; keeping this one scalar keeps coverage on
	// the BinaryExpr → projection path explicit.
	expr := parsePromQL(b, `(up * 2) > 1`)
	s := schema.DefaultOTelMetrics()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_Aggregation(b *testing.B) {
	expr := parsePromQL(b, `sum by (le)(rate(http_request_duration_seconds_bucket[1m]))`)
	s := schema.DefaultOTelMetrics()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_Subquery(b *testing.B) {
	expr := parsePromQL(b, `max_over_time(rate(http_requests_total[1m])[5m:30s])`)
	s := schema.DefaultOTelMetrics()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

// TestAllocs_Lower pins the per-query alloc count for each
// representative PromQL shape. The ceilings are intentionally generous
// (current value + ~25-40% slack) so this is a regression detector —
// catastrophic regressions trip, micro-fluctuations don't. Re-baseline
// (and bump the ceilings) only when an intentional change shifts the
// numbers; the goal is signal, not a target.
func TestAllocs_Lower(t *testing.T) {
	// AllocsPerRun panics when called from a parallel test or
	// subtest; this test is intentionally serial to satisfy that
	// constraint.

	cases := []struct {
		name   string
		query  string
		maxAvg float64 // ceiling on allocs/op
	}{
		// Baselines (recorded on a 1M-row test host):
		// instant 48 (was 13 before LWR landed; instant selectors
		//   now build Project(Aggregate(Filter(Scan))) instead of
		//   Filter(Scan) to implement PromQL's Latest-With-Respect-to-T
		//   semantics — fix for sum-over-stored-samples + eval-ts-
		//   boundary bugs in the bare-selector path).
		// range 17 / binary 27 / aggregation 40 / subquery 21.
		// Ceilings = baseline × ~2-3 to keep the test regression-
		// focused; a multi-× spike means somebody slipped a heap
		// allocation into a fast path.
		{"instant", `up`, 70},
		{"range", `rate(http_requests_total[5m])`, 60},
		{"binary", `(up * 2) > 1`, 130},
		{"aggregation", `sum by (le)(rate(http_request_duration_seconds_bucket[1m]))`, 130},
		{"subquery", `max_over_time(rate(http_requests_total[1m])[5m:30s])`, 70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			s := schema.DefaultOTelMetrics()
			got := testing.AllocsPerRun(100, func() {
				_, _ = promql.Lower(context.Background(), expr, s)
			})
			if got > tc.maxAvg {
				t.Errorf("Lower(%q) avg allocs = %.1f; want <= %.1f", tc.query, got, tc.maxAvg)
			}
			t.Logf("Lower(%q) avg allocs = %.1f (ceiling %.1f)", tc.query, got, tc.maxAvg)
		})
	}
}
