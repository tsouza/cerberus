package info

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// baseSnapshot returns a representative static fingerprint for the handler
// tests to mutate per-case.
func baseSnapshot() Snapshot {
	return Snapshot{
		Service:      "cerberus",
		Version:      "1.6.1",
		Revision:     "abc1234",
		GoVersion:    "go1.23.0",
		Heads:        []string{"prom", "loki", "tempo"},
		CHAddress:    "clickhouse:9000",
		CHDatabase:   "otel",
		OptSelection: "auto,columnar_result_decode",
		OptMode:      "enforcing",
	}
}

// baseOptState returns a representative capability resolution for the handler
// tests to mutate per-case.
func baseOptState() OptState {
	return OptState{
		ServerVersion:       "25.8",
		ServerVersionSource: ServerVersionSourceProbe,
		Enabled:             []string{"aggregation_in_order", "columnar_result_decode", "condition_cache"},
	}
}

// staticOpts adapts an OptState into the Options.Optimizations closure.
func staticOpts(st OptState) func() OptState {
	return func() OptState { return st }
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
		Snapshot:      snap,
		Optimizations: staticOpts(baseOptState()),
		StartTime:     time.Now().Add(-90 * time.Second),
		Reachable:     func(context.Context) bool { return true },
		Breaker:       func() string { return "closed" },
		SchemaReady:   func() bool { return true },
		Ready:         func(context.Context) bool { return true },
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
	snap.OptSelection = "auto,columnar_result_decode"
	snap.OptMode = "permissive"
	st := baseOptState()
	st.ServerVersion = "25.9"
	st.Enabled = []string{"columnar_result_decode", "ts_grid_range"}

	h := New(Options{Snapshot: snap, Optimizations: staticOpts(st)})
	got, _ := decodeInfo(t, h)

	if got.Optimizations.Selection != "auto,columnar_result_decode" {
		t.Errorf("optimizations.selection = %q", got.Optimizations.Selection)
	}
	if got.Optimizations.Mode != "permissive" {
		t.Errorf("optimizations.mode = %q; want permissive", got.Optimizations.Mode)
	}
	if got.Optimizations.ResolvedAgainstVersion != "25.9" {
		t.Errorf("optimizations.resolvedAgainstVersion = %q; want 25.9", got.Optimizations.ResolvedAgainstVersion)
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

// TestInfo_ResultCacheHitsAndMisses (cerberus issue #2781) confirms the
// query-result-cache tally surfaces verbatim under resultCache.{hits,misses}.
func TestInfo_ResultCacheHitsAndMisses(t *testing.T) {
	h := New(Options{
		Snapshot:      baseSnapshot(),
		Optimizations: staticOpts(baseOptState()),
		ResultCache:   func() ResultCacheState { return ResultCacheState{Hits: 42, Misses: 7} },
	})
	got, _ := decodeInfo(t, h)

	if got.ResultCache.Hits != 42 {
		t.Errorf("resultCache.hits = %d; want 42", got.ResultCache.Hits)
	}
	if got.ResultCache.Misses != 7 {
		t.Errorf("resultCache.misses = %d; want 7", got.ResultCache.Misses)
	}
}

// TestInfo_ResultCacheNilFuncDefaultsToZero confirms a handler wired without
// Options.ResultCache reports 0/0 rather than an absent field — the honest
// answer for a deployment where result_cache never resolved in (below the
// version floor, or the boot capability probe came back Forbidden).
func TestInfo_ResultCacheNilFuncDefaultsToZero(t *testing.T) {
	h := New(Options{Snapshot: baseSnapshot()})
	got, _ := decodeInfo(t, h)

	if got.ResultCache.Hits != 0 || got.ResultCache.Misses != 0 {
		t.Errorf("resultCache with no resolver = %+v; want zero", got.ResultCache)
	}
}

// TestInfo_FilesystemCacheConfigured (cerberus issue #2780) confirms a
// configured server-side filesystem cache surfaces verbatim under
// filesystemCache.{configured,caches,maxSizeBytes,currentSizeBytes,currentElements}.
func TestInfo_FilesystemCacheConfigured(t *testing.T) {
	h := New(Options{
		Snapshot:      baseSnapshot(),
		Optimizations: staticOpts(baseOptState()),
		FilesystemCache: func(context.Context) FilesystemCacheState {
			return FilesystemCacheState{
				Configured:       true,
				Caches:           1,
				MaxSizeBytes:     10 << 30, // 10 GiB
				CurrentSizeBytes: 6 << 30,  // 6 GiB
				CurrentElements:  12345,
			}
		},
	})
	got, _ := decodeInfo(t, h)

	if !got.FilesystemCache.Configured {
		t.Errorf("filesystemCache.configured = false; want true")
	}
	if got.FilesystemCache.Caches != 1 {
		t.Errorf("filesystemCache.caches = %d; want 1", got.FilesystemCache.Caches)
	}
	if got.FilesystemCache.MaxSizeBytes != 10<<30 {
		t.Errorf("filesystemCache.maxSizeBytes = %d; want %d", got.FilesystemCache.MaxSizeBytes, 10<<30)
	}
	if got.FilesystemCache.CurrentSizeBytes != 6<<30 {
		t.Errorf("filesystemCache.currentSizeBytes = %d; want %d", got.FilesystemCache.CurrentSizeBytes, 6<<30)
	}
	if got.FilesystemCache.CurrentElements != 12345 {
		t.Errorf("filesystemCache.currentElements = %d; want 12345", got.FilesystemCache.CurrentElements)
	}
}

// TestInfo_FilesystemCacheNilFuncDefaultsToZero confirms a handler wired
// without Options.FilesystemCache reports Configured=false with every
// counter at 0 rather than an absent field — the honest answer for a
// handler wired without chclient.QueryFilesystemCacheState.
func TestInfo_FilesystemCacheNilFuncDefaultsToZero(t *testing.T) {
	h := New(Options{Snapshot: baseSnapshot()})
	got, _ := decodeInfo(t, h)

	want := filesystemCacheInfo{}
	if got.FilesystemCache != want {
		t.Errorf("filesystemCache with no resolver = %+v; want zero", got.FilesystemCache)
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
			st := baseOptState()
			st.ServerVersion = tc.ver
			st.ServerVersionSource = tc.source

			h := New(Options{Snapshot: baseSnapshot(), Optimizations: staticOpts(st)})
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
		Snapshot:      snap,
		Optimizations: staticOpts(baseOptState()),
		Reachable:     func(context.Context) bool { return false },
		Breaker:       func() string { return "open" },
		SchemaReady:   func() bool { return false },
		Ready:         func(context.Context) bool { return false },
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
	if got.ClickHouse.ServerVersion != "" {
		t.Errorf("serverVersion with no resolver = %q; want empty", got.ClickHouse.ServerVersion)
	}
	if len(got.Optimizations.Enabled) != 0 {
		t.Errorf("optimizations.enabled with no resolver = %v; want empty", got.Optimizations.Enabled)
	}
}

// TestInfo_OptimizationsAreLive is the regression pin for the capability
// re-probe: /info must re-read the resolution on EVERY request, so a set that
// changes under a running process (a ClickHouse upgrade crossing a feature
// floor) is visible on the next scrape rather than at the next restart. A
// handler that captured the resolution once at construction — the boot-snapshot
// shape this replaced — returns the first answer twice and fails here.
func TestInfo_OptimizationsAreLive(t *testing.T) {
	var current atomic.Pointer[OptState]
	before := OptState{ServerVersion: "25.8", ServerVersionSource: ServerVersionSourceProbe, Enabled: []string{"aggregation_in_order"}}
	current.Store(&before)

	h := New(Options{
		Snapshot:      baseSnapshot(),
		Optimizations: func() OptState { return *current.Load() },
	})

	got, _ := decodeInfo(t, h)
	if got.ClickHouse.ServerVersion != "25.8" {
		t.Fatalf("serverVersion before upgrade = %q; want 25.8", got.ClickHouse.ServerVersion)
	}
	if len(got.Optimizations.Enabled) != 1 || got.Optimizations.Enabled[0] != "aggregation_in_order" {
		t.Fatalf("enabled before upgrade = %v; want [aggregation_in_order]", got.Optimizations.Enabled)
	}

	after := OptState{ServerVersion: "25.9", ServerVersionSource: ServerVersionSourceProbe, Enabled: []string{"aggregation_in_order", "ts_grid_range"}}
	current.Store(&after)

	got, _ = decodeInfo(t, h)
	if got.ClickHouse.ServerVersion != "25.9" {
		t.Errorf("serverVersion after upgrade = %q; want 25.9", got.ClickHouse.ServerVersion)
	}
	if got.Optimizations.ResolvedAgainstVersion != "25.9" {
		t.Errorf("resolvedAgainstVersion after upgrade = %q; want 25.9", got.Optimizations.ResolvedAgainstVersion)
	}
	if len(got.Optimizations.Enabled) != 2 || got.Optimizations.Enabled[1] != "ts_grid_range" {
		t.Errorf("enabled after upgrade = %v; want [aggregation_in_order ts_grid_range]", got.Optimizations.Enabled)
	}
}
