package grpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
)

// TestSearch_AttachesByteBudget_NoBypass is the gRPC no-bypass ratchet — the
// streaming-transport sibling of api/tempo's
// TestSearch_AttachesByteBudget_NoBypass (handler_drain_byte_budget_test.go
// in package tempo_test). #1509 found that HTTP ratchet covers /api/search,
// /api/search/recent, and /api/traces/{id}, but the gRPC Search RPC
// (search.go) — which attaches the SAME fail-closed wide-projection byte
// budget via chclient.WithDrainByteBudget — had no equivalent test. A
// regression that dropped that `ctx = chclient.WithDrainByteBudget(...)`
// line would leave the traffic Grafana's Tempo gRPC datasource actually
// sends draining wide ResourceAttributes/SpanAttributes maps unbounded, and
// nothing in CI would catch it.
//
// Search is the only gRPC RPC implemented today (the other six return
// codes.Unimplemented via the embedded UnimplementedStreamingQuerierServer —
// see service.go); this is a single-row ratchet rather than a table because
// there is only one drain path to cover. A future RPC that drains wide maps
// (e.g. a streaming trace-by-ID lookup) must add a row here the same way the
// HTTP ratchet gained /api/traces/{id} in review.
func TestSearch_AttachesByteBudget_NoBypass(t *testing.T) {
	t.Parallel()

	q := &stubCursorQuerier{}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := drainSearch(t, stream); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if !q.sawByteBudget {
		t.Errorf("gRPC Search did not attach the wide-projection drain byte budget — a wide-map drain would run uncharged")
	}
}
