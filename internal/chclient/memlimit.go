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
// when it enforces a cap, so these errors are breaker-neutral (see
// breaker.record) and the API heads map them onto the same
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

// isMemoryLimitExceeded reports whether err is a ClickHouse
// MEMORY_LIMIT_EXCEEDED rejection — either already wrapped as a
// *MemoryLimitError or still the raw *clickhouse.Exception with code
// 241 (the form breaker.record sees, since the breaker observes the
// driver error before chclient wraps it).
func isMemoryLimitExceeded(err error) bool {
	if errors.Is(err, ErrMemoryLimitExceeded) {
		return true
	}
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && ex.Code == chCodeMemoryLimitExceeded
}
