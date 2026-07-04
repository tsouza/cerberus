package grpc_test

import (
	"errors"
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
