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
	"github.com/tsouza/cerberus/internal/chsql"
)

// chGridNativeGuardError builds the error chain chclient produces when
// RangeBucketGridNative's own throwIf guard fires: a
// *chclient.ThrowIfError wrapping the typed *clickhouse.Exception (code
// 395), never a string match — mirrors chMemLimitError's own shape
// (handler_memory_limit_test.go) for the sibling guard family.
func chGridNativeGuardError() error {
	return &chclient.ThrowIfError{
		Message: chsql.RangeBucketGridNativeBudgetMessage,
		Cause: &clickhouse.Exception{
			Code:    395,
			Name:    "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			Message: chsql.RangeBucketGridNativeBudgetMessage + ": while executing 'FUNCTION throwIf(...)",
		},
	}
}

// chGridNativeDensityGuardError is chGridNativeGuardError's counterpart for
// RangeBucketGridNative's SECOND, density-aware bound (issue #2523) — same
// shape, the OTHER guard's message, proving the two are wired as distinct,
// independently-detected cases in classifyThrowIfGuardError rather than one
// message accidentally also matching the other's prefix.
func chGridNativeDensityGuardError() error {
	return &chclient.ThrowIfError{
		Message: chsql.RangeBucketGridNativeDensityBudgetMessage,
		Cause: &clickhouse.Exception{
			Code:    395,
			Name:    "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			Message: chsql.RangeBucketGridNativeDensityBudgetMessage + ": while executing 'FUNCTION throwIf(...)",
		},
	}
}

// gridNativeGuardCursor yields its canned samples, then terminates with
// the RangeBucketGridNative resource-bound rejection — the mid-stream
// shape a real query_range cursor drain sees when the guard fires
// (mirrors memLimitCursor's own shape). guardErr selects which of the two
// independent RangeBucketGridNative guards' errors this cursor drain ends
// on.
type gridNativeGuardCursor struct {
	samples  []chclient.Sample
	idx      int
	cur      chclient.Sample
	guardErr error
}

func (c *gridNativeGuardCursor) Next() bool {
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *gridNativeGuardCursor) Sample() chclient.Sample { return c.cur }
func (c *gridNativeGuardCursor) Err() error {
	if c.idx >= len(c.samples) {
		if c.guardErr != nil {
			return c.guardErr
		}
		return chGridNativeGuardError()
	}
	return nil
}
func (c *gridNativeGuardCursor) Close() error     { return nil }
func (c *gridNativeGuardCursor) Inspected() int64 { return int64(c.idx) }

// gridNativeGuardQuerier reuses stubQuerier for every endpoint except
// QueryCursor, which fails the drain mid-stream with the
// RangeBucketGridNative guard rejection. guardErr, when set, overrides
// chGridNativeGuardError's default axis1 error — TestQueryRange_RangeBucketGridNativeDensityGuard422
// sets it to chGridNativeDensityGuardError() to exercise the density guard
// instead.
type gridNativeGuardQuerier struct {
	stubQuerier
	guardErr error
}

func (q *gridNativeGuardQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &gridNativeGuardCursor{samples: q.samples, guardErr: q.guardErr}, nil
}

// assertGridNativeGuard422 decodes a Prom error envelope and pins the
// resource-exhausted contract for RangeBucketGridNative's own guard: HTTP
// 422, errorType "execution", the exact guard message — the same wire
// shape every other throwIf resource bound in classifyThrowIfGuardError
// already gets (see handler_memory_limit_test.go's assertMemoryLimit422
// for the sibling real-ClickHouse-OOM contract).
func assertGridNativeGuard422(t *testing.T, resp *http.Response) {
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
	if body.Error != chsql.RangeBucketGridNativeBudgetMessage {
		t.Fatalf("error message: got %q, want %q", body.Error, chsql.RangeBucketGridNativeBudgetMessage)
	}
}

// assertGridNativeDensityGuard422 is assertGridNativeGuard422's counterpart
// for the density guard's own distinct message.
func assertGridNativeDensityGuard422(t *testing.T, resp *http.Response) {
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
	if body.Error != chsql.RangeBucketGridNativeDensityBudgetMessage {
		t.Fatalf("error message: got %q, want %q", body.Error, chsql.RangeBucketGridNativeDensityBudgetMessage)
	}
}

// TestQueryRange_RangeBucketGridNativeGuard422 is the regression pin for
// issue #2522's wire-shape half: RangeBucketGridNative's own throwIf
// resource bound (#2486/#2504, chsql.RangeBucketGridNativeBudgetMessage)
// was never wired into classifyThrowIfGuardError, so every real firing of
// this guard fell through to the generic 502 errorType=internal branch —
// exactly the 502 body run 32688649627's nightly dashboard job observed —
// instead of the intended 422 errorType=execution every sibling resource
// bound already gets. Proves the wiring fix: a query_range cursor drain
// that aborts with this guard's message now answers 422, not 502.
func TestQueryRange_RangeBucketGridNativeGuard422(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &gridNativeGuardQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			},
		},
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
	assertGridNativeGuard422(t, resp)
}

// TestQueryRange_RangeBucketGridNativeDensityGuard422 is issue #2523's own
// wire-shape regression pin, mirroring TestQueryRange_RangeBucketGridNativeGuard422
// exactly but for the SECOND, density-aware bound this issue adds
// (chsql.RangeBucketGridNativeDensityBudgetMessage) — wired into
// classifyThrowIfGuardError from the moment the guard was added, so this
// pins that it was never left to fall through to the generic 502 the way
// #2522 found the first guard had been.
func TestQueryRange_RangeBucketGridNativeDensityGuard422(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &gridNativeGuardQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			},
		},
	}
	q.guardErr = chGridNativeDensityGuardError()
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
	assertGridNativeDensityGuard422(t, resp)
}
