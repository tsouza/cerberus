package chclient

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/actuals"
)

func TestShardQueryID_RoundTrips(t *testing.T) {
	const request = "4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-17"
	for _, tc := range []struct{ index, count int }{{0, 1}, {0, 4}, {3, 4}, {11, 12}} {
		id := ShardQueryID(request, tc.index, tc.count)
		got, ok := ParseShardQueryID(id)
		want := ShardQueryIDParts{Request: request, Index: tc.index, Count: tc.count}
		if !ok || got != want {
			t.Errorf("ParseShardQueryID(%q) = %+v, %v; want %+v, true", id, got, ok, want)
		}
	}
}

func TestParseShardQueryID_RejectsNonShardIDs(t *testing.T) {
	for _, id := range []string{
		"",
		"4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-17", // a route-A statement
		"-shard-0-of-2",       // no request
		"req-shard-0",         // no count
		"req-shard-x-of-2",    // non-numeric index
		"req-shard-0-of-y",    // non-numeric count
		"req-shard-2-of-2",    // index past the count
		"req-shard--1-of-2",   // negative index
		"req-shard-0-of-0",    // no shards
		"req-shard-0-of-2x-1", // count followed by garbage
	} {
		if got, ok := ParseShardQueryID(id); ok {
			t.Errorf("ParseShardQueryID(%q) = %+v, true; want not a shard statement's id", id, got)
		}
	}
}

// TestFreshQueryID_KeepsShardIdentity: the columnar fallback's re-dispatch of
// a shard statement is still that shard of that request, under an id no other
// statement uses.
func TestFreshQueryID_KeepsShardIdentity(t *testing.T) {
	original := ShardQueryID("req", 2, 5)
	ctx := WithQueryID(context.Background(), original)

	first, ctx := freshQueryID(ctx)
	second, _ := freshQueryID(ctx)
	for _, id := range []string{first, second} {
		got, ok := ParseShardQueryID(id)
		if want := (ShardQueryIDParts{Request: "req", Index: 2, Count: 5}); !ok || got != want {
			t.Errorf("re-dispatch id %q parses to %+v, %v; want %+v", id, got, ok, want)
		}
	}
	if first == original || second == original || first == second {
		t.Fatalf("re-dispatch ids %q, %q are not distinct from each other and from %q", first, second, original)
	}

	// A route-A statement's re-dispatch gains no shard identity.
	plain, _ := freshQueryID(context.Background())
	if _, ok := ParseShardQueryID(plain); ok {
		t.Fatalf("route-A re-dispatch id %q parses as a shard statement's", plain)
	}
}

// TestQueryContext_PacketPathStandsDownWithoutProgressPackets pins the
// transport gate on the actuals packet path. Over the native protocol a
// captured dispatch's query_id is claimed and its packets are recorded; over
// HTTP, which streams no progress packets, the dispatch is neither claimed —
// the query-log row with its real totals stays admissible — nor recorded as
// an observation of nothing, and a routed request's fold never completes.
func TestQueryContext_PacketPathStandsDownWithoutProgressPackets(t *testing.T) {
	const shape = "cerb:agg;rw"
	for _, tc := range []struct {
		name         string
		protocol     clickhouse.Protocol
		wantClaimed  bool
		wantObserved int
	}{
		{name: "native", protocol: clickhouse.Native, wantClaimed: true, wantObserved: 1},
		{name: "http", protocol: clickhouse.HTTP, wantClaimed: false, wantObserved: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{protocol: tc.protocol}

			tracker := actuals.NewTracker(actuals.DefaultConfig())
			ctx := WithActualsCapture(WithProgressFor(context.Background(), "promql"), tracker, shape)
			ctx = c.queryContext(ctx)
			id := queryIDFromContext(ctx)
			if claimed := !tracker.ClaimQueryLogRow(id); claimed != tc.wantClaimed {
				t.Errorf("query_id claimed by the packet path = %v, want %v", claimed, tc.wantClaimed)
			}
			flushProgress(ctx)
			if n := observationsOf(tracker, shape); n != tc.wantObserved {
				t.Errorf("packet path recorded %d observations, want %d", n, tc.wantObserved)
			}

			// A two-shard routed request through the same client.
			routed := actuals.NewTracker(actuals.DefaultConfig())
			outer := WithShardActualsFold(WithActualsCapture(WithProgressFor(context.Background(), "promql"), routed, shape), 2)
			for i := range 2 {
				shardCtx := c.queryContext(WithQueryID(WithProgressFor(outer, "promql"), ShardQueryID("req", i, 2)))
				recorderFromContext(shardCtx).onProgress(&clickhouse.Progress{Rows: 10})
				flushProgress(shardCtx)
			}
			if n := observationsOf(routed, shape); n != tc.wantObserved {
				t.Errorf("routed request: packet path recorded %d observations, want %d", n, tc.wantObserved)
			}
		})
	}
}

func observationsOf(tracker *actuals.Tracker, shape string) int {
	report, ok := tracker.Snapshot(shape)
	if !ok {
		return 0
	}
	return report.Observations
}
