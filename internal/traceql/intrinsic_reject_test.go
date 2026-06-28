package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestUnbackedIntrinsicComparisonsLowerConstant pins the parity contract
// for comparisons against intrinsics the OTel-CH span-row schema cannot
// answer (trace-scoped rootName / rootServiceName / traceDuration,
// per-span childCount, per-event timeSinceStart, instrumentation-scoped
// attributes). Reference Tempo's `/api/search` ACCEPTS each of these —
// the absent operand resolves to StaticNil and the comparison evaluates
// StaticFalse, so the query returns a 2xx empty result, not a 4xx. The
// rejection-parity layer flagged the old loud-422 behaviour as a
// wrong_rejection. Cerberus now lowers them to a constant-false
// predicate, matching reference's status class (2xx) and its
// matches-nothing semantics. The earlier worry — a silent
// `SpanAttributes['rootName']` map lookup returning a confidently-wrong
// result — does not apply: a constant-false predicate is the *correct*
// answer, not a guess.
func TestUnbackedIntrinsicComparisonsLowerConstant(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ rootName = "GET /home" }`,
		`{ rootServiceName = "frontend" }`,
		`{ traceDuration > 100ms }`,
		`{ trace:rootName = "GET /home" }`,
		`{ trace:rootService = "frontend" }`,
		`{ trace:duration > 100ms }`,
		`{ span:childCount > 0 }`,
		`{ event:timeSinceStart > 1ms }`,
		`{ instrumentation.deployment = "blue" }`,
		`{ instrumentation.deployment != "blue" }`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Errorf("Lower(%q): want successful constant lowering, got error: %v", q, err)
		}
	}
}

// TestNestedIntrinsicsLowerInComparisons pins the supported half of the
// nested-intrinsic surface: event:name / link:traceID / link:spanID in
// comparisons lower to NestedArrayExists against the Events / Links
// Nested subfields (the SQL shape is pinned by the
// test/spec/traceql/*_intrinsic.txtar fixtures; this test pins the
// flipped-operand form those fixtures don't cover).
func TestNestedIntrinsicsLowerInComparisons(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ event:name = "exception" }`,
		`{ event:name =~ "exc.*" }`,
		`{ "exception" = event:name }`,
		`{ link:traceID = "a0000000000000000000000000000001" }`,
		`{ link:spanID != "0000000000000001" }`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Errorf("Lower(%q): unexpected error: %v", q, err)
		}
	}
}
