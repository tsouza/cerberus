package migrateinventory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lokiStatsHandler returns an httptest handler answering /loki/api/v1/index/stats
// with a fixed body for each recognized selector, and a 404 for anything else
// — closing over a mutable failSet so tests can flip individual selectors
// between "answers" and "fails" without spinning up a second server.
func lokiStatsHandler(t *testing.T, bodies map[string]string, failSet map[string]bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != lokiIndexStatsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sel := r.URL.Query().Get("query")
		if failSet[sel] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, ok := bodies[sel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

// TestLokiProbe_RanksSelectorsByStreams: three selectors answer out of
// stream-count order; Probe re-ranks them descending by Streams (never
// trusting call order) and truncates to top.
func TestLokiProbe_RanksSelectorsByStreams(t *testing.T) {
	bodies := map[string]string{
		`{app="a"}`: `{"streams":10,"chunks":1,"entries":100,"bytes":1000}`,
		`{app="b"}`: `{"streams":900,"chunks":2,"entries":200,"bytes":2000}`,
		`{app="c"}`: `{"streams":450,"chunks":3,"entries":300,"bytes":3000}`,
	}
	srv := httptest.NewServer(lokiStatsHandler(t, bodies, nil))
	t.Cleanup(srv.Close)

	inv, err := NewLokiClient(srv.URL).Probe(context.Background(),
		[]string{`{app="a"}`, `{app="b"}`, `{app="c"}`}, "", 2)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(inv.Selectors) != 2 {
		t.Fatalf("selectors = %d, want 2 (truncated to top)", len(inv.Selectors))
	}
	if inv.Selectors[0].Selector != `{app="b"}` || inv.Selectors[0].Streams != 900 {
		t.Errorf("rank #1 = %+v, want {app=\"b\"} with 900 streams", inv.Selectors[0])
	}
	if inv.Selectors[1].Selector != `{app="c"}` || inv.Selectors[1].Streams != 450 {
		t.Errorf("rank #2 = %+v, want {app=\"c\"} with 450 streams", inv.Selectors[1])
	}
	if inv.Selectors[0].Chunks != 2 || inv.Selectors[0].Entries != 200 || inv.Selectors[0].Bytes != 2000 {
		t.Errorf("rank #1 should carry through chunks/entries/bytes, got %+v", inv.Selectors[0])
	}
}

// TestLokiProbe_ZeroSelectorsIsValidationError: an empty selector list is
// rejected before any HTTP call — Probe must not even need a live server.
func TestLokiProbe_ZeroSelectorsIsValidationError(t *testing.T) {
	client := NewLokiClient("http://unreachable.invalid:1")
	_, err := client.Probe(context.Background(), nil, "", 10)
	if err == nil {
		t.Fatal("Probe should reject zero selectors")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("error should explain the requirement, got: %v", err)
	}
}

// TestLokiProbe_AllSelectorsFailIsHardError: every selector's index/stats
// call failing must surface a non-nil error, never an empty-but-successful
// Selectors slice (that would misrepresent "nothing configured" as "checked,
// nothing found").
func TestLokiProbe_AllSelectorsFailIsHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	inv, err := NewLokiClient(srv.URL).Probe(context.Background(), []string{`{app="a"}`, `{app="b"}`}, "", 10)
	if err == nil {
		t.Fatalf("Probe should hard-error when every selector fails, got inv: %+v", inv)
	}
	if !strings.Contains(err.Error(), `{app="a"}`) || !strings.Contains(err.Error(), `{app="b"}`) {
		t.Errorf("error should name both failed selectors, got: %v", err)
	}
}

// TestLokiProbe_PartialFailureAppendsNotes: one of three selectors failing
// yields two ranked results plus a Notes entry naming the failed selector —
// the failure is counted, never silently swallowed.
func TestLokiProbe_PartialFailureAppendsNotes(t *testing.T) {
	bodies := map[string]string{
		`{app="ok1"}`: `{"streams":10,"chunks":1,"entries":100,"bytes":1000}`,
		`{app="ok2"}`: `{"streams":20,"chunks":1,"entries":100,"bytes":1000}`,
	}
	failSet := map[string]bool{`{app="bad"}`: true}
	srv := httptest.NewServer(lokiStatsHandler(t, bodies, failSet))
	t.Cleanup(srv.Close)

	inv, err := NewLokiClient(srv.URL).Probe(context.Background(),
		[]string{`{app="ok1"}`, `{app="bad"}`, `{app="ok2"}`}, "", 10)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(inv.Selectors) != 2 {
		t.Fatalf("selectors = %d, want 2 (the two that succeeded), got %+v", len(inv.Selectors), inv.Selectors)
	}
	if len(inv.Notes) != 1 {
		t.Fatalf("notes = %d, want 1, got %+v", len(inv.Notes), inv.Notes)
	}
	if !strings.Contains(inv.Notes[0], `{app="bad"}`) {
		t.Errorf("note should name the failed selector, got: %q", inv.Notes[0])
	}
}

// TestLokiProbe_WindowAppliesRealQueryRange pins that a non-empty window is
// actually threaded through as start/end params, unlike the Prometheus
// Client's cosmetic-only Window.
func TestLokiProbe_WindowAppliesRealQueryRange(t *testing.T) {
	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		_, _ = w.Write([]byte(`{"streams":1,"chunks":1,"entries":1,"bytes":1}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := NewLokiClient(srv.URL).Probe(context.Background(), []string{`{app="a"}`}, "1h", 10); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if gotStart == "" || gotEnd == "" {
		t.Fatalf("a non-empty window should set start/end query params, got start=%q end=%q", gotStart, gotEnd)
	}
}

// TestLokiProbe_EmptyWindowOmitsRange pins that an empty window sends no
// start/end params at all, letting the server apply its own default range.
func TestLokiProbe_EmptyWindowOmitsRange(t *testing.T) {
	sawStart, sawEnd := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawStart = r.URL.Query()["start"]
		_, sawEnd = r.URL.Query()["end"]
		_, _ = w.Write([]byte(`{"streams":1,"chunks":1,"entries":1,"bytes":1}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := NewLokiClient(srv.URL).Probe(context.Background(), []string{`{app="a"}`}, "", 10); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if sawStart || sawEnd {
		t.Errorf("an empty window should omit start/end params, got start present=%v end present=%v", sawStart, sawEnd)
	}
}

// TestLokiProbe_InvalidWindowIsValidationError: an unparseable window is
// rejected before any HTTP call, matching the Prometheus Options.Validate
// discipline for the same kind of input.
func TestLokiProbe_InvalidWindowIsValidationError(t *testing.T) {
	client := NewLokiClient("http://unreachable.invalid:1")
	_, err := client.Probe(context.Background(), []string{`{app="a"}`}, "banana", 10)
	if err == nil {
		t.Fatal("Probe should reject an unparseable window")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error should name the bad window value, got: %v", err)
	}
}

// TestLokiProbe_InvalidTopIsValidationError: a non-positive top is rejected
// before any HTTP call, mirroring the Prometheus Options.Validate guard.
func TestLokiProbe_InvalidTopIsValidationError(t *testing.T) {
	client := NewLokiClient("http://unreachable.invalid:1")
	_, err := client.Probe(context.Background(), []string{`{app="a"}`}, "", 0)
	if err == nil {
		t.Fatal("Probe should reject top <= 0")
	}
}
