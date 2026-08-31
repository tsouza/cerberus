//go:build integration

// realch_ts_grid_instant_memory_integration_test.go measures the REAL,
// production-shaped memory difference cerberus issue #2748 exists to close:
// an INSTANT (bare, non-query_range) `rate(<counter>[<wide-range>])` over
// real data, once through the pre-existing fan-out
// (emitWindowedArrayExtrapolated's per-series groupArray) and once through
// the new ts_grid_instant native lowering
// (chplan.RangeWindowGridNativeInstant, the degenerate one-point grid).
//
// # Why this package, not test/perf/smoke
//
// The issue's own claim is that a WIDE lookback (`rate(m[1d])`, `rate(m[30d])`
// — the shape alerting/recording rules routinely write) makes the fan-out's
// per-series groupArray scale with real retained-sample volume. Proving that
// needs the SAME real, scrubbed production sample this package already loads
// (loader.go's loadSumSample, svc_http_requests_total.parquet) — reusing it
// directly, in this package, is what lets this test read the real numbers off
// system.query_log without a second parquet fixture or a second copy of the
// Attributes-tuple-to-Map loading SQL (loader.go's own doc explains why THAT
// SQL is not itself shared with test/perf/smoke, which has no loader at all
// yet). Loading test/perf/smoke's own full 14-day sample instead of this
// package's already-checked-out one-day trim would multiply this repo's LFS
// bandwidth footprint for no proof-strength gain — the trim is still real,
// captured production data (testdata/samples/README.md), just narrower in
// calendar span.
//
// # Why its own ClickHouse container, not perfNightlyCHImage
//
// ts_grid_instant floors at 26.5 (chopt.FeatureTSGridInstant's own doc) —
// ABOVE perfNightlyCHImage's pinned 25.9, which predates the extreme-
// parameter / staleness overflow fixes (#103223/#105319) the floor exists
// for. This test boots its OWN dedicated container at 26.6-alpine instead,
// mirroring how the strict-scan lane's quantile_prom_histogram and
// ts_grid_last_over_time real-CH tests each pin their own floor-appropriate
// image rather than reusing their job's shared one.
//
// # What this test does NOT do
//
// It carries no committed baseline and no PRONG assertions — this package's
// OWN TestPerfNightlyRealCH already owns that regression-detection machinery
// for its five calibrated sentinels. This test's job is a real-data
// measurement reported once in the PR that introduced ts_grid_instant; it
// still runs under the `integration` build tag on the nightly schedule (not
// on every PR — merge_posture "never" in .github/ci-lanes.json, matching
// this package's own perf-nightly lane) so it stays honest as the codebase
// evolves, but the only assertion it makes is the qualitative one the issue
// itself claims: the native lowering must not use MORE memory than the
// fan-out for the SAME real query.
//
// Run locally with:
//
//	go test -tags=integration -count=1 -run TestTSGridInstantMemory_RealCH ./test/perf/nightly/...
package nightly

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// tsGridInstantCHImage is pinned above the ts_grid_instant 26.5 floor — see
// this file's own doc for why it cannot reuse perfNightlyCHImage (25.9).
const tsGridInstantCHImage = "clickhouse/clickhouse-server:26.6-alpine"

// tsGridInstantLookback is the wide-lookback shape cerberus issue #2748
// names explicitly as the alerting/recording-rule idiom this feature
// targets. Only one: this package's trimmed sample spans a single real
// captured window (2026-08-18 09:00:01-13:29:59 UTC, testdata/samples/
// README.md) narrower than even 1 day, so a wider lookback parameter
// (`[7d]`, `[30d]`) would scan the IDENTICAL real rows and prove nothing
// about scanning MORE data — it would only restate that the parameter
// itself does not change eligibility, already covered by
// internal/chsql/range_window_grid_native_instant_chdb_test.go's dual-emit
// parity test at several lookbacks.
const tsGridInstantLookback = "1d"

func TestTSGridInstantMemory_RealCH_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	container, client := startTSGridInstantCH(ctx, t)
	conn := client.Conn()

	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("apply metrics DDL: %v", err)
	}

	sumRows, err := loadSumSample(ctx, container, client, sampleParquetPath(t, "svc_http_requests_total.parquet"))
	if err != nil {
		t.Fatalf("load sum sample: %v", err)
	}
	t.Logf("loaded %d real rows from the trimmed svc_http_requests_total sample", sumRows)

	if err := conn.Exec(ctx, "OPTIMIZE TABLE otel_metrics_sum FINAL"); err != nil {
		t.Fatalf("optimize otel_metrics_sum: %v", err)
	}

	var maxTS time.Time
	if err := conn.QueryRow(ctx, "SELECT max(TimeUnix) FROM otel_metrics_sum WHERE MetricName = ?", sumMetric).Scan(&maxTS); err != nil {
		t.Fatalf("max(TimeUnix): %v", err)
	}
	t.Logf("real sample max TimeUnix = %s (the instant eval anchor below is pinned to)", maxTS)

	// Resolve the two lowerer tables directly against the real connected
	// server: fan-out-only (today's behaviour) and native-with-instant
	// (ts_grid_range AND ts_grid_instant explicitly listed — ts_grid_instant
	// is AutoSelect=false, see chopt.FeatureTSGridInstant's own doc, so
	// "auto" alone would not activate it).
	fanoutSet := chopttest.ResolveEnabledSet(ctx, t, client, "off")
	nativeSet := chopttest.ResolveEnabledSet(ctx, t, client, chopt.FeatureTSGridRange+","+chopt.FeatureTSGridInstant)
	if !nativeSet.Has(chopt.FeatureTSGridInstant) {
		t.Fatalf("chopt feature %q did not resolve enabled against %s — "+
			"this lane's own memory comparison would be vacuous against a server too old for the 26.5 floor",
			chopt.FeatureTSGridInstant, tsGridInstantCHImage)
	}

	// AggregationTemporalityColumn cleared: this comparison targets the axis
	// ts_grid_instant actually changes (the memory shape of the windowed-
	// array reducer), not issue #1628's separate DELTA-vs-CUMULATIVE runtime
	// branch — nativeTSGridInstantNode (correctly) declines any window
	// carrying a TemporalityColumn unconditionally (cerberus issue #2843
	// tracks extending it), so leaving the column wired would silently
	// compare native's instant arm against itself declining to fire, not
	// against the fan-out it replaces. Every existing native-lowerer
	// differential in this codebase clears the same column for the
	// identical reason (see e.g.
	// internal/chsql/range_window_grid_native_chdb_test.go's own comment).
	metricsSchema := schema.DefaultOTelMetrics()
	metricsSchema.AggregationTemporalityColumn = ""
	handler := prom.New(client, metricsSchema, nil)
	mux := http.NewServeMux()
	handler.Mount(mux)

	logSource := optcorpus.NewCHQueryLogSource(conn, 30*time.Second, time.Hour)
	query := fmt.Sprintf("rate(%s[%s])", sumMetric, tsGridInstantLookback)

	handler.Lowerers = chopttest.BuildRangeLowerers(fanoutSet)
	fanoutID := "ts-grid-instant-fanout"
	if code := runTSGridInstantQuery(t, mux, query, maxTS, fanoutID); code != http.StatusOK {
		t.Fatalf("fanout: HTTP %d (want 200)", code)
	}
	fanoutRow := tsGridInstantQueryLogRow(ctx, t, conn, logSource, fanoutID)

	handler.Lowerers = chopttest.BuildRangeLowerers(nativeSet)
	nativeID := "ts-grid-instant-native"
	if code := runTSGridInstantQuery(t, mux, query, maxTS, nativeID); code != http.StatusOK {
		t.Fatalf("native: HTTP %d (want 200)", code)
	}
	nativeRow := tsGridInstantQueryLogRow(ctx, t, conn, logSource, nativeID)
	// AssertNativeFunctionFired reads the query text back separately
	// (SourceRow carries the memory/exit-status numbers this file needs, not
	// the raw SQL) — proves the instant native lowerer genuinely fired
	// rather than silently falling back to fan-out (the "hollow green"
	// failure realch_native_lowerers_integration_test.go's own doc names).
	chopttest.AssertNativeFunctionFired(ctx, t, conn, nativeID, "timeSeriesRateToGrid")

	t.Logf("lookback=%s: fan-out peak memory_usage = %d bytes; native peak memory_usage = %d bytes "+
		"(native is %.1f%% of fan-out)",
		tsGridInstantLookback, fanoutRow.MemoryUsage, nativeRow.MemoryUsage,
		100*float64(nativeRow.MemoryUsage)/float64(fanoutRow.MemoryUsage))

	if nativeRow.MemoryUsage > fanoutRow.MemoryUsage {
		t.Errorf("native peak memory %d bytes EXCEEDS fan-out's %d bytes — the native lowering is "+
			"supposed to use flat, not more, memory than the groupArray fan-out it replaces",
			nativeRow.MemoryUsage, fanoutRow.MemoryUsage)
	}
}

// tsGridInstantQueryLogRow flushes system.query_log and returns the single
// finished row for queryID, failing loudly if it is missing or not exactly
// one row — mirrors internal/chopttest/activation.go's own queryTextForID
// contract but returns the full row so the memory number is available too.
func tsGridInstantQueryLogRow(ctx context.Context, t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, logSource *optcorpus.CHQueryLogSource, queryID string,
) optcorpus.SourceRow {
	t.Helper()
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("query_id %s: flush logs: %v", queryID, err)
	}
	rows, err := logSource.FinishedByQueryID(ctx, []string{queryID})
	if err != nil {
		t.Fatalf("query_id %s: read query_log: %v", queryID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("query_id %s: expected 1 query_log row, got %d", queryID, len(rows))
	}
	if rows[0].ExitStatus != optcorpus.ExitOK {
		t.Fatalf("query_id %s: query_log reports a non-OK exit (%v)", queryID, rows[0].ExitStatus)
	}
	return rows[0]
}

// runTSGridInstantQuery issues one INSTANT /api/v1/query GET at ts, with ctx
// pre-stamped to queryID so the ClickHouse dispatch it triggers carries
// EXACTLY this query_id in system.query_log — the instant-mode sibling of
// this package's own runNightlyQuery-style helpers.
func runTSGridInstantQuery(t *testing.T, mux *http.ServeMux, query string, ts time.Time, queryID string) int {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("time", formatPromTime(ts))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+params.Encode(), nil)
	req = req.WithContext(chclient.WithQueryID(req.Context(), queryID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Logf("instant query %q (id=%s): HTTP %d body: %s", query, queryID, rec.Code, rec.Body.String())
	}
	return rec.Code
}

// startTSGridInstantCH starts a real ClickHouse container at
// tsGridInstantCHImage (>= the 26.5 ts_grid_instant floor — see this file's
// own doc for why it cannot reuse startPerfNightlyCH's 25.9-alpine),
// mirroring that function's own shape.
func startTSGridInstantCH(ctx context.Context, t *testing.T) (*tcclickhouse.ClickHouseContainer, *chclient.Client) {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		tsGridInstantCHImage,
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
