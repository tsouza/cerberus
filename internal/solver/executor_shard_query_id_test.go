package solver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestExecute_ShardQueryIDsCarryTheRequestIdentity pins the identity a
// query-log reader folds a routed request's shard rows by: every shard
// statement's query_id names the same request, its own shard index, and the
// request's shard count, and that id is the one the shard's dispatch carries.
func TestExecute_ShardQueryIDsCarryTheRequestIdentity(t *testing.T) {
	const k = 4
	q := newFakeQuerier(1)
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	first, ok := chclient.ParseShardQueryID(info.ShardQueryIDs[0])
	if !ok {
		t.Fatalf("shard 0 query_id %q carries no shard identity", info.ShardQueryIDs[0])
	}
	for i, id := range info.ShardQueryIDs {
		got, ok := chclient.ParseShardQueryID(id)
		if !ok {
			t.Fatalf("shard %d query_id %q carries no shard identity", i, id)
		}
		want := chclient.ShardQueryIDParts{Request: first.Request, Index: i, Count: k}
		if got != want {
			t.Errorf("shard %d query_id %q parses to %+v, want %+v", i, id, got, want)
		}
		if dispatched := chclient.QueryIDFromContext(q.ctxByShard[i]); dispatched != id {
			t.Errorf("shard %d dispatched under query_id %q, want the recorded %q", i, dispatched, id)
		}
	}

	// A second request gets its own identity, so two requests' rows never fold
	// together.
	cur2, info2, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if _, err := drainAll(cur2); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	_ = cur2.Close()
	second, _ := chclient.ParseShardQueryID(info2.ShardQueryIDs[0])
	if second.Request == first.Request {
		t.Fatalf("two routed requests share the request id %q", first.Request)
	}
}
