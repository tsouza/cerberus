package grpc_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
)

// TestUnimplemented_AllRPCsReturnUnimplemented is the proof-of-life
// for the gRPC scaffold: every StreamingQuerier RPC that has not yet
// been ported must dial, the gRPC handshake must complete, and the
// server must answer codes.Unimplemented. PR 2 ported Search out of
// this matrix and into search_test.go (the subtest below is gone);
// PRs 3 + 4 continue the same migration for the tag-list and metrics
// RPCs.
//
// The test uses bufconn (the standard in-memory gRPC transport) so it
// doesn't need a real network listener and stays hermetic — no port
// binding, no h2c upgrade dance, no goroutine leaks across parallel
// test packages.
func TestUnimplemented_AllRPCsReturnUnimplemented(t *testing.T) {
	t.Parallel()

	lis := bufconn.Listen(1 << 20)
	srv := tempogrpc.NewServer(tempogrpc.NewService(nil, nil, nil))
	go func() {
		if err := srv.Serve(lis); err != nil {
			// Serve returns ErrServerStopped on GracefulStop; that's
			// the normal shutdown path and doesn't need to fail the
			// test.
			t.Logf("grpc Serve returned: %v", err)
		}
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

	// drainErr consumes a stream until EOF and returns the first non-EOF
	// error. The Unimplemented status arrives on the first Recv from a
	// server-streaming RPC even though the RPC opens "successfully" at
	// the transport layer.
	drainErr := func(t *testing.T, stream interface{ RecvMsg(m any) error }) error {
		t.Helper()
		for {
			var msg any
			if err := stream.RecvMsg(&msg); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
	}

	assertUnimplemented := func(t *testing.T, name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: want Unimplemented, got nil", name)
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("%s: want grpc status, got %v", name, err)
		}
		if st.Code() != codes.Unimplemented {
			t.Fatalf("%s: want code Unimplemented, got %s", name, st.Code())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Search has been ported (search.go + search_test.go) and the two
	// metrics RPCs have been ported (metrics.go + metrics_test.go).
	// The matrix below covers the four tag-list RPCs that remain
	// Unimplemented today; the dropped subtests are exercised
	// end-to-end against the streaming pipeline in their per-RPC
	// suites.

	t.Run("SearchTags", func(t *testing.T) {
		stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		assertUnimplemented(t, "SearchTags", drainErr(t, stream))
	})

	t.Run("SearchTagsV2", func(t *testing.T) {
		stream, err := client.SearchTagsV2(ctx, &tempopb.SearchTagsRequest{})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		assertUnimplemented(t, "SearchTagsV2", drainErr(t, stream))
	})

	t.Run("SearchTagValues", func(t *testing.T) {
		stream, err := client.SearchTagValues(ctx, &tempopb.SearchTagValuesRequest{TagName: "service.name"})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		assertUnimplemented(t, "SearchTagValues", drainErr(t, stream))
	})

	t.Run("SearchTagValuesV2", func(t *testing.T) {
		stream, err := client.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{TagName: "service.name"})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		assertUnimplemented(t, "SearchTagValuesV2", drainErr(t, stream))
	})
}

// TestServer_AdmitDisabledPath confirms NewServer with a Service whose
// Limiter is nil still constructs and serves — the gRPC equivalent of
// the CERBERUS_ADMIT_DISABLED=true path. Stream interceptor goes
// through admit.StreamInterceptor's nil-receiver pass-through branch.
func TestServer_AdmitDisabledPath(t *testing.T) {
	t.Parallel()

	lis := bufconn.Listen(1 << 20)
	srv := tempogrpc.NewServer(tempogrpc.NewService(nil, nil, nil))
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc Serve returned: %v", err)
		}
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

	// One sanity check is enough — the Unimplemented matrix is
	// covered above; this test exists only to pin that the nil-Limiter
	// construction path works end-to-end. Use SearchTags rather than
	// Search because PR 2 ported Search to a real handler (a nil
	// Handler would return codes.Internal here, not the Unimplemented
	// status this test asserts on); SearchTags remains Unimplemented
	// until PR 3, which is the right shape for the admit-disabled probe.
	client := tempopb.NewStreamingQuerierClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	var msg any
	err = stream.RecvMsg(&msg)
	if err == nil {
		t.Fatalf("want Unimplemented error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want grpc status, got %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code: want Unimplemented, got %s", st.Code())
	}
}
