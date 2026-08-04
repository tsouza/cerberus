//go:build chdb

// chDB-backed variant of TestTail_OverflowRowsNotDropped
// (tail_overflow_internal_test.go). The stub-backed test proves the
// overflow *policy* — a truncated poll window must be re-queried on the
// next tick rather than skipped — against cursorAwareQuerier, a fake
// that hand-parses the inlined SQL text and returns exactly the rows a
// real ClickHouse would for the given cursor/limit. That proves the
// policy without ever reaching the *condition*: a real producer
// (ClickHouse rows already sitting in a table) outrunning a real
// consumer (the WebSocket client draining them at a fixed limit per
// poll).
//
// This file drives the identical scenario — five rows sharing one poll
// window, a tail opened with limit=2 so the first poll's batch is
// truncated — through the real handler, real buildTailSQL, and a real
// chDB-backed Querier. Because production's per-tick query is fully
// parameterised (?-placeholders via chsql, not the inlined literals the
// stub's regex has to reverse-engineer), no cursorAwareQuerier-style
// SQL-parsing shim is needed here: chDB just answers the query it
// receives, so a bug in the cursor-advance arithmetic (the historical
// regression this pins) would surface exactly as it would in
// production — truncated delivery, not a query-shape mismatch.
//
// Lives in package loki (not loki_test) so it can reuse the unexported
// tailPollInterval var from tail.go, and must NOT t.Parallel() for the
// same reason tail_overflow_internal_test.go can't: it mutates that
// package-global.

package loki

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// tailOverflowLogsDDL is the minimal otel_logs projection buildTailSQL
// touches: Timestamp (poll-window bound + ORDER BY), Body (the line),
// and ResourceAttributes (the `{job="api"}` stream-selector target).
// Engine = Memory: the tail poll is a straight SELECT ... WHERE
// Timestamp BETWEEN ... ORDER BY Timestamp LIMIT n, no MergeTree
// features involved.
const tailOverflowLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;`

// TestTail_ChDB_OverflowRowsNotDropped is the chDB-backed sibling of
// TestTail_OverflowRowsNotDropped. Five rows sharing one poll window are
// seeded directly into a real chDB otel_logs table; the tail is opened
// with limit=2 and an explicit `start` pinned before the earliest row,
// so the first poll's window (which covers all five rows) comes back
// truncated. A correct cursor-advance re-queries the remainder on
// subsequent polls instead of jumping the cursor to `end` and silently
// dropping it — see the nextCursor comment in runTailLoop (tail.go)
// for the exact invariant this exercises.
//
// The rows are timestamped in the FIXED PAST (relative to the seed
// moment) rather than a few milliseconds in the future: chDB table
// creation + INSERT is not instantaneous the way the stub querier's
// in-memory slice is, so anchoring rows to "dial time + a few ms" (as
// the stub sibling does) risks the seed itself outrunning the offset
// and the rows already being in the past by the time the tail dials.
// Passing an explicit `start` query parameter — set once, before
// seeding starts — sidesteps that race entirely: the first poll's
// window is [start, now], and now is always well after every seeded
// row's timestamp by the time the WebSocket is dialed.
func TestTail_ChDB_OverflowRowsNotDropped(t *testing.T) {
	// Speed up polling so the five rows drain quickly across ticks.
	prev := tailPollInterval
	tailPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tailPollInterval = prev })

	base := time.Now().UTC()
	const dt64Layout = "2006-01-02 15:04:05.000000000"
	want := []string{"line-1", "line-2", "line-3", "line-4", "line-5"}

	var inserts strings.Builder
	inserts.WriteString("INSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES\n")
	for i, line := range want {
		ts := base.Add(time.Duration(i) * time.Millisecond).Format(dt64Layout)
		comma := ","
		if i == len(want)-1 {
			comma = ";"
		}
		fmt.Fprintf(&inserts, "    (toDateTime64('%s', 9), '%s', map('job', 'api'))%s\n", ts, line, comma)
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, tailOverflowLogsDDL+inserts.String())

	h := New(c, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// start is pinned to the earliest row's timestamp (inclusive lower
	// bound on the tail's first poll) so the whole 5-row window is in
	// scope regardless of how long seeding took.
	conn := dialTailFrom(t, srv, `{job="api"}`, 2, base)

	// Collect lines until all five arrive or a short deadline elapses.
	// Assert each is received exactly once and in ascending order —
	// identical assertions to the stub-backed sibling, now proven
	// against a real engine.
	var got []string
	seen := map[string]int{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(got) < len(want) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var chunk struct {
			Streams []struct {
				Values [][]any `json:"values"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			t.Fatalf("unmarshal: %v (payload=%s)", err, payload)
		}
		for _, st := range chunk.Streams {
			for _, pair := range st.Values {
				if len(pair) == 2 {
					if line, ok := pair[1].(string); ok {
						seen[line]++
						got = append(got, line)
					}
				}
			}
		}
	}

	if len(got) != len(want) {
		t.Fatalf("overflow rows dropped: received %d of %d lines (got=%v); pre-fix code delivers only the first 2", len(got), len(want), got)
	}
	for _, line := range want {
		if seen[line] != 1 {
			t.Errorf("line %q received %d times, want exactly 1 (got=%v)", line, seen[line], got)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("lines not received in ascending order: %v", got)
	}
}

// dialTailFrom is dialTailLimit (tail_overflow_internal_test.go) plus
// an explicit `start` query parameter, encoded as Unix nanoseconds (the
// logcli convention format.ParseTimeUnixScaled recognises). Kept local
// to this file since only the chDB variant needs to pin the poll
// window's lower bound independently of the dial moment.
func dialTailFrom(t *testing.T, srv *httptest.Server, query string, limit int, start time.Time) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/loki/api/v1/tail"
	qv := url.Values{}
	qv.Set("query", query)
	qv.Set("limit", strconv.Itoa(limit))
	qv.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	u.RawQuery = qv.Encode()

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (status=%d)", err, resp.StatusCode)
		}
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
