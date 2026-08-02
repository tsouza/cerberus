package grpc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestGRPCStatusFor pins the gRPC half of the Tempo head's shared error
// classification: every sentinel family reaches the code its ErrClass maps
// to, through the engine's `execute:` wrap as well as bare. Before the
// classification was unified, the metrics RPCs ran a three-case ladder that
// collapsed the budgets, both timeout forms and cancellation onto
// codes.Internal — the Search RPC's ladder disagreed with it, and both
// disagreed with the HTTP handlers.
func TestGRPCStatusFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"canceled", context.Canceled, codes.Canceled},
		{"canceled/execute-wrapped", fmt.Errorf("engine: execute: %w", context.Canceled), codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.Unavailable},
		{"query-timeout", chclient.ErrQueryTimeout, codes.Unavailable},
		{"query-timeout/execute-wrapped", fmt.Errorf("engine: execute: %w", chclient.ErrQueryTimeout), codes.Unavailable},
		{"circuit-open", chclient.ErrCircuitOpen, codes.Unavailable},
		{"drain-bytes", &chclient.DrainByteBudgetError{Limit: 256 << 20}, codes.ResourceExhausted},
		{"too-many-samples", &chclient.TooManySamplesError{Limit: 3}, codes.ResourceExhausted},
		{"memory-limit", &chclient.MemoryLimitError{Limit: 1 << 30}, codes.ResourceExhausted},
		{"parse-stage", fmt.Errorf("%w: bad traceql", tempo.ErrParseStage), codes.InvalidArgument},
		{"lower-stage", fmt.Errorf("%w: unsupported", tempo.ErrLowerStage), codes.InvalidArgument},
		{"emit-stage", errors.New("engine: emit: boom"), codes.Internal},
		{"execute-stage", errors.New("engine: execute: CH refused"), codes.Unavailable},
		{"unclassified", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(grpc.GRPCStatusForTest(tc.err)); got != tc.want {
			t.Errorf("%s: grpcStatusFor(%v) code = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if err := grpc.GRPCStatusForTest(nil); err != nil {
		t.Errorf("grpcStatusFor(nil) = %v, want nil", err)
	}
}
