//go:build integration

// realch_perfnightly_integration_test.go — the nightly, real-data
// measurement + regression gate (#2370 PR A + PR B).
//
// # Why this lane exists
//
// test/perf/smoke's real-ClickHouse sentinel differential (#2370 PR 1,
// merged) proves 3 fixed, calibrated sentinels on SYNTHETIC data would have
// caught the #2364 incident — but it's a snapshot, not a growing net. This
// harness is the next slice of the actual "large-scale, realistic-scale
// performance/resource-cost test lane" #2370 itself is titled: it loads the
// REAL 14-day production sample (test/perf/smoke/testdata/samples/*.parquet,
// #2411, already scrubbed) — trimmed to one representative day for
// sustainable nightly LFS bandwidth, see testdata/samples/README.md — and
// issues a curated sentinel corpus broader than the smoke tier's 3 fixed
// queries, covering construct families the smoke tier never reaches at all:
// a CLASSIC (bucket/bounds) histogram_quantile (the real sample is
// classic-shaped, not exponential — issue #2408's arrayJoin bucket-rate
// fan-out), a Gauge aggregation (zero smoke-tier coverage of this signal
// type), a plain counter rate, a cross-series ratio shape, and a DERIVED
// native (exponential) histogram_quantile — the real production sample has
// no captured exponential-histogram data at all, so loader.go re-buckets
// the real classic sample into an OTel exponential layout at the same real
// cardinality/cadence (see loader.go's loadNativeHistogramFromClassicSample
// doc comment) rather than reusing smoke's own CI-fixture-scale synthetic
// one.
//
// PR A shipped no baseline or gate — its test plan was "run it, print the
// numbers." Two of those numbers (request_rate_by_method,
// error_ratio_by_namespace) turned out to be issue #2429: a plain
// `sum by()(rate())`-shaped query genuinely OOMing at this sample's real
// series count and anchor grid, despite applySpillSettings' always-on
// spill. #2429 is now fixed — internal/chsql's rate-window fanout resource
// bound rejects both cleanly (422) instead of letting ClickHouse OOM — so
// this PR (PR B) is the first run with real, POST-FIX numbers to calibrate
// a committed baseline against, the same "task 4: real numbers, not the
// plan's starting-point guesses" methodology test/perf/smoke's own PR
// followed.
//
// # The gate
//
// Two-pronged, mirroring test/perf/smoke's perf-smoke-baseline.json exactly
// (see its own realch_perfsmoke_integration_test.go for the fuller
// rationale this reuses verbatim):
//
//   - PRONG (a): an absolute, cap-relative ceiling every sentinel's peak
//     memory must stay under (nightlyMemoryCapFraction), independent of any
//     committed baseline — catches a regression even on a freshly-created,
//     baseline-less sentinel.
//   - PRONG (b): a committed per-sentinel ceiling in nightly-baseline.json,
//     the calibration-time max-of-N measurement times a headroom multiplier
//     (Sentinel.BaselineHeadroom, per-sentinel) — tighter than PRONG (a), so it is what
//     actually catches a real regression long before the absolute ceiling
//     would.
//
// A THIRD check this lane adds beyond test/perf/smoke's shape, because two
// of its five sentinels are EXPECTED to be rejected rather than to
// complete: every repeat's HTTP status must match sentinel.ExpectedStatus
// exactly, and when that status isn't 200 the response body must carry
// sentinel.ExpectedErrorSubstring. A status-class change either
// direction — the #2429 bound silently breaking (OOM risk returns) or
// silently over-broadening (a legitimate query starts being rejected) — is
// itself the regression signal for those two sentinels, and is checked
// BEFORE the memory prongs (a rejected query's "peak memory at rejection"
// is a real, trackable number too, but only meaningful once the rejection
// itself is confirmed to be the RIGHT one).
//
// Gated behind the `integration` build tag (Docker required); wired into
// .github/workflows/perf-nightly.yml via `just perf-nightly-integration`.
// Regenerate the baseline via `just update-nightly-perf-baseline`
// (UPDATE_NIGHTLY_PERF_BASELINE=1) — never hand-edited (invariant 9).
//
// # Reporting
//
// Every sentinel's final verdict (status-class match, both memory prongs,
// and the single overall Pass) is also captured into a results.go
// SentinelResult and written, at the end of a non-calibration run, to
// PERF_NIGHTLY_RESULTS_JSON — see results.go's own doc comment for why:
// two of the five sentinels above are SUPPOSED to be rejected, and
// cerberus's own engine.go correctly logs a WARN line for every rejected
// query unconditionally, so the raw `go test -v` log alone cannot tell a
// human apart "expected rejection" from "real regression" without manually
// cross-referencing sentinels.go. perf-nightly-step-summary.mjs renders
// that JSON into perf-nightly.yml's own $GITHUB_STEP_SUMMARY as an
// unambiguous verdict table, ahead of the raw log.
package nightly

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// perfNightlyCHImageEnv names the ClickHouse image the lane boots.
// perf-nightly.yml runs the lane once per server of its matrix, each leg
// setting this through `just perf-nightly-integration <image>`; the bound set
// a run asserts is the one calibrated on the release line the booted server
// reports (nightly-baseline.json).
const perfNightlyCHImageEnv = "PERF_NIGHTLY_CH_IMAGE"

const perfNightlyDB = "default"

// perfNightlyMemoryCapBytes, nightlyMemoryCapFraction and the
// nightlyCapCeilingBytes / committedCeilingBytes derivation they feed live in
// the untagged baseline.go, so the unit lane can assert the derived bounds
// without Docker.

// Per-sentinel committed-ceiling headroom multipliers used to be one
// package-wide nightlyBaselineHeadroom constant (1.5x, mirroring
// test/perf/smoke's sentinelBaselineHeadroom). A real noise investigation
// found the four sentinels' run-to-run peak-memory variance differs by
// nearly an order of magnitude, so a single multiplier was either too loose
// for the quiet sentinels or flake-risked the noisy ones — see each
// sentinel's own BaselineHeadroom field and sentinels.go's *BaselineHeadroom
// constants for the real measurements backing each value.

// nightlySentinelRepeats (N) mirrors test/perf/smoke's sentinelRepeats — a
// ceiling gate wants the worst observed case, so max-of-N, never
// median/mean.
const nightlySentinelRepeats = 5

// nightlyPlanningSamples is how many interleaved planning-time samples of a
// sentinel's statement and of its planning peer's the planning prong takes;
// it compares the minimum of each.
const nightlyPlanningSamples = 3

// maxPlanningPeerRatio bounds how much longer a sentinel's statement may take
// to plan than its planning peer's (Sentinel.PlanningPeer). The plural native
// quantile over a rate()-folded native histogram plans in 1.2-1.4x the time
// of the single-phi quantile over the same input, and in 20-45x when it
// reaches ClickHouse under the analyzer the native-histogram analyzer fix
// turns off — measured on 24.8, 25.9 and 26.6. 4x leaves the first a wide
// margin and sits far below the second.
const maxPlanningPeerRatio = 4.0

func TestPerfNightlyRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	container, client := startPerfNightlyCH(ctx, t)
	conn := client.Conn()
	version := serverReleaseLine(ctx, t, conn)
	t.Logf("ClickHouse release line %s", version)

	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}

	histRows, err := loadHistogramSample(ctx, container, client, sampleParquetPath(t, "svc_http_request_duration_seconds.parquet"))
	if err != nil {
		t.Fatalf("load histogram sample: %v", err)
	}
	t.Logf("loaded classic histogram sample: %d rows", histRows)

	sumRows, err := loadSumSample(ctx, container, client, sampleParquetPath(t, "svc_http_requests_total.parquet"))
	if err != nil {
		t.Fatalf("load sum sample: %v", err)
	}
	t.Logf("loaded counter sample: %d rows", sumRows)

	gaugeRows, err := loadGaugeSample(ctx, container, client, sampleParquetPath(t, "kube_pod_status_reason.parquet"))
	if err != nil {
		t.Fatalf("load gauge sample: %v", err)
	}
	t.Logf("loaded gauge sample: %d rows", gaugeRows)

	// Derives the native-histogram sentinel's data from the classic sample
	// loadHistogramSample just loaded — see loader.go's
	// loadNativeHistogramFromClassicSample doc comment for why this is a
	// DERIVATION rather than a fifth parquet load: the real production
	// sample has no captured exponential-histogram data at all.
	nativeHistRows, err := loadNativeHistogramFromClassicSample(ctx, client)
	if err != nil {
		t.Fatalf("derive native histogram from classic sample: %v", err)
	}
	t.Logf("derived native histogram sample: %d rows", nativeHistRows)

	// The exporter-keyed deployments (sentinels.go's Deployment): the
	// exporter's own DDL in stockKeyDB, holding the counter sample, and the
	// same tables renamed in stockKeyRenamedDB.
	stockKeyClient := newPerfNightlyClient(ctx, t, container, stockKeyDB)
	stockKeyRenamedClient := newPerfNightlyClient(ctx, t, container, stockKeyRenamedDB)
	createStockKeyDeployments(ctx, t, container, client, stockKeyClient)

	// OPTIMIZE ... FINAL before measuring: controls background-merge-state
	// noise so a sentinel's memory reading reflects the query itself, not an
	// in-flight merge competing for the same cap (same rationale as
	// test/perf/smoke's optimizeTableFinal).
	for _, table := range []string{
		"otel_metrics_histogram", "otel_metrics_sum", "otel_metrics_gauge", "otel_metrics_exponential_histogram",
		stockKeyDB + ".otel_metrics_sum", stockKeyRenamedDB + "." + stockKeyRenamedTables["otel_metrics_sum"],
	} {
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+table+" FINAL"); err != nil {
			t.Fatalf("optimize table %s: %v", table, err)
		}
	}

	// ONE probe-and-resolve pass, exactly as cmd/cerberus's boot does, feeding
	// every boot-wired axis this lane needs from it.
	set := chopttest.ResolveEnabledSet(ctx, t, client, "auto")
	// pipelineConns read each deployment's database, the one its dispatched
	// statements resolve unqualified table names against.
	pipelineConns := map[Deployment]driver.Conn{
		DeploymentProvisioned:     conn,
		DeploymentStockKey:        stockKeyClient.Conn(),
		DeploymentStockKeyRenamed: stockKeyRenamedClient.Conn(),
	}
	muxes := map[Deployment]*http.ServeMux{
		DeploymentProvisioned:     nightlyMux(ctx, t, client, perfNightlyDB, schema.DefaultOTelMetrics(), set),
		DeploymentStockKey:        nightlyMux(ctx, t, stockKeyClient, stockKeyDB, schema.DefaultOTelMetrics(), set),
		DeploymentStockKeyRenamed: nightlyMux(ctx, t, stockKeyRenamedClient, stockKeyRenamedDB, stockKeyRenamedMetrics(), set),
	}

	logSource := optcorpus.NewCHQueryLogSource(conn, 30*time.Second, time.Hour)
	known := knownSettings(ctx, t, conn)

	baseline := loadNightlyBaseline(t)
	versionBaseline, _ := baseline.forVersion(version)
	update := os.Getenv("UPDATE_NIGHTLY_PERF_BASELINE") == "1"
	calibrate := calibrationSelection(t, update)
	updated := make(map[string]nightlyBound, len(Sentinels))

	// results accumulates one SentinelResult per sentinel regardless of
	// pass/fail — a subtest's t.Errorf/t.Fatalf aborts only that subtest's
	// own goroutine (see results.go's doc comment), so this loop always
	// runs to completion and the write below always has something to
	// render, even on a real regression. Skipped entirely in calibration
	// mode (update=true): there is no committed baseline to compare
	// against yet, so "pass/fail" is not a meaningful concept for that run.
	results := make([]SentinelResult, 0, len(Sentinels))

	for _, sentinel := range Sentinels {
		if reason, out := sentinel.NotOn[version]; out {
			t.Logf("%s is not in ClickHouse %s's sentinel set: %s", sentinel.Name, version, reason)
			continue
		}
		if update && !calibrate(sentinel.Name) {
			continue
		}
		mux := muxes[sentinel.Deployment]
		sentinel.QuerySettings = settingsServerKnows(sentinel.QuerySettings, known)

		t.Run(sentinel.Name, func(t *testing.T) {
			// result accumulates this sentinel's outcome as the subtest
			// progresses; Pass defaults to false and is only ever flipped to
			// true once every check below has actually succeeded, so a
			// t.Fatalf anywhere in this closure (which aborts the goroutine
			// via runtime.Goexit, skipping everything textually after it)
			// still leaves an honest, correctly-failing result behind for
			// the Cleanup below to capture.
			result := SentinelResult{
				Name:           sentinel.Name,
				Family:         sentinel.Family,
				ExpectedStatus: sentinel.ExpectedStatus,
				Rejected:       sentinel.ExpectedStatus != http.StatusOK,
			}
			if !update {
				// t.Cleanup (unlike code textually after a t.Fatalf) always
				// runs, including after the Goexit a fatal assertion
				// triggers — this is what lets a genuinely regressed
				// sentinel still show up in the step summary as a clear
				// FAIL instead of silently vanishing from the report.
				t.Cleanup(func() { results = append(results, result) })
			}

			// One untimed, unmeasured warm-up — mirrors test/perf/smoke's own
			// rationale: the OPTIMIZE ... FINAL pass above leaves this
			// sentinel's tables cold, and the first query after that pays a
			// real extra allocation this warm-up absorbs.
			warmupID := fmt.Sprintf("perfnightly-%s-warmup", sentinel.Name)
			if code, body := runSentinelOnce(t, mux, sentinel, warmupID); code != sentinel.ExpectedStatus {
				result.ActualStatus = code
				t.Fatalf("%s warm-up: HTTP %d (want %d): %s", sentinel.Name, code, sentinel.ExpectedStatus, body)
			} else if sentinel.ExpectedStatus != http.StatusOK && !strings.Contains(body, sentinel.ExpectedErrorSubstring) {
				result.ActualStatus = code
				t.Fatalf("%s warm-up: HTTP %d body does not carry %q: %s", sentinel.Name, code, sentinel.ExpectedErrorSubstring, body)
			}

			// The planning prong, once per sentinel: planning time does not
			// depend on the data a repeat reads, and an unbounded plan costs
			// minutes per sample, so sampling it on every repeat would only
			// slow the run down.
			if sentinel.PlanningPeer != nil {
				peerID := fmt.Sprintf("perfnightly-%s-planning-peer", sentinel.Name)
				peer := sentinel
				peer.Params = sentinel.PlanningPeer
				if code, body := runSentinelOnce(t, mux, peer, peerID); code != http.StatusOK {
					t.Fatalf("%s planning peer: HTTP %d: %s", sentinel.Name, code, body)
				}
				if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
					t.Fatalf("%s: flush logs: %v", sentinel.Name, err)
				}
				ratio, planned, peerPlanned, abandoned, err := planningRatio(ctx, pipelineConns[sentinel.Deployment], warmupID, peerID)
				if err != nil {
					t.Fatalf("%s: %v", sentinel.Name, err)
				}
				t.Logf("%s: planning min-of-%d %v against its peer's %v (%.2fx; bound %.0fx; %d of %d samples abandoned at the bound)",
					sentinel.Name, nightlyPlanningSamples, planned, peerPlanned, ratio, maxPlanningPeerRatio, abandoned, nightlyPlanningSamples)
				if ratio > maxPlanningPeerRatio {
					t.Fatalf("%s: planning the dispatched statement takes %.2fx its peer's (%v against %v), over the %.0fx bound — %s may have regressed",
						sentinel.Name, ratio, planned, peerPlanned, maxPlanningPeerRatio, sentinel.Family)
				}
			}

			var maxBytes uint64
			for i := range nightlySentinelRepeats {
				queryID := fmt.Sprintf("perfnightly-%s-%d", sentinel.Name, i)
				code, body := runSentinelOnce(t, mux, sentinel, queryID)
				result.ActualStatus = code
				// The status-class check comes first and is fatal, not just a
				// logged finding: for the two ExpectedStatus=422 sentinels this
				// IS the #2429 regression signal (the bound silently breaking
				// or over-broadening), not a missed memory ceiling.
				if code != sentinel.ExpectedStatus {
					t.Fatalf("%s repeat %d: HTTP %d (want %d) — a status-class change is itself the regression here: %s",
						sentinel.Name, i, code, sentinel.ExpectedStatus, body)
				}
				if sentinel.ExpectedStatus != http.StatusOK && !strings.Contains(body, sentinel.ExpectedErrorSubstring) {
					t.Fatalf("%s repeat %d: HTTP %d body does not carry %q — a different fault than intended may be firing: %s",
						sentinel.Name, i, code, sentinel.ExpectedErrorSubstring, body)
				}

				if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
					t.Fatalf("%s repeat %d: flush logs: %v", sentinel.Name, i, err)
				}
				rows, err := logSource.FinishedByQueryID(ctx, []string{queryID})
				if err != nil {
					t.Fatalf("%s repeat %d: read query_log: %v", sentinel.Name, i, err)
				}
				if len(rows) != 1 {
					t.Fatalf("%s repeat %d: expected 1 query_log row for query_id %q, got %d",
						sentinel.Name, i, queryID, len(rows))
				}
				if rows[0].MemoryUsage > maxBytes {
					maxBytes = rows[0].MemoryUsage
				}

				// The plan prong, on every repeat like the status check: a
				// mechanism that fires intermittently is as broken as one
				// that never fires.
				if sentinel.WantPipelineStep != "" {
					pipeline, err := pipelineSteps(ctx, pipelineConns[sentinel.Deployment], queryID)
					if err != nil {
						t.Fatalf("%s repeat %d: %v", sentinel.Name, i, err)
					}
					if !strings.Contains(pipeline, sentinel.WantPipelineStep) {
						t.Fatalf("%s repeat %d: EXPLAIN PIPELINE of the dispatched statement has no %s — %s may have regressed:\n%s",
							sentinel.Name, i, sentinel.WantPipelineStep, sentinel.Family, pipeline)
					}
				}
			}
			// Every repeat above matched sentinel.ExpectedStatus exactly, or
			// this point is never reached (a mismatch is fatal).
			result.StatusOK = true

			capCeiling := nightlyCapCeilingBytes
			t.Logf("%s (%s): max-of-%d peak memory_usage = %d bytes (%.1f%% of %d-byte cap; absolute ceiling %d), expected status %d",
				sentinel.Name, sentinel.Family, nightlySentinelRepeats, maxBytes,
				100*float64(maxBytes)/float64(perfNightlyMemoryCapBytes), perfNightlyMemoryCapBytes, capCeiling, sentinel.ExpectedStatus)

			result.MaxOfNBytes = maxBytes
			result.CapCeilingBytes = capCeiling
			result.CapFractionPct = 100 * float64(maxBytes) / float64(perfNightlyMemoryCapBytes)
			result.CapOK = !exceedsCapCeiling(maxBytes)

			if update {
				// committedCeilingBytes clamps to capCeiling — see its own
				// comment for why an unclamped PRONG (b) ceiling gates nothing.
				ceiling := committedCeilingBytes(maxBytes, sentinel.BaselineHeadroom)
				updated[sentinel.Name] = nightlyBound{
					Name: sentinel.Name, ExpectedStatus: sentinel.ExpectedStatus,
					MaxOfNBytes: maxBytes, CeilingBytes: ceiling,
				}
				return
			}

			// PRONG (a): absolute, cap-relative ceiling.
			if exceedsCapCeiling(maxBytes) {
				t.Errorf("%s: peak memory %d bytes exceeds the absolute cap-relative ceiling %d bytes "+
					"(%.0f%% of the %d-byte cap) — %s may have regressed",
					sentinel.Name, maxBytes, capCeiling, 100*nightlyMemoryCapFraction, perfNightlyMemoryCapBytes, sentinel.Family)
			}

			// PRONG (b): committed per-sentinel ceiling.
			bound, ok := baselineFor(versionBaseline, sentinel.Name)
			if !ok {
				t.Fatalf("%s: no committed bound for ClickHouse %s in nightly-baseline.json — run `just update-nightly-perf-baseline`",
					sentinel.Name, version)
			}
			result.HasBaseline = true
			if bound.ExpectedStatus != sentinel.ExpectedStatus {
				t.Fatalf("%s: committed baseline expects status %d but the sentinel now expects %d — "+
					"run `just update-nightly-perf-baseline` if this change is genuinely intended",
					sentinel.Name, bound.ExpectedStatus, sentinel.ExpectedStatus)
			}
			result.BaselineCeilingBytes = bound.CeilingBytes
			result.BaselineOK = !exceedsCommittedCeiling(maxBytes, bound)
			if exceedsCommittedCeiling(maxBytes, bound) {
				// The committed ceiling's ACTUAL ratio to the calibration
				// measurement, not sentinel.BaselineHeadroom: a sentinel whose
				// ceiling committedCeilingBytes clamped to the absolute one
				// carries less than its nominal multiple.
				headroom := float64(bound.CeilingBytes) / float64(bound.MaxOfNBytes)
				t.Errorf("%s: peak memory %d bytes exceeds the committed ceiling %d bytes (measured max-of-N was "+
					"%d at calibration time, %.2fx headroom) — %s may have regressed; only run "+
					"`just update-nightly-perf-baseline` if the increase is genuinely intended",
					sentinel.Name, maxBytes, bound.CeilingBytes, bound.MaxOfNBytes, headroom, sentinel.Family)
			}

			// Every check above passed — this is the ONLY place Pass is set
			// true, so any earlier t.Fatalf/t.Errorf leaves it at its zero
			// value (false).
			result.Pass = result.StatusOK && result.CapOK && result.BaselineOK
		})
	}

	if !update {
		if err := writeResultsJSON(version, results); err != nil {
			t.Fatalf("write nightly results JSON: %v", err)
		}
	}

	if update {
		for _, bound := range versionBaseline.Sentinels {
			if _, measured := updated[bound.Name]; !measured && !calibrate(bound.Name) {
				updated[bound.Name] = bound
			}
		}
		writeNightlyBaseline(t, baseline, version, updated)
	}
}

// runSentinelOnce issues one GET for sentinel over the sample's fixed
// window (sampleWindowStart / sampleWindowEnd, sentinels.go) with ctx
// pre-stamped to queryID so the ClickHouse dispatch that results carries
// EXACTLY this query_id in system.query_log. Returns the HTTP status and
// response body — the body matters here (unlike test/perf/smoke's
// same-named helper) because ExpectedStatus != 200 sentinels need it to
// confirm the RIGHT guard fired.
func runSentinelOnce(t *testing.T, mux *http.ServeMux, sentinel Sentinel, queryID string) (int, string) {
	t.Helper()
	start, end, step := sampleWindowStart, sampleWindowEnd, sentinelStep
	if !sentinel.WindowStart.IsZero() {
		start, end, step = sentinel.WindowStart, sentinel.WindowEnd, sentinel.Step
	}
	params := sentinel.Params(start, end)
	if start.Equal(end) {
		params.Set("time", formatPromTime(end))
	} else {
		params.Set("start", formatPromTime(start))
		params.Set("end", formatPromTime(end))
		params.Set("step", step.String())
	}

	req := httptest.NewRequest(http.MethodGet, sentinel.Path+"?"+params.Encode(), nil)
	reqCtx := chclient.WithQueryID(req.Context(), queryID)
	for name, value := range sentinel.QuerySettings {
		reqCtx = chclient.WithQuerySetting(reqCtx, name, value)
	}
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func formatPromTime(t time.Time) string {
	return fmt.Sprintf("%.3f", float64(t.UnixNano())/1e9)
}

// sampleParquetPath resolves a trimmed sample's path relative to this test
// file's own package directory, independent of the working directory `go
// test` is invoked from.
func sampleParquetPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "samples", name)
}

// nightlyClassicHistogramLowerer reads the SAME chopt.FeatureTSGridHistogram
// decision cmd/cerberus's own boot wiring (nativeRangeLowerers, main.go)
// makes off the already-resolved set, returning just the one
// promql.ClassicHistogramWindowLowerer classic_histogram_quantile_by_route
// needs. The probe-and-resolve itself is chopttest.ResolveEnabledSet's, run
// once by the caller and shared with the SettingsRules axis, so this lane has
// exactly one version probe and one resolved set — the same posture a real
// boot has.
func nightlyClassicHistogramLowerer(t *testing.T, set chopt.EnabledSet) promql.ClassicHistogramWindowLowerer {
	t.Helper()
	enabled := set.Has(chopt.FeatureTSGridHistogram)
	t.Logf("ts_grid_histogram enabled=%v", enabled)

	// Matches cmd/cerberus's own nativeRangeLowerers exactly.
	if enabled {
		return promql.NativeClassicHistogramWindowLowerer{
			Fallback: promql.FanoutClassicHistogramWindowLowerer{},
		}
	}
	return promql.FanoutClassicHistogramWindowLowerer{}
}

func startPerfNightlyCH(ctx context.Context, t *testing.T) (*tcclickhouse.ClickHouseContainer, *chclient.Client) {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		perfNightlyImage(t),
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(perfNightlyDB),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	return container, newPerfNightlyClient(ctx, t, container, perfNightlyDB)
}

// perfNightlyImage is the image perfNightlyCHImageEnv names. Unset is an
// error rather than a default: a default would pick one server silently, and
// the bound set the run asserts follows the server.
func perfNightlyImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv(perfNightlyCHImageEnv)
	if image == "" {
		t.Fatalf("%s is unset — run the lane through `just perf-nightly-integration <image>`", perfNightlyCHImageEnv)
	}
	return image
}

// serverReleaseLine is the "major.minor" release line of the server conn
// reaches, the key its bound set is filed under.
func serverReleaseLine(ctx context.Context, t *testing.T, conn driver.Conn) string {
	t.Helper()
	var raw string
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&raw); err != nil {
		t.Fatalf("read the server version: %v", err)
	}
	v, ok := chopt.ParseVersion(raw)
	if !ok {
		t.Fatalf("unparseable server version %q", raw)
	}
	return releaseLine(v)
}

// knownSettings is the set of setting names the server conn reaches has.
func knownSettings(ctx context.Context, t *testing.T, conn driver.Conn) map[string]bool {
	t.Helper()
	rows, err := conn.Query(ctx, "SELECT name FROM system.settings")
	if err != nil {
		t.Fatalf("read system.settings: %v", err)
	}
	defer rows.Close()
	known := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read system.settings: %v", err)
		}
		known[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read system.settings: %v", err)
	}
	return known
}

// settingsServerKnows is the subset of settings the server has. Every
// Sentinel.QuerySettings entry switches a server mechanism off so it cannot
// carry state between repeats (settingUseQueryConditionCache); a server
// without the setting has no such mechanism — the query condition cache
// arrived in 25.3 — and rejects the unknown name outright, so the entry is
// dropped there rather than sent.
func settingsServerKnows(settings map[string]any, known map[string]bool) map[string]any {
	out := make(map[string]any, len(settings))
	for name, value := range settings {
		if known[name] {
			out[name] = value
		}
	}
	return out
}

// newPerfNightlyClient connects to database on container with the lane's
// memory cap, closing the client when t ends.
func newPerfNightlyClient(ctx context.Context, t *testing.T, container *tcclickhouse.ClickHouseContainer, database string) *chclient.Client {
	t.Helper()
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
		Database:            database,
		Username:            "cerberus",
		Password:            "cerberus",
		BreakerDisabled:     true,
		MaxQueryMemoryBytes: perfNightlyMemoryCapBytes,
	})
	if err != nil {
		t.Fatalf("connect %s: %v", database, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// createStockKeyDeployments creates DeploymentStockKey's tables from the
// exporter's DDL, loads the counter sample into them, and copies them to
// DeploymentStockKeyRenamed's tables. admin runs the DDL; stockKeyClient
// reads the stock-key database the sample loads into.
func createStockKeyDeployments(ctx context.Context, t *testing.T, container *tcclickhouse.ClickHouseContainer, admin, stockKeyClient *chclient.Client) {
	t.Helper()
	conn := admin.Conn()
	stmts, err := chsqltest.UpstreamDateTimeMetricsDDL(stockKeyDB)
	if err != nil {
		t.Fatalf("render the exporter's metrics DDL: %v", err)
	}
	for _, stmt := range append([]string{"CREATE DATABASE " + stockKeyDB}, stmts...) {
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("create the stock-key deployment: %v\n%s", err, stmt)
		}
	}
	rows, err := loadSumSample(ctx, container, stockKeyClient, sampleParquetPath(t, "svc_http_requests_total.parquet"))
	if err != nil {
		t.Fatalf("load the counter sample into %s: %v", stockKeyDB, err)
	}
	t.Logf("loaded counter sample into %s: %d rows", stockKeyDB, rows)

	if err := conn.Exec(ctx, "CREATE DATABASE "+stockKeyRenamedDB); err != nil {
		t.Fatalf("create %s: %v", stockKeyRenamedDB, err)
	}
	for from, to := range stockKeyRenamedTables {
		src, dst := stockKeyDB+"."+from, stockKeyRenamedDB+"."+to
		for _, stmt := range []string{"CREATE TABLE " + dst + " AS " + src, "INSERT INTO " + dst + " SELECT * FROM " + src} {
			if err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("copy %s to %s: %v", src, dst, err)
			}
		}
	}
}

// stockKeyRenamedMetrics is the metrics schema naming DeploymentStockKeyRenamed's
// tables.
func stockKeyRenamedMetrics() schema.Metrics {
	m := schema.DefaultOTelMetrics()
	m.GaugeTable = stockKeyRenamedTables[m.GaugeTable]
	m.SumTable = stockKeyRenamedTables[m.SumTable]
	m.HistogramTable = stockKeyRenamedTables[m.HistogramTable]
	m.ExpHistogramTable = stockKeyRenamedTables[m.ExpHistogramTable]
	return m
}

// nightlyMux mounts a prom handler on client's database the way cmd/cerberus
// wires one from the boot-resolved set: the classic-histogram lowering, the
// per-query settings rules, and the SchemaStrategies the requirements check
// resolves from the deployed tables, with the server facts set decides.
func nightlyMux(ctx context.Context, t *testing.T, client *chclient.Client, database string, metrics schema.Metrics, set chopt.EnabledSet) *http.ServeMux {
	t.Helper()
	handler := prom.New(client, metrics, nil)

	// prom.New's zero-value Lowerers table would leave
	// chplan.RangeBucketGridNative unreachable whatever the server's
	// version: promql.RangeLowerers.withDefaults normalises every nil field
	// to its fan-out impl at the lowering entry (issue #2487).
	handler.Lowerers.ClassicHistogram = nightlyClassicHistogramLowerer(t, set)

	// prom.New leaves Engine.Settings at its zero value, which applies
	// nothing, so no SettingsRules mechanism would be reachable here
	// (cerberus issue #2820).
	rules := choptwire.SettingsRules(set, metrics, schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	if rules.ResultCache {
		t.Fatalf("the resolved set enabled chopt.FeatureResultCache — a query RESULT cache would serve "+
			"repeats 1..%d of every sentinel from cache, so the max-of-N ceiling would stop measuring the "+
			"query and start measuring a cache hit", nightlySentinelRepeats-1)
	}
	handler.Engine.SetSettings(rules)

	res := preflight.Run(ctx, client, preflight.Requirements{
		Database: database,
		Metrics:  metrics,
		Logs:     schema.DefaultOTelLogs(),
		Traces:   schema.DefaultOTelTraces(),
		Signals:  preflight.Signals{Metrics: true},
	})
	if res.Fatal != nil {
		t.Fatalf("requirements check on %s: %v", database, res.Fatal)
	}
	cell := chsql.NewSchemaStrategiesCell(res.Strategies)
	choptwire.SetServerStrategies(cell, set)
	handler.Strategies = cell
	t.Logf("%s: optimize_aggregation_in_order=%v sorting-key prefixes %v prewhere-by-server=%v", database,
		rules.OptimizeAggregationInOrder, cell.Load().Metrics.SortingKeyPrefixes, cell.Load().Metrics.PrewhereByServer)

	mux := http.NewServeMux()
	handler.Mount(mux)
	return mux
}

// dispatchedStatement reads back the statement cerberus dispatched under
// queryID from system.query_log: its text and the settings it ran with.
func dispatchedStatement(ctx context.Context, conn driver.Conn, queryID string) (string, clickhouse.Settings, error) {
	var (
		settings map[string]string
		query    string
	)
	if err := conn.QueryRow(ctx, `SELECT Settings, query FROM system.query_log `+
		`WHERE type = 'QueryFinish' AND query_id = ? ORDER BY event_time_microseconds DESC LIMIT 1`, queryID).
		Scan(&settings, &query); err != nil {
		return "", nil, fmt.Errorf("read query_log for %q: %w", queryID, err)
	}
	stamped := make(clickhouse.Settings, len(settings))
	for name, value := range settings {
		stamped[name] = value
	}
	return query, stamped, nil
}

// pipelineSteps returns EXPLAIN PIPELINE of the statement cerberus
// dispatched under queryID, rebuilt from system.query_log: its text and the
// settings it ran with.
func pipelineSteps(ctx context.Context, conn driver.Conn, queryID string) (string, error) {
	query, stamped, err := dispatchedStatement(ctx, conn, queryID)
	if err != nil {
		return "", err
	}
	rows, err := conn.Query(clickhouse.Context(ctx, clickhouse.WithSettings(stamped)), "EXPLAIN PIPELINE "+query)
	if err != nil {
		return "", fmt.Errorf("EXPLAIN PIPELINE of %q: %w", queryID, err)
	}
	defer rows.Close()
	var pipeline strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		pipeline.WriteString(line)
		pipeline.WriteByte('\n')
	}
	return pipeline.String(), rows.Err()
}

// planningTime is the wall time ClickHouse takes to plan the statement
// cerberus dispatched under queryID — EXPLAIN PLAN of its text under the
// settings it ran with, which analyses and plans the statement without
// executing it. A positive limit abandons the sample once it runs that long
// and reports the limit itself with over set: a plan that blew through its
// bound costs the run the bound, not the minutes the regressed plan takes.
func planningTime(ctx context.Context, conn driver.Conn, queryID string, limit time.Duration) (took time.Duration, over bool, err error) {
	query, stamped, err := dispatchedStatement(ctx, conn, queryID)
	if err != nil {
		return 0, false, err
	}
	sampleCtx := ctx
	if limit > 0 {
		var cancel context.CancelFunc
		sampleCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	began := time.Now()
	explain := func() error {
		rows, err := conn.Query(clickhouse.Context(sampleCtx, clickhouse.WithSettings(stamped)), "EXPLAIN PLAN "+query)
		if err != nil {
			return err
		}
		defer rows.Close()
		var step string
		for rows.Next() {
			if err := rows.Scan(&step); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := explain(); err != nil {
		// The driver reports the abandoned sample as an I/O timeout on its
		// read deadline, not always as the context's own error, so the
		// deadline having passed is what identifies it.
		if deadline, ok := sampleCtx.Deadline(); ok && limit > 0 && ctx.Err() == nil && !time.Now().Before(deadline) {
			return limit, true, nil
		}
		return 0, false, fmt.Errorf("EXPLAIN PLAN of %q: %w", queryID, err)
	}
	return time.Since(began), false, nil
}

// planningRatio is the sentinel's planning prong: the ratio of the minimum
// planning time of the statement dispatched under queryID to the minimum
// planning time of the one dispatched under peerID, over
// nightlyPlanningSamples interleaved samples of each. The minimum is each
// statement's cost with the least interference from whatever else the host
// is running, and interleaving exposes both statements to the same host. A
// sample of the sentinel's statement is abandoned at maxPlanningPeerRatio
// times the peer sample before it, and counts as exactly that ratio.
func planningRatio(ctx context.Context, conn driver.Conn, queryID, peerID string) (ratio float64, query, peer time.Duration, abandoned int, err error) {
	for range nightlyPlanningSamples {
		peerSample, _, err := planningTime(ctx, conn, peerID, 0)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		limit := time.Duration(float64(peerSample) * maxPlanningPeerRatio)
		querySample, over, err := planningTime(ctx, conn, queryID, limit)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		if over {
			abandoned++
			// Abandoned at the bound: one nanosecond past it, so a run whose
			// every sample was abandoned fails the prong.
			querySample = limit + time.Nanosecond
		}
		if peer == 0 || peerSample < peer {
			peer = peerSample
		}
		if query == 0 || querySample < query {
			query = querySample
		}
	}
	return float64(query) / float64(peer), query, peer, abandoned, nil
}

// calibrationSelection reports which sentinels a calibration run measures:
// every one, or — when UPDATE_NIGHTLY_PERF_BASELINE_SENTINELS lists names —
// only those, keeping every other sentinel's committed bound as it is. It
// fails on a listed name no sentinel carries.
func calibrationSelection(t *testing.T, update bool) func(name string) bool {
	t.Helper()
	listed := os.Getenv("UPDATE_NIGHTLY_PERF_BASELINE_SENTINELS")
	if !update || listed == "" {
		return func(string) bool { return true }
	}
	selected := make(map[string]bool)
	for name := range strings.SplitSeq(listed, ",") {
		selected[strings.TrimSpace(name)] = true
	}
	for name := range selected {
		if !slices.ContainsFunc(Sentinels, func(s Sentinel) bool { return s.Name == name }) {
			t.Fatalf("UPDATE_NIGHTLY_PERF_BASELINE_SENTINELS names %q, which is no sentinel", name)
		}
	}
	return func(name string) bool { return selected[name] }
}

// --- baseline load/write (invariant 9: never hand-edited) -----------------
//
// The file format, the loader and the writer live in the untagged baseline.go;
// these two wrappers add only this lane's test plumbing (t.Fatalf, and the
// calibration path's tolerance for a not-yet-created file).

func loadNightlyBaseline(t *testing.T) nightlyBaseline {
	t.Helper()
	b, err := readNightlyBaseline()
	if err != nil {
		if os.Getenv("UPDATE_NIGHTLY_PERF_BASELINE") == "1" && errors.Is(err, fs.ErrNotExist) {
			return nightlyBaseline{}
		}
		t.Fatalf("%v — run `just update-nightly-perf-baseline` to create it", err)
	}
	return b
}

// writeNightlyBaseline orders updated (keyed by sentinel name) by version's
// sentinel set (SentinelsFor),
// files it as version's bound set in baseline, and hands the result to
// writeNightlyBaselineFile — this is the ONLY code path that writes
// nightly-baseline.json, gated on UPDATE_NIGHTLY_PERF_BASELINE=1. Every other
// version's bound set is written back unchanged.
func writeNightlyBaseline(t *testing.T, baseline nightlyBaseline, version string, updated map[string]nightlyBound) {
	t.Helper()
	set := SentinelsFor(version)
	bounds := make([]nightlyBound, 0, len(set))
	for _, s := range set {
		bound, ok := updated[s.Name]
		if !ok {
			t.Fatalf("writeNightlyBaseline: no measurement for sentinel %q", s.Name)
		}
		bounds = append(bounds, bound)
	}
	next, err := baseline.withVersion(version, bounds)
	if err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := writeNightlyBaselineFile(next); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("wrote ClickHouse %s's bounds to %s", version, nightlyBaselinePath)
}
