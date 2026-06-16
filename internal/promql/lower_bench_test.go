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
		//
		// rc.5 (resource-attribute merge): every selector now wraps a
		// Project that rebinds Attributes to
		// `mapUpdate(sanitize(ResourceAttributes), sanitize(Attributes))`
		// (the always-on resource-label merge). The precedence-parity fix
		// sanitizes BOTH maps' keys (so a dotted key colliding in both
		// resolves Attributes-wins on its sanitized wire spelling), which
		// doubles the sanitize subtree (a second mapFromArrays/arrayMap/
		// Lambda), adding ~4 more allocs/op.
		//
		// dedicated-key exclusion (this change): the resource source map is
		// now ALWAYS wrapped in `mapFilter((k,v) -> k NOT IN ('service.name',
		// 'service_name'), ResourceAttributes)` so the dedicated ServiceName
		// column isn't double-promoted via the resource arm. The mapFilter +
		// Lambda + InList(2 lits) subtree adds ~14-28 allocs/op per selector.
		// Re-baselined: instant 149, range 123, binary 163, aggregation 192,
		// subquery 127. Ceilings keep ~1.1-1.3× headroom over the new
		// baseline so a future slip still trips.
		{"instant", `up`, 180},
		{"range", `rate(http_requests_total[5m])`, 150},
		{"binary", `(up * 2) > 1`, 200},
		{"aggregation", `sum by (le)(rate(http_request_duration_seconds_bucket[1m]))`, 230},
		{"subquery", `max_over_time(rate(http_requests_total[1m])[5m:30s])`, 155},
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
