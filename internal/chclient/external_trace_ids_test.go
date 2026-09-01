package chclient

import (
	"context"
	"testing"
)

// TestWithExternalTraceIDs_ReturnsDerivedContext pins the pure client-side
// half of issue #2783's helper: given a normal id set it returns a nil error
// and a ctx that carries clickhouse-go's QueryOptions value (added by
// clickhouse.Context/WithExternalTable) — a DIFFERENT context.Context value
// than the one passed in, since context.WithValue always wraps into a new
// concrete type. The wire-level behaviour (the driver actually serialising
// the table onto a live query, a real server accepting it, idx_trace_id
// pruning through it) is verified against a real ClickHouse in
// internal/api/tempo's integration lane — chclient has no live connection to
// assert against here, only that this function builds and attaches the
// table without error.
func TestWithExternalTraceIDs_ReturnsDerivedContext(t *testing.T) {
	t.Parallel()

	base := context.Background()
	ctx, err := WithExternalTraceIDs(base, "ext_trace_ids", "TraceId", []string{"aabb", "ccdd"})
	if err != nil {
		t.Fatalf("WithExternalTraceIDs: %v", err)
	}
	if ctx == base {
		t.Fatalf("WithExternalTraceIDs returned the SAME context.Context as base — clickhouse.Context should always derive a new value")
	}
}

// TestWithExternalTraceIDs_EmptyIDsStillSucceeds pins the vacuous case: an
// empty id slice still builds a valid (zero-row) external table rather than
// erroring — runStructuralTwoPhase never calls this with zero ids in
// practice (it returns before reaching restrictStructural when phase A found
// nothing), but the helper itself should not assume that invariant.
func TestWithExternalTraceIDs_EmptyIDsStillSucceeds(t *testing.T) {
	t.Parallel()

	if _, err := WithExternalTraceIDs(context.Background(), "ext_trace_ids", "TraceId", nil); err != nil {
		t.Fatalf("WithExternalTraceIDs(nil ids): %v", err)
	}
}
