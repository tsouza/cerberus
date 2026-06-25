package info

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// baseSnapshot returns a representative static fingerprint for the handler
// tests to mutate per-case.
func baseSnapshot() Snapshot {
	return Snapshot{
		Service:                   "cerberus",
		Version:                   "1.6.1",
		Revision:                  "abc1234",
		GoVersion:                 "go1.23.0",
		Heads:                     []string{"prom", "loki", "tempo"},
		CHAddress:                 "clickhouse:9000",
		CHDatabase:                "otel",
		ServerVersion:             "25.8",
		ServerVersionSource:       ServerVersionSourceProbe,
		OptSelection:              "auto,columnar_result_decode",
		OptMode:                   "enforcing",
		OptResolvedAgainstVersion: "25.8",
		OptEnabled:                []string{"aggregation_in_order", "columnar_result_decode", "condition_cache"},
	}
}

// decodeInfo issues GET /info against a freshly-mounted handler and decodes
// the body, failing the test on any transport/JSON error.
func decodeInfo(t *testing.T, h *Handler) (infoResponse, int) {
	t.Helper()
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var got infoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /info body: %v (body=%q)", err, rec.Body.String())
	}
	return got, rec.Code
}

// TestInfo_StaticFields confirms the boot-captured Snapshot is echoed
// verbatim into the body and that /info always returns 200.
func TestInfo_StaticFields(t *testing.T) {
	snap := baseSnapshot()
	h := New(Options{
		Snapshot:    snap,
		StartTime:   time.Now().Add(-90 * time.Second),
		Reachable:   func(context.Context) bool { return true },
		Breaker:     func() string { return "closed" },
		SchemaReady: func() bool { return true },
		Ready:       func(context.Context) bool { return true },
	})

	got, code := decodeInfo(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200", code)
	}
	if got.Service != "cerberus" {
		t.Errorf("service = %q; want cerberus", got.Service)
	}
	if got.Version != snap.Version {
		t.Errorf("version = %q; want %q", got.Version, snap.Version)
	}
	if got.Revision != snap.Revision {
		t.Errorf("revision = %q; want %q", got.Revision, snap.Revision)
	}
	if got.GoVersion != snap.GoVersion {
		t.Errorf("goVersion = %q; want %q", got.GoVersion, snap.GoVersion)
	}
	if len(got.Heads) != 3 || got.Heads[0] != "prom" {
		t.Errorf("heads = %v; want [prom loki tempo]", got.Heads)
	}
	if got.UptimeSeconds < 89 || got.UptimeSeconds > 120 {
		t.Errorf("uptimeSeconds = %d; want ~90", got.UptimeSeconds)
	}
	if got.ClickHouse.Address != snap.CHAddress {
		t.Errorf("clickhouse.address = %q; want %q", got.ClickHouse.Address, snap.CHAddress)
	}
	if got.ClickHouse.Database != snap.CHDatabase {
		t.Errorf("clickhouse.database = %q; want %q", got.ClickHouse.Database, snap.CHDatabase)
	}
}

// TestInfo_OptimizationsEnabled is the headline assertion: the resolved
// EnabledSet ids surface verbatim under optimizations.enabled.
func TestInfo_OptimizationsEnabled(t *testing.T) {
	snap := baseSnapshot()
	snap.OptEnabled = []string{"columnar_result_decode", "ts_grid_range"}
	snap.OptSelection = "auto,columnar_result_decode"
	snap.OptMode = "permissive"
	snap.OptResolvedAgainstVersion = "25.6"

	h := New(Options{Snapshot: snap})
	got, _ := decodeInfo(t, h)

	if got.Optimizations.Selection != "auto,columnar_result_decode" {
		t.Errorf("optimizations.selection = %q", got.Optimizations.Selection)
	}
	if got.Optimizations.Mode != "permissive" {
		t.Errorf("optimizations.mode = %q; want permissive", got.Optimizations.Mode)
	}
	if got.Optimizations.ResolvedAgainstVersion != "25.6" {
		t.Errorf("optimizations.resolvedAgainstVersion = %q; want 25.6", got.Optimizations.ResolvedAgainstVersion)
	}
	want := []string{"columnar_result_decode", "ts_grid_range"}
	if len(got.Optimizations.Enabled) != len(want) {
		t.Fatalf("optimizations.enabled = %v; want %v", got.Optimizations.Enabled, want)
	}
	for i, id := range want {
		if got.Optimizations.Enabled[i] != id {
			t.Errorf("optimizations.enabled[%d] = %q; want %q", i, got.Optimizations.Enabled[i], id)
		}
	}
}

// TestInfo_ServerVersionSource verifies probe-vs-fallback round-trips
// faithfully — the field that makes the 24.8-floor pin obvious.
func TestInfo_ServerVersionSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		ver    string
	}{
		{"probe", ServerVersionSourceProbe, "25.8"},
		{"fallback", ServerVersionSourceFallback, "24.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := baseSnapshot()
			snap.ServerVersion = tc.ver
			snap.ServerVersionSource = tc.source

			h := New(Options{Snapshot: snap})
			got, _ := decodeInfo(t, h)

			if got.ClickHouse.ServerVersion != tc.ver {
				t.Errorf("serverVersion = %q; want %q", got.ClickHouse.ServerVersion, tc.ver)
			}
			if got.ClickHouse.ServerVersionSource != tc.source {
				t.Errorf("serverVersionSource = %q; want %q", got.ClickHouse.ServerVersionSource, tc.source)
			}
		})
	}
}

// TestInfo_LiveState confirms the injected live closures drive the
// reachability/breaker/schemaReady/ready fields on every request.
func TestInfo_LiveState(t *testing.T) {
	snap := baseSnapshot()
	h := New(Options{
		Snapshot:    snap,
		Reachable:   func(context.Context) bool { return false },
		Breaker:     func() string { return "open" },
		SchemaReady: func() bool { return false },
		Ready:       func(context.Context) bool { return false },
	})

	got, code := decodeInfo(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200 even when unready", code)
	}
	if got.ClickHouse.Reachable {
		t.Error("clickhouse.reachable = true; want false")
	}
	if got.ClickHouse.Breaker != "open" {
		t.Errorf("clickhouse.breaker = %q; want open", got.ClickHouse.Breaker)
	}
	if got.ClickHouse.SchemaReady {
		t.Error("clickhouse.schemaReady = true; want false")
	}
	if got.Ready {
		t.Error("ready = true; want false")
	}
}

// TestInfo_NilFuncsSafeDefaults confirms a partially-wired handler still
// emits a well-formed body with safe defaults.
func TestInfo_NilFuncsSafeDefaults(t *testing.T) {
	h := New(Options{Snapshot: baseSnapshot()})
	got, code := decodeInfo(t, h)

	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200", code)
	}
	if got.ClickHouse.Reachable {
		t.Error("reachable default = true; want false")
	}
	if got.ClickHouse.Breaker != "closed" {
		t.Errorf("breaker default = %q; want closed", got.ClickHouse.Breaker)
	}
	if !got.ClickHouse.SchemaReady {
		t.Error("schemaReady default = false; want true")
	}
	if got.Ready {
		t.Error("ready default = true; want false")
	}
	if got.UptimeSeconds != 0 {
		t.Errorf("uptimeSeconds with zero StartTime = %d; want 0", got.UptimeSeconds)
	}
}
