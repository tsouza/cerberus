package chclient

import (
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chCodeMemoryLimitExceeded is ClickHouse's MEMORY_LIMIT_EXCEEDED server
// error code (ErrorCodes.cpp: 241). The server raises it when a query
// crosses either the per-query `max_memory_usage` setting cerberus
// stamps on data-plane queries (see Config.MaxQueryMemoryBytes) or a
// server-side cap (`max_server_memory_usage` / the OvercommitTracker) —
// the k3d dashboard run 27277793810 hit the latter mid-stream on a
// 24h/15s matrix query.
const chCodeMemoryLimitExceeded = 241

// ErrMemoryLimitExceeded is the sentinel matched (via errors.Is) when a
// data-plane ClickHouse query was aborted by the server for exceeding a
// memory limit (CH error code 241, MEMORY_LIMIT_EXCEEDED). It is the
// memory-side sibling of [ErrTooManySamples]: a per-query resource
// rejection, NOT a transport failure — ClickHouse is alive and healthy
// when it enforces a cap, so these errors are breaker-neutral (they classify
// as breakerScopeStatement — see breaker_classify.go) and the API heads map them onto the same
// resource-exhausted wire shapes as the sample budget (prom 422
// errorType=execution, loki 400 limit-style, tempo 422).
//
// The concrete error is *MemoryLimitError, which wraps this sentinel
// and carries the configured per-query cap.
var ErrMemoryLimitExceeded = errors.New("query memory limit exceeded")

// MemoryLimitError is the concrete error chclient surfaces when
// ClickHouse rejects a data-plane query with MEMORY_LIMIT_EXCEEDED
// (code 241). It wraps [ErrMemoryLimitExceeded] (errors.Is matches)
// and the underlying *clickhouse.Exception (errors.As still reaches
// it), and carries the configured per-query cap so API handlers can
// render head-idiomatic over-limit messages.
type MemoryLimitError struct {
	// Limit is the per-query `max_memory_usage` cap (bytes) the Client
	// was configured with (Config.MaxQueryMemoryBytes). 0 means no
	// per-query cap was configured — the rejection came from a
	// ClickHouse server-side limit instead.
	Limit int64
	// Cause is the underlying ClickHouse error — typically the
	// *clickhouse.Exception carrying code 241 and the server's
	// "Memory limit (…) exceeded" message.
	Cause error
}

func (e *MemoryLimitError) Error() string {
	if e.Limit > 0 {
		return fmt.Sprintf(
			"chclient: query memory limit exceeded: ClickHouse aborted the query for exceeding the per-query memory limit (%d bytes)",
			e.Limit,
		)
	}
	return "chclient: query memory limit exceeded: ClickHouse aborted the query for exceeding a server-side memory limit"
}

// Unwrap exposes both the sentinel (for errors.Is) and the underlying
// ClickHouse exception (for errors.As against *clickhouse.Exception).
func (e *MemoryLimitError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrMemoryLimitExceeded}
	}
	return []error{ErrMemoryLimitExceeded, e.Cause}
}

// wrapMemoryLimit converts a raw driver error into a *MemoryLimitError
// when (and only when) the error chain carries a ClickHouse exception
// with code 241 (MEMORY_LIMIT_EXCEEDED). Every other error passes
// through untouched. limit is the Client's configured per-query cap,
// recorded on the wrapper so handlers can name it in their rejection
// messages.
//
// Detection is typed — errors.As against *clickhouse.Exception, never
// string matching — so a query whose result data happens to contain
// "Memory limit" cannot be misclassified.
func wrapMemoryLimit(err error, limit int64) error {
	if err == nil {
		return nil
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == chCodeMemoryLimitExceeded {
		return &MemoryLimitError{Limit: limit, Cause: err}
	}
	return err
}

// ApportionMemoryBytes divides a configured `max_memory_usage` cap by
// divisor, clamped to a minimum of 1 — ClickHouse treats a literal 0 as
// UNLIMITED, the exact opposite of "apportion the cap". A divisor below 1
// (an unset field, or a construction path that never sets one) is treated
// as 1, an exact no-op divide, rather than dividing by zero.
//
// This is the ONE apportionment formula cerberus's per-shard admission
// control uses, shared by both sites a `Distributed`-engine read needs it
// (cerberus issues #3081, #3122) so they can never independently drift:
//
//   - Client.querySettings (this package) apportions the client-wide
//     MaxQueryMemoryBytes cap by Config.DataShardCount ALONE, for every
//     data-plane query the solver never split ("route A" —
//     internal/solver/executor.go's terminology). A single logical
//     statement still fans out across every data shard once
//     DataShardCount > 1, so it needs apportioning even with no solver
//     split at all (kEff == 1, in the solver's own vocabulary).
//   - internal/solver/executor.go's Execute apportions the SAME cap
//     (read back via MaxQueryMemoryBytes) by kEff x DataShardCount for a
//     genuine K-shard fan-out, so total cluster-wide exposure across every
//     concurrently-running shard never exceeds route A's own single-
//     statement total.
func ApportionMemoryBytes(cap, divisor int64) int64 {
	if divisor < 1 {
		divisor = 1
	}
	v := cap / divisor
	if v < 1 {
		// Only reachable if cap < divisor — an unrealistic (byte-scale cap,
		// or an extreme shard/fan-out count) configuration. Guards against
		// stamping a literal 0, which ClickHouse's max_memory_usage setting
		// treats as UNLIMITED.
		v = 1
	}
	return v
}
