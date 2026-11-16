package seed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// WaitLokiHoldsFixture's contract: every pushed stream reads back over
// [SeedStart, LogQueryEnd()) with exactly the pushed entry timestamps. These
// tests serve the range query from a stub that, like a reference Loki still
// ingesting, returns part of a stream before it returns all of it.

// stubLokiRange answers /loki/api/v1/query_range for the streams it holds.
// held(poll) returns how many of each stream's entries, oldest first, the
// stub returns on that poll.
type stubLokiRange struct {
	streams []Stream
	held    func(poll int) int

	mu     sync.Mutex
	polls  int
	starts []string
	ends   []string
}

func (s *stubLokiRange) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		s.mu.Lock()
		// One poll queries every stream; the poll index advances when the
		// first stream is asked for again.
		if LogSelector(s.streams[0].Labels) == q.Get("query") {
			s.polls++
		}
		poll := s.polls
		s.starts = append(s.starts, q.Get("start"))
		s.ends = append(s.ends, q.Get("end"))
		s.mu.Unlock()

		var result []LogRangeStream
		for _, st := range s.streams {
			if LogSelector(st.Labels) != q.Get("query") {
				continue
			}
			n := min(s.held(poll), len(st.Entries))
			values := make([][]string, 0, n)
			for _, e := range st.Entries[:n] {
				values = append(values, []string{strconv.FormatInt(e.Time.UnixNano(), 10), e.Line})
			}
			result = append(result, LogRangeStream{Labels: st.Labels, Values: values})
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": result}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *stubLokiRange) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

// gateWindow is a fixture window whose streams carry one entry per sample
// time, ending at AnchorEnd.
func gateWindow() Window {
	return NewWindow(time.Date(2026, 11, 15, 12, 30, 20, 0, time.UTC))
}

func gateStreams(w Window) []Stream {
	var out []Stream
	for _, service := range []string{"checkout", "payments"} {
		s := Stream{Labels: map[string]string{"service_name": service, "format": "json"}}
		for i, ts := range w.SampleTimes() {
			s.Entries = append(s.Entries, Entry{Time: ts, Line: service + " line " + strconv.Itoa(i)})
		}
		out = append(out, s)
	}
	return out
}

func TestWaitLokiHoldsFixturePassesOnceEveryPushedEntryReadsBack(t *testing.T) {
	t.Parallel()
	w := gateWindow()
	streams := gateStreams(w)
	all := len(streams[0].Entries)
	// The first two polls return every stream one entry short — the last
	// one, the reading of a reference that has not made its newest record
	// visible yet. The third returns all of them.
	stub := &stubLokiRange{streams: streams, held: func(poll int) int {
		if poll < 3 {
			return all - 1
		}
		return all
	}}
	srv := stub.server(t)
	if err := WaitLokiHoldsFixture(context.Background(), srv.URL, streams, w, lokiGatePassBudget); err != nil {
		t.Fatalf("gate should pass once every pushed entry reads back: %v", err)
	}
	if got := stub.pollCount(); got != 3 {
		t.Fatalf("gate returned after %d polls, want 3: it passed while entries were still missing", got)
	}
	wantStart := strconv.FormatInt(w.SeedStart.UnixNano(), 10)
	wantEnd := strconv.FormatInt(w.LogQueryEnd().UnixNano(), 10)
	for i := range stub.starts {
		if stub.starts[i] != wantStart || stub.ends[i] != wantEnd {
			t.Fatalf("query %d read [%s, %s), want the whole fixture [%s, %s)",
				i, stub.starts[i], stub.ends[i], wantStart, wantEnd)
		}
	}
}

func TestWaitLokiHoldsFixtureFailsWhileAStreamIsOneEntryShort(t *testing.T) {
	t.Parallel()
	w := gateWindow()
	streams := gateStreams(w)
	all := len(streams[0].Entries)
	stub := &stubLokiRange{streams: streams, held: func(int) int { return all - 1 }}
	srv := stub.server(t)
	err := WaitLokiHoldsFixture(context.Background(), srv.URL, streams, w, lokiGateBudget)
	if err == nil {
		t.Fatal("gate passed while every stream read back one entry short of the push")
	}
	last := w.AnchorEnd.Format(time.RFC3339Nano)
	if !strings.Contains(err.Error(), "returned "+strconv.Itoa(all-1)+" entries, want the "+strconv.Itoa(all)) ||
		!strings.Contains(err.Error(), last) {
		t.Fatalf("timeout must name the shortfall and the missing entry %s, got: %v", last, err)
	}
}

func TestWaitLokiHoldsFixtureFailsWhenADifferentEntryReadsBack(t *testing.T) {
	t.Parallel()
	w := gateWindow()
	streams := gateStreams(w)
	// The stub holds the same count but one entry a step off the pushed
	// instant: a count alone would pass it.
	held := gateStreams(w)
	shifted := held[1].Entries[len(held[1].Entries)-1].Time.Add(time.Second)
	held[1].Entries[len(held[1].Entries)-1].Time = shifted
	stub := &stubLokiRange{streams: held, held: func(int) int { return len(held[0].Entries) }}
	srv := stub.server(t)
	err := WaitLokiHoldsFixture(context.Background(), srv.URL, streams, w, lokiGateBudget)
	if err == nil {
		t.Fatal("gate passed with an entry at an instant this run never pushed")
	}
	missing := held[1].Entries[len(held[1].Entries)-1].Time.Add(-time.Second).Format(time.RFC3339Nano)
	if !strings.Contains(err.Error(), LogSelector(held[1].Labels)) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("timeout must name the second stream and its unreturned entry %s, got: %v", missing, err)
	}
}

func TestWaitLokiHoldsFixtureRejectsAnEmptyFixture(t *testing.T) {
	t.Parallel()
	err := WaitLokiHoldsFixture(context.Background(), "http://127.0.0.1:0", nil, gateWindow(), lokiGateBudget)
	if err == nil || !strings.Contains(err.Error(), "no log streams") {
		t.Fatalf("gate must refuse to wait on an empty push, got: %v", err)
	}
}
