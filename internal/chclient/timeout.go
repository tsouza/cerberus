package chclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chCodeTimeoutExceeded is ClickHouse's TIMEOUT_EXCEEDED server error
// code (ErrorCodes.cpp: 159). The server raises it when a query crosses
// the per-query `max_execution_time` setting cerberus stamps on
// data-plane queries (see Config.QueryTimeoutSeconds) while
// `timeout_overflow_mode` is `throw` (the cerberus default). It is the
// wall-clock sibling of MEMORY_LIMIT_EXCEEDED (code 241): a per-query
// resource cap the server enforces, NOT a transport failure — ClickHouse
// is alive and healthy when it aborts an over-long query.
const chCodeTimeoutExceeded = 159

// settingMaxExecutionTime is the ClickHouse per-query wall-clock cap
// (seconds). Stamped on every data-plane query when a query timeout is
// configured (Config.QueryTimeoutSeconds > 0) so a pathological query
// gets a server-side deadline instead of holding a pooled connection +
// admit slot for its full unbounded duration.
const settingMaxExecutionTime = "max_execution_time"

// settingTimeoutOverflowMode names the ClickHouse setting that selects
// what the server does when `max_execution_time` is crossed. Cerberus
// sends `throw` (the value of timeoutOverflowModeThrow) so the query is
// ABORTED with TIMEOUT_EXCEEDED (code 159) rather than silently returning
// partial results (`break`) — partial results would be a correctness lie
// to a Prom/Loki/Tempo client expecting a complete answer or an error.
const (
	settingTimeoutOverflowMode = "timeout_overflow_mode"
	timeoutOverflowModeThrow   = "throw"
)

// ErrQueryTimeout is the sentinel matched (via errors.Is) when a
// data-plane ClickHouse query was aborted by the server for exceeding
// its wall-clock cap (CH error code 159, TIMEOUT_EXCEEDED). It is the
// wall-clock sibling of [ErrMemoryLimitExceeded]: a per-query resource
// rejection, NOT a transport failure — ClickHouse is alive and healthy
// when it enforces a cap, so these errors are breaker-neutral (they classify
// as breakerScopeStatement — see breaker_classify.go) and the API heads map them onto the head-idiomatic
// timeout wire shapes (prom 503 errorType=timeout, loki/tempo
// equivalents).
//
// The concrete error is *QueryTimeoutError, which wraps this sentinel
// and carries the configured per-query timeout.
var ErrQueryTimeout = errors.New("query execution timeout exceeded")

// QueryTimeoutError is the concrete error chclient surfaces when
// ClickHouse rejects a data-plane query with TIMEOUT_EXCEEDED (code
// 159). It wraps [ErrQueryTimeout] (errors.Is matches) and the
// underlying *clickhouse.Exception (errors.As still reaches it), and
// carries the configured per-query timeout so API handlers can render
// head-idiomatic over-budget messages.
type QueryTimeoutError struct {
	// Timeout is the per-query `max_execution_time` cap the query ran
	// under (Config.QueryTimeoutSeconds, or a smaller per-request
	// ?timeout= override). 0 means no cap was configured on the Client —
	// the rejection came from a ClickHouse server-side limit instead.
	Timeout time.Duration
	// Cause is the underlying ClickHouse error — typically the
	// *clickhouse.Exception carrying code 159 and the server's
	// "Timeout exceeded: elapsed … maximum: …" message.
	Cause error
}

func (e *QueryTimeoutError) Error() string {
	if e.Timeout > 0 {
		return fmt.Sprintf(
			"chclient: query execution timeout exceeded: ClickHouse aborted the query for exceeding the per-query execution time limit (%s)",
			e.Timeout,
		)
	}
	return "chclient: query execution timeout exceeded: ClickHouse aborted the query for exceeding a server-side execution time limit"
}

// Unwrap exposes both the sentinel (for errors.Is) and the underlying
// ClickHouse exception (for errors.As against *clickhouse.Exception).
func (e *QueryTimeoutError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrQueryTimeout}
	}
	return []error{ErrQueryTimeout, e.Cause}
}

// wrapQueryTimeout converts a raw driver error into a *QueryTimeoutError
// when (and only when) the error chain carries a ClickHouse exception
// with code 159 (TIMEOUT_EXCEEDED). Every other error passes through
// untouched. timeout is the cap the query ran under, recorded on the
// wrapper so handlers can name it in their rejection messages.
//
// Detection is typed — errors.As against *clickhouse.Exception, never
// string matching — so a query whose result data happens to contain
// "Timeout exceeded" cannot be misclassified.
func wrapQueryTimeout(err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == chCodeTimeoutExceeded {
		return &QueryTimeoutError{Timeout: timeout, Cause: err}
	}
	return err
}

type queryTimeoutKeyType struct{}

var queryTimeoutKey = queryTimeoutKeyType{}

// settingMaxBlockSize names the ClickHouse setting that caps the number of
// rows a source operator emits per block. A per-request override is
// threaded via WithMaxBlockSize for callers that need the server to
// re-check between-block resource caps (e.g. max_execution_time) at a
// finer granularity than the default 65505-row block — used by the
// chaos_sleep build so a sleepEachRow over a numbers() source is read as
// single-row blocks (per-block sleep stays under max_sleep_in_seconds)
// and max_execution_time aborts it part-way through with code 159.
const settingMaxBlockSize = "max_block_size"

type maxBlockSizeKeyType struct{}

var maxBlockSizeKey = maxBlockSizeKeyType{}

// WithMaxBlockSize returns a ctx carrying a per-request max_block_size
// override (rows per source block). A non-positive value is inert (the
// server default applies). Like WithQueryTimeout it is a context value,
// not a Client field, so concurrent requests can't cross-contaminate.
func WithMaxBlockSize(ctx context.Context, rows uint64) context.Context {
	return context.WithValue(ctx, maxBlockSizeKey, rows)
}

// maxBlockSizeFromContext returns the per-request override installed by
// WithMaxBlockSize, or 0 when none was set.
func maxBlockSizeFromContext(ctx context.Context) uint64 {
	n, _ := ctx.Value(maxBlockSizeKey).(uint64)
	return n
}

// WithQueryTimeout returns a ctx that overrides the Client's configured
// per-query execution timeout for this one request. The API heads call
// it when an inbound request carries the standard Prometheus
// `?timeout=<duration>` query param (min'd with the configured default,
// matching upstream Prometheus): the smaller of the two becomes the
// per-request `max_execution_time` the data-plane query runs under.
//
// The signal is a context value rather than a Client field so it is
// per-request, not per-connection: two concurrent requests with
// different ?timeout= values must not cross-contaminate each other's
// settings. A non-positive duration clears any override (the configured
// default applies).
func WithQueryTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, queryTimeoutKey, d)
}

// QueryTimeoutFromContext returns the per-request override installed by
// WithQueryTimeout, and whether one was installed at all.
//
// It is exported so the layer that installs the carrier can prove it did.
// Without a reader, deleting the WithQueryTimeout call in an HTTP handler
// changes nothing a test can see — the context deadline installed beside
// it still fires and the handler still unblocks — while ClickHouse keeps
// burning CPU on a query nobody is waiting for.
func QueryTimeoutFromContext(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(queryTimeoutKey).(time.Duration)
	return d, ok
}

// classifyDriverErr maps a raw clickhouse-go driver error onto the typed
// per-query resource rejections chclient surfaces — *MemoryLimitError
// (code 241), *ThrowIfError (code 395), and *QueryTimeoutError (code 159)
// — so the API heads can map each onto its head-idiomatic wire shape. Any
// other error (or nil) passes through untouched. The three codes are
// mutually exclusive, so the order of the checks does not matter; the
// timeout wrapper is given the effective per-query cap (ctx override
// min'd with the configured default) so its message names the real
// budget the query ran under.
//
// It is the single wrap site every data-plane query method routes its
// open-time and drain-time errors through, replacing the bare
// wrapMemoryLimit call so the memory + throwIf + timeout classifications
// stay in lock-step across all of them.
func (c *Client) classifyDriverErr(ctx context.Context, err error) error {
	return wrapQueryTimeout(wrapThrowIf(wrapMemoryLimit(err, c.maxMemory)), c.effectiveQueryTimeout(ctx))
}

// hiddenDeadlineContext hides ctx's own Deadline() from anything reading it
// downstream — Done(), Err() and Value() all delegate straight through to
// ctx unchanged, so cancellation still fires at exactly the same instant it
// always did; only the Deadline() ACCESSOR reports "none".
//
// This exists to work around a clickhouse-go/v2 behaviour that otherwise
// silently overrides cerberus's own per-query `max_execution_time` setting.
// clickhouse-go/v2's queryOptions() (context.go) derives its own
// `max_execution_time` from the ctx it is handed — `time.Until(ctx.
// Deadline()) + 5s` — and stamps it into the outgoing settings map whenever
// ctx carries a Deadline() more than a second away, UNCONDITIONALLY
// clobbering whatever value querySettings already placed there. Because that
// derived cap is, by construction, always LATER than ctx's own Done() firing
// time (ctx-remaining + 5s is always > ctx-remaining), ClickHouse's clean
// server-side abort (TIMEOUT_EXCEEDED, code 159 — the very thing
// classifyDriverErr/wrapQueryTimeout exist to classify) can never win the
// race against the caller's own Go ctx: every data-plane query that runs its
// full configured budget ends up cancelled through the client-ctx path
// instead. That path is itself racy inside the driver (conn_process.go races
// the outer ctx.Done() select against the raw socket's own
// ctx.Deadline()-derived read/write deadline — see startReadWriteTimeout),
// and when the raw-socket side of that race wins, ClickHouse never receives
// a protocol-level Cancel packet at all: it only notices the dropped TCP
// connection, which surfaces server-side as a NetException instead of a
// clean cancellation. This was confirmed against a live ClickHouse instance:
// the same request-shaped query flipped between a clean
// QUERY_WAS_CANCELLED_BY_CLIENT (code 735) and a bare "client has dropped
// the connection" NetException (code 236) across otherwise-identical runs,
// with ClickHouse's own max_execution_time never getting a chance to fire.
//
// Hiding Deadline() from the driver disarms both the auto-override AND the
// internal race: chclient's own querySettings-computed `max_execution_time`
// (or its deliberate absence, when no timeout is configured — see
// Config.QueryTimeout's "0 = don't set the setting" contract) reaches
// ClickHouse unmodified and gets the first chance to abort the query
// cleanly; the caller's Go ctx remains the backstop it was always meant to
// be (queryContext / reqctx.ApplyQueryTimeout's own doc comment), unblocking
// a hung call exactly when it always did, via the SAME Done() channel.
func hiddenDeadlineContext(ctx context.Context) context.Context {
	if _, ok := ctx.Deadline(); !ok {
		return ctx
	}
	return noDeadlineContext{ctx}
}

// noDeadlineContext embeds a context.Context so Done() / Err() / Value() all
// delegate to it unchanged, while Deadline() is overridden to report "none".
// See hiddenDeadlineContext.
type noDeadlineContext struct {
	context.Context
}

func (noDeadlineContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

// effectiveQueryTimeout resolves the wall-clock cap a query runs under:
// the Client's configured default (c.queryTimeout) unless ctx carries a
// per-request WithQueryTimeout override, in which case the SMALLER of
// the two wins (matching Prometheus's `min(query.timeout, ?timeout=)`).
// Returns 0 when no cap applies at all (timeout disabled and no
// override), so querySettings can omit max_execution_time entirely.
func (c *Client) effectiveQueryTimeout(ctx context.Context) time.Duration {
	def := c.queryTimeout
	override, _ := QueryTimeoutFromContext(ctx)
	switch {
	case override <= 0:
		return def
	case def <= 0:
		return override
	case override < def:
		return override
	default:
		return def
	}
}
