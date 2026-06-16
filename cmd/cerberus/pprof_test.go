package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestMaybeMountPProf_RegistersWhenEnabled pins that maybeMountPProf, when
// enabled, wires the net/http/pprof handlers onto the supplied mux:
// /debug/pprof/ serves the index and /debug/pprof/heap serves the heap
// profile. The gate (CERBERUS_DEBUG_PPROF → enabled) decides whether the
// surface mounts; this test verifies that when it does, the endpoints respond,
// so a refactor can't silently drop the heap route the e2e OOM diagnostics
// depend on.
func TestMaybeMountPProf_RegistersWhenEnabled(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	maybeMountPProf(mux, true, discardLogger())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap?debug=0"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestMaybeMountPProf_NoopWhenDisabled is the negative half: with enabled=false
// (the production default when CERBERUS_DEBUG_PPROF is unset) the call is a
// no-op and the pprof routes 404, proving the profiling surface is genuinely
// off unless explicitly enabled.
func TestMaybeMountPProf_NoopWhenDisabled(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	maybeMountPProf(mux, false, discardLogger())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("disabled pprof: status %d, want 404", resp.StatusCode)
	}
}
