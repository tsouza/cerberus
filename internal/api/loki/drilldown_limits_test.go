package loki_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestDrilldownLimits_HappyPath drives /loki/api/v1/drilldown-limits and
// asserts the upstream wire shape: a flat top-level JSON object (NOT the
// {status, data} envelope), with pattern_ingester_enabled and the
// published limits cerberus actually implements. Grafana's Logs
// Drilldown app (preinstalled in 12.x) probes this on boot; a 404 here
// surfaced as the compose-smoke failure that motivated the endpoint.
func TestDrilldownLimits_HappyPath(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/drilldown-limits")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}

	var out loki.DrilldownLimitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !out.PatternIngesterEnabled {
		t.Errorf("pattern_ingester_enabled=false; cerberus serves /patterns so it must advertise true")
	}
	if got, want := out.Limits["volume_enabled"], true; got != want {
		t.Errorf("limits.volume_enabled=%v, want %v", got, want)
	}
	if got, want := out.Limits["discover_log_levels"], true; got != want {
		t.Errorf("limits.discover_log_levels=%v, want %v", got, want)
	}
	// JSON numbers decode as float64; the cap mirrors maxLogQueryLimit.
	if got, want := out.Limits["max_entries_limit_per_query"], float64(5000); got != want {
		t.Errorf("limits.max_entries_limit_per_query=%v, want %v", got, want)
	}
}

// TestDrilldownLimits_VersionThreading pins that the handler surfaces
// the same build identifier the /status/buildinfo probe reports, so the
// Drilldown app's version gating sees a consistent value.
func TestDrilldownLimits_VersionThreading(t *testing.T) {
	t.Parallel()

	h := loki.New(&stubQuerier{}, schema.DefaultOTelLogs(), nil)
	h.Version = "v1.2.3-test"
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/drilldown-limits")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var out loki.DrilldownLimitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Version != "v1.2.3-test" {
		t.Errorf("version=%q, want %q", out.Version, "v1.2.3-test")
	}
}

// TestDrilldownLimits_PostRejected pins upstream parity: Loki registers
// drilldown-limits as GET-only (pkg/loki/loki.go), so a POST must fall
// through to the JSON-shaped loki 404/405 surface, not silently succeed.
func TestDrilldownLimits_PostRejected(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/loki/api/v1/drilldown-limits", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST returned 200; upstream registers GET only")
	}
}
