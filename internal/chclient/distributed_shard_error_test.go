package chclient

import (
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chShardUnavailableException builds the typed driver exception ClickHouse
// raises for ALL_CONNECTION_TRIES_FAILED — the shape
// PoolWithFailoverBase<TNestedPool>::getMany() throws once a Distributed
// data shard's usable-replica count falls below min_entries.
func chShardUnavailableException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    279,
		Name:    "ALL_CONNECTION_TRIES_FAILED",
		Message: "All connection tries failed. Log: \n\nCode: 209. DB::NetException: Timeout exceeded while connecting to socket",
	}
}

// chStaleReplicaException builds the typed driver exception ClickHouse
// raises for ALL_REPLICAS_ARE_STALE — the same getMany() call, thrown
// instead when every reachable replica is stale and
// fallback_to_stale_replicas_for_distributed_queries=0 forbids serving one.
func chStaleReplicaException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    369,
		Name:    "ALL_REPLICAS_ARE_STALE",
		Message: "Could not find enough connections to up-to-date replicas. Got: 0, needed: 1",
	}
}

// TestWrapDistributedShardErr_Code279 — a CH exception with code 279
// becomes a *ShardUnavailableError, matchable both via errors.Is (the
// ErrShardUnavailable sentinel) and errors.As (the underlying
// *clickhouse.Exception still reachable).
func TestWrapDistributedShardErr_Code279(t *testing.T) {
	t.Parallel()

	got := wrapDistributedShardErr(chShardUnavailableException())

	var shardErr *ShardUnavailableError
	if !errors.As(got, &shardErr) {
		t.Fatalf("wrapDistributedShardErr returned %T (%v); want *ShardUnavailableError", got, got)
	}
	if !errors.Is(got, ErrShardUnavailable) {
		t.Error("errors.Is(got, ErrShardUnavailable) = false; want true")
	}
	if errors.Is(got, ErrStaleReplicaFallbackDenied) {
		t.Error("errors.Is(got, ErrStaleReplicaFallbackDenied) = true; want false (codes are mutually exclusive)")
	}
	var ex *clickhouse.Exception
	if !errors.As(got, &ex) || ex.Code != 279 {
		t.Errorf("underlying *clickhouse.Exception not reachable via errors.As (ex=%v)", ex)
	}
}

// TestWrapDistributedShardErr_Code369 — a CH exception with code 369
// becomes a *StaleReplicaFallbackDeniedError, matchable both via errors.Is
// (the ErrStaleReplicaFallbackDenied sentinel) and errors.As (the
// underlying *clickhouse.Exception still reachable).
func TestWrapDistributedShardErr_Code369(t *testing.T) {
	t.Parallel()

	got := wrapDistributedShardErr(chStaleReplicaException())

	var staleErr *StaleReplicaFallbackDeniedError
	if !errors.As(got, &staleErr) {
		t.Fatalf("wrapDistributedShardErr returned %T (%v); want *StaleReplicaFallbackDeniedError", got, got)
	}
	if !errors.Is(got, ErrStaleReplicaFallbackDenied) {
		t.Error("errors.Is(got, ErrStaleReplicaFallbackDenied) = false; want true")
	}
	if errors.Is(got, ErrShardUnavailable) {
		t.Error("errors.Is(got, ErrShardUnavailable) = true; want false (codes are mutually exclusive)")
	}
	var ex *clickhouse.Exception
	if !errors.As(got, &ex) || ex.Code != 369 {
		t.Errorf("underlying *clickhouse.Exception not reachable via errors.As (ex=%v)", ex)
	}
}

// TestWrapDistributedShardErr_PassThrough — nil, non-exception errors, and
// exceptions with other codes (including the adjacent 241/159 codes the
// sibling wrappers own) pass through unchanged: classification is typed
// code-279/369-only, never string matching, and never steals a code that
// belongs to wrapMemoryLimit/wrapQueryTimeout.
func TestWrapDistributedShardErr_PassThrough(t *testing.T) {
	t.Parallel()

	if got := wrapDistributedShardErr(nil); got != nil {
		t.Errorf("wrapDistributedShardErr(nil) = %v; want nil", got)
	}

	plain := errors.New("dial tcp: connection refused, mentioning All connection tries failed")
	if got := wrapDistributedShardErr(plain); got != plain {
		t.Errorf("plain error was rewritten to %v; want pass-through (no string matching)", got)
	}

	for _, ex := range []*clickhouse.Exception{
		{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table otel.missing does not exist"},
		chMemLimitException(),
		chTimeoutException(),
	} {
		got := wrapDistributedShardErr(ex)
		if !errors.Is(got, error(ex)) {
			t.Errorf("code-%d exception was rewritten to %v; want pass-through", ex.Code, got)
		}
		if errors.Is(got, ErrShardUnavailable) || errors.Is(got, ErrStaleReplicaFallbackDenied) {
			t.Errorf("code-%d exception classified as a distributed-shard error; want only codes 279/369", ex.Code)
		}
	}
}

// TestShardUnavailableError_Unwrap — Unwrap exposes both the sentinel and
// the underlying cause, with and without a Cause set.
func TestShardUnavailableError_Unwrap(t *testing.T) {
	t.Parallel()

	withCause := &ShardUnavailableError{Cause: chShardUnavailableException()}
	if !errors.Is(withCause, ErrShardUnavailable) {
		t.Error("errors.Is(withCause, ErrShardUnavailable) = false; want true")
	}
	var ex *clickhouse.Exception
	if !errors.As(withCause, &ex) {
		t.Error("errors.As(withCause, &ex) = false; want true")
	}

	bare := &ShardUnavailableError{}
	if !errors.Is(bare, ErrShardUnavailable) {
		t.Error("errors.Is(bare, ErrShardUnavailable) = false; want true even with a nil Cause")
	}
}

// TestStaleReplicaFallbackDeniedError_Unwrap — Unwrap exposes both the
// sentinel and the underlying cause, with and without a Cause set.
func TestStaleReplicaFallbackDeniedError_Unwrap(t *testing.T) {
	t.Parallel()

	withCause := &StaleReplicaFallbackDeniedError{Cause: chStaleReplicaException()}
	if !errors.Is(withCause, ErrStaleReplicaFallbackDenied) {
		t.Error("errors.Is(withCause, ErrStaleReplicaFallbackDenied) = false; want true")
	}
	var ex *clickhouse.Exception
	if !errors.As(withCause, &ex) {
		t.Error("errors.As(withCause, &ex) = false; want true")
	}

	bare := &StaleReplicaFallbackDeniedError{}
	if !errors.Is(bare, ErrStaleReplicaFallbackDenied) {
		t.Error("errors.Is(bare, ErrStaleReplicaFallbackDenied) = false; want true even with a nil Cause")
	}
}
