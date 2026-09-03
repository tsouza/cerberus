package lsyntax

import "testing"

// LogQL lexer micro-benchmarks (Layer 12). `lower_bench_test.go` times the
// plan-building half of the head; these time the scan that feeds it.
//
// The three shapes are chosen for which parts of the token loop they drive
// rather than for realism alone: a stream selector is almost all identifiers
// and single-character punctuation, a pipeline is dominated by the
// two-character operator scanners (`|=`, `|~`, `!=`, `=~`), and the metric
// form adds the range, number and duration scanners. Between them every
// branch of the loop's dispatch is executed except the error paths.

// benchQueries are the lexer benchmark inputs, named by the dispatch arms
// each one drives.
var benchQueries = map[string]string{
	"StreamMatcher": `{job="api", cluster="eu-west-1", namespace="prod"}`,
	"PipelineOps":   `{job="api"} |= "error" != "debug" |~ "5[0-9]{2}" !~ "healthz" | json | line_format "{{.msg}}"`,
	"MetricForm":    `sum by (job) (rate({job="api"} |= "error" [5m])) / sum by (job) (rate({job="api"}[5m]))`,
}

func BenchmarkLex(b *testing.B) {
	for name, q := range benchQueries {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(q)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				toks, err := lex(q)
				if err != nil {
					b.Fatalf("lex(%q): %v", q, err)
				}
				if len(toks) == 0 {
					b.Fatalf("lex(%q) returned no tokens", q)
				}
			}
		})
	}
}
