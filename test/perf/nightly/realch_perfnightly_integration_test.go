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
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// perfNightlyCHImage matches every other strict-scan/perf real-CH lane's
// pinned tag (test/perf/smoke's own perfSmokeCHImage).
const perfNightlyCHImage = "clickhouse/clickhouse-server:25.9-alpine"

const perfNightlyDB = "default"

// perfNightlyMemoryCapBytes matches CERBERUS_CH_QUERY_MAX_MEMORY's default —
// see test/perf/smoke's sentinelMemoryCapBytes for why this is the value a
// real deployment runs under, not a value tuned for this test.
const perfNightlyMemoryCapBytes int64 = 1 << 30 // 1 GiB

// nightlyMemoryCapFraction is PRONG (a)'s absolute ceiling. Same rationale
// as test/perf/smoke's sentinelMemoryCapFraction (above
// spillThreshold(cap)'s implied 0.5, below 1.0's OOM boundary), but NOT the
// same value — 0.75 (smoke's own midpoint pick) does not fit this lane's
// real measured data. Calibrated against this lane's own real
// post-#2429-fix, max-of-5 numbers: the two ExpectedStatus=200 sentinels
// measured 0.9% and 76.2% of cap; the two ExpectedStatus=422 sentinels
// (rejected BEFORE the expensive stage runs, by design) measured 25.9% and
// 27.6% at rejection. pod_status_reason_gauge's 76.2% already exceeds
// 0.75x — a plain `sum by (reason) (...)` gauge aggregation over this
// sample's real 7,024-series cardinality (855 distinct pods, only 8
// distinct `reason` values it collapses to) genuinely costs this much;
// filed as issue #2435 for its own investigation rather than silently
// absorbed into a looser fraction picked to avoid looking at it. 0.85
// keeps real, verified margin (~8 points) above that measured cost while
// staying meaningfully below ClickHouse's own 1.0 OOM boundary.
const nightlyMemoryCapFraction = 0.85

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

func TestPerfNightlyRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	container, client := startPerfNightlyCH(ctx, t)
	conn := client.Conn()

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

	// OPTIMIZE ... FINAL before measuring: controls background-merge-state
	// noise so a sentinel's memory reading reflects the query itself, not an
	// in-flight merge competing for the same cap (same rationale as
	// test/perf/smoke's optimizeTableFinal).
	for _, table := range []string{"otel_metrics_histogram", "otel_metrics_sum", "otel_metrics_gauge", "otel_metrics_exponential_histogram"} {
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+table+" FINAL"); err != nil {
			t.Fatalf("optimize table %s: %v", table, err)
		}
	}

	metricsHandler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	// Wire the SAME classic-histogram native/fan-out decision cmd/cerberus's
	// own boot path makes (nativeRangeLowerers, main.go), scoped to just this
	// one field — see nightlyClassicHistogramLowerer's own doc comment for
	// why prom.New's zero-value Lowerers table alone would leave
	// chplan.RangeBucketGridNative permanently unreachable here regardless of
	// perfNightlyCHImage's pinned version.
	metricsHandler.Lowerers.ClassicHistogram = nightlyClassicHistogramLowerer(ctx, t, client)
	mux := http.NewServeMux()
	metricsHandler.Mount(mux)

	logSource := optcorpus.NewCHQueryLogSource(conn, 30*time.Second, time.Hour)

	baseline := loadNightlyBaseline(t)
	update := os.Getenv("UPDATE_NIGHTLY_PERF_BASELINE") == "1"
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

			var maxBytes uint64
			for i := 0; i < nightlySentinelRepeats; i++ {
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
			}
			// Every repeat above matched sentinel.ExpectedStatus exactly, or
			// this point is never reached (a mismatch is fatal).
			result.StatusOK = true

			// math.Round forces this through runtime float64 arithmetic rather
			// than Go's exact-rational constant folding — (1<<30)*0.85 is not
			// itself an exact integer, unlike smoke's own 0.75 (which happens
			// to divide 2^30 evenly), so a bare uint64(...) conversion of the
			// constant expression fails to compile.
			capCeiling := uint64(math.Round(float64(perfNightlyMemoryCapBytes) * nightlyMemoryCapFraction))
			t.Logf("%s (%s): max-of-%d peak memory_usage = %d bytes (%.1f%% of %d-byte cap; absolute ceiling %d), expected status %d",
				sentinel.Name, sentinel.Family, nightlySentinelRepeats, maxBytes,
				100*float64(maxBytes)/float64(perfNightlyMemoryCapBytes), perfNightlyMemoryCapBytes, capCeiling, sentinel.ExpectedStatus)

			result.MaxOfNBytes = maxBytes
			result.CapCeilingBytes = capCeiling
			result.CapFractionPct = 100 * float64(maxBytes) / float64(perfNightlyMemoryCapBytes)
			result.CapOK = maxBytes <= capCeiling

			if update {
				// Clamp to capCeiling: for a sentinel already close to the
				// absolute ceiling (pod_status_reason_gauge measured 76.6%
				// against a 1.5x headroom — 1.5x of THAT already exceeds the
				// 1 GiB cap outright), an unclamped committed ceiling would
				// sit above PRONG (a)'s own bound, making PRONG (b)
				// permanently unable to fire — a looser "tighter" check is a
				// gate that silently never gates. PRONG (b) must never be
				// looser than PRONG (a).
				ceiling := min(uint64(float64(maxBytes)*sentinel.BaselineHeadroom), capCeiling)
				updated[sentinel.Name] = nightlyBound{
					Name: sentinel.Name, ExpectedStatus: sentinel.ExpectedStatus,
					MaxOfNBytes: maxBytes, CeilingBytes: ceiling,
				}
				return
			}

			// PRONG (a): absolute, cap-relative ceiling.
			if maxBytes > capCeiling {
				t.Errorf("%s: peak memory %d bytes exceeds the absolute cap-relative ceiling %d bytes "+
					"(%.0f%% of the %d-byte cap) — %s may have regressed",
					sentinel.Name, maxBytes, capCeiling, 100*nightlyMemoryCapFraction, perfNightlyMemoryCapBytes, sentinel.Family)
			}

			// PRONG (b): committed per-sentinel ceiling.
			bound, ok := baselineFor(baseline, sentinel.Name)
			if !ok {
				t.Fatalf("%s: no committed bound in nightly-baseline.json — run `just update-nightly-perf-baseline`", sentinel.Name)
			}
			result.HasBaseline = true
			if bound.ExpectedStatus != sentinel.ExpectedStatus {
				t.Fatalf("%s: committed baseline expects status %d but the sentinel now expects %d — "+
					"run `just update-nightly-perf-baseline` if this change is genuinely intended",
					sentinel.Name, bound.ExpectedStatus, sentinel.ExpectedStatus)
			}
			result.BaselineCeilingBytes = bound.CeilingBytes
			result.BaselineOK = maxBytes <= bound.CeilingBytes
			if maxBytes > bound.CeilingBytes {
				t.Errorf("%s: peak memory %d bytes exceeds the committed ceiling %d bytes (measured max-of-N was "+
					"%d at calibration time, %.2fx headroom) — %s may have regressed; only run "+
					"`just update-nightly-perf-baseline` if the increase is genuinely intended",
					sentinel.Name, maxBytes, bound.CeilingBytes, bound.MaxOfNBytes, sentinel.BaselineHeadroom, sentinel.Family)
			}

			// Every check above passed — this is the ONLY place Pass is set
			// true, so any earlier t.Fatalf/t.Errorf leaves it at its zero
			// value (false).
			result.Pass = result.StatusOK && result.CapOK && result.BaselineOK
		})
	}

	if !update {
		if err := writeResultsJSON(results); err != nil {
			t.Fatalf("write nightly results JSON: %v", err)
		}
	}

	if update {
		writeNightlyBaseline(t, updated)
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
	params.Set("start", formatPromTime(start))
	params.Set("end", formatPromTime(end))
	params.Set("step", step.String())

	req := httptest.NewRequest(http.MethodGet, sentinel.Path+"?"+params.Encode(), nil)
	req = req.WithContext(chclient.WithQueryID(req.Context(), queryID))
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

// nightlyClassicHistogramLowerer probes the live container's ClickHouse
// version and experimental-setting capability and resolves the SAME
// chopt.FeatureTSGridHistogram decision cmd/cerberus's own boot wiring
// (nativeRangeLowerers, main.go) makes, returning just the one
// promql.ClassicHistogramWindowLowerer classic_histogram_quantile_by_route
// needs.
//
// Without this, metricsHandler.Lowerers stays at prom.New's zero value:
// promql.RangeLowerers.withDefaults normalizes every nil field to its
// concrete FAN-OUT impl at the lowering entry, so a bare prom.New handler is
// permanently fan-out-only regardless of the connected server's version —
// bumping perfNightlyCHImage past the 25.9 floor alone would never make
// chplan.RangeBucketGridNative fire in this harness. That gap is real and
// repo-wide (test/perf/smoke and internal/api/prom's own real-CH integration
// tests build their handlers the identical zero-Lowerers way), so this fix
// is deliberately scoped to just the ONE field this lane's histogram
// sentinel needs, not a general native-lowerers-in-tests refactor — see
// issue #2487, which tracks wiring the other six native ts_grid families
// (rate/staleness/changes/resets/deriv/predict_linear) across the other
// lanes.
func nightlyClassicHistogramLowerer(ctx context.Context, t *testing.T, client *chclient.Client) promql.ClassicHistogramWindowLowerer {
	t.Helper()
	version, err := client.ProbeVersion(ctx)
	if err != nil {
		t.Fatalf("probe clickhouse version: %v", err)
	}
	capability := client.ProbeTSGridCapability(ctx)
	set, warnings, err := chopt.Resolve(chopt.Config{
		Optimizations: "auto",
		Mode:          chopt.Enforcing,
		Capability:    capability,
	}, version)
	if err != nil {
		t.Fatalf("resolve clickhouse optimizations: %v", err)
	}
	for _, w := range warnings {
		t.Logf("ch_opt: %s", w)
	}
	enabled := set.Has(chopt.FeatureTSGridHistogram)
	t.Logf("probed clickhouse %s, ts_grid capability %s, ts_grid_histogram enabled=%v",
		version.String(), capability.String(), enabled)

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
		perfNightlyCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(perfNightlyDB),
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
		Database:            perfNightlyDB,
		Username:            "cerberus",
		Password:            "cerberus",
		BreakerDisabled:     true,
		MaxQueryMemoryBytes: perfNightlyMemoryCapBytes,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return container, client
}

// --- baseline load/write (invariant 9: never hand-edited) -----------------

// nightlyBaselinePath is the committed bound file, a sibling of
// perf-smoke-baseline.json one level up from this package.
const nightlyBaselinePath = "../nightly-baseline.json"

// nightlyBound is one sentinel's committed bound: the calibration-time
// max-of-N measurement (kept for the diff/failure message), the
// headroom-multiplied ceiling the gate actually asserts against, and the
// HTTP status the sentinel was calibrated to expect (PRONG (b) itself
// re-checks this against the sentinel's CURRENT ExpectedStatus, catching a
// baseline gone stale relative to sentinels.go rather than silently
// comparing memory across two different outcome classes).
type nightlyBound struct {
	Name           string `json:"name"`
	ExpectedStatus int    `json:"expected_status"`
	MaxOfNBytes    uint64 `json:"max_of_n_bytes"`
	CeilingBytes   uint64 `json:"ceiling_bytes"`
}

type nightlyBaseline struct {
	Sentinels []nightlyBound `json:"sentinels"`
}

func baselineFor(b nightlyBaseline, name string) (nightlyBound, bool) {
	for _, s := range b.Sentinels {
		if s.Name == name {
			return s, true
		}
	}
	return nightlyBound{}, false
}

func loadNightlyBaseline(t *testing.T) nightlyBaseline {
	t.Helper()
	buf, err := os.ReadFile(nightlyBaselinePath)
	if err != nil {
		if os.Getenv("UPDATE_NIGHTLY_PERF_BASELINE") == "1" {
			return nightlyBaseline{}
		}
		t.Fatalf("read baseline %s: %v — run `just update-nightly-perf-baseline` to create it", nightlyBaselinePath, err)
	}
	var b nightlyBaseline
	if err := json.Unmarshal(buf, &b); err != nil {
		t.Fatalf("parse baseline %s: %v", nightlyBaselinePath, err)
	}
	return b
}

// writeNightlyBaseline serialises updated (keyed by sentinel name, in
// Sentinels order) as pretty JSON with a trailing newline, so the committed
// file diffs cleanly and is never hand-edited (invariant 9) — this is the
// ONLY code path that writes nightly-baseline.json, gated on
// UPDATE_NIGHTLY_PERF_BASELINE=1.
func writeNightlyBaseline(t *testing.T, updated map[string]nightlyBound) {
	t.Helper()
	out := nightlyBaseline{Sentinels: make([]nightlyBound, 0, len(Sentinels))}
	for _, s := range Sentinels {
		bound, ok := updated[s.Name]
		if !ok {
			t.Fatalf("writeNightlyBaseline: no measurement for sentinel %q", s.Name)
		}
		out.Sentinels = append(out.Sentinels, bound)
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(nightlyBaselinePath, buf, 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("wrote %s", nightlyBaselinePath)
}
