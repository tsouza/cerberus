package chclient

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// columnar_early_decline_test.go — the dispatch-count pin for #2043.
//
// The columnar matrix decode declines every non-matrix shape, and a decline
// routes the query down the row path under a FRESH query_id — a second
// physical execution. The caller-declared ResponseShape is on the ctx before
// any dial, so a declared non-matrix shape must be declined BEFORE the ch-go
// pool is touched: the fallback then costs one execution, not two.
//
// These tests count both dispatch surfaces independently — the row path's
// driver.Conn.Query calls and the columnar path's TCP connections to the
// (fake) ClickHouse address — so "exactly one dispatch" is asserted as a
// behaviour rather than inferred from control flow.

// earlyDeclineFixtureRows is the row count the fake row-path conn answers
// with: enough to prove the fallback cursor actually decoded the result set,
// small enough to drain inline.
const earlyDeclineFixtureRows = 3

// countingConn is a driver.Conn fake that counts row-path dispatches — one
// per Query call — and answers each with a fixed-size generated result set.
type countingConn struct {
	chaosConn
	queries atomic.Int64
}

func (c *countingConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.queries.Add(1)
	return newGenRows(earlyDeclineFixtureRows), nil
}

// dialCounter is a TCP listener standing in for the ClickHouse address the
// columnar ch-go pool dials. It counts every accepted connection and closes
// it immediately: the columnar path's handshake then fails, but the count has
// already recorded that a dispatch was attempted — which is the whole point.
type dialCounter struct {
	ln    net.Listener
	dials atomic.Int64
}

func newDialCounter(t *testing.T) *dialCounter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &dialCounter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.dials.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return d
}

func (d *dialCounter) addr() string { return d.ln.Addr().String() }

// newCountingColumnarClient wires a Client whose decode strategy is the
// columnar one — row fallback on a counting driver.Conn, columnar pool
// pointed at the counting listener — so a test can attribute every dispatch
// to exactly one of the two paths.
func newCountingColumnarClient(t *testing.T, dc *dialCounter) (*Client, *countingConn, columnarDecoder) {
	t.Helper()
	conn := &countingConn{}
	client := newWithConn(conn)
	dec := columnarDecoder{
		matrix: newColumnarMatrixDecoder(Config{
			Addr:        dc.addr(),
			DialTimeout: columnarEarlyDeclineDialTimeout,
		}),
		fallback: rowDecoder{},
	}
	client.cursorDecoder = dec
	t.Cleanup(dec.close)
	return client, conn, dec
}

// columnarEarlyDeclineDialTimeout bounds the positive control's dial against
// a listener that answers no ClickHouse handshake, so a failed dial fails the
// test quickly instead of parking on the platform default.
const columnarEarlyDeclineDialTimeout = 2 * time.Second

// TestColumnarDecode_NonMatrixShapeDispatchesExactlyOnce is the #2043
// regression pin: a query whose caller declared a non-matrix ResponseShape
// (here Loki's log-stream shape) must reach ClickHouse EXACTLY ONCE, on the
// row path. Before the fix the columnar path dispatched first and only then
// consulted the declared shape in bindColumns, so the same query executed
// twice — once columnar, once again under a fresh query_id on the fallback.
func TestColumnarDecode_NonMatrixShapeDispatchesExactlyOnce(t *testing.T) {
	t.Parallel()

	dc := newDialCounter(t)
	client, conn, dec := newCountingColumnarClient(t, dc)

	ctx := WithResponseShape(context.Background(), "loki-streams")
	cursor, err := client.QueryCursor(ctx, "SELECT Line, Attributes, TimeUnix, Metadata FROM logs")
	// The dial count is read first so a regression reports the defect (a
	// dispatch the declared shape had already ruled out) rather than the
	// transport error that dispatch happens to surface.
	if got := dc.dials.Load(); got != 0 {
		t.Fatalf("columnar path opened %d connection(s) to ClickHouse for a declared non-matrix shape, want 0 — the ResponseShape gate is being evaluated AFTER the dispatch it was meant to prevent (#2043)", got)
	}
	if err != nil {
		t.Fatalf("QueryCursor: %v", err)
	}
	defer func() { _ = cursor.Close() }()

	// The row fallback still answers the query correctly — the fix removes a
	// wasted execution, it does not change the result.
	drained := 0
	for cursor.Next() {
		if got := cursor.Sample().MetricName; got != "up" {
			t.Fatalf("row-fallback Sample().MetricName = %q, want %q", got, "up")
		}
		drained++
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("cursor.Err: %v", err)
	}
	if drained != earlyDeclineFixtureRows {
		t.Fatalf("row fallback drained %d rows, want %d", drained, earlyDeclineFixtureRows)
	}

	if got := conn.queries.Load(); got != 1 {
		t.Fatalf("row path dispatched %d quer(ies), want exactly 1", got)
	}
	if dec.matrix.pool != nil {
		t.Fatal("columnar ch-go pool was acquired for a declared non-matrix shape — the decline must happen before acquirePool")
	}
}

// TestColumnarDecode_MatrixShapeStillDispatchesColumnar is the positive
// control for the counter above: with the matrix ResponseShape declared, the
// columnar path DOES engage and dial, so the zero-dial assertion in the
// non-matrix test is a real observation and not a harness that can never see
// a columnar dispatch at all. The dial's handshake fails (the listener speaks
// no ClickHouse protocol) — the dispatch attempt is what is being pinned.
func TestColumnarDecode_MatrixShapeStillDispatchesColumnar(t *testing.T) {
	t.Parallel()

	dc := newDialCounter(t)
	client, _, _ := newCountingColumnarClient(t, dc)

	ctx, cancel := context.WithTimeout(context.Background(), columnarEarlyDeclineDialTimeout)
	defer cancel()
	ctx = WithResponseShape(ctx, ResponseShapeMatrix)
	cursor, err := client.QueryCursor(ctx, "SELECT MetricName, Attributes, TimeUnix, Value FROM metrics")
	if cursor != nil {
		_ = cursor.Close()
	}
	if err == nil {
		t.Fatal("QueryCursor against a listener that speaks no ClickHouse protocol returned no error")
	}
	if got := dc.dials.Load(); got < 1 {
		t.Fatalf("columnar path opened %d connection(s) for the declared matrix shape, want at least 1 — the harness cannot observe a columnar dispatch, so the non-matrix zero-dial assertion would be vacuous", got)
	}
}
