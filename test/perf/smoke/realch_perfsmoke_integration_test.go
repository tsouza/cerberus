//go:build integration

// realch_perfsmoke_integration_test.go — the real-ClickHouse perf-smoke
// sentinel differential (#2370 PR 1).
//
// # Why this lane exists
//
// On 2026-08-18, PR #2358 disabled ClickHouse's query analyzer for
// native-histogram plan shapes to fix a real timeout (#2355), validated only
// by a manual 8-run timing comparison on CI-fixture-scale data. That
// silently removed a CSE fold the SQL emitter relied on. 6h8m later, prod
// hit MEMORY_LIMIT_EXCEEDED (#2364) — peak memory ranging ~101 MiB to
// 6.4+ GiB — because two array-scan quantities were no longer folded into
// single scans. Fixed in 38 minutes once diagnosed (#2366). The failure mode
// (memory, not time) was invisible at CI scale and only appears at real
// cardinality on a real ClickHouse server. Before this lane, NOTHING in CI
// ran real CH with real memory measurement.
//
// This test drives the mounted PRODUCTION prom + tempo handlers over real
// HTTP against a real ClickHouse (testcontainers-go), seeded at the scale
// each incident's own root-cause mechanism needs to actually engage (see
// sentinels.go's Sentinels and seed.go's builders), and reads peak per-query
// memory back from system.query_log via optcorpus.CHQueryLogSource — the same
// production reader the async query_log corpus reconciler uses. It is issue
// #2370's small, ships-fast first slice: a targeted sentinel differential for
// the EXACT incident, not the full multi-week design #2370 itself describes.
//
// # Two servers, not one
//
// The corpus is tiered by smoke.ServerFloor and the harness boots ONE
// container per floor: perfSmokeCHImage (25.9-alpine, the tag every other
// strict-scan real-CH lane uses) for FloorBase, and perfSmokeJoinSpillCHImage
// for FloorJoinSpill, whose mechanism chopt gates behind a 26.4 server. Each
// constant's own comment carries the why.
//
// # Two boot-resolved axes, and two kinds of lane
//
// Both handlers carry the per-query engine.SettingsRules a real deployment
// resolves at boot (chopttest.BuildSettingsRules, cerberus issue #2820) —
// without it Engine.Settings stays at its zero value, which applies NOTHING,
// and no sentinel here could measure a settings-rule mechanism at all. The
// OTHER boot-resolved axis, the native ts_grid_* RangeLowerers table, stays at
// prom.New's fan-out-only zero value on every floor's BASE ("auto") lane;
// wiring it there is issue #2487's own scope.
//
// A sentinel whose mechanism is opt-in-only — AutoSelect: false and, unlike
// join_spill, carrying NO chopt version floor at all — cannot be reached by
// "auto" on any server, so cerberus issue #3050 adds a SECOND kind of lane,
// distinct from smoke.ServerFloor's server-CAPABILITY tiering: one extra
// prom.Handler / tempo.Handler pair per distinct explicit
// CERBERUS_CH_OPTIMIZATIONS listing a floor's own sentinels declare
// (Sentinel.Optimizations), with BOTH boot-resolved axes wired from that
// listing's own resolved chopt.EnabledSet. See startSentinelLane's own doc
// for the construction and sentinelLane.muxFor for how a sentinel picks
// its lane.
//
// Gated behind the `integration` build tag (Docker required); wired into the
// already-required strict-scan CI lane via `just perf-smoke-integration`.
package smoke

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// perfSmokeCHImage pins the same ClickHouse server image every other
// strict-scan real-CH lane uses (traces_scan_window_integration_test.go
// etc.) — testcontainers' own wait-strategy against this alpine tag doesn't
// hit the healthcheck race docker-compose's Ubuntu-base image does. It backs
// smoke.FloorBase, and every pre-existing sentinel's committed baseline is a
// measurement of THIS server.
const perfSmokeCHImage = "clickhouse/clickhouse-server:25.9-alpine"

// perfSmokeJoinSpillCHImage backs smoke.FloorJoinSpill: a SECOND container,
// alongside (never instead of) perfSmokeCHImage.
//
// chopt.FeatureJoinSpill floors at 26.4 — max_bytes_before_external_join does
// not exist below it, and an unknown setting name errors the whole query
// rather than no-op'ing — so the join-spill stamp cannot fire on
// perfSmokeCHImage at all, no matter what sentinel is written for it. The two
// tiers COEXIST rather than one replacing the other for two independent
// reasons: sub-floor behaviour (the stamp must stay absent, the lowering
// byte-for-byte unchanged) is a supported path this lane still has to
// measure, and bumping the shared image would move every existing sentinel's
// calibrated baseline — the numbers this corpus exists to hold still — for a
// feature none of them exercise.
//
// The tag is 26.6-alpine rather than a 26.4 one because it is the image this
// repo ALREADY pins and pre-pulls for its highest-floor lanes (the Justfile's
// CH_STRICT_SCAN_IMAGE, also used by test/perf/nightly's ts_grid_instant
// measurement), so the second tier costs strict-scan no new image download.
// The FLOOR is chopt's 26.4; the IMAGE is the repo's existing pin at or above
// it. Which of the two it is never has to be guessed: the harness asserts
// chopt.FeatureJoinSpill actually resolved in against the live container
// before running a single FloorJoinSpill sentinel.
const perfSmokeJoinSpillCHImage = "clickhouse/clickhouse-server:26.6-alpine"

const perfSmokeDB = "default"

// --- calibrated constants (task 4) ----------------------------------------
//
// Every number below was measured, not guessed, against a real
// testcontainers ClickHouse on this branch. See each constant's own comment
// for the specific run that produced it; the PR description carries the full
// calibration log. The memory cap, its ceiling fraction and the baseline
// headroom — the numbers the ceilings themselves are derived from — live in
// the untagged baseline.go alongside that derivation, so the unit lane can
// assert the derived bounds without Docker.

// sentinelRepeats (N) is the max-of-N repeat count each sentinel's memory
// measurement is taken over — a CEILING gate wants the worst observed case,
// so max, never median or mean. N=5 was checked against real repeat spread
// during calibration: peak-memory variance across 5 repeats stayed under 2%
// for every sentinel (see sentinelBaselineHeadroom's comment for the
// concrete numbers), so 5 is kept rather than widened.
const sentinelRepeats = 5

// spillSeriesCount is Sentinel 2's calibrated `session_id` cardinality.
// The plan's starting-point guess was ~200,000 series; real measurement
// showed that guess was roughly 20x too high for a 1 GiB cap — at the
// wideCounterScrapeInterval density below, series counts from 20,000 up
// already exceed the cap outright (20,000 series measured 1.06 GiB > cap;
// 40,000+ OOMs, code 241). Sweeping downward against a real testcontainers
// ClickHouse 25.8-alpine at the fixed 1 GiB cap: 8,000 series -> 563 MiB,
// 10,000 -> ~640-650 MiB (comfortably above spillThreshold(1 GiB)=512 MiB,
// so the spill path is genuinely engaged, not incidentally unreached),
// 12,000 -> 717 MiB, 15,000 -> 878 MiB (already over the 0.75x/805 MiB
// absolute ceiling). 10,000 is the calibrated value: solidly past the 512
// MiB spill threshold with real margin below both the absolute ceiling and
// the 1 GiB cap itself, and stable within 2% across repeats.
const spillSeriesCount = 10_000

// wideTraceCount is Sentinel 3's calibrated trace count (2 spans/trace).
// Swept against a real testcontainers ClickHouse 25.8-alpine at the fixed
// 1 GiB cap: 10,000 traces -> 338 MiB, 20,000 -> 393 MiB, 30,000 -> 334 MiB
// (stable, but BELOW spillThreshold(1 GiB)=512 MiB — spill not yet
// engaged), 40,000 -> 682 MiB (comfortably above the 512 MiB spill
// threshold, so applyCompareMemoryBound's spill setting is genuinely
// engaged, and comfortably under the 0.75x/805 MiB absolute ceiling with
// ~15% margin). 40,000 is the calibrated value: the smallest of the swept
// sizes that clears the spill threshold with real margin on both sides.
const wideTraceCount = 40_000

// joinSpillSeriesCount is the join-spill tier's `session_id` cardinality,
// calibrated the same way spillSeriesCount was and against the same fixed
// 1 GiB cap, but on perfSmokeJoinSpillCHImage: the sentinel joins TWO
// aggregations over the seeded counter, so its cost is not spillSeriesCount's
// 10,000-series one. Measured max-of-5 against a real
// clickhouse/clickhouse-server:26.6-alpine container at this value:
// 449,714,998 bytes, 41.9% of the cap — real work on both sides of the join,
// with margin under the 0.75x/805 MiB absolute ceiling wide enough that the
// 1.5x committed ceiling still lands below it and so actually gates.
//
// Deliberately NOT pushed up until the join's build side crosses
// spillThreshold(cap) and the spill genuinely engages: this sentinel's
// load-bearing assertion is the STAMP (Sentinel.RequiredQuerySettings), not
// whether ClickHouse chose to act on it, and a cardinality tuned to sit right
// at the spill boundary would make the memory prongs flap around that
// boundary for no gain in what the sentinel actually proves.
const joinSpillSeriesCount = 4_000

// --- harness --------------------------------------------------------------

// sentinelWindowEnd anchors every sentinel's query_range window. Fixed
// (not time.Now()) so a re-run against a freshly-seeded container is
// byte-reproducible.
var sentinelWindowEnd = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// sentinelLane is one floor's fully-wired substrate: the connection its
// query_log is read through, the mux(es) its sentinels are issued against,
// and the query_log reader their memory is measured with.
//
// muxes is keyed by Sentinel.Optimizations: "" is the base "auto"-resolved
// lane every pre-existing sentinel runs against, and any other key is an
// opt-in-only lane startSentinelLane builds specifically for a sentinel
// whose mechanism needs an explicit CERBERUS_CH_OPTIMIZATIONS listing
// "auto" itself never turns on (cerberus issue #3050) — see
// muxFor's own doc.
type sentinelLane struct {
	conn      driver.Conn
	muxes     map[string]*http.ServeMux
	logSource *optcorpus.CHQueryLogSource
}

// muxFor returns the ServeMux sentinel must be driven against: its own
// opt-in lane if Sentinel.Optimizations names one, otherwise the floor's
// base "auto" lane. Fatals loudly rather than silently falling back — a
// missing lane means startSentinelLane and this corpus have drifted, and a
// silent fallback to the base lane would make an opt-in-only sentinel pass
// vacuously against the fan-out path it exists to move past.
func (l sentinelLane) muxFor(t *testing.T, sentinel Sentinel) *http.ServeMux {
	t.Helper()
	mux, ok := l.muxes[sentinel.Optimizations]
	if !ok {
		t.Fatalf("%s: no lane wired for optimizations %q — startSentinelLane must build one for every "+
			"distinct Sentinel.Optimizations value a floor's sentinels declare", sentinel.Name, sentinel.Optimizations)
	}
	return mux
}

func TestPerfSmokeRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	end := sentinelWindowEnd
	start := end.Add(-sentinelWindow)

	baseline := loadPerfSmokeBaseline(t)
	update := os.Getenv("UPDATE_PERF_SMOKE_BASELINE") == "1"
	updated := make(map[string]sentinelBound, len(Sentinels))

	// One container per floor, base first. Both are brought up in the same
	// test so the single committed baseline file stays the product of ONE
	// calibration run — two top-level tests would each write it and the
	// second would silently drop the first's measurements.
	for _, floor := range []ServerFloor{FloorBase, FloorJoinSpill} {
		sentinels := SentinelsForFloor(floor)
		if len(sentinels) == 0 {
			t.Fatalf("floor %s has no sentinels — the tier is booting a real ClickHouse for nothing", floor)
		}
		lane := startSentinelLane(ctx, t, floor, start, end)
		runSentinelFloor(ctx, t, lane, sentinels, start, end, baseline, update, updated)
	}

	if update {
		writePerfSmokeBaseline(t, updated)
	}
}

// runSentinelFloor drives one floor's sentinels against its own lane: the
// per-sentinel warm-up, the max-of-N memory measurement, the required-settings
// assertion, and the two ceiling prongs (or, in calibration mode, the bound
// capture).
func runSentinelFloor(
	ctx context.Context, t *testing.T, lane sentinelLane, sentinels []Sentinel,
	start, end time.Time, baseline perfSmokeBaseline, update bool, updated map[string]sentinelBound,
) {
	t.Helper()
	conn, logSource := lane.conn, lane.logSource

	for _, sentinel := range sentinels {
		t.Run(sentinel.Name, func(t *testing.T) {
			mux := lane.muxFor(t, sentinel)

			// One untimed, unmeasured warm-up: startSentinelLane's
			// OPTIMIZE ... FINAL pass leaves this sentinel's table's
			// marks/data cold, and the FIRST query
			// to touch it after that pays a real (measured, up to ~15% in
			// calibration) extra allocation this warm-up absorbs — mirroring
			// scale_wall_pin_chdb_test.go's bestOfWall, which does the identical
			// thing for its OWN first-call cost, just for TIME rather than memory.
			warmupID := fmt.Sprintf("perfsmoke-%s-warmup", sentinel.Name)
			if code := runSentinelOnce(t, mux, sentinel, start, end, warmupID); code != http.StatusOK {
				t.Fatalf("%s warm-up: HTTP %d (want 200)", sentinel.Name, code)
			}

			var maxBytes uint64
			for i := 0; i < sentinelRepeats; i++ {
				queryID := fmt.Sprintf("perfsmoke-%s-%d", sentinel.Name, i)
				code := runSentinelOnce(t, mux, sentinel, start, end, queryID)
				if code != http.StatusOK {
					t.Fatalf("%s repeat %d: HTTP %d (want 200) — likely a memory-cap breach (code 241) or a "+
						"rejected shape; the mechanism this sentinel targets (%s) may have regressed",
						sentinel.Name, i, code, sentinel.Mechanism)
				}
				if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
					t.Fatalf("%s repeat %d: flush logs: %v", sentinel.Name, i, err)
				}
				rows, err := logSource.FinishedByQueryID(ctx, []string{queryID})
				if err != nil {
					t.Fatalf("%s repeat %d: read query_log: %v", sentinel.Name, i, err)
				}
				if len(rows) != 1 {
					t.Fatalf("%s repeat %d: expected 1 query_log row for query_id %q, got %d — "+
						"the sentinel query dispatched more (or fewer) than one CH statement",
						sentinel.Name, i, queryID, len(rows))
				}
				if rows[0].ExitStatus != optcorpus.ExitOK {
					t.Fatalf("%s repeat %d: query_log reports a non-OK exit (%v) for query_id %q",
						sentinel.Name, i, rows[0].ExitStatus, queryID)
				}
				if rows[0].MemoryUsage > maxBytes {
					maxBytes = rows[0].MemoryUsage
				}

				// The settings-rule assertion, checked on EVERY repeat rather
				// than once: this is the only prong that can tell a fired
				// mechanism from an unwired one (see
				// Sentinel.RequiredQuerySettings), and a rule that fires
				// intermittently is as broken as one that never fires.
				for setting, want := range sentinel.RequiredSettings(sentinelMemoryCapBytes) {
					chopttest.AssertQuerySettingStamped(ctx, t, conn, queryID, setting, want)
				}
			}

			const capCeiling = sentinelCapCeilingBytes
			t.Logf("%s (%s): max-of-%d peak memory_usage = %d bytes (%.1f%% of %d-byte cap; absolute ceiling %d)",
				sentinel.Name, sentinel.Mechanism, sentinelRepeats, maxBytes,
				100*float64(maxBytes)/float64(sentinelMemoryCapBytes), sentinelMemoryCapBytes, capCeiling)

			if update {
				// committedCeilingBytes clamps to capCeiling — see its own
				// comment for why an unclamped PRONG (b) ceiling gates
				// nothing (#2906).
				ceiling := committedCeilingBytes(maxBytes)
				updated[sentinel.Name] = sentinelBound{Name: sentinel.Name, MaxOfNBytes: maxBytes, CeilingBytes: ceiling}
				return
			}

			// PRONG (a): absolute, cap-relative ceiling.
			if maxBytes > capCeiling {
				t.Errorf("%s: peak memory %d bytes exceeds the absolute cap-relative ceiling %d bytes "+
					"(%.0f%% of the %d-byte cap) — %s may have regressed",
					sentinel.Name, maxBytes, capCeiling, 100*sentinelMemoryCapFraction, sentinelMemoryCapBytes, sentinel.Mechanism)
			}

			// PRONG (b): committed per-sentinel ceiling.
			bound, ok := baselineFor(baseline, sentinel.Name)
			if !ok {
				t.Fatalf("%s: no committed bound in perf-smoke-baseline.json — run `just update-perf-smoke-baseline`", sentinel.Name)
			}
			if maxBytes > bound.CeilingBytes {
				// The headroom reported is the committed ceiling's ACTUAL
				// ratio to the calibration measurement, not
				// sentinelBaselineHeadroom: a sentinel whose ceiling
				// committedCeilingBytes clamped to the absolute one carries
				// less than the nominal multiple, and printing the nominal
				// figure would misreport how much margin the sentinel really
				// had.
				headroom := float64(bound.CeilingBytes) / float64(bound.MaxOfNBytes)
				t.Errorf("%s: peak memory %d bytes exceeds the committed ceiling %d bytes (measured max-of-N was "+
					"%d at calibration time, %.2fx headroom) — %s may have regressed; only run "+
					"`just update-perf-smoke-baseline` if the increase is genuinely intended",
					sentinel.Name, maxBytes, bound.CeilingBytes, bound.MaxOfNBytes, headroom, sentinel.Mechanism)
			}
		})
	}
}

// runSentinelOnce issues one GET for sentinel over [start, end) with ctx
// pre-stamped to queryID (via chclient.WithQueryID) so the ClickHouse
// dispatch that results carries EXACTLY this query_id in system.query_log —
// no log_comment correlation needed, since each sentinel dispatches exactly
// one CH statement per request (a single lowered plan, Solver nil under
// prom.New / tempo.New's wiring, so route A always applies).
func runSentinelOnce(t *testing.T, mux *http.ServeMux, sentinel Sentinel, start, end time.Time, queryID string) int {
	t.Helper()
	params := sentinel.Params(start, end)
	if sentinel.Path == "/api/v1/query_range" {
		params.Set("start", formatPromTime(start))
		params.Set("end", formatPromTime(end))
		params.Set("step", sentinel.Step.String())
	} else {
		params.Set("start", fmt.Sprintf("%d", start.UnixNano()))
		params.Set("end", fmt.Sprintf("%d", end.UnixNano()))
		if sentinel.Step > 0 {
			params.Set("step", sentinel.Step.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, sentinel.Path+"?"+params.Encode(), nil)
	req = req.WithContext(chclient.WithQueryID(req.Context(), queryID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Logf("%s: HTTP %d body: %s", sentinel.Name, rec.Code, rec.Body.String())
	}
	return rec.Code
}

func formatPromTime(t time.Time) string {
	return fmt.Sprintf("%.3f", float64(t.UnixNano())/1e9)
}

// optimizeTableFinal runs OPTIMIZE TABLE ... FINAL on table so background
// merge state doesn't add noise to the memory reading that follows.
func optimizeTableFinal(ctx context.Context, t *testing.T, client *chclient.Client, table string) {
	t.Helper()
	if err := client.Conn().Exec(ctx, "OPTIMIZE TABLE "+table+" FINAL"); err != nil {
		t.Fatalf("optimize table %s: %v", table, err)
	}
}

// startSentinelLane brings up one floor's ClickHouse, applies the DDL, seeds
// exactly the fixtures that floor's sentinels read, and mounts the production
// handlers with the SAME two boot-resolved axes cmd/cerberus wires from a
// chopt.EnabledSet.
//
// The base ("auto") lane wires only ONE of those two axes — the SettingsRules
// one (chopttest.BuildSettingsRules). Its RangeLowerers axis stays at
// prom.New's zero value, which promql.RangeLowerers.withDefaults normalises
// to the concrete fan-out impls, exactly as this lane has always run: wiring
// the native ts_grid_* families into the OTHER real-CH lanes is issue #2487's
// own scope, and flipping them here would silently re-calibrate every
// existing sentinel's committed baseline as a side effect of a
// settings-rules change.
//
// Wiring SettingsRules is what closes issue #2820: prom.New / tempo.New leave
// Engine.Settings at its zero value, which applies NOTHING, so before this
// every SettingsRules mechanism — aggregation_in_order and condition_cache as
// much as join_spill — was unreachable in this corpus regardless of the
// server it ran against.
//
// Beyond the base lane, this function builds one ADDITIONAL lane per
// distinct non-empty Sentinel.Optimizations value among floor's own
// sentinels (cerberus issue #3050): a feature like
// chopt.FeatureSortedSlabOverTime is AutoSelect: false and carries no chopt
// version floor at all, so "auto" alone never activates it on ANY server —
// the only way a sentinel reaches it is an explicit
// CERBERUS_CH_OPTIMIZATIONS listing, resolved into its OWN chopt.EnabledSet,
// its OWN chopttest.BuildSettingsRules, and — unlike the base lane — its OWN
// chopttest.BuildRangeLowerers. Wiring RangeLowerers here does not touch the
// base lane's own fan-out-only posture or re-calibrate any existing
// sentinel's baseline: the opt-in lane is a SEPARATE prom.Handler /
// tempo.Handler pair mounted on its own ServeMux, sharing only the
// underlying ClickHouse connection.
func startSentinelLane(ctx context.Context, t *testing.T, floor ServerFloor, start, end time.Time) sentinelLane {
	t.Helper()

	sentinels := SentinelsForFloor(floor)
	image := perfSmokeCHImage
	signals := []ddl.Signal{ddl.Metrics, ddl.Traces}
	if floor == FloorJoinSpill {
		image = perfSmokeJoinSpillCHImage
		// Metrics only: the join-spill tier's sentinel reads the seeded
		// counter and nothing else, and seeding wideTraceCount traces it
		// never queries would cost this lane real time for no measurement.
		signals = []ddl.Signal{ddl.Metrics}
	}

	client := startPerfSmokeCH(ctx, t, image)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, signals); err != nil {
		t.Fatalf("%s: apply DDL: %v", floor, err)
	}

	// Tables OPTIMIZE ... FINAL is run against below, in seed order.
	var tables []string
	switch floor {
	case FloorBase:
		histRows, err := SeedNativeHistogramAtScale(ctx, conn, NativeHistogramMetric, NativeHistogramSeriesCount, start, end)
		if err != nil {
			t.Fatalf("seed native histogram: %v", err)
		}
		t.Logf("seeded native histogram: %d rows, %d series", histRows, NativeHistogramSeriesCount)

		counterRows, err := SeedHighCardinalityCounter(ctx, conn, WideCounterMetric, spillSeriesCount, start, end)
		if err != nil {
			t.Fatalf("seed high-cardinality counter: %v", err)
		}
		t.Logf("seeded high-cardinality counter: %d rows, %d series", counterRows, spillSeriesCount)

		traceRows, err := SeedWideAttributeTraces(ctx, conn, wideTraceCount, start, end)
		if err != nil {
			t.Fatalf("seed wide-attribute traces: %v", err)
		}
		t.Logf("seeded wide-attribute traces: %d rows, %d traces", traceRows, wideTraceCount)

		gaugeRows, err := SeedSortedSlabOverTimeGauge(ctx, conn, SortedSlabOverTimeGaugeMetric, SortedSlabOverTimeSeriesCount, start, end)
		if err != nil {
			t.Fatalf("seed sorted-slab gauge: %v", err)
		}
		t.Logf("seeded sorted-slab gauge: %d rows, %d series", gaugeRows, SortedSlabOverTimeSeriesCount)

		tables = []string{"otel_metrics_exponential_histogram", "otel_metrics_sum", "otel_traces", "otel_metrics_gauge"}
	case FloorJoinSpill:
		counterRows, err := SeedHighCardinalityCounter(ctx, conn, WideCounterMetric, joinSpillSeriesCount, start, end)
		if err != nil {
			t.Fatalf("seed high-cardinality counter: %v", err)
		}
		t.Logf("seeded high-cardinality counter: %d rows, %d series", counterRows, joinSpillSeriesCount)

		tables = []string{"otel_metrics_sum"}
	}

	// OPTIMIZE ... FINAL before measuring: controls background-merge-state
	// noise so a sentinel's memory reading reflects the query itself, not an
	// in-flight merge competing for the same cap.
	for _, table := range tables {
		optimizeTableFinal(ctx, t, client, table)
	}

	metricsSchema := schema.DefaultOTelMetrics()
	tracesSchema := schema.DefaultOTelTraces()
	metricsHandler := prom.New(client, metricsSchema, nil)
	tracesHandler := tempo.New(client, tracesSchema, "v-perf-smoke", nil)
	mux := http.NewServeMux()
	metricsHandler.Mount(mux)
	tracesHandler.Mount(mux)

	// "auto" is the CERBERUS_CH_OPTIMIZATIONS default a real deployment boots
	// under: no explicit opt-in, best-available on the connected server. Every
	// AutoSelect feature the container's own probed version supports resolves
	// in, join_spill included on the >= 26.4 tier.
	set := chopttest.ResolveEnabledSet(ctx, t, client, "auto")
	rules := chopttest.BuildSettingsRules(set, metricsSchema, tracesSchema, schema.DefaultOTelLogs())
	if rules.ResultCache {
		t.Fatalf("%s: the resolved set enabled chopt.FeatureResultCache — a query RESULT cache would serve "+
			"repeats 1..%d of every sentinel from cache, so the max-of-N ceiling would stop measuring the "+
			"query and start measuring a cache hit", floor, sentinelRepeats-1)
	}
	if floor == FloorJoinSpill && !rules.JoinSpill {
		t.Fatalf("%s: chopt.FeatureJoinSpill did NOT resolve enabled against %s — every join_spill sentinel "+
			"below would pass vacuously against a server that cannot stamp %s at all",
			floor, image, settingMaxBytesBeforeExternalJoin)
	}
	metricsHandler.Engine.SetSettings(rules)
	tracesHandler.Engine.SetSettings(rules)
	t.Logf("%s (%s): settings rules wired: %+v", floor, image, rules)

	muxes := map[string]*http.ServeMux{"": mux}
	for _, opt := range optInLaneKeys(sentinels) {
		muxes[opt] = buildOptInLane(ctx, t, client, floor, image, opt, sentinels, metricsSchema, tracesSchema)
	}

	return sentinelLane{
		conn:      conn,
		muxes:     muxes,
		logSource: optcorpus.NewCHQueryLogSource(conn, 30*time.Second, time.Hour),
	}
}

// optInLaneKeys collects the distinct non-empty Sentinel.Optimizations
// values among sentinels, in first-seen order — the set of additional lanes
// startSentinelLane must build beyond the base "auto" one.
func optInLaneKeys(sentinels []Sentinel) []string {
	seen := make(map[string]bool, len(sentinels))
	var keys []string
	for _, s := range sentinels {
		if s.Optimizations == "" || seen[s.Optimizations] {
			continue
		}
		seen[s.Optimizations] = true
		keys = append(keys, s.Optimizations)
	}
	return keys
}

// buildOptInLane resolves opt against client's real connected server,
// asserts every sentinel that declares a RequiredFeature under this exact
// Optimizations string actually resolved it enabled (the same
// "activation, not just a 200" guard
// TestNativeRangeLowerers_RealCH_Integration applies via its own
// enabled.Has(family.Feature) check), then mounts a SEPARATE prom.Handler /
// tempo.Handler pair — its own SettingsRules AND its own
// chopttest.BuildRangeLowerers, both resolved from opt — on a fresh
// ServeMux. Sharing client rather than opening a second connection: opt-in
// features like chopt.FeatureSortedSlabOverTime carry no version floor, so
// they need no server capability the base lane's own connection lacks.
func buildOptInLane(
	ctx context.Context, t *testing.T, client *chclient.Client, floor ServerFloor, image, opt string,
	sentinels []Sentinel, metricsSchema schema.Metrics, tracesSchema schema.Traces,
) *http.ServeMux {
	t.Helper()

	set := chopttest.ResolveEnabledSet(ctx, t, client, opt)
	for _, s := range sentinels {
		if s.Optimizations != opt || s.RequiredFeature == "" {
			continue
		}
		if !set.Has(s.RequiredFeature) {
			t.Fatalf("%s: chopt feature %q did NOT resolve enabled against optimizations %q on %s (%s) — "+
				"this sentinel's own activation would be vacuous against a server or listing that cannot "+
				"reach the mechanism at all", s.Name, s.RequiredFeature, opt, floor, image)
		}
	}

	rules := chopttest.BuildSettingsRules(set, metricsSchema, tracesSchema, schema.DefaultOTelLogs())
	lowerers := chopttest.BuildRangeLowerers(set)

	metricsHandler := prom.New(client, metricsSchema, nil)
	metricsHandler.Lowerers = lowerers
	tracesHandler := tempo.New(client, tracesSchema, "v-perf-smoke", nil)
	mux := http.NewServeMux()
	metricsHandler.Mount(mux)
	tracesHandler.Mount(mux)
	metricsHandler.Engine.SetSettings(rules)
	tracesHandler.Engine.SetSettings(rules)
	t.Logf("%s (%s): opt-in lane %q wired: enabled=%v settings=%+v", floor, image, opt, set.IDs(), rules)

	return mux
}

func startPerfSmokeCH(ctx context.Context, t *testing.T, image string) *chclient.Client {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		image,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(perfSmokeDB),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	client, err := chclient.New(chclient.Config{
		Addr:                host + ":" + port.Port(),
		Database:            perfSmokeDB,
		Username:            "cerberus",
		Password:            "cerberus",
		BreakerDisabled:     true,
		MaxQueryMemoryBytes: sentinelMemoryCapBytes,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// --- baseline load/write (invariant 9: never hand-edited) -----------------
//
// The file format, the loader and the writer live in the untagged baseline.go;
// these two wrappers add only this lane's test plumbing (t.Fatalf, and the
// calibration path's tolerance for a not-yet-created file).

func loadPerfSmokeBaseline(t *testing.T) perfSmokeBaseline {
	t.Helper()
	b, err := readPerfSmokeBaseline()
	if err != nil {
		if os.Getenv("UPDATE_PERF_SMOKE_BASELINE") == "1" && errors.Is(err, fs.ErrNotExist) {
			return perfSmokeBaseline{}
		}
		t.Fatalf("%v — run `just update-perf-smoke-baseline` to create it", err)
	}
	return b
}

// writePerfSmokeBaseline orders updated (keyed by sentinel name) by Sentinels
// and hands it to writePerfSmokeBaselineFile — this is the ONLY code path that
// writes perf-smoke-baseline.json, gated on UPDATE_PERF_SMOKE_BASELINE=1.
func writePerfSmokeBaseline(t *testing.T, updated map[string]sentinelBound) {
	t.Helper()
	bounds := make([]sentinelBound, 0, len(Sentinels))
	for _, s := range Sentinels {
		bound, ok := updated[s.Name]
		if !ok {
			t.Fatalf("writePerfSmokeBaseline: no measurement for sentinel %q", s.Name)
		}
		bounds = append(bounds, bound)
	}
	if err := writePerfSmokeBaselineFile(bounds); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("wrote %s", perfSmokeBaselinePath)
}
