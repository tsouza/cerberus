package loki

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMinePatternsClearsE2ESeedFloor pins the class of regression that
// broke the "dashboard" e2e gate for 12+ hours after #2205 added the
// minimumPatternVolume floor (mirroring upstream Loki's minClusterSize):
// the rolling e2e seed (test/e2e/seed/cmd/seed/main.go's insertLogsSQL)
// generates otel_logs rows for {service_name="api"} that, once the
// rolling re-seeder (`just e2e-seed-rolling`, 30 s ticks) has accumulated
// a realistic handful of ticks, must produce at least one drain cluster
// whose volume clears minimumPatternVolume — otherwise the
// /loki/api/v1/patterns e2e assertion
// (test/e2e/playwright/loki_ux.spec.ts:152, "patterns: the /patterns
// endpoint extracts drain clusters from log bodies") sees
// body.data.length == 0 on every run, deterministically, forever: the
// pre-fix seed split ~14 api rows/tick evenly across 5 body templates and
// up to 2 severity levels (minePatterns trains one drain.Miner per
// level), which tops out around 3 lines/tick/cluster and never clears
// the 30-line floor even after the rolling re-seeder's full ~587 s
// retention window (test/e2e/seed/cmd/seed/stale.go's logsStaleMargin).
//
// This test mirrors insertLogsSQL's row-generation formula in Go — the
// SQL is the source of truth; keep the two in sync:
//
//	service  = ['api', 'frontend', 'db'][number % 3]
//	severity = multiIf(number % 3 = 0, 'WARN', number % 5 = 0, 'ERROR', 'INFO')
//	body     = multiIf(number % 3 = 0, 'handled request', ...) + " id=" + number
//
// api rows (number % 3 == 0) are pinned to one severity + one body
// template, so every api row lands in the SAME drain cluster instead of
// splitting across templates/levels.
func TestMinePatternsClearsE2ESeedFloor(t *testing.T) {
	t.Parallel()

	const (
		tickInterval = 30 * time.Second // e2e-seed-rolling's --re-seed-interval
		rowsPerTick  = 40               // insertLogsSQL: FROM numbers(40)
		cadence      = 15 * time.Second // insertLogsSQL: (number - 20) * 15 SECOND

		// realisticDwellTicks is a conservative lower bound on how many
		// rolling re-seed ticks land before the dashboard e2e's
		// Playwright patterns spec fires: `just e2e-run`'s full Go spec
		// suite against the live stack runs between `e2e-seed-rolling`
		// start and the Playwright step, comfortably clearing several
		// ticks in every observed run.
		realisticDwellTicks = 6
	)

	now := time.Now()
	start := now.Add(-5 * time.Minute) // last5MinWindow() in loki_ux.spec.ts
	end := now

	var lines []chclient.TimestampedLine
	for tick := 0; tick < realisticDwellTicks; tick++ {
		tickNow := now.Add(-time.Duration(tick) * tickInterval)
		for n := 0; n < rowsPerTick; n++ {
			if n%3 != 0 {
				continue // insertLogsSQL: arrayElement(['api','frontend','db'], number%3+1) != 'api'
			}
			ts := tickNow.Add(time.Duration(n-20) * cadence)
			if ts.Before(start) || ts.After(end) {
				continue
			}
			lines = append(lines, chclient.TimestampedLine{
				Timestamp: ts,
				Severity:  "WARN", // insertLogsSQL: multiIf(number % 3 = 0, 'WARN', ...)
				Body:      fmt.Sprintf("handled request id=%d", n),
			})
		}
	}

	got := minePatterns(lines, start, end, minimumPatternSampleResolution)
	if len(got) == 0 {
		t.Fatalf("0 clusters after %d re-seed ticks (%d api lines in the query window) — "+
			"the e2e seed fixture no longer clears minimumPatternVolume=%d; "+
			"see test/e2e/seed/cmd/seed/main.go's insertLogsSQL",
			realisticDwellTicks, len(lines), minimumPatternVolume)
	}
	if v := patternTestVolume(got[0]); v < minimumPatternVolume {
		t.Fatalf("best cluster volume=%d want >= minimumPatternVolume=%d after %d re-seed ticks",
			v, minimumPatternVolume, realisticDwellTicks)
	}
}
