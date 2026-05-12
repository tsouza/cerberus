package traceql

import (
	"testing"

	"github.com/grafana/tempo/pkg/traceql"
)

// TestParserSmoke pins that the upstream Tempo TraceQL parser accepts
// every shape cerberus claims to support, plus a wider catalog of
// real-world queries from Grafana 11's TraceQL Search UI.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		// Span selectors — attribute matchers
		`{ resource.service.name = "frontend" }`,
		`{ resource.service.name != "backend" }`,
		`{ resource.service.name =~ "front.*" }`,
		`{ resource.service.name !~ "back.*" }`,

		// Span-scoped attributes
		`{ span.http.method = "GET" }`,
		`{ .http.method = "GET" }`, // default span scope
		`{ span.http.status_code >= 500 }`,
		`{ span.http.status_code < 400 }`,

		// Intrinsics
		`{ duration > 100ms }`,
		`{ duration >= 1s }`,
		`{ duration < 50ms }`,
		`{ name = "GET /home" }`,
		`{ name =~ "GET /.*" }`,
		`{ kind = "client" }`,
		`{ statusMessage = "internal error" }`,
		`{ trace:id = "abcdef0123456789" }`,
		`{ span:id = "0123456789abcdef" }`,
		`{ parent = "fedcba9876543210" }`,

		// Static value types
		`{ .attempt = 1 }`,                   // int
		`{ .ratio = 0.95 }`,                  // float
		`{ .ok = true }`,                     // bool
		`{ .name = "quoted with spaces" }`,   // string
		"{ .name = `backtick with spaces` }", // backtick raw
		`{ .timeout = 30s }`,                 // duration

		// Boolean compounds
		`{ resource.service.name = "frontend" && duration > 100ms }`,
		`{ resource.service.name = "frontend" || resource.service.name = "api" }`,
		`{ resource.service.name = "frontend" && (duration > 100ms || span.http.status_code >= 500) }`,

		// Structural ops (parser accepts all 4 even if cerberus only lowers two)
		`{ resource.service.name = "frontend" } > { resource.service.name = "api" }`,
		`{ resource.service.name = "api" } < { resource.service.name = "frontend" }`,
		`{ resource.service.name = "frontend" } >> { resource.service.name = "db" }`,
		`{ resource.service.name = "db" } << { resource.service.name = "frontend" }`,

		// Pipeline aggregations
		`{ resource.service.name = "frontend" } | count() > 0`,
		`{ resource.service.name = "frontend" } | count() >= 5`,
		`{ resource.service.name = "api" } | avg(duration) > 100ms`,
		`{ resource.service.name = "api" } | max(duration) < 1s`,
		`{ resource.service.name = "api" } | min(duration) >= 10ms`,
		`{ resource.service.name = "api" } | sum(duration) > 1m`,

		// Select projections
		`{ resource.service.name = "frontend" } | select(span.http.method)`,
		`{ resource.service.name = "frontend" } | select(span.http.method, span.http.status_code)`,
		`{ resource.service.name = "frontend" } | select(resource.service.namespace, resource.k8s.cluster.name)`,

		// Scalar arithmetic in attribute expressions
		`{ .a + .b > 10 }`,
		`{ .a * 2 = .b }`,
		`{ .duration / 2 < 100ms }`,

		// Negation
		`{ !(.a = .b) }`,

		// Real-world Grafana TraceQL Search UI shapes
		`{ resource.service.name = "frontend" && span.http.status_code >= 500 } | count() > 0`,
		`{ duration > 1s && resource.service.name = "api" }`,
		`{ resource.deployment.environment = "prod" && span.http.method = "POST" }`,
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := traceql.Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("Parse(%q) returned nil expression", q)
			}
		})
	}
}

// TestParserSmoke_Rejected pins that broken TraceQL is rejected at
// parse time, so cerberus's lowering never has to defend.
func TestParserSmoke_Rejected(t *testing.T) {
	t.Parallel()

	cases := []string{
		`wharblgarbl`,      // unknown identifier
		`{ 2 <> 3 }`,       // invalid operator
		`{ .a = .b `,       // missing close brace
		`({ .a } | { .b }`, // missing close paren
		`{ .a } | `,        // incomplete pipe
		`{ + }`,            // malformed
		`{ .a } | count(`,  // incomplete aggregate
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := traceql.Parse(q)
			if err == nil {
				t.Fatalf("Parse(%q) should have rejected; accepted instead", q)
			}
		})
	}
}
