package chclient

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// maxTransportRetries bounds how many EXTRA times a read-path call is
// re-driven against the pool after a broken-connection error before the
// breaker is told the call failed. The first attempt plus this many retries
// means a call makes at most maxTransportRetries+1 dials.
//
// This mirrors Go's database/sql, which is the canonical precedent for the
// exact failure this fixes. When a driver returns driver.ErrBadConn ("the
// server having earlier closed the connection"), database/sql transparently
// retries on a fresh connection up to maxBadConnRetries=2 pooled attempts and
// then one final guaranteed-fresh dial — IMMEDIATELY, with NO backoff,
// because the server is up and a fresh dial succeeds at once (delaying would
// only slow recovery). clickhouse-go's NATIVE clickhouse.Conn interface (the
// one cerberus uses, not the database/sql wrapper) does NOT get that retry —
// the broken-conn error surfaces straight to the caller — so cerberus
// replicates the database/sql contract here. 3 ≈ database/sql's 2-pooled +
// 1-fresh shape; the upper bound also lets a single request drain a small
// pool of stale conns left by a restarted pod.
//
// Why this is needed (the ch-pod-kill flap, run 27509796946). When the
// ClickHouse pod is killed and recreated (k3d Recreate, PVC-backed), the new
// pod comes up at a fresh IP but the OLD pod's TCP connections are NOT cleanly
// closed — they sit in ESTABLISHED with no FIN/RST. clickhouse-go's acquire()
// validates a pooled conn with a non-blocking socket read (conn_check.go); an
// idle-but-not-yet-noticed-dead socket returns EAGAIN, so isBad() reports the
// STALE conn healthy and hands it out. The query against it then fails with a
// broken-conn error, and clickhouse-go evicts THAT conn (release(conn, err) →
// close) — but the failure has ALREADY surfaced to chclient, which recorded it
// against the breaker. With several stale conns pooled, each first use trips
// one breaker failure; in bursts that re-opens the breaker and it flaps
// open↔half-open↔closed for the full recovery deadline. Retrying re-acquires
// PAST the just-evicted bad conn (dialling fresh, or grabbing another conn the
// next failed attempt likewise evicts), so a handful of stale conns is
// absorbed silently instead of re-tripping the breaker.
//
// No backoff, and a genuine outage still trips the breaker. A dead backend
// fails the dial on EVERY attempt, so the bounded loop costs at most
// maxTransportRetries+1 fast immediate dials and then returns the broken-conn
// error to record() — so the breaker trips exactly as before for a real
// outage. Recovery from a restart is fast and deterministic; a real CH-down is
// not masked.
const maxTransportRetries = 3

// isBrokenConnError reports whether err is a broken-CONNECTION failure — the
// pooled conn (or the dial to a still-recovering pod) failed at the transport
// layer, so a retry on a fresh conn can transparently absorb it. It mirrors
// clickhouse-go's own (unexported) isConnBrokenError predicate — io.EOF /
// EPIPE / ECONNRESET / *net.OpError — the exact set the driver uses to decide
// a conn is no longer usable, plus ECONNREFUSED (a fresh dial to a pod still
// in warm-up) and io.ErrUnexpectedEOF (a half-read response on a dropped
// conn). Using the SAME classification the driver evicts on keeps the retry
// in lock-step with which conns the pool actually drops between attempts.
//
// It is deliberately a NARROW allow-list, not a broad "any non-Exception
// error" denylist: only errors that genuinely indicate a dead conn are
// retried. A *clickhouse.Exception (the server answered with a typed error —
// CH is alive), a pool acquire-timeout (local saturation), a context
// cancellation/deadline (the caller walked away), and ErrCircuitOpen are NOT
// broken-conn errors and must surface on the first attempt.
func isBrokenConnError(err error) bool {
	if err == nil {
		return false
	}
	// A typed server exception is positive proof CH is alive — never a
	// transport failure, even if its message happens to mention a socket.
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return false
	}
	// Caller-driven cancellation / deadline is not a CH fault; ErrCircuitOpen
	// and acquire-timeout are local signals. None are retryable.
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, clickhouse.ErrAcquireConnTimeout) ||
		errors.Is(err, ErrCircuitOpen) {
		return false
	}
	// The driver's own broken-conn set (clickhouse-go isConnBrokenError) plus
	// the dial-refused / unexpected-EOF shapes a restarted backend produces.
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// withTransportRetry invokes call and, when it fails with a broken-connection
// error (isBrokenConnError), re-invokes it on a freshly-acquired pooled conn
// up to maxTransportRetries more times — immediately, no backoff (the
// database/sql ErrBadConn precedent: the server is up, a fresh dial succeeds
// at once). It returns the result of the LAST attempt: the first
// non-broken-conn outcome (success, or a CH-semantic error that must surface),
// or — if every attempt was a broken-conn error — the final error. That
// returned error is what the caller records against the breaker, so a stale
// conn the retry recovered from never counts as a breaker failure, while a
// genuinely-down CH (every attempt a broken-conn error) still does.
//
// Each retry's call re-runs c.conn.<op>, whose acquire() skips the conn the
// previous failed attempt already evicted — so the retry dials fresh against
// the restarted backend rather than re-using the dead conn.
//
// SAFETY: only read-path opens (SELECT) and Ping route through this. SELECTs
// are idempotent, so a retry can never double-apply an effect. The write path
// (Exec: DDL/DML, incl. INSERT) deliberately does NOT retry — a broken pipe
// mid-send could have partially applied, and the database/sql ErrBadConn
// contract itself forbids retrying when "the database server might have
// performed the operation."
//
// ctx is honoured between attempts: a cancelled/expired ctx stops the retry
// loop immediately (the next call would fail with the ctx error anyway;
// short-circuiting avoids a needless dial).
func withTransportRetry[T any](ctx context.Context, call func() (T, error)) (T, error) {
	res, err := call()
	for attempt := 0; attempt < maxTransportRetries && isBrokenConnError(err); attempt++ {
		if ctx.Err() != nil {
			break
		}
		res, err = call()
	}
	return res, err
}

// queryOpen opens a result set against the pool with broken-conn retry: a
// stale pooled conn handed out by acquire() after a CH restart fails the
// initial round-trip, which clickhouse-go evicts; the retry re-acquires past
// it and dials fresh. It is the single open-time seam every read-path query
// method routes through, so the stale-conn recovery is uniform across all of
// them. The DRAIN side (rows.Err()) is deliberately NOT retried — once rows
// have started flowing a mid-stream drop can't be replayed without
// double-reading.
func (c *Client) queryOpen(ctx context.Context, sql string, args ...any) (driver.Rows, error) {
	return withTransportRetry(ctx, func() (driver.Rows, error) {
		return c.conn.Query(ctx, sql, args...)
	})
}

// pingOpen pings the pool with broken-conn retry so a stale pooled conn does
// not surface a transport error to the readiness probe / breaker when a fresh
// dial would succeed. Ping is a read-only round-trip, so retrying is safe.
func (c *Client) pingOpen(ctx context.Context) error {
	_, err := withTransportRetry(ctx, func() (struct{}, error) {
		return struct{}{}, c.conn.Ping(ctx)
	})
	return err
}
