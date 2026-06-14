package chclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// staleConn models the ch-pod-kill failure: the first failUntil Query /
// Ping calls return brokenErr (a stale pooled conn handed out by acquire
// after a restart), and every call after that succeeds (the retry re-dialled
// a fresh conn). It counts calls so a test can assert how many attempts the
// retry made.
//
// It is the unit-test stand-in for clickhouse-go's pool behaviour: each
// failed Query against a stale conn evicts that conn (release(conn, err) →
// close), so the NEXT acquire dials fresh — modelled here by simply making
// the next call succeed once failUntil attempts are spent. failForever makes
// every call fail (a genuinely-down backend) so the breaker-trip path stays
// covered.
type staleConn struct {
	brokenErr   error
	failUntil   int32 // first N calls fail with brokenErr, then succeed
	failForever bool  // every call fails (models a dead backend)
	calls       atomic.Int32
}

func (c *staleConn) fail() error {
	n := c.calls.Add(1)
	if c.failForever {
		return c.brokenErr
	}
	if n <= c.failUntil {
		return c.brokenErr
	}
	return nil
}

func (c *staleConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	if err := c.fail(); err != nil {
		return nil, err
	}
	return &chaosRows{}, nil
}

func (c *staleConn) Ping(context.Context) error { return c.fail() }

func (c *staleConn) Contributors() []string { return nil }
func (c *staleConn) ServerVersion() (*driver.ServerVersion, error) {
	return &driver.ServerVersion{}, nil
}
func (c *staleConn) Select(context.Context, any, string, ...any) error { return c.fail() }
func (c *staleConn) QueryRow(context.Context, string, ...any) driver.Row {
	return chaosRow{c.fail()}
}

func (c *staleConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, c.fail()
}
func (c *staleConn) Exec(context.Context, string, ...any) error              { return c.fail() }
func (c *staleConn) AsyncInsert(context.Context, string, bool, ...any) error { return c.fail() }
func (c *staleConn) Stats() driver.Stats                                     { return driver.Stats{} }
func (c *staleConn) Close() error                                            { return nil }

// errBrokenPipe is the canonical broken-conn error a stale pooled conn to a
// restarted CH pod surfaces (clickhouse-go classifies EPIPE / ECONNRESET /
// EOF / *net.OpError as broken). Wrapped so the test also exercises the
// errors.Is chain-walk isBrokenConnError relies on.
var errBrokenPipe = fmt.Errorf("clickhouse: send data: %w", syscall.EPIPE)

// TestQueryCursor_StaleConn_RecoversSilently — a single stale pooled conn
// (one broken-pipe failure, then a fresh conn succeeds) is absorbed by the
// retry: QueryCursor returns a usable cursor, and — critically — the breaker
// records a SUCCESS, not a failure, so a lone stale conn can never advance
// the breaker toward OPEN. This is the core of the ch-pod-kill flap fix.
func TestQueryCursor_StaleConn_RecoversSilently(t *testing.T) {
	t.Parallel()
	conn := &staleConn{brokenErr: errBrokenPipe, failUntil: 1}
	client := newWithConn(conn)

	cursor, err := client.QueryCursor(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("QueryCursor: %v; want silent recovery after one stale conn", err)
	}
	_ = cursor.Close()

	if got := conn.calls.Load(); got != 2 {
		t.Errorf("conn.calls = %d; want 2 (1 stale + 1 fresh retry)", got)
	}
	if st := client.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q; want closed (a recovered stale conn is no failure)", st)
	}
}

// TestQuery_StaleConn_DoesNotTripBreaker — even a burst of queries, each
// hitting ONE stale conn before recovering, must never trip the breaker:
// the default threshold is 5 consecutive failures, but every query recovers
// on retry so the failure counter stays at 0. Without the retry each of
// these would have been a recorded failure and the 5th would OPEN the
// breaker — exactly the flap the chaos lane saw.
func TestQuery_StaleConn_DoesNotTripBreaker(t *testing.T) {
	t.Parallel()
	conn := &staleConn{brokenErr: errBrokenPipe}
	client := newWithConn(conn)

	const queries = breakerThreshold * 3 // well past the trip threshold
	for i := 0; i < queries; i++ {
		// Each query sees exactly one fresh stale conn: bump failUntil so the
		// next single call fails, then succeeds on retry.
		conn.failUntil = conn.calls.Load() + 1
		if _, err := client.Query(context.Background(), "SELECT 1"); err != nil {
			t.Fatalf("Query %d: %v; want silent recovery", i, err)
		}
		if st := client.br.currentState(); st != "closed" {
			t.Fatalf("after query %d breaker state = %q; want closed", i, st)
		}
	}
}

// TestQuery_DeadBackend_StillTripsBreaker — a genuinely-down CH (every
// attempt a broken-conn error, including all retries) must STILL trip the
// breaker. The retry only absorbs a recovered stale conn; an outage where
// the fresh dial also fails exhausts the bound, returns the broken-conn
// error to record(), and after breakerThreshold such failures the breaker
// OPENs. This proves the fix doesn't mask a real outage.
func TestQuery_DeadBackend_StillTripsBreaker(t *testing.T) {
	t.Parallel()
	conn := &staleConn{brokenErr: errBrokenPipe, failForever: true}
	client := newWithConn(conn)

	for i := 0; i < breakerThreshold; i++ {
		if _, err := client.Query(context.Background(), "SELECT 1"); err == nil {
			t.Fatalf("Query %d: nil error; want broken-conn error from a dead backend", i)
		}
	}
	if st := client.br.currentState(); st != "open" {
		t.Errorf("breaker state = %q; want open after %d dead-backend failures", st, breakerThreshold)
	}
	// Each failing query made maxTransportRetries+1 attempts before giving up.
	wantAttempts := int32(breakerThreshold * (maxTransportRetries + 1))
	if got := conn.calls.Load(); got != wantAttempts {
		t.Errorf("conn.calls = %d; want %d (%d queries × %d attempts)",
			got, wantAttempts, breakerThreshold, maxTransportRetries+1)
	}
}

// TestQuery_ManyStaleConns_DrainedInOneRequest — a pool holding several
// stale conns is drained within a single request: the retry re-acquires past
// each evicted bad conn until it lands a fresh one, all within
// maxTransportRetries. The query succeeds and the breaker stays closed.
func TestQuery_ManyStaleConns_DrainedInOneRequest(t *testing.T) {
	t.Parallel()
	conn := &staleConn{brokenErr: errBrokenPipe, failUntil: maxTransportRetries}
	client := newWithConn(conn)

	if _, err := client.Query(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Query: %v; want recovery within %d retries", err, maxTransportRetries)
	}
	if st := client.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q; want closed", st)
	}
}

// TestPing_StaleConn_RecoversSilently — the readiness-probe path (Ping) gets
// the same stale-conn recovery, so /readyz doesn't flap red on a transient
// stale conn after a restart.
func TestPing_StaleConn_RecoversSilently(t *testing.T) {
	t.Parallel()
	conn := &staleConn{brokenErr: errBrokenPipe, failUntil: 1}
	client := newWithConn(conn)

	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v; want silent recovery after one stale conn", err)
	}
	if st := client.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q; want closed", st)
	}
}

// TestIsBrokenConnError pins the classification contract: the broken-conn
// shapes a restarted backend produces are retryable; a server-side typed
// exception, a context error, an acquire-timeout, and ErrCircuitOpen are NOT
// (they must surface on the first attempt rather than burning retries).
func TestIsBrokenConnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected-eof", io.ErrUnexpectedEOF, true},
		{"epipe", syscall.EPIPE, true},
		{"econnreset", syscall.ECONNRESET, true},
		{"econnrefused", syscall.ECONNREFUSED, true},
		{"wrapped-epipe", fmt.Errorf("send data: %w", syscall.EPIPE), true},
		{"net-op-error", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"wrapped-net-op-error", fmt.Errorf("dial ch: %w", &net.OpError{Op: "read", Err: io.EOF}), true},
		{"ch-exception", &clickhouse.Exception{Code: 241, Message: "memory limit"}, false},
		{"wrapped-ch-exception", fmt.Errorf("query: %w", &clickhouse.Exception{Code: 159}), false},
		{"context-canceled", context.Canceled, false},
		{"context-deadline", context.DeadlineExceeded, false},
		{"acquire-timeout", clickhouse.ErrAcquireConnTimeout, false},
		{"circuit-open", ErrCircuitOpen, false},
		{"plain-error", errors.New("some other error"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBrokenConnError(tc.err); got != tc.want {
				t.Errorf("isBrokenConnError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWithTransportRetry_StopsOnCtxCancel — a cancelled ctx halts the retry
// loop immediately rather than burning the full retry budget against a
// backend the caller has already abandoned.
func TestWithTransportRetry_StopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	_, err := withTransportRetry(ctx, func() (struct{}, error) {
		calls++
		return struct{}{}, errBrokenPipe
	})
	if !errors.Is(err, syscall.EPIPE) {
		t.Errorf("err = %v; want the broken-pipe error surfaced", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d; want 1 (cancelled ctx stops the retry loop)", calls)
	}
}
