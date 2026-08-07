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
	"sync"
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
// The rows are minted lazily, on the first polling query, anchored to that
// query's own lower bound — which IS the moment the tail was dialled. Fixing
// the row timestamps to a wall clock read before the server was even started
// would race: on a loaded runner the dial can land after the anchor, putting
// every row below the tail's lower bound and delivering nothing at all.
type cursorAwareQuerier struct {
	// lines are the log lines to mint, in ascending order.
	lines []string
	// leadTime places the first row that far after the tail's start, so
	// the earliest polls see an empty window and the one that follows
	// covers every row at once.
	leadTime time.Duration
	// spacing separates consecutive rows, keeping them unambiguous after
	// the nanosecond cursor bump.
	spacing time.Duration

	// mu guards the fields below against the polling goroutine racing the
	// test goroutine's read.
	mu     sync.Mutex
	master []chclient.Sample // minted on first poll, sorted ASC by Timestamp
	// truncated records whether any single window held more matching rows
	// than its LIMIT admitted. That window IS the bug's trigger, so a run
	// that never produced one proves nothing.
	truncated bool
}

// sawTruncatedWindow reports whether some poll window overflowed its LIMIT.
func (q *cursorAwareQuerier) sawTruncatedWindow() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.truncated
}

// mint seeds the row set relative to start, once.
func (q *cursorAwareQuerier) mint(start time.Time) []chclient.Sample {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.master == nil {
		q.master = make([]chclient.Sample, len(q.lines))
		for i, line := range q.lines {
			q.master[i] = chclient.Sample{
				MetricName: line,
				Labels:     map[string]string{"job": "api"},
				Timestamp:  start.Add(q.leadTime + time.Duration(i)*q.spacing),
			}
		}
	}
	return q.master
}

var (
	dt64Re  = regexp.MustCompile(`toDateTime64\('([^']+)', 9\)`)
	limitRe = regexp.MustCompile(`LIMIT (\d+)`)
)

const dt64Layout = "2006-01-02 15:04:05.000000000"

const (
	// tailRowLeadTime is how far after the tail's start the seeded rows
	// begin. It is a whole number of poll intervals so the polls before it
	// see an empty window, leaving the cursor parked below the rows.
	tailRowLeadTime = 20 * time.Millisecond
	// tailRowSpacing separates consecutive seeded rows.
	tailRowSpacing = time.Millisecond
)

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

	master := q.mint(lower)
	matched := 0
	out := make([]chclient.Sample, 0, len(master))
	for _, s := range master {
		ts := s.Timestamp.UTC()
		if (ts.Equal(lower) || ts.After(lower)) && (ts.Equal(upper) || ts.Before(upper)) {
			matched++
			if limit <= 0 || len(out) < limit {
				out = append(out, s)
			}
		}
	}
	if limit > 0 && matched > limit {
		q.mu.Lock()
		q.truncated = true
		q.mu.Unlock()
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
// (they open one lead time after the tail's start, so the polls before that
// see nothing and leave the cursor parked below them, and the next poll's
// [cursor, now] covers them all) but the tail is opened with limit=2. A
// correct cursor-advance
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

	// Seed five rows with DISTINCT timestamps packed into a tight 4ms span
	// that opens tailRowLeadTime after the tail's start, so the earliest
	// polls see an empty window and the one after covers ALL five at once —
	// the over-limit window the bug drops. tailRowSpacing keeps them
	// unambiguous after the nanosecond cursor bump. The querier anchors them
	// to the start it observes rather than to a wall clock read here, which
	// setup latency could already have overtaken.
	want := []string{"line-1", "line-2", "line-3", "line-4", "line-5"}
	q := &cursorAwareQuerier{lines: want, leadTime: tailRowLeadTime, spacing: tailRowSpacing}

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

	if !q.sawTruncatedWindow() {
		t.Fatalf("no poll window ever exceeded its LIMIT (got=%v) — the rows drained one window "+
			"at a time, so this run never reached the over-limit path the guard exists for", got)
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
