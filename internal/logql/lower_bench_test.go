package logql_test

import (
	"context"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// LogQL micro-benchmarks (Layer 12). Same shape as the PromQL siblings:
// each Benchmark times Lower on a pre-parsed expression and reports
// allocs. Three representative shapes — stream-only matcher, pipeline
// with line filters, and the metric form via count_over_time — cover
// the dispatcher's three top-level switch arms.

func parseLogQL(b *testing.B, query string) syntax.Expr {
	b.Helper()
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		b.Fatalf("ParseExpr(%q): %v", query, err)
	}
	return expr
}

func BenchmarkLower_StreamMatcher(b *testing.B) {
	expr := parseLogQL(b, `{job="api"}`)
	s := schema.DefaultOTelLogs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := logql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_LineFilterChain(b *testing.B) {
	expr := parseLogQL(b, `{job="api"} |= "error" |~ "5[0-9]{2}"`)
	s := schema.DefaultOTelLogs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := logql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

func BenchmarkLower_MetricForm(b *testing.B) {
	expr := parseLogQL(b, `count_over_time({job="api"}[5m])`)
	s := schema.DefaultOTelLogs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := logql.Lower(context.Background(), expr, s); err != nil {
			b.Fatalf("Lower: %v", err)
		}
	}
}

// TestAllocs_Lower pins per-query allocation ceilings for the three
// representative LogQL shapes. See PromQL lower_bench_test.go for the
// rationale on baseline + slack — values land at current + ~30%.
func TestAllocs_Lower(t *testing.T) {
	// AllocsPerRun forbids parallel execution; this test is serial.

	cases := []struct {
		name   string
		query  string
		maxAvg float64
	}{
		// Baselines: 15 / 21 / 25 — ceilings = baseline × ~3.
		// `metric_form` was lifted from 80 → 100 when detected_level was
		// added as a synthesised identity dimension on bare range
		// aggregations (mapConcat + mapFilter + multiIf nodes added to
		// every count_over_time / rate / ... lowering), then 100 → 130
		// when the detected_level source gained reference Loki's
		// structured-metadata precedence cascade (a multiIf over the
		// LogAttributes level/severity keys ahead of the SeverityText
		// fallback — see detectedLevelSourceExpr). Current observed
		// value is 119; the 130 ceiling keeps ~10% slack.
		{"stream_matcher", `{job="api"}`, 45},
		{"line_filter_chain", `{job="api"} |= "error" |~ "5[0-9]{2}"`, 70},
		{"metric_form", `count_over_time({job="api"}[5m])`, 130},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			s := schema.DefaultOTelLogs()
			got := testing.AllocsPerRun(100, func() {
				_, _ = logql.Lower(context.Background(), expr, s)
			})
			if got > tc.maxAvg {
				t.Errorf("Lower(%q) avg allocs = %.1f; want <= %.1f", tc.query, got, tc.maxAvg)
			}
			t.Logf("Lower(%q) avg allocs = %.1f (ceiling %.1f)", tc.query, got, tc.maxAvg)
		})
	}
}
