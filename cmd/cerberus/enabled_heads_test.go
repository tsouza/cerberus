package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// One representative route per head. A request to an ENABLED head's route is
// dispatched to its handler (which 4xx/5xxs without a real query or CH, but
// NEVER 404s — the route exists); a request to a DISABLED head's route hits
// the bare mux and 404s (the route was never mounted).
const (
	promRoute  = "/api/v1/query"
	lokiRoute  = "/loki/api/v1/query"
	tempoRoute = "/api/search"
)

// buildHeadsTestServer wires the same root mux shape run() builds — the API
// mux (gated by CERBERUS_ENABLED_HEADS) under "/", plus the unconditional
// /healthz + /readyz probes — against a lazily-constructed client whose CH is
// unreachable. chclient.New never dials, and the routing assertions below only
// distinguish "route mounted" (non-404) from "route absent" (404), so no live
// ClickHouse is needed.
func buildHeadsTestServer(t *testing.T, enabledHeads string) *httptest.Server {
	t.Helper()
	t.Setenv("CERBERUS_ENABLED_HEADS", enabledHeads)

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:         unreachableAddr(t),
		Database:     "otel",
		DialTimeout:  time.Second,
		MaxOpenConns: 10,
		MaxIdleConns: 5,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := slog.New(slog.NewTextHandler(httptestDiscard{}, &slog.HandlerOptions{Level: slog.LevelError}))

	prom, loki, tempo := newAdmitLimiters(cfg, logger)
	traceMux := http.NewServeMux()
	if _, err := mountAPIHeads(context.Background(), traceMux, client, cfg, chopt.EnabledSet{}, prom, loki, tempo, logger); err != nil {
		t.Fatalf("mountAPIHeads: %v", err)
	}

	rootMux := http.NewServeMux()
	healthHandler := health.New(health.Options{
		Pinger:      client.ForHead(chclient.HeadProbe),
		SchemaReady: func() bool { return true },
		CacheTTL:    -1,
	})
	healthHandler.Mount(rootMux)
	rootMux.Handle("/", traceMux)

	srv := httptest.NewServer(rootMux)
	t.Cleanup(srv.Close)
	return srv
}

// httptestDiscard is an io.Writer sink for the test logger.
type httptestDiscard struct{}

func (httptestDiscard) Write(p []byte) (int, error) { return len(p), nil }

// TestMountAPIHeads_SingleHeadServesOnlyItsRoutes is the falsifiable gate:
// with CERBERUS_ENABLED_HEADS=prom, the prom route is served while the loki
// and tempo routes 404, and /healthz still 200s. If the build/Mount gating
// regressed (all heads always mounted), the loki/tempo 404 assertions fail.
func TestMountAPIHeads_SingleHeadServesOnlyItsRoutes(t *testing.T) {
	srv := buildHeadsTestServer(t, "prom")

	if got := getStatus(t, srv.URL+promRoute); got == http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=prom = 404; want the prom route served (non-404)", promRoute)
	}
	if got := getStatus(t, srv.URL+lokiRoute); got != http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=prom = %d; want 404 (loki head not mounted)", lokiRoute, got)
	}
	if got := getStatus(t, srv.URL+tempoRoute); got != http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=prom = %d; want 404 (tempo head not mounted)", tempoRoute, got)
	}
	if got := getStatus(t, srv.URL+"/healthz"); got != http.StatusOK {
		t.Errorf("GET /healthz with CERBERUS_ENABLED_HEADS=prom = %d; want 200 (probes are unconditional)", got)
	}
}

// TestMountAPIHeads_LokiOnly mirrors the single-head gate for the loki head so
// the proof isn't prom-specific.
func TestMountAPIHeads_LokiOnly(t *testing.T) {
	srv := buildHeadsTestServer(t, "loki")

	if got := getStatus(t, srv.URL+lokiRoute); got == http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=loki = 404; want the loki route served (non-404)", lokiRoute)
	}
	if got := getStatus(t, srv.URL+promRoute); got != http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=loki = %d; want 404 (prom head not mounted)", promRoute, got)
	}
	if got := getStatus(t, srv.URL+tempoRoute); got != http.StatusNotFound {
		t.Errorf("GET %s with CERBERUS_ENABLED_HEADS=loki = %d; want 404 (tempo head not mounted)", tempoRoute, got)
	}
}

// TestMountAPIHeads_DefaultServesAllThree confirms the backward-compatible
// default: an unset CERBERUS_ENABLED_HEADS mounts every head's routes.
func TestMountAPIHeads_DefaultServesAllThree(t *testing.T) {
	srv := buildHeadsTestServer(t, "")

	for _, route := range []string{promRoute, lokiRoute, tempoRoute} {
		if got := getStatus(t, srv.URL+route); got == http.StatusNotFound {
			t.Errorf("GET %s with default CERBERUS_ENABLED_HEADS = 404; want served (all three heads mounted by default)", route)
		}
	}
}
