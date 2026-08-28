package main

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/api/info"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
)

// chOptLive is the process-wide holder of the ClickHouse capability resolution
// currently in force. Every consumer that reports or acts on the resolution
// reads it from here, so there is exactly one answer to "what can this server
// do" at any instant and no consumer can be left holding a copy the re-probe
// has already superseded.
//
// The resolution is stored behind an atomic pointer because it is read from
// request goroutines (/info) while the re-probe goroutine replaces it: a whole
// new chOptResolution is published in one store, so a reader sees the version,
// the fallback flag, and the feature set of ONE resolution, never a mixture of
// two.
type chOptLive struct {
	res atomic.Pointer[chOptResolution]
}

// newCHOptLive seeds the holder with the boot resolution.
func newCHOptLive(res chOptResolution) *chOptLive {
	l := &chOptLive{}
	l.res.Store(&res)
	return l
}

// get returns the resolution in force right now.
func (l *chOptLive) get() chOptResolution {
	return *l.res.Load()
}

// store publishes res as the resolution in force from now on.
func (l *chOptLive) store(res chOptResolution) {
	l.res.Store(&res)
}

// infoState renders the current resolution for the /info fingerprint. It is the
// closure infoOptions hands the info handler, so a scrape taken seconds after a
// rolling ClickHouse upgrade reports the capabilities cerberus is ACTUALLY
// emitting against rather than the ones it booted with.
func (l *chOptLive) infoState() info.OptState {
	res := l.get()
	source := info.ServerVersionSourceProbe
	if res.VersionFallback {
		source = info.ServerVersionSourceFallback
	}
	return info.OptState{
		ServerVersion:       res.ResolvedVersion.String(),
		ServerVersionSource: source,
		Enabled:             res.Set.IDs(),
	}
}

// chOptConsumers are the query-path surfaces a resolved capability set decides,
// collected so a re-resolution can swap all of them in one pass. Both are
// derived values, not copies of the set: the engines take the per-query
// SettingsRules the set gates, and the prom handler takes the native-lowering
// dispatch table the set selects.
//
// Nothing else in the process reads the set on the query path. The only other
// set-gated decision, the client-side columnar matrix decode, is off the version
// axis entirely (columnar_result_decode is AlwaysAvailable and opt-in only), so
// no server upgrade can move it and it stays a boot-time swap.
type chOptConsumers struct {
	// engines are the built heads' engines, in mount order. A disabled head has
	// no engine here, so the swap touches exactly what this process serves.
	engines []*engine.Engine
	// prom is the PromQL handler, or nil when the prom head is disabled. It is
	// the only head with a native-lowering dispatch table.
	prom *prom.Handler
}

// apply installs the decisions set implies on every consumer. The two swaps are
// independent pointer stores, so a query dispatched between them lowers with one
// table and executes with the other's settings — both of which are individually
// valid for the same server, since a capability the set dropped only ever
// DISABLES a native path and a capability it gained only ever enables one.
func (c chOptConsumers) apply(cfg config.Config, set chopt.EnabledSet) {
	rules := settingsRules(cfg, set)
	for _, e := range c.engines {
		e.SetSettings(rules)
	}
	if c.prom != nil {
		c.prom.SetLowerers(nativeRangeLowerers(set))
	}
}

// chOptReprobeInterval is the cadence at which cerberus re-reads the connected
// ClickHouse server's capabilities. It is a compromise between two costs that
// pull in opposite directions: a shorter interval spends two short-lived
// connections and two trivial statements more often against a server whose
// answer almost never changes, and a longer one leaves a pod emitting the
// fallback shape for longer after an upgrade has already made the native shape
// available. Five minutes bounds the post-upgrade blind window to well under
// the time a rolling ClickHouse upgrade itself takes, at a probe rate that is
// invisible next to query traffic.
const chOptReprobeInterval = 5 * time.Minute

// chOptFloorRetryInterval is the cadence used INSTEAD while the resolution in
// force is a floor fallback — i.e. the probe that produced it could not reach
// the server at all.
//
// The two states are not symmetric, and pacing them the same way was a bug.
// The steady-state interval above is priced against a server whose answer
// "almost never changes", which is true of a healthy deployment and false of a
// pod that lost the startup race with its own ClickHouse: that pod is pinned to
// the supported floor, every native path is silently unavailable, and the
// answer is expected to change the moment the server finishes coming up. Five
// minutes of that is not a blind window on an upgrade, it is five minutes of
// emitting the slow shape for no reason — long enough that a deployment can
// serve its first traffic, and a whole e2e suite can run, entirely on the
// fallback lowerers (#2696).
//
// Ten seconds is chosen against the same cost model: the probe is two
// short-lived connections and two trivial statements, and it only runs at this
// rate while the process is in a state it wants to leave. As soon as a probe
// answers, the loop returns to the steady cadence.
const chOptFloorRetryInterval = 10 * time.Second

// nextReprobeDelay is how long to wait before the next resolution attempt,
// given the one currently in force.
func nextReprobeDelay(res chOptResolution, steady time.Duration) time.Duration {
	if res.VersionFallback {
		floor := chOptFloorRetryInterval
		// Never wait LONGER than the steady cadence just because a caller
		// configured an interval shorter than the fast-retry constant.
		if steady < floor {
			return steady
		}
		return floor
	}
	return steady
}

// reprobeCHOptimizations re-resolves the ClickHouse capability set on a fixed
// cadence and swaps a changed result into the query path, so a cluster upgraded
// under a running cerberus starts using the newly-available native paths without
// a restart. It runs until ctx is cancelled (SIGTERM / process shutdown).
//
// Each tick repeats exactly the boot resolution — probe the server version, run
// the experimental-setting capability canary, and resolve the SAME configured
// selection against both — so the running process can never reach a state boot
// could not have produced. What it deliberately does NOT repeat is the boot's
// fatal-on-config-fault behaviour: a selection naming an unknown or unsupported
// feature id would have failed startup, and the selection cannot change under a
// running process, so a resolve error here is evidence of something transient
// and is logged and retried rather than taken as grounds to kill a serving pod.
//
// A probe that cannot reach the server resolves against the supported floor,
// exactly as boot does. That is what makes the loop symmetric: a pod that booted
// while ClickHouse was down pinned itself to the floor, and this is the path
// that lifts it off the floor once the server answers, with no restart.
//
// Only a GENUINE transition is logged and swapped. The steady state — the
// overwhelming majority of ticks — compares equal and does nothing, so the log
// carries one line per real capability change and an operator watching an
// upgrade sees the transition rather than having to find it in a stream of
// repetitions.
func reprobeCHOptimizations(
	ctx context.Context,
	logger *slog.Logger,
	cfg config.Config,
	live *chOptLive,
	consumers chOptConsumers,
	interval time.Duration,
) {
	// A timer rather than a ticker: the delay is re-derived from the
	// resolution in force after every attempt, so a pod pinned to the floor
	// retries at chOptFloorRetryInterval and drops back to the steady cadence
	// the moment a probe answers.
	timer := time.NewTimer(nextReprobeDelay(live.get(), interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		next, ok := resolveCHOptimizationsOnce(ctx, logger, cfg)
		if !ok {
			// The resolution in force is unchanged, so the delay is derived
			// from it: still on the floor means still retrying fast.
			timer.Reset(nextReprobeDelay(live.get(), interval))
			continue
		}
		prev := live.get()
		timer.Reset(nextReprobeDelay(next, interval))
		if next.Set.Equal(prev.Set) && next.ResolvedVersion == prev.ResolvedVersion {
			continue
		}
		// Publish BEFORE swapping the consumers: /info then never reports a
		// capability set older than the one the query path is already emitting
		// against, which is the direction an operator can act on.
		live.store(next)
		consumers.apply(cfg, next.Set)
		logger.Info(
			"clickhouse optimizations re-resolved",
			"server_version", next.ResolvedVersion.String(),
			"previous_server_version", prev.ResolvedVersion.String(),
			"enabled", strings.Join(next.Set.IDs(), ","),
			"previous_enabled", strings.Join(prev.Set.IDs(), ","),
		)
	}
}

// resolveCHOptimizationsOnce runs one probe-and-resolve pass and reports the
// result, or ok=false when the resolution failed and the caller must keep the
// set already in force. It is the re-probe's half of resolveCHOptimizations:
// the same version probe, the same capability canary, and the same resolver
// against the same configured selection — but with none of boot's side effects
// (no config back-fill, no columnar-decode swap, no fatal exit), because those
// are decisions a process makes once and the re-probe must not re-make.
func resolveCHOptimizationsOnce(ctx context.Context, logger *slog.Logger, cfg config.Config) (chOptResolution, bool) {
	resolvedVersion, err := probeVersionOverBootstrap(ctx, cfg.ClickHouse)
	versionFallback := err != nil
	if err != nil {
		resolvedVersion = supportedFloorVersion
	}

	set, _, err := chopt.Resolve(chopt.Config{
		Optimizations: cfg.CHOptimizations,
		Mode:          cfg.CHOptimizationsMode,
		LegacyTSGrid:  cfg.LegacyTSGridFlag,
		Capability:    probeTSGridCapabilityOverBootstrap(ctx, cfg.ClickHouse),
	}, resolvedVersion)
	if err != nil {
		// Unreachable for a selection that already resolved at boot (the
		// selection is fixed for the life of the process), so this is logged
		// rather than swallowed: it would mean the resolver disagreed with
		// itself, which an operator needs to see.
		logger.Warn("clickhouse optimizations re-resolve failed; keeping the set in force", "err", err)
		return chOptResolution{}, false
	}
	// The resolver's warnings are boot-time operator guidance (permissive skips,
	// the legacy-alias deprecation) about a selection that has not changed, so
	// re-logging them every tick would say nothing new.
	return chOptResolution{
		Set:             set,
		ResolvedVersion: resolvedVersion,
		VersionFallback: versionFallback,
	}, true
}
