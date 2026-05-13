package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// FuzzParse drives random inputs through the upstream TraceQL parser and,
// on a successful parse, through cerberus's lowering. The goal is
// panic-freedom: any panic surfaces as a fuzz crash with the offending
// seed promoted to testdata/fuzz/FuzzParse/.
//
// RC3 R3.11 — seed corpus is intentionally small; CI runs `-fuzztime=120s`
// per parser weekly to grow the corpus organically.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{ resource.service.name = "frontend" }`,
		`{ duration > 100ms }`,
		`{ span.http.status_code >= 500 }`,
		`{ resource.service.name = "frontend" && duration > 100ms }`,
		`{ resource.service.name = "frontend" } | count() > 0`,
		`{ resource.service.name = "api" } | avg(duration) > 100ms`,
		`{ resource.service.name = "frontend" } | select(span.http.method)`,
		`{ resource.service.name = "frontend" } > { resource.service.name = "api" }`,
		`{ name =~ "GET /.*" }`,
		`{ .a + .b > 10 }`,
		`{ !(.a = .b) }`,
		`{ resource.deployment.environment = "prod" && span.http.method = "POST" }`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	s := schema.DefaultOTelTraces()

	f.Fuzz(func(t *testing.T, q string) {
		expr, err := tempo.Parse(q)
		if err != nil {
			return
		}
		_, _ = traceql.Lower(context.Background(), expr, s)
	})
}
