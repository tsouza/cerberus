//go:build chdb

package chclienttest

import "github.com/tsouza/cerberus/internal/chclient"

// sliceCursor is the in-memory chclient.Cursor used by Client.QueryCursor.
// Mirrors the production rowsCursor lifecycle: Next advances, Sample
// yields the current row, Err reports any saved error, Close is
// idempotent.
type sliceCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	// drained counts Next() calls that returned true — the rows the consumer
	// pulled off the cursor. Mirrors the production rowsCursor.seen counter so
	// the boundsdrain harness can read a drain count from the chdb test cursor
	// exactly as it does from the prod cursor.
	drained int64
}

func newSliceCursor(samples []chclient.Sample) *sliceCursor {
	return &sliceCursor{samples: samples, idx: -1}
}

func (c *sliceCursor) Next() bool {
	c.idx++
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.drained++
	return true
}

func (c *sliceCursor) Sample() chclient.Sample { return c.cur }
func (c *sliceCursor) Err() error              { return nil }
func (c *sliceCursor) Close() error            { return nil }

// Inspected returns the number of rows the consumer pulled — the count of
// Next() calls that returned true. Matches the production cursor's seen-based
// drain count so a streaming-path test reads the buffer size identically.
func (c *sliceCursor) Inspected() int64 { return c.drained }
