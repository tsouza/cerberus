package info

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getAutotune(t *testing.T, opts Options) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	New(opts).Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info/autotune", nil))
	return rec
}

func TestHandleAutotune_ServesStatus(t *testing.T) {
	want := AutotuneStatus{
		Enabled:    true,
		Active:     true,
		Reason:     "active",
		Configured: ThresholdInfo{MinFanout: 16, MinAnchorPairs: 4000},
		Live:       ThresholdInfo{MinFanout: 8, MinAnchorPairs: 1928},
		Stats:      AutotuneStats{Ticks: 3, AppliedTicks: 1, TicksSinceChange: 2},
		Outcome:    AutotuneOutcome{HasSignal: true, OOMMinFanout: 8, RouteAOoms: 2, RouteBExecutions: 1500, RouteBOoms: 0},
	}
	rec := getAutotune(t, Options{Autotune: func() (AutotuneStatus, bool) { return want, true }})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got AutotuneStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Active || got.Reason != "active" || got.Live.MinFanout != 8 {
		t.Errorf("status: got %+v", got)
	}
	if got.Stats.Ticks != 3 || got.Stats.AppliedTicks != 1 {
		t.Errorf("stats: got %+v", got.Stats)
	}
	if !got.Outcome.HasSignal || got.Outcome.RouteBExecutions != 1500 || got.Outcome.RouteBOoms != 0 {
		t.Errorf("outcome: got %+v", got.Outcome)
	}
}

func TestHandleAutotune_NotConfigured(t *testing.T) {
	// No Autotune func wired → 404.
	rec := getAutotune(t, Options{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAutotune_Unavailable(t *testing.T) {
	// Func present but reports unavailable → 404.
	rec := getAutotune(t, Options{Autotune: func() (AutotuneStatus, bool) { return AutotuneStatus{}, false }})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
