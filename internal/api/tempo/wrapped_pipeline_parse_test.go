package tempo

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// wrappedPipelineSyntaxError is the reference parser's message for a `|`
// after a parenthesised pipeline at the top level of a query.
const wrappedPipelineSyntaxError = "syntax error: unexpected |, expecting with"

// metricsEntrypointExplainName names the metricsEntrypoints row that drives
// the explain preview's metrics arm below its parse: it calls parseExpr and
// parseMetrics directly, so a parse error reaches it without the parse-stage
// marker explainLang.Parse adds and classifies as an internal error. A
// parse-error test drives explain through explainLang.Parse instead
// (parseSurfaces).
const metricsEntrypointExplainName = "explain offline preview"

// parseSurfaces is every entrypoint that parses a client's TraceQL: the
// serving metrics entrypoints, HTTP /api/search, the SearchResult core the
// gRPC Search RPC shares with it, and the offline explain preview through
// explainLang.Parse, which parses before routing to its metrics or search
// arm.
func parseSurfaces(t *testing.T) []metricsEntrypoint {
	t.Helper()
	var out []metricsEntrypoint
	for _, ep := range metricsEntrypoints {
		if ep.name != metricsEntrypointExplainName {
			out = append(out, ep)
		}
	}
	if len(out) != len(metricsEntrypoints)-1 {
		t.Fatalf("metricsEntrypoints has no %q row to replace", metricsEntrypointExplainName)
	}
	return append(
		out,
		metricsEntrypoint{
			name: "http /api/search",
			invoke: func(t *testing.T, h *Handler, query string) (int, string) {
				vals := url.Values{}
				vals.Set("q", query)
				return httpOutcome(t, h.handleSearch, "/api/search?"+vals.Encode())
			},
		},
		metricsEntrypoint{
			name: "grpc Search (SearchResult)",
			invoke: func(_ *testing.T, h *Handler, query string) (int, string) {
				_, err := h.SearchResult(context.Background(), query, DefaultSearchLimit)
				return errOutcome(err)
			},
		},
		metricsEntrypoint{
			name: "explain offline preview (explainLang.Parse)",
			invoke: func(_ *testing.T, h *Handler, query string) (int, string) {
				ctx := ExplainContext(context.Background(),
					pipelineFixtureStart, pipelineFixtureEnd, DefaultSearchLimit)
				_, _, err := NewExplainLang(h.Schema, pipelineFixtureStep).Parse(ctx, query)
				return errOutcome(err)
			},
		},
	)
}

// TestWrappedPipelineFollowedByPipeIsAParseError pins that a parenthesised
// pipeline followed by `|` is a syntax error on every surface, as the
// reference grammar has no production that lets `|` follow one: the metrics
// form must not reach the emitter (a 500) and the search form must not reach
// the lowering (a 422). Both queries go through every search and metrics
// entrypoint, since the parse runs before either routes on the query shape.
func TestWrappedPipelineFollowedByPipeIsAParseError(t *testing.T) {
	t.Parallel()

	queries := []string{
		`({ } | count() > 1) | rate()`,
		`({ } | count() > 1) | { }`,
	}
	surfaces := parseSurfaces(t)
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			for _, ep := range surfaces {
				t.Run(ep.name, func(t *testing.T) {
					t.Parallel()
					status, msg := ep.invoke(t, newPipelineHandler(), query)
					if status != http.StatusBadRequest {
						t.Errorf("status: got %d want %d (msg=%s)", status, http.StatusBadRequest, msg)
					}
					if !strings.Contains(msg, wrappedPipelineSyntaxError) {
						t.Errorf("message %q does not carry the parse error %q", msg, wrappedPipelineSyntaxError)
					}
				})
			}
		})
	}
}
