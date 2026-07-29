package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/config"
)

// Layer 8 — the ClickHouse connection age-eviction default against the chaos
// lane's recovery deadline.
//
// ConnMaxLifetime is the backstop that bounds recovery after a ClickHouse pod
// is force-killed: the pod's socket can stay ESTABLISHED with no FIN/RST, the
// driver's per-acquire check passes the dead conn through as healthy, and every
// query on it blocks until something AGES it out. TCP keepalive is the primary
// detector, but the lifetime is what holds when keepalive is off — and the
// chaos lane asserts the breaker closes again within BREAKER_CLOSE_DEADLINE_MS.
//
// A lifetime at or near that deadline therefore has no margin at all: the very
// eviction the assertion depends on lands at the moment the assertion gives up,
// and the lane fails as an unreproducible "flake" rather than as a config
// error. Widening the default is exactly the kind of change that looks free in
// review — fewer redials, less churn — while silently consuming the whole
// budget, so the two numbers are pinned to each other here instead of living in
// prose in two files that nothing compares.
//
// Neither number is asserted absolutely: the pin is the RATIO, so tuning either
// side stays possible as long as the margin survives.

// connLifetimeDeadlineDivisor is the margin the age-eviction default must keep
// against the chaos breaker-close deadline. Halfway is the loosest ratio under
// which an eviction still has a full second window to be OBSERVED closing the
// breaker before the deadline expires.
const connLifetimeDeadlineDivisor = 2

// chaosRunScript owns the breaker-close deadline the chaos lane enforces.
const chaosRunScript = ".github/scripts/chaos-run.mjs"

// breakerCloseDeadlinePattern extracts the deadline from its declaration,
// tolerating JS numeric separators (300_000).
var breakerCloseDeadlinePattern = regexp.MustCompile(
	`(?m)^const\s+BREAKER_CLOSE_DEADLINE_MS\s*=\s*([0-9_]+)\s*;`,
)

func TestCHConnMaxLifetimeKeepsChaosRecoveryMargin(t *testing.T) {
	path := filepath.Join(repoRoot, filepath.FromSlash(chaosRunScript))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", chaosRunScript, err)
	}
	match := breakerCloseDeadlinePattern.FindSubmatch(src)
	if match == nil {
		t.Fatalf("%s declares no BREAKER_CLOSE_DEADLINE_MS; this pin relates the ClickHouse "+
			"age-eviction default to that deadline and cannot be silently dropped", chaosRunScript)
	}
	ms, err := strconv.Atoi(strings.ReplaceAll(string(match[1]), "_", ""))
	if err != nil {
		t.Fatalf("BREAKER_CLOSE_DEADLINE_MS = %q: %v", match[1], err)
	}
	deadline := time.Duration(ms) * time.Millisecond

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	lifetime := cfg.ClickHouse.ConnMaxLifetime
	if lifetime <= 0 {
		t.Fatalf("default ConnMaxLifetime = %s; a non-positive lifetime disables age eviction entirely", lifetime)
	}

	ceiling := deadline / connLifetimeDeadlineDivisor
	if lifetime > ceiling {
		t.Errorf("default CERBERUS_CH_CONN_MAX_LIFETIME = %s, over the %s ceiling implied by "+
			"BREAKER_CLOSE_DEADLINE_MS = %s in %s. A stale connection from a force-killed ClickHouse "+
			"pod is evicted no sooner than the lifetime, so the chaos lane's breaker-close assertion "+
			"would be racing the very eviction it depends on",
			lifetime, ceiling, deadline, chaosRunScript)
	}
}
