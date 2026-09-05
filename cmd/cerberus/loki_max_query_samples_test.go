package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// lokiMaxQuerySamplesWant is deliberately unlike every default in play (0
// in a bare chclient.Config, and 0 was exactly what the Loki head's
// engine carried before this fix), so a wiring that reaches the handler
// can only have come from the client this test built.
const lokiMaxQuerySamplesWant = 12345

// newLokiClientWithSamples builds a client that never dials (chclient.New
// only validates options) carrying the given MaxQuerySamples.
func newLokiClientWithSamples(t *testing.T, maxSamples int64) *chclient.Client {
	t.Helper()
	client, err := chclient.New(chclient.Config{
		Addr:            unreachableAddr(t),
		Database:        "otel",
		DialTimeout:     time.Second,
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		MaxQuerySamples: maxSamples,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestNewLokiHandler_WiresMaxQuerySamples pins the fix for issue #2055:
// newLokiHandler must carry the client's configured MaxQuerySamples onto
// Handler.Engine, the same way newPromHandler already does (main.go,
// "MaxQuerySamples: client.MaxQuerySamples()"). Left at the zero-value
// engine.Engine literal's default, requireSubquerySampleBudget
// (internal/engine/anchor_budget.go) fail-opens — `maxSamples <= 0`
// returns nil before it ever looks at the plan — so the plan-time
// anchor-grid gate silently never fires on the Loki head.
func TestNewLokiHandler_WiresMaxQuerySamples(t *testing.T) {
	client := newLokiClientWithSamples(t, lokiMaxQuerySamplesWant)
	cfg := config.Config{Logs: schema.DefaultOTelLogs()}
	limiters := newAdmitLimiters(cfg, quietLogger())

	h := newLokiHandler(client, cfg, chopt.EnabledSet{}, limiters, quietLogger(), engine.ResourceBoundOverrides{}, nil)

	if h.Engine == nil {
		t.Fatal("Handler.Engine is nil")
	}
	if h.Engine.MaxQuerySamples != lokiMaxQuerySamplesWant {
		t.Fatalf("Handler.Engine.MaxQuerySamples = %d, want %d — the Loki head's plan-time "+
			"anchor-grid budget gate fail-opens on every subquery until this is wired",
			h.Engine.MaxQuerySamples, lokiMaxQuerySamplesWant)
	}
}

// TestMountAPIHeads_EveryBuiltEngineCarriesMaxQuerySamples is the
// suggested-check half of issue #2055's fix: a regression test asserting
// every built head's engine carries a positive MaxQuerySamples closes the
// CLASS of "this head's engine silently fail-opens the sample-budget
// gate" rather than pinning only the Loki leg. Runs all three heads
// through the real mountAPIHeads wiring, off one client whose
// MaxQuerySamples is positive, and requires every engine it hands to the
// optimization-corpus consumer set to have inherited that value.
func TestMountAPIHeads_EveryBuiltEngineCarriesMaxQuerySamples(t *testing.T) {
	client := newLokiClientWithSamples(t, lokiMaxQuerySamplesWant)
	cfg := config.Config{
		Logs:   schema.DefaultOTelLogs(),
		Schema: schema.DefaultOTelMetrics(),
		Traces: schema.DefaultOTelTraces(),
		EnabledHeads: config.EnabledHeads{
			config.HeadProm:  {},
			config.HeadLoki:  {},
			config.HeadTempo: {},
		},
	}
	logger := quietLogger()
	limiters := newAdmitLimiters(cfg, logger)

	heads, err := mountAPIHeads(t.Context(), http.NewServeMux(), client, cfg, chopt.EnabledSet{}, limiters, logger, engine.ResourceBoundOverrides{}, promql.ResourceBounds{}, preflightAttrStrategies{})
	if err != nil {
		t.Fatalf("mountAPIHeads: %v", err)
	}

	engines := heads.consumers.engines
	if len(engines) != 3 {
		t.Fatalf("mountAPIHeads built %d engine(s) with all three heads enabled, want 3", len(engines))
	}
	for i, eng := range engines {
		if eng.MaxQuerySamples != lokiMaxQuerySamplesWant {
			t.Errorf("engine %d: MaxQuerySamples = %d, want %d — this head's plan-time "+
				"anchor-grid budget gate fail-opens on every subquery until this is wired",
				i, eng.MaxQuerySamples, lokiMaxQuerySamplesWant)
		}
	}
}
