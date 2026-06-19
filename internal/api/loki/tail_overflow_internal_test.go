package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// cursorAwareQuerier faithfully emulates ClickHouse for the /tail polling
// query. Unlike tailStubQuerier (which returns canned chunks and ignores the
// cursor), this fake parses the inlined time bounds and LIMIT out of the SQL
// string and returns exactly the rows a real CH would for that window:
//
//	rows WHERE cursor <= Timestamp <= end, ORDER BY Timestamp ASC, LIMIT n
//
// The time bounds are rendered inline as `toDateTime64('YYYY-MM-DD
// HH:MM:SS.fffffffff', 9)` literals (lower = cursor `>=`, upper = end `<=`);
// the limit is rendered as `LIMIT n` (NOT positional args). This is what lets
// the test catch the overflow-drop bug: when a window holds more than `limit`
// matching rows, a correct cursor-advance must re-query the truncated tail on
// the next poll instead of skipping past `end`.
type cursorAwareQuerier struct {
	master []chclient.Sample // seeded, sorted ASC by Timestamp
}

var (
	dt64Re  = regexp.MustCompile(`toDateTime64\('([^']+)', 9\)`)
	limitRe = regexp.MustCompile(`LIMIT (\d+)`)
)

const dt64Layout = "2006-01-02 15:04:05.000000000"

func (q *cursorAwareQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	bounds := dt64Re.FindAllStringSubmatch(sql, -1)
	if len(bounds) != 2 {
		// Not the tail polling query (or shape changed); return nothing
		// rather than guess.
		return nil, nil
	}
	lower, err := time.Parse(dt64Layout, bounds[0][1])
	if err != nil {
		return nil, err
	}
	upper, err := time.Parse(dt64Layout, bounds[1][1])
	if err != nil {
		return nil, err
	}

	limit := -1
	if m := limitRe.FindStringSubmatch(sql); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, err
		}
		limit = n
	}

	out := make([]chclient.Sample, 0, len(q.master))
	for _, s := range q.master {
		ts := s.Timestamp.UTC()
		if (ts.Equal(lower) || ts.After(lower)) && (ts.Equal(upper) || ts.Before(upper)) {
			out = append(out, s)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// The remaining Querier methods are unused by /tail; satisfy the interface.
func (q *cursorAwareQuerier) QueryIndexStats(context.Context, string, ...any) (chclient.IndexStatsRow, error) {
	return chclient.IndexStatsRow{}, nil
}

func (q *cursorAwareQuerier) QueryIndexVolume(context.Context, string, ...any) ([]chclient.IndexVolumeRow, error) {
	return nil, nil
}

func (q *cursorAwareQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return nil, nil
}

func (q *cursorAwareQuerier) QueryDetectedFieldRows(context.Context, string, ...any) ([]chclient.DetectedFieldRow, error) {
	return nil, nil
}

func (q *cursorAwareQuerier) QueryTimestampedLines(context.Context, string, ...any) ([]chclient.TimestampedLine, error) {
	return nil, nil
}

func (q *cursorAwareQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return nil, nil
}

// TestTail_OverflowRowsNotDropped is the regression guard for the
// poll-window-exceeds-limit data-loss bug. Five rows share ONE poll window
// (all timestamps in the past, so the very first poll's [cursor, now] covers
// them all) but the tail is opened with limit=2. A correct cursor-advance
// must drain the truncated overflow across subsequent polls and deliver all
// five lines exactly once, in order. The pre-fix code seeded the cursor at
// `end` and jumped past the whole window, delivering only the first 2 of 5.
//
// This test lives in package loki (internal) so it can shorten the unexported
// tailPollInterval; it must NOT t.Parallel() because it mutates that package
// global.
func TestTail_OverflowRowsNotDropped(t *testing.T) {
	// Speed up polling so the five rows drain quickly across ticks.
	prev := tailPollInterval
	tailPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tailPollInterval = prev })

	// Seed five rows with DISTINCT timestamps clustered just after "now".
	// The tail's default lower bound (`start`) is the dial moment, so the
	// rows must sit at/after it; they're packed into a tight 4ms span a few
	// ms in the future so that within a couple of 10ms polls a single
	// window's upper bound (`end = now`) covers ALL of them at once — the
	// over-limit window the bug drops. 1ms spacing keeps them unambiguous
	// after the nanosecond cursor bump.
	base := time.Now().UTC().Add(20 * time.Millisecond)
	want := []string{"line-1", "line-2", "line-3", "line-4", "line-5"}
	master := make([]chclient.Sample, len(want))
	for i, line := range want {
		master[i] = chclient.Sample{
			MetricName: line,
			Labels:     map[string]string{"job": "api"},
			Timestamp:  base.Add(time.Duration(i) * time.Millisecond),
		}
	}
	q := &cursorAwareQuerier{master: master}

	h := New(q, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	conn := dialTailLimit(t, srv, `{job="api"}`, 2)

	// Collect lines until all five arrive or a short deadline elapses. Assert
	// each is received exactly once and in ascending order.
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

// dialTailLimit is dialTail with an explicit ?limit=. Kept local to the
// internal test so it can set the limit param the overflow case needs.
func dialTailLimit(t *testing.T, srv *httptest.Server, query string, limit int) *websocket.Conn {
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
