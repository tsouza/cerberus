package grpc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMapEngineError_ResourceExhaustedFamily pins that the per-query resource
// budgets map to codes.ResourceExhausted (the gRPC sibling of HTTP 422),
// symmetric with the HTTP head's classifySearchErr — not a misleading Internal.
func TestMapEngineError_ResourceExhaustedFamily(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		&chclient.DrainByteBudgetError{Limit: 256 << 20},
		&chclient.TooManySamplesError{Limit: 3},
	} {
		got := status.Code(grpc.MapEngineErrorForTest(err))
		if got != codes.ResourceExhausted {
			t.Errorf("mapEngineError(%v) code = %v, want ResourceExhausted", err, got)
		}
	}
	// A generic error still maps to Internal.
	if got := status.Code(grpc.MapEngineErrorForTest(errors.New("boom"))); got != codes.Internal {
		t.Errorf("generic error code = %v, want Internal", got)
	}
}

// TestMapEngineError_ContextErrors pins that cancellation / deadline — now
// surfaced at the eager SearchResult boundary (the old streaming path caught it
// per-row) — maps to the proper gRPC status, including through the engine's
// `execute: %w` wrap, not a misleading Internal.
func TestMapEngineError_ContextErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want codes.Code
	}{
		{context.Canceled, codes.Canceled},
		// Deadline + CH wall-clock timeout both map to Unavailable (the gRPC 503),
		// symmetric with HTTP classifySearchErr which maps both to 503.
		{context.DeadlineExceeded, codes.Unavailable},
		{chclient.ErrQueryTimeout, codes.Unavailable},
		{fmt.Errorf("engine: execute: %w", context.Canceled), codes.Canceled},
		{fmt.Errorf("engine: execute: %w", chclient.ErrQueryTimeout), codes.Unavailable},
	}
	for _, tc := range cases {
		if got := status.Code(grpc.MapEngineErrorForTest(tc.err)); got != tc.want {
			t.Errorf("mapEngineError(%v) code = %v, want %v", tc.err, got, tc.want)
		}
	}
}
