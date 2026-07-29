package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// promLookbackEnv is the retention an operator states through
// CERBERUS_PROM_METADATA_LOOKBACK in these tests. It is written the way an
// operator states a retention — 90 days — which also pins that the retention
// units survive the whole env -> Config -> Handler chain rather than only the
// loader. The value is deliberately unlike every default in play (0 in the
// config, 14d in the handler fallback), so a lookback that reaches the handler
// can only have come from here.
const promLookbackEnv = "90d"

// promLookbackWant is promLookbackEnv as a duration: d is a fixed 24h, so
// 90d is exactly 2160h.
const promLookbackWant = 2160 * time.Hour

// newPromHandlerForTest builds the prom handler exactly as run() does, off a
// config resolved from the process environment. chclient.New never dials, and
// nothing here issues a query — the assertions are on handler fields the
// wiring sets.
func newPromHandlerForTest(t *testing.T) *promHandlerUnderTest {
	t.Helper()

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
	promLimiter, _, _ := newAdmitLimiters(cfg, logger)
	h := newPromHandler(client, cfg, chopt.EnabledSet{}, nil, promLimiter, logger)
	return &promHandlerUnderTest{cfg: cfg, lookback: h.MetadataLookback}
}

// promHandlerUnderTest carries the two ends of the seam under test: what the
// config loader resolved, and what the built handler actually carries.
type promHandlerUnderTest struct {
	cfg      config.Config
	lookback time.Duration
}

// TestPromMetadataLookback_EnvReachesHandler pins the whole wiring chain —
// CERBERUS_PROM_METADATA_LOOKBACK -> config.FromEnv -> newPromHandler ->
// Handler.MetadataLookback. Both halves were already covered in isolation
// (the loader parses the env, the handler honors the field), which left the
// ASSIGNMENT between them untested: dropping it silently reverted every
// deployment to the handler's 14d fallback while both isolated tests stayed
// green.
func TestPromMetadataLookback_EnvReachesHandler(t *testing.T) {
	t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", promLookbackEnv)

	got := newPromHandlerForTest(t)

	if got.cfg.PromMetadataLookback != promLookbackWant {
		t.Fatalf("config.PromMetadataLookback = %s, want %s", got.cfg.PromMetadataLookback, promLookbackWant)
	}
	if got.lookback != promLookbackWant {
		t.Fatalf("Handler.MetadataLookback = %s, want %s (the env value never reached the handler)",
			got.lookback, promLookbackWant)
	}
}

// TestPromMetadataLookback_UnsetLeavesHandlerFallback pins the other side of
// the same seam: with the knob unset the handler must carry ZERO, which is
// what hands the bound to internal/api/prom's own fallback horizon. A
// non-zero value here would mean cmd/cerberus had derived a bound from
// something that is not evidence of physical retention — the silent-drop
// failure the derived form caused.
func TestPromMetadataLookback_UnsetLeavesHandlerFallback(t *testing.T) {
	// Schema-provisioning TTLs are the config values that LOOK like retention
	// but are not: they are a no-op unless auto-create is on, and their DDL is
	// CREATE TABLE IF NOT EXISTS, so they never reach a table an OTel
	// collector already created. Setting them here (without auto-create, the
	// default) asserts they do not leak into the scan bound.
	t.Setenv("CERBERUS_SCHEMA_TTL", "168h")
	t.Setenv("CERBERUS_SCHEMA_TTL_METRICS", "336h")

	got := newPromHandlerForTest(t)

	if got.cfg.AutoCreateSchema {
		t.Fatalf("AutoCreateSchema = true, want the false default so the TTLs below are the no-op case")
	}
	if got.cfg.PromMetadataLookback != 0 {
		t.Fatalf("config.PromMetadataLookback = %s, want 0 with the knob unset", got.cfg.PromMetadataLookback)
	}
	if got.lookback != 0 {
		t.Fatalf("Handler.MetadataLookback = %s, want 0 so the handler's own fallback applies; "+
			"a provisioning TTL must not become the scan bound", got.lookback)
	}
}
