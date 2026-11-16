package tempo

import (
	"net/http"
	"strings"
	"testing"
)

// TestReferenceValidateRejectionsAre400 pins that a query reference Tempo
// parses but rejects in its Validate — a scalar filter with an operand shape
// other than an aggregate against a literal, or a topk / bottomk limit that
// is not positive — answers 400 with Tempo's message on every surface that
// parses a client's TraceQL, before any search or metrics routing.
func TestReferenceValidateRejectionsAre400(t *testing.T) {
	t.Parallel()

	const (
		lhs   = "invalid TraceQL query: scalar filter lhs of type (%v) not yet supported"
		rhs   = "invalid TraceQL query: scalar filter rhs of type (%v) not yet supported"
		limit = "invalid TraceQL query: limit must be greater than 0"
	)
	cases := []struct{ query, want string }{
		{`{ } | count() + 1 > 2`, lhs},
		{`{ } | count() * 2 > 1`, lhs},
		{`{ } | 1 > count()`, lhs},
		{`{ } | count() > 1 + 1`, rhs},
		{`{ } | rate() | topk(0)`, limit},
		{`{ } | rate() | bottomk(0)`, limit},
	}
	surfaces := parseSurfaces(t)
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			t.Parallel()
			for _, ep := range surfaces {
				t.Run(ep.name, func(t *testing.T) {
					t.Parallel()
					status, msg := ep.invoke(t, newPipelineHandler(), c.query)
					if status != http.StatusBadRequest {
						t.Errorf("status: got %d want %d (msg=%s)", status, http.StatusBadRequest, msg)
					}
					if !strings.Contains(msg, c.want) {
						t.Errorf("message %q does not carry the validation error %q", msg, c.want)
					}
				})
			}
		})
	}
}
