package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/info"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// The ClickHouse capability set describes a server, and a server changes under
// a running process. These tests pin the two halves that keep the process
// honest about it: the live holder every consumer reads the resolution through,
// and the loop that replaces it.

// quietLogger is a logger the re-probe can write to without polluting output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// resolutionAt builds a resolution carrying the named features, as chopt.Resolve
// would return for a server at version.
func resolutionAt(t *testing.T, version chopt.Version, ids ...string) chOptResolution {
	t.Helper()
	selection := "off"
	if len(ids) > 0 {
		selection = ""
		for i, id := range ids {
			if i > 0 {
				selection += ","
			}
			selection += id
		}
	}
	set, _, err := chopt.Resolve(chopt.Config{
		Optimizations: selection,
		Mode:          chopt.Permissive,
		Capability:    chopt.CapabilityAvailable,
	}, version)
	if err != nil {
		t.Fatalf("chopt.Resolve(%q, %v): %v", selection, version, err)
	}
	return chOptResolution{Set: set, ResolvedVersion: version}
}

// TestCHOptLive_PublishesTheResolutionInForce — the holder starts on the boot
// resolution and reports whatever was stored last, so every consumer reading
// through it sees one answer rather than its own stale copy.
func TestCHOptLive_PublishesTheResolutionInForce(t *testing.T) {
	t.Parallel()

	boot := resolutionAt(t, supportedFloorVersion, chopt.FeatureAggregationInOrder)
	live := newCHOptLive(boot)

	if got := live.get(); !got.Set.Equal(boot.Set) || got.ResolvedVersion != supportedFloorVersion {
		t.Fatalf("get() = %+v; want the boot resolution", got)
	}

	upgraded := resolutionAt(t, chopt.Version{Major: 25, Minor: 9},
		chopt.FeatureAggregationInOrder, chopt.FeatureTSGridRange)
	live.store(upgraded)

	got := live.get()
	if !got.Set.Has(chopt.FeatureTSGridRange) {
		t.Errorf("after store: enabled = %v; want the upgraded set", got.Set.IDs())
	}
	if got.ResolvedVersion != (chopt.Version{Major: 25, Minor: 9}) {
		t.Errorf("after store: version = %v; want 25.9", got.ResolvedVersion)
	}
}

// TestCHOptLive_InfoStateTracksTheSwap is the /info half of the fix: the
// fingerprint reports the capabilities cerberus is emitting against NOW. A
// snapshot captured at boot would tell an operator watching a rolling upgrade
// that nothing happened, which is the opposite of what /info is for.
func TestCHOptLive_InfoStateTracksTheSwap(t *testing.T) {
	t.Parallel()

	live := newCHOptLive(resolutionAt(t, supportedFloorVersion, chopt.FeatureAggregationInOrder))

	before := live.infoState()
	if before.ServerVersion != supportedFloorVersion.String() {
		t.Fatalf("before: ServerVersion = %q; want %q", before.ServerVersion, supportedFloorVersion.String())
	}
	if before.ServerVersionSource != info.ServerVersionSourceProbe {
		t.Errorf("before: ServerVersionSource = %q; want %q", before.ServerVersionSource, info.ServerVersionSourceProbe)
	}

	live.store(resolutionAt(t, chopt.Version{Major: 25, Minor: 9},
		chopt.FeatureAggregationInOrder, chopt.FeatureTSGridRange))

	after := live.infoState()
	if after.ServerVersion != "25.9" {
		t.Errorf("after: ServerVersion = %q; want 25.9", after.ServerVersion)
	}
	if !containsID(after.Enabled, chopt.FeatureTSGridRange) {
		t.Errorf("after: Enabled = %v; want it to carry %s", after.Enabled, chopt.FeatureTSGridRange)
	}
}

// TestCHOptLive_InfoStateReportsTheFallbackSource — a resolution reached
// without a live version answer must say so, or an operator reads the assumed
// floor as a measurement of their server.
func TestCHOptLive_InfoStateReportsTheFallbackSource(t *testing.T) {
	t.Parallel()

	res := resolutionAt(t, supportedFloorVersion)
	res.VersionFallback = true

	if got := newCHOptLive(res).infoState().ServerVersionSource; got != info.ServerVersionSourceFallback {
		t.Errorf("ServerVersionSource = %q; want %q", got, info.ServerVersionSourceFallback)
	}
}

// containsID reports whether ids carries want.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestCHOptConsumers_CoverEveryServedHead — the swap can only reach a consumer
// the wiring collected, so the consumer set has to track CERBERUS_ENABLED_HEADS
// exactly: one engine per head this process built, and the prom handler
// whenever the prom head is served. A head missing here would keep serving the
// boot posture forever while the log line, /info, and the live holder all claim
// the upgrade landed.
//
// What the swap then DOES to a consumer is pinned where the state lives:
// internal/engine (settings) and internal/api/prom (lowering strategy table).
func TestCHOptConsumers_CoverEveryServedHead(t *testing.T) {
	for _, tc := range []struct {
		enabled     string
		wantEngines int
		wantProm    bool
	}{
		{"prom", 1, true},
		{"loki", 1, false},
		{"tempo", 1, false},
		{"loki,tempo", 2, false},
		{"", 3, true},
	} {
		t.Run("enabled="+tc.enabled, func(t *testing.T) {
			consumers := mountedConsumers(t, tc.enabled)
			if got := len(consumers.engines); got != tc.wantEngines {
				t.Errorf("engines = %d; want %d — a head with no engine here is never re-swapped", got, tc.wantEngines)
			}
			if got := consumers.prom != nil; got != tc.wantProm {
				t.Errorf("prom handler collected = %v; want %v", got, tc.wantProm)
			}
		})
	}
}

// mountedConsumers builds the heads CERBERUS_ENABLED_HEADS selects and returns
// the consumer set the re-probe would swap through. chclient.New never dials,
// so no ClickHouse is needed.
func mountedConsumers(t *testing.T, enabledHeads string) chOptConsumers {
	t.Helper()
	t.Setenv("CERBERUS_ENABLED_HEADS", enabledHeads)

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	cfg.ClickHouse.Addr = unreachableAddr(t)

	logger := quietLogger()
	promLimiter, lokiLimiter, tempoLimiter := newAdmitLimiters(cfg, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	heads, err := mountAPIHeads(ctx, http.NewServeMux(), lazyClient(t), cfg, chopt.EnabledSet{},
		promLimiter, lokiLimiter, tempoLimiter, logger)
	if err != nil {
		t.Fatalf("mountAPIHeads: %v", err)
	}
	return heads.consumers
}

// TestCHOptConsumers_ApplyToleratesAHeadThatIsNotServed — under the chart's
// split mode a process may serve no prom head at all, so apply must swap what
// exists and skip what does not rather than dereference a nil handler.
func TestCHOptConsumers_ApplyToleratesAHeadThatIsNotServed(t *testing.T) {
	t.Parallel()

	set := resolutionAt(t, chopt.Version{Major: 25, Minor: 9}, chopt.FeatureAggregationInOrder).Set
	cfg := config.Config{Schema: schema.DefaultOTelMetrics()}

	chOptConsumers{engines: []*engine.Engine{{}}}.apply(cfg, set)
	chOptConsumers{}.apply(cfg, set)
}

// TestReprobeCHOptimizations_SwapsWhenTheAnswerChanges — the loop's whole job.
// The probe here cannot reach a server, so every pass resolves against the
// supported floor; seeding the holder with a richer resolution means the first
// tick must correct it. That is the same path a pod that booted while
// ClickHouse was down takes in reverse, and it proves the loop actually
// publishes rather than merely ticking.
func TestReprobeCHOptimizations_SwapsWhenTheAnswerChanges(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		CHOptimizations:     "auto",
		CHOptimizationsMode: chopt.Permissive,
		Schema:              schema.DefaultOTelMetrics(),
	}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond

	// Seed with a posture no unreachable probe can produce, so a swap is
	// observable and a loop that never publishes fails.
	live := newCHOptLive(resolutionAt(t, chopt.Version{Major: 25, Minor: 9},
		chopt.FeatureAggregationInOrder, chopt.FeatureTSGridRange))
	e := &engine.Engine{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live,
			chOptConsumers{engines: []*engine.Engine{e}}, time.Millisecond)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if got := live.get(); got.ResolvedVersion == supportedFloorVersion && !got.Set.Has(chopt.FeatureTSGridRange) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the re-probe never corrected the resolution: %+v", live.get())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reprobeCHOptimizations did not return on ctx cancellation — it would outlive shutdown")
	}
}

// TestReprobeCHOptimizations_StopsOnContextCancel — the loop is bound to the
// process lifetime, so a cancelled ctx must return it rather than leak a
// ticking goroutine past shutdown.
func TestReprobeCHOptimizations_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), config.Config{}, newCHOptLive(chOptResolution{}),
			chOptConsumers{}, time.Hour)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reprobeCHOptimizations ignored a cancelled context")
	}
}
