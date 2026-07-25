package main

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/solver"
)

// TestMountAPIHeads_PromStartsNoBackgroundLoop pins the invariant that the
// sharded-pushdown solver is configured entirely up front: mounting the prom
// head (the only head that builds a solver) starts no long-lived goroutine, so
// the routing gate a request sees is a pure function of the process
// configuration.
//
// The assertion is falsifiable by construction. The goroutine inventory is
// snapshotted immediately BEFORE mountAPIHeads, and the lifecycle context is
// still LIVE when the check runs — a background loop bound to that context
// would be executing at exactly that moment and would fail the check. Cancelling
// first would let a real loop exit and hide itself.
//
// The solver runs in auto mode (the only mode carrying a cost gate) and the
// query_log corpus is deliberately turned on with the chtable sink — that pair
// was the retired threshold-autotune loop's own start condition, so this is
// the ONE configuration under which a surviving loop would have had a reason
// to run. A test that never sets these would pass whether or not the loop had
// actually been removed, since with corpus off (the default) the loop's own
// gate would already have kept it from starting. The corpus reconciler this PR
// KEEPS is unaffected by this choice: it needs a live ClickHouse connection to
// build its sink, the client here points at an unreachable address, so the
// reconciler degrades to disabled and contributes no goroutine either —
// isolating this check to exactly the autotune loop's own start condition.
func TestMountAPIHeads_PromStartsNoBackgroundLoop(t *testing.T) {
	t.Setenv("CERBERUS_ENABLED_HEADS", "prom")
	t.Setenv(solver.EnvRoute, solver.ModeAuto)
	t.Setenv("CERBERUS_CH_OPT_CORPUS_ENABLED", "true")
	t.Setenv("CERBERUS_CH_OPT_CORPUS_SINK_MODE", "chtable")

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

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Snapshot AFTER every dependency is constructed and BEFORE the mount, so
	// the verification below attributes any surviving goroutine to
	// mountAPIHeads alone.
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreCurrent(),
	}

	if _, err := mountAPIHeads(ctx, http.NewServeMux(), client, cfg, chopt.EnabledSet{}, prom, loki, tempo, logger); err != nil {
		t.Fatalf("mountAPIHeads: %v", err)
	}

	// ctx is deliberately still live here.
	goleak.VerifyNone(t, opts...)
}
