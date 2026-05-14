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
	return true
}

func (c *sliceCursor) Sample() chclient.Sample { return c.cur }
func (c *sliceCursor) Err() error              { return nil }
func (c *sliceCursor) Close() error            { return nil }
