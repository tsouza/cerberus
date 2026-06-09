package chclient

import (
	"context"
	"fmt"
	"sync"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/trace"
)

// Cursor is a forward-only iterator over a Sample result set. Use it to
// stream rows out of ClickHouse without materialising the full slice in
// process memory — the canonical pattern for `query_range` matrix
// responses, where a long-window / fine-step query can produce millions
// of rows.
//
// Lifecycle: call Next() in a loop; while it returns true, Sample()
// yields the current row. When Next() returns false, check Err() — a
// non-nil value means the iterator terminated due to a decode or
// transport error rather than end-of-stream. Close() releases the
// underlying CH resources and MUST be called exactly once, typically via
// `defer cursor.Close()` immediately after a successful QueryCursor.
type Cursor interface {
	Next() bool
	Sample() Sample
	Err() error
	Close() error
}

// rowsCursor wraps a driver.Rows and decodes each row positionally into a
// Sample. The driver's Rows is itself an iterator over the wire stream,
// so allocations per row stay bounded — only the current Sample is kept
// in memory.
type rowsCursor struct {
	rows driver.Rows
	cur  Sample
	err  error
	// span is the `execute` pipeline-stage span opened by QueryCursor.
	// Held by the cursor (rather than closed when QueryCursor returns)
	// so that row decode + CH wire transit are billed to the execute
	// stage — the iteration loop is part of the round-trip's cost.
	span trace.Span
	// rec is the progress recorder latched at QueryCursor time. The
	// cursor flushes it on Close so the rows/bytes_read histograms
	// observe the per-query total exactly once — irrespective of how
	// long the caller takes to drain rows.
	rec *progressRecorder
	// closeOnce serialises Close() so the span / rec / rows nil-outs
	// happen exactly once, even under concurrent Close calls (e.g. a
	// caller's `defer cursor.Close()` racing a context-cancellation
	// path that also calls Close).
	closeOnce sync.Once
	closeErr  error
}

// Next advances the cursor to the next row. Returns false when the
// stream is exhausted or when a decode error occurred; in the error case
// Err() returns the cause.
func (c *rowsCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if !c.rows.Next() {
		if err := c.rows.Err(); err != nil {
			c.err = fmt.Errorf("chclient: rows.Err: %w", err)
		}
		return false
	}
	var s Sample
	var labels map[string]string
	if err := c.rows.Scan(&s.MetricName, &labels, &s.Timestamp, &s.Value); err != nil {
		c.err = fmt.Errorf("chclient: scan: %w", err)
		return false
	}
	s.Labels = labels
	c.cur = s
	return true
}

// Sample returns the row that the most recent Next() call landed on.
// Calling Sample before Next, or after Next has returned false, yields
// the zero value.
func (c *rowsCursor) Sample() Sample { return c.cur }

// Err returns any non-EOF error that terminated iteration. It is safe to
// call after Close.
func (c *rowsCursor) Err() error { return c.err }

// Close releases the underlying driver.Rows. Safe to call multiple
// times AND from multiple goroutines concurrently; the underlying
// teardown runs exactly once via sync.Once, and subsequent calls return
// the same error the first call returned.
//
// The first call also flushes the progress recorder so the per-query
// rows/bytes histograms see the totals exactly once. Subsequent calls
// are no-ops.
func (c *rowsCursor) Close() error {
	c.closeOnce.Do(func() {
		if c.span != nil {
			if c.err != nil {
				c.span.RecordError(c.err)
			}
			c.span.End()
			c.span = nil
		}
		if c.rec != nil {
			c.rec.flush()
			c.rec = nil
		}
		if c.rows == nil {
			return
		}
		err := c.rows.Close()
		c.rows = nil
		if err != nil {
			c.closeErr = fmt.Errorf("chclient: rows.Close: %w", err)
		}
	})
	return c.closeErr
}

// QueryCursor runs sql with positional args and returns a forward-only
// Cursor over the result set. The SQL must project (MetricName,
// Attributes, TimeUnix, Value) in that order — Scan binds positionally,
// matching Client.Query.
//
// Compared to Query, QueryCursor keeps only one Sample resident in
// process memory at a time, which is the only way to keep RAM bounded
// for long-window `query_range` requests. Callers MUST Close the cursor
// to return its connection to the pool.
//
// Guarded by the circuit breaker (see [Client] doc). The breaker
// observes the open-call outcome only — once the cursor is returned,
// iteration errors propagate via cursor.Err() but are NOT re-recorded
// against the breaker. A single failed query is one failure, not N
// where N is the number of rows the caller drained before hitting the
// transport drop.
func (c *Client) QueryCursor(ctx context.Context, sql string, args ...any) (Cursor, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	rows, err := c.conn.Query(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		span.End()
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	return &rowsCursor{rows: rows, span: span, rec: recorderFromContext(ctx)}, nil
}
