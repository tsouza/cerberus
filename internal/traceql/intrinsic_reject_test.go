package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestUnbackedIntrinsicsAreRejected pins the loud-rejection contract
// for intrinsics the OTel-CH span-row schema cannot answer. Before
// this gate, every one of these lowered to a
// `SpanAttributes['<intrinsic name>']` map lookup — syntactically
// valid SQL that matched zero rows, i.e. a confidently-wrong empty
// result (the exact failure mode the LogQL ip() rejection closed for
// the Loki head). The showcase-traceql dashboard pins the wire-level
// 422s; this test pins the lowering-layer error + message shape.
func TestUnbackedIntrinsicsAreRejected(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	cases := []struct {
		query   string
		wantSub string
	}{
		{`{ rootName = "GET /home" }`, "trace-scoped"},
		{`{ rootServiceName = "frontend" }`, "trace-scoped"},
		{`{ traceDuration > 100ms }`, "trace-scoped"},
		{`{ trace:rootName = "GET /home" }`, "trace-scoped"},
		{`{ trace:rootService = "frontend" }`, "trace-scoped"},
		{`{ trace:duration > 100ms }`, "trace-scoped"},
		{`{ span:childCount > 0 }`, "per-span child counts"},
		{`{ event:timeSinceStart > 1ms }`, "per-event timestamp arithmetic"},
		{`{ instrumentation.deployment = "blue" }`, "no scope-attributes column"},
		// Bare nested-intrinsic references outside a comparison have no
		// flat column to project.
		{`{ } | select(event:name)`, "only supported in comparisons"},
		// Metrics group-by on an unbacked intrinsic flows through the
		// same gate.
		{`{ } | rate() by (rootName)`, "trace-scoped"},
	}
	for _, tc := range cases {
		expr, err := tempo.Parse(tc.query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.query, err)
		}
		_, err = traceql.Lower(context.Background(), expr, s)
		if err == nil {
			t.Errorf("Lower(%q): want rejection containing %q, got nil error", tc.query, tc.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("Lower(%q): error %q does not contain %q", tc.query, err, tc.wantSub)
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
