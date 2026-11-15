package tempo

import (
	"net/http"
	"strings"
	"testing"
)

// TestSpansetShapeSyntaxErrorsAre400 pins that a spanset operation mixing
// a parenthesised pipeline with an operand that is not one, an aggregate
// stage with no comparison after it, and a pipeline led by coalesce() are
// syntax errors on every surface that parses a client's TraceQL: a 400
// carrying the reference parser's message, before any search or metrics
// routing.
func TestSpansetShapeSyntaxErrorsAre400(t *testing.T) {
	t.Parallel()

	cases := []struct{ query, want string }{
		{`({ } | count() > 1) && { }`, "parse error at line 1, col 24: syntax error: unexpected {, expecting ("},
		{`{ } && ({ } | count() > 1)`, "parse error at line 1, col 13: syntax error: unexpected |"},
		{`({ }) && ({ } | count() > 1)`, "parse error at line 1, col 15: syntax error: unexpected |"},
		{`({ .a = 1 } | count() > 1) >> { .b = 2 }`, "parse error at line 1, col 31: syntax error: unexpected {, expecting ("},
		{`({ } >> ({ } | count() > 1)) | rate()`, "parse error at line 1, col 14: syntax error: unexpected |"},
		{`{ } | count()`, "parse error at line 1, col 14: syntax error: unexpected $end"},
		{`{ } | avg(duration)`, "parse error at line 1, col 20: syntax error: unexpected $end"},
		{`{ } | count() | rate()`, "parse error at line 1, col 15: syntax error: unexpected |"},
		{`coalesce() | { }`, "parse error at line 1, col 1: syntax error: unexpected coalesce"},
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
						t.Errorf("message %q does not carry the parse error %q", msg, c.want)
					}
				})
			}
		})
	}
}
