package chclient

import (
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// This file is cerberus issue #3078's ClickHouse Distributed-query error
// taxonomy extension. It follows the exact typed-wrap pattern memlimit.go
// (code 241) and timeout.go (code 159) already establish: a chCode<Name>
// constant naming the verified ClickHouse ErrorCodes.cpp entry, an
// Err<Name> sentinel matched via errors.Is, a <Name>Error wrapper carrying
// the underlying *clickhouse.Exception (errors.As still reaches it), and a
// wrap<Name> converter consulted from classifyDriverErr. Detection is
// always typed (errors.As against *clickhouse.Exception), never string
// matching against err.Error().
//
// The two codes below were verified against ClickHouse's own source
// (src/Common/PoolWithFailoverBase.h's getMany(), which every Distributed
// query's connection-acquisition path runs through), not assumed from the
// issue's own description: the issue named a third code,
// "SHARD_HAS_NO_REPLICAS", that DOES NOT EXIST anywhere in the ClickHouse
// codebase (a `search/code` sweep of github.com/ClickHouse/ClickHouse
// returned zero hits) — the nearest same-named constant,
// SHARD_HAS_NO_CONNECTIONS (297, src/Interpreters/Cluster.cpp), is a
// cluster-config PARSE-time error ("No cluster elements (shard, node)
// specified in config"), never raised by a running query, so it is not
// used here. The real runtime "a shard has no usable replica" code is
// ALL_CONNECTION_TRIES_FAILED (279), already named correctly elsewhere in
// the same issue sentence — this file uses that one, and separately covers
// the issue's own "replica-staleness path" with ALL_REPLICAS_ARE_STALE
// (369), which getMany() throws from the SAME function for the distinct
// "reachable but stale" condition.

// chCodeAllConnectionTriesFailed is ClickHouse's ALL_CONNECTION_TRIES_FAILED
// server error code (src/Common/ErrorCodes.cpp: 279). Verified directly
// against src/Common/PoolWithFailoverBase.h's
// PoolWithFailoverBase<TNestedPool>::getMany(): once fewer than min_entries
// replicas of a shard came back usable, it throws
// `NetException(ALL_CONNECTION_TRIES_FAILED, "All connection tries failed.
// Log: \n\n{}\n", fail_messages)`. With cerberus's own pinned
// skip_unavailable_shards=0 (settingSkipUnavailableShards's own doc) every
// shard's min_entries is 1, so this fires the instant one Distributed data
// shard has NO reachable replica at all — the partial-shard-failure
// scenario this issue's error taxonomy exists to translate.
//
// It is ALREADY a breakerScopeServerHealth code in breaker_classify.go
// (chproto.ErrAllConnectionTriesFailed) — a real per-shard outage is
// exactly the "backend cannot serve this replica's traffic" condition the
// breaker exists to trip on — so this file adds no breaker-classification
// change for it, only the typed wrapper the API heads consume.
const chCodeAllConnectionTriesFailed = 279

// chCodeAllReplicasAreStale is ClickHouse's ALL_REPLICAS_ARE_STALE server
// error code (src/Common/ErrorCodes.cpp: 369). Verified against the same
// getMany(): when up_to_date_count stays below min_entries AND
// fallback_to_stale_replicas_for_distributed_queries is false, it throws
// `Exception(ALL_REPLICAS_ARE_STALE, "Could not find enough connections to
// up-to-date replicas. Got: {}, needed: {}", ...)` instead of silently
// widening the candidate set to stale replicas. This is the "replica-
// staleness path" this issue's Problem statement names, and it is only
// REACHABLE at all because of cerberus's own
// settingFallbackToStaleReplicasForDistributedQueries=0 pin — ClickHouse's
// OWN default for that setting is 1 (silently serve a stale replica), under
// which this code can never surface.
const chCodeAllReplicasAreStale = 369

// ErrShardUnavailable is the sentinel matched (via errors.Is) when a
// Distributed-engine query aborted because ClickHouse could not reach any
// replica of at least one data shard (CH error code 279,
// ALL_CONNECTION_TRIES_FAILED). It signals partial ClickHouse-cluster
// infrastructure trouble, not a cerberus fault and not a resource-limit
// rejection — the initiator is healthy, one data shard behind the
// Distributed wrapper table is not. The concrete error is
// *ShardUnavailableError.
var ErrShardUnavailable = errors.New("distributed shard has no reachable replica")

// ShardUnavailableError is the concrete error chclient surfaces when
// ClickHouse rejects a data-plane query with ALL_CONNECTION_TRIES_FAILED
// (code 279). It wraps [ErrShardUnavailable] (errors.Is matches) and the
// underlying *clickhouse.Exception (errors.As still reaches it), whose
// Message carries ClickHouse's own per-replica connection-failure log.
type ShardUnavailableError struct {
	// Cause is the underlying ClickHouse error — the *clickhouse.Exception
	// carrying code 279 and the server's "All connection tries failed"
	// message.
	Cause error
}

func (e *ShardUnavailableError) Error() string {
	return "chclient: distributed shard unavailable: ClickHouse could not reach any replica of at least one data shard (skip_unavailable_shards=0 — cerberus issue #3078)"
}

// Unwrap exposes both the sentinel (for errors.Is) and the underlying
// ClickHouse exception (for errors.As against *clickhouse.Exception).
func (e *ShardUnavailableError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrShardUnavailable}
	}
	return []error{ErrShardUnavailable, e.Cause}
}

// ErrStaleReplicaFallbackDenied is the sentinel matched (via errors.Is)
// when a Distributed-engine query aborted because every reachable replica
// of at least one data shard was stale and cerberus's own
// fallback_to_stale_replicas_for_distributed_queries=0 pin forbids silently
// answering from one anyway (CH error code 369, ALL_REPLICAS_ARE_STALE).
// ClickHouse is healthy — a replica genuinely lagged replication — and this
// is cerberus's own correctness posture (fail loud, never silently stale)
// doing its job, not an outage. The concrete error is
// *StaleReplicaFallbackDeniedError.
var ErrStaleReplicaFallbackDenied = errors.New("distributed replica staleness fallback denied")

// StaleReplicaFallbackDeniedError is the concrete error chclient surfaces
// when ClickHouse rejects a data-plane query with ALL_REPLICAS_ARE_STALE
// (code 369). It wraps [ErrStaleReplicaFallbackDenied] (errors.Is matches)
// and the underlying *clickhouse.Exception (errors.As still reaches it).
type StaleReplicaFallbackDeniedError struct {
	// Cause is the underlying ClickHouse error — the *clickhouse.Exception
	// carrying code 369 and the server's "Could not find enough
	// connections to up-to-date replicas" message.
	Cause error
}

func (e *StaleReplicaFallbackDeniedError) Error() string {
	return "chclient: distributed replica staleness fallback denied: every reachable replica of at least one data shard is stale and fallback_to_stale_replicas_for_distributed_queries=0 (cerberus issue #3078)"
}

// Unwrap exposes both the sentinel (for errors.Is) and the underlying
// ClickHouse exception (for errors.As against *clickhouse.Exception).
func (e *StaleReplicaFallbackDeniedError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrStaleReplicaFallbackDenied}
	}
	return []error{ErrStaleReplicaFallbackDenied, e.Cause}
}

// wrapDistributedShardErr converts a raw driver error into a
// *ShardUnavailableError or *StaleReplicaFallbackDeniedError when (and only
// when) the error chain carries a ClickHouse exception with a matching
// code. Every other error passes through untouched.
//
// Detection is typed — errors.As against *clickhouse.Exception, mirroring
// wrapMemoryLimit / wrapQueryTimeout — never string matching, so a query
// whose result data happens to contain either error's own wording cannot be
// misclassified. The two codes are mutually exclusive (ClickHouse raises at
// most one per getMany() call), so check order does not matter.
func wrapDistributedShardErr(err error) error {
	if err == nil {
		return nil
	}
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) {
		return err
	}
	switch ex.Code {
	case chCodeAllConnectionTriesFailed:
		return &ShardUnavailableError{Cause: err}
	case chCodeAllReplicasAreStale:
		return &StaleReplicaFallbackDeniedError{Cause: err}
	default:
		return err
	}
}
