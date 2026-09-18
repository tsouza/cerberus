// Package cadence is the one Go source for the rolling re-seeder's tick
// interval. The seeder (test/e2e/seed/cmd/seed) takes it as the
// --re-seed-interval flag, and every launcher passes the same value:
// docker-compose.yml's seed service `command`, and
// .github/scripts/e2e-seed-rolling.mjs's reseedIntervalFlag. Neither of
// those can import a Go constant, so test/regression pins both literals to
// ReSeedIntervalFlag; the Go tests that reason about the cadence — the
// seeder's own reseed_stability_test.go and the stale-margin bounds in
// test/regression/seed_test.go — read it from here.
package cadence

import "time"

// RollingReSeedInterval is how often the background rolling re-seeder
// re-anchors every seeded family on the wall clock. Every per-family
// stale-row margin (test/e2e/seed/cmd/seed/stale.go) is reasoned against
// it: a margin below one tick collects the previous tick on every DELETE
// (at most two copies visible), a wider one bounds duplication at roughly
// margin / tick copies.
const RollingReSeedInterval = 30 * time.Second

// ReSeedIntervalFlag is the exact flag the launchers pass to the seeder.
const ReSeedIntervalFlag = "--re-seed-interval=30s"
