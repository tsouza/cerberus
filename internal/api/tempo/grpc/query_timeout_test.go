package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

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

// rpcQueryBudget is the Handler.QueryTimeout the deadline test runs under:
// short enough to keep the test fast, long enough that the RPC reaches the
// querier before it fires.
const rpcQueryBudget = 100 * time.Millisecond

// rpcDeadlineWait bounds how long the test waits for the stream to
// terminate; an RPC that installs no deadline never answers a hanging
// backend, so this is what turns that hang into a failure.
const rpcDeadlineWait = 5 * time.Second

// TestSearch_HangingBackendReleasesTheAdmitSlotAtTheQueryTimeout pins that
// a gRPC RPC runs its query under the Handler's configured QueryTimeout,
// exactly as every Tempo HTTP entrypoint does: the client sets no deadline
// on the stream (Grafana's streaming datasource sets none), the backend
// never answers, and the RPC must still terminate at the budget with the
// timeout's Unavailable code — and, because the interceptor's admit slot
// is released when the handler returns, the limiter's single slot must be
// free again afterwards. Before this the RPCs ran on the bare
// stream.Context(), so a hung ClickHouse held the slot of the limiter this
// service shares with the HTTP surface until the driver's own read timeout.
func TestSearch_HangingBackendReleasesTheAdmitSlotAtTheQueryTimeout(t *testing.T) {
	t.Parallel()

	limiter := admit.New("tempo", 1)
	q := &stubCursorQuerier{block: true}
	handler := tempo.New(q, schema.DefaultOTelTraces(), "test", nil)
	handler.QueryTimeout = rpcQueryBudget
	svc := tempogrpc.NewService(handler, limiter, nil)
	srv := tempogrpc.NewServer(svc)

	lis := bufconn.Listen(1 << 20)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := tempopb.NewStreamingQuerierClient(conn)

	// No client-side deadline: the only budget in play is the server's.
	stream, err := client.Search(context.Background(), &tempopb.SearchRequest{Query: "{}", Limit: 20})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, recvErr := drainSearch(t, stream)
		done <- recvErr
	}()

	var recvErr error
	select {
	case recvErr = <-done:
	case <-time.After(rpcDeadlineWait):
		t.Fatalf("the stream did not terminate within %s: the RPC ran its query with no deadline and hung on the backend", rpcDeadlineWait)
	}
	if recvErr == nil {
		t.Fatal("stream completed cleanly against a backend that never answers")
	}
	st, ok := status.FromError(recvErr)
	if !ok || st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable (the head's timeout class); err = %v", st.Code(), recvErr)
	}
	if !waitForBool(&q.released, time.Second) {
		t.Error("the server's Query never observed the deadline — the budget was not installed on the query ctx")
	}

	// The handler returned, so the interceptor released its slot: the
	// limiter's only slot must be acquirable again.
	release, ok := limiter.Acquire(context.Background())
	if !ok {
		t.Fatal("the admit slot was still held after the RPC timed out")
	}
	release()
}
