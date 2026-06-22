package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// memoryLimitWant is the exact wire message the Prom head must emit
// for a ClickHouse MEMORY_LIMIT_EXCEEDED (code 241) abort under the
// 1 GiB per-query cap. Hardcoded here — not built from the handler's
// helper — so a drift in the production message fails this pin. The
// k3d/compose stacks set CERBERUS_CH_QUERY_MAX_MEMORY=1073741824, and
// test/e2e/playwright/iterate-time-ranges.spec.ts pins this same
// string as its 422 contract; the three must stay in lock-step.
const memoryLimitWant = "query processing would use too much memory in query execution (ClickHouse memory limit exceeded; per-query cap 1073741824 bytes)"

// chMemLimitError builds the error chain chclient produces when
// ClickHouse rejects a data-plane query with code 241: a
// *chclient.MemoryLimitError wrapping the typed *clickhouse.Exception
// (never a string match — see internal/chclient/memlimit.go).
func chMemLimitError(limit int64) error {
	return &chclient.MemoryLimitError{
		Limit: limit,
		Cause: &clickhouse.Exception{
			Code:    241,
			Name:    "MEMORY_LIMIT_EXCEEDED",
			Message: "Memory limit (total) exceeded: would use 2.12 GiB, maximum: 1.80 GiB",
		},
	}
}

// memLimitCursor yields its canned samples, then terminates with the
// memory-limit rejection — the mid-stream shape from k3d run
// 27277793810, where ClickHouse killed the 24h/15s matrix query after
// streaming began (cursor.Err() returned `code: 241`).
type memLimitCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	limit   int64
	err     error
}

func (c *memLimitCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.idx >= len(c.samples) {
		c.err = chMemLimitError(c.limit)
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *memLimitCursor) Sample() chclient.Sample { return c.cur }
func (c *memLimitCursor) Err() error              { return c.err }
func (c *memLimitCursor) Close() error            { return nil }
func (c *memLimitCursor) Inspected() int64        { return int64(c.idx) }

// memLimitQuerier reuses stubQuerier for every endpoint except
// QueryCursor, which fails the drain mid-stream with the
// memory-limit rejection.
type memLimitQuerier struct {
	stubQuerier
	limit int64
}

func (q *memLimitQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &memLimitCursor{samples: q.samples, limit: q.limit}, nil
}

// assertMemoryLimit422 decodes a Prom error envelope and pins the
// resource-exhausted contract: HTTP 422, errorType "execution", the
// exact memory-limit message — the same wire shape as the sample
// budget (#746), because both are per-query resource rejections from
// a healthy backend.
func assertMemoryLimit422(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = resp.Body.Close()
	if body.Status != "error" {
		t.Fatalf("status field: got %q, want \"error\"", body.Status)
	}
	if body.ErrorType != "execution" {
		t.Fatalf("errorType: got %q, want \"execution\"", body.ErrorType)
	}
	if body.Error != memoryLimitWant {
		t.Fatalf("error message: got %q, want %q", body.Error, memoryLimitWant)
	}
}

// TestQueryRange_MemoryLimit422 — the regression pin for k3d run
// 27277793810: when ClickHouse aborts the query_range cursor drain
// mid-stream with MEMORY_LIMIT_EXCEEDED (code 241), the handler must
// answer with the resource-exhausted rejection — 422
// errorType=execution naming the per-query cap — instead of the 502
// errorType=internal the run produced.
func TestQueryRange_MemoryLimit422(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &memLimitQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			},
		},
		limit: 1 << 30,
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf(
		"%s/api/v1/query_range?query=up&start=%d&end=%d&step=15",
		srv.URL, ts.Add(-time.Hour).Unix(), ts.Unix(),
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertMemoryLimit422(t, resp)
}

// TestQuery_MemoryLimit422 — instant-query sibling: a 241 raised at
// query open arrives through engine.Query wrapped as
// `engine: execute: ...` and must map onto the identical 422 contract.
func TestQuery_MemoryLimit422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: chMemLimitError(1 << 30)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertMemoryLimit422(t, resp)
}
