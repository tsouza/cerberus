package main

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestNextReprobeDelay_FloorFallbackRetriesFast pins the asymmetry between the
// two states the re-probe can be in.
//
// A pod that lost the startup race with its own ClickHouse resolves against the
// supported floor, which silently disables every native lowering for as long as
// that resolution stays in force. Pacing that state at the steady-state cadence
// meant a deployment could serve its first traffic — and a whole e2e suite
// could run — entirely on the fallback shape (#2696). The steady cadence is
// priced for a server whose answer almost never changes; the floor state is one
// the process is actively trying to leave.
func TestNextReprobeDelay_FloorFallbackRetriesFast(t *testing.T) {
	t.Parallel()

	onFloor := chOptResolution{VersionFallback: true}
	if got := nextReprobeDelay(onFloor, chOptReprobeInterval); got != chOptFloorRetryInterval {
		t.Errorf("a floor fallback must re-probe at %v, got %v — every native path stays\n"+
			"unavailable until it does", chOptFloorRetryInterval, got)
	}

	probed := chOptResolution{Set: chopt.EnabledSet{}, VersionFallback: false}
	if got := nextReprobeDelay(probed, chOptReprobeInterval); got != chOptReprobeInterval {
		t.Errorf("a resolution the server actually answered must use the steady cadence %v, got %v",
			chOptReprobeInterval, got)
	}
}

// TestNextReprobeDelay_NeverWaitsLongerThanTheSteadyCadence pins that the fast
// path can only ever shorten the wait. A caller configuring an interval below
// the fast-retry constant (the tests do) must not be slowed down by it.
func TestNextReprobeDelay_NeverWaitsLongerThanTheSteadyCadence(t *testing.T) {
	t.Parallel()

	const tiny = time.Millisecond
	if got := nextReprobeDelay(chOptResolution{VersionFallback: true}, tiny); got != tiny {
		t.Errorf("with a steady cadence of %v the floor retry must not stretch to %v, got %v",
			tiny, chOptFloorRetryInterval, got)
	}
}

// TestCapabilitiesResolved_TracksTheLiveResolution pins that the readiness
// condition reads the LIVE resolution, not the boot one — the whole point is
// that it clears when a probe answers, without a restart.
func TestCapabilitiesResolved_TracksTheLiveResolution(t *testing.T) {
	t.Parallel()

	live := newCHOptLive(chOptResolution{VersionFallback: true})
	cond := capabilitiesResolved(live)

	resolved, reason := cond()
	if resolved {
		t.Fatal("a floor fallback must hold readiness")
	}
	if reason == "" {
		t.Error("the /readyz body needs a reason, not a bare false")
	}

	live.store(chOptResolution{VersionFallback: false})
	if resolved, _ := cond(); !resolved {
		t.Error("readiness must clear once a probe answers, without a restart")
	}
}
