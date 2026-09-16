package grpc_test

import (
	"context"
	"net"
	"testing"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestNewServer_StreamInterceptorRejectsAtCap proves, end to end through
// the REAL *grpc.Server NewServer builds, that a saturated *admit.Limiter
// makes a dialed gRPC client observe codes.ResourceExhausted on a
// streaming RPC — the gRPC half of the symmetry NewServer's doc comment
// claims ("a saturated Tempo head rejects gRPC and HTTP traffic
// symmetrically"). The HTTP half is already pinned by
// TestConformance_TempoAdmitRejectsAtCap in internal/api/tempo.
//
// Unlike TestStreamInterceptorRejectsAtCap (internal/api/admit), which
// calls Limiter.StreamInterceptor() directly against a fake
// grpc.ServerStream, this test never touches the interceptor function
// itself — it dials a real client against a real *grpc.Server built by
// NewServer, so it proves grpc.ChainStreamInterceptor actually wires the
// limiter ahead of the registered StreamingQuerierServer, not just that
// the interceptor's own logic is correct in isolation.
func TestNewServer_StreamInterceptorRejectsAtCap(t *testing.T) {
	t.Parallel()

	// Saturate the real admission limiter's single slot before any RPC
	// is dialed, so the first stream the client opens is guaranteed to
	// land on a full semaphore.
	limiter := admit.New("tempo", 1)
	release, ok := limiter.Acquire(context.Background())
	if !ok {
		t.Fatalf("setup acquire: want ok")
	}
	t.Cleanup(release)

	handler := tempo.New(&stubCursorQuerier{}, schema.DefaultOTelTraces(), "test", nil)
	svc := tempogrpc.NewService(handler, limiter, nil)
	srv := tempogrpc.NewServer(svc)

	lis := bufconn.Listen(1 << 20)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.GracefulStop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := tempopb.NewStreamingQuerierClient(conn)

	// Search is a real, non-scaffold RPC (internal/api/tempo/grpc/search.go);
	// its request content is irrelevant here — admission rejection must
	// happen before the interceptor chain ever dispatches into the
	// handler, so an otherwise-valid request still gets rejected.
	stream, err := client.Search(context.Background(), &tempopb.SearchRequest{Query: "{}", Limit: 20})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := drainSearch(t, stream)
	if recvErr == nil {
		t.Fatalf("want ResourceExhausted, got a completed stream with no error")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("want grpc status, got %v", recvErr)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("code: got %s, want ResourceExhausted (err=%v)", st.Code(), recvErr)
	}
}

// TestNewServer_StreamInterceptorAdmitsUnderCap is the control for
// TestNewServer_StreamInterceptorRejectsAtCap: with the limiter's slot
// free, the same real *grpc.Server / real client wiring must let the
// stream reach the registered Service and complete normally. Without
// this, a bug that rejected every stream unconditionally would also
// make the cap test above pass for the wrong reason.
func TestNewServer_StreamInterceptorAdmitsUnderCap(t *testing.T) {
	t.Parallel()

	limiter := admit.New("tempo", 1)

	handler := tempo.New(&stubCursorQuerier{}, schema.DefaultOTelTraces(), "test", nil)
	svc := tempogrpc.NewService(handler, limiter, nil)
	srv := tempogrpc.NewServer(svc)

	lis := bufconn.Listen(1 << 20)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.GracefulStop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := tempopb.NewStreamingQuerierClient(conn)

	stream, err := client.Search(context.Background(), &tempopb.SearchRequest{Query: "{}", Limit: 20})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, recvErr := drainSearch(t, stream); recvErr != nil {
		t.Fatalf("want clean stream completion under cap, got %v", recvErr)
	}
}
