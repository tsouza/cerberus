//go:build integration

// realch_native_lowerers_integration_test.go proves the seven ts_grid_*
// native range-window lowerers cerberus's own boot wiring
// (cmd/cerberus/main.go's nativeRangeLowerers) can activate — rate,
// increase, staleness (resample), changes, resets, deriv, predict_linear —
// each fires against a REAL ClickHouse server, not just that the request
// they drive still returns HTTP 200.
//
// # Why this test exists
//
// Every real-CH integration test that builds a prom.Handler constructs it
// as prom.New(client, schema, nil) and leaves .Lowerers at its zero value.
// promql.RangeLowerers.withDefaults normalizes every nil field to its
// concrete fan-out impl at the lowering entry, so those handlers are
// permanently fan-out-only regardless of the connected server's version —
// confirmed for this package's own TestPerfSmokeRealCH and
// internal/api/prom/handler_histogram_integration_test.go. Nothing in CI
// outside a full binary boot (docker-compose / e2e / compose-smoke, none of
// which assert a specific native family activated) ever exercised
// NativeRateLowerer, NativeIncreaseLowerer, NativeStalenessLowerer, NativeChangesLowerer,
// NativeResetsLowerer, NativeDerivLowerer, or NativePredictLinearLowerer —
// issue #2487.
//
// This test wires a *prom.Handler the way a real deployment's boot path
// does (via internal/chopttest, extracted from cmd/cerberus's own
// nativeRangeLowerers so the two can't silently drift) and, for each
// family, issues the query_range shape test/spec/promql's own
// native_*_range_step.txtar fixtures pin as that family's native-eligible
// idiom, then reads the emitted SQL back out of system.query_log and
// asserts the expected native ts_grid_* function name is actually in it —
// not just that the request succeeded. A passing HTTP 200 alone cannot
// distinguish "the native lowerer fired" from "it silently fell back to
// fan-out", which is exactly the "hollow green" failure mode this repo's
// own project memory warns about.
//
// Gated behind the `integration` build tag (Docker required); wired into
// strict-scan.yml alongside this package's own TestPerfSmokeRealCH. Run
// locally with:
//
//	go test -tags=integration -count=1 -run TestNativeRangeLowerers_RealCH ./test/perf/smoke/...
package smoke

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// nativeLowererCounterMetric / nativeLowererGaugeMetric are the two series
// this test seeds: a monotonic counter (rate, resets) and a gauge (changes,
// deriv, predict_linear, and the bare-selector staleness/resample shape).
// Named away from every other sentinel in this package's own metric names
// to make a query_log cross-match between the two tests impossible.
const (
	nativeLowererCounterMetric = "native_lowerer_counter_total"
	nativeLowererGaugeMetric   = "native_lowerer_gauge"
)

// nativeLowererScrapeInterval is the seeded sample cadence; nativeLowererRange
// (the PromQL range-vector selector every windowed family below uses) is
// wide enough at this cadence to guarantee >= 2 samples in every anchor's
// window, clearing deriv/predict_linear's native >= 2-samples-per-window
// floor (test/spec/promql/native_deriv_range_step.txtar's own doc comment).
const (
	nativeLowererScrapeInterval = 30 * time.Second
	nativeLowererRange          = 5 * time.Minute
	nativeLowererRangeSelector  = "5m"
	nativeLowererStep           = time.Minute
	nativeLowererWindow         = 15 * time.Minute
	// nativeLowererPredictHorizonSeconds is predict_linear's projection
	// offset. A whole-second literal — the ONLY native-eligible shape (a
	// computed or fractional horizon delegates to fan-out; see
	// native_predict_linear_range_step.txtar's own doc comment).
	nativeLowererPredictHorizonSeconds = 3600
	// nativeLowererGaugeModulus / nativeLowererGaugeSlope shape the seeded
	// gauge waveform (sawtooth-plus-drift) so changes() sees real value
	// transitions and deriv()/predict_linear() fit a non-degenerate
	// (non-zero-slope) line — see seedNativeLowererGauge.
	nativeLowererGaugeModulus = 7
	nativeLowererGaugeSlope   = 0.1
)

// nativeLowererHost is the single `host` label value seeded onto both
// series, so every family's `sum by(host)(...)` query — mirroring the
// idiom test/spec/promql's own native_*_range_step.txtar fixtures pin —
// groups down to exactly one series.
const nativeLowererHost = "native-lowerer-host"

// nativeLowererFamily describes one ts_grid_* native family this test
// activates: the PromQL query that reaches it (the exact idiom its own
// test/spec/promql/native_*_range_step.txtar fixture pins as
// native-eligible), the chopt feature id gating it, and the ClickHouse
// function name proving activation in the emitted SQL.
type nativeLowererFamily struct {
	Name    string
	Feature string
	Query   string
	WantFn  string
}

// nativeLowererFamilies is deliberately just the six families issue #2487
// names plus increase (issue #2744, a rate() sibling reusing the same
// timeSeriesRateToGrid aggregate) — NativeRateLowerer, NativeIncreaseLowerer,
// NativeStalenessLowerer, NativeChangesLowerer, NativeResetsLowerer,
// NativeDerivLowerer, NativePredictLinearLowerer. ClassicHistogram is out of
// scope here: test/perf/nightly's own nightlyClassicHistogramLowerer already
// gives it real per-family activation coverage.
var nativeLowererFamilies = []nativeLowererFamily{
	{
		Name:    "rate",
		Feature: chopt.FeatureTSGridRange,
		Query:   fmt.Sprintf("sum by(host) (rate(%s[%s]))", nativeLowererCounterMetric, nativeLowererRangeSelector),
		WantFn:  "timeSeriesRateToGrid",
	},
	{
		Name:    "increase",
		Feature: chopt.FeatureTSGridIncrease,
		Query:   fmt.Sprintf("sum by(host) (increase(%s[%s]))", nativeLowererCounterMetric, nativeLowererRangeSelector),
		WantFn:  "timeSeriesRateToGrid",
	},
	{
		Name:    "staleness",
		Feature: chopt.FeatureTSGridResample,
		Query:   nativeLowererGaugeMetric,
		WantFn:  "timeSeriesResampleToGridWithStaleness",
	},
	{
		Name:    "changes",
		Feature: chopt.FeatureTSGridChanges,
		Query:   fmt.Sprintf("sum by(host) (changes(%s[%s]))", nativeLowererGaugeMetric, nativeLowererRangeSelector),
		WantFn:  "timeSeriesChangesToGrid",
	},
	{
		Name:    "resets",
		Feature: chopt.FeatureTSGridResets,
		Query:   fmt.Sprintf("sum by(host) (resets(%s[%s]))", nativeLowererCounterMetric, nativeLowererRangeSelector),
		WantFn:  "timeSeriesResetsToGrid",
	},
	{
		Name:    "deriv",
		Feature: chopt.FeatureTSGridDeriv,
		Query:   fmt.Sprintf("sum by(host) (deriv(%s[%s]))", nativeLowererGaugeMetric, nativeLowererRangeSelector),
		WantFn:  "timeSeriesDerivToGrid",
	},
	{
		Name:    "predict_linear",
		Feature: chopt.FeatureTSGridPredictLinear,
		Query: fmt.Sprintf("sum by(host) (predict_linear(%s[%s], %d))",
			nativeLowererGaugeMetric, nativeLowererRangeSelector, nativeLowererPredictHorizonSeconds),
		WantFn: "timeSeriesPredictLinearToGrid",
	},
}

func TestNativeRangeLowerers_RealCH_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := startPerfSmokeCH(ctx, t)
	conn := client.Conn()

	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("apply metrics DDL: %v", err)
	}

	end := sentinelWindowEnd
	start := end.Add(-nativeLowererWindow)
	seedStart := start.Add(-nativeLowererRange)

	seedNativeLowererCounter(ctx, t, conn, seedStart, end)
	seedNativeLowererGauge(ctx, t, conn, seedStart, end)

	lowerers, enabled := chopttest.WireAllNativeLowerers(ctx, t, client)
	for _, family := range nativeLowererFamilies {
		if !enabled.Has(family.Feature) {
			t.Fatalf("chopt feature %q (family %s) did not resolve enabled against %s — "+
				"this lane's own activation assertions would be vacuous against a server too old "+
				"for the ts_grid_* floor", family.Feature, family.Name, perfSmokeCHImage)
		}
	}

	metricsHandler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	metricsHandler.Lowerers = lowerers
	mux := http.NewServeMux()
	metricsHandler.Mount(mux)

	for _, family := range nativeLowererFamilies {
		t.Run(family.Name, func(t *testing.T) {
			queryID := "native-lowerer-" + family.Name
			code := runNativeLowererQuery(t, mux, family.Query, start, end, queryID)
			if code != http.StatusOK {
				t.Fatalf("%s: HTTP %d (want 200) for query %q", family.Name, code, family.Query)
			}
			chopttest.AssertNativeFunctionFired(ctx, t, conn, queryID, family.WantFn)
		})
	}
}

// runNativeLowererQuery issues one query_range GET over [start, end) at
// nativeLowererStep, with ctx pre-stamped to queryID (chclient.WithQueryID)
// so the ClickHouse dispatch it triggers carries EXACTLY this query_id in
// system.query_log. Uses httptest.NewRecorder + ServeMux.ServeHTTP directly
// (in-process, no real network round trip) rather than httptest.Server +
// http.Get: a real transport hop would build a fresh server-side request
// with a fresh context, losing the stamped query_id before it ever reaches
// the handler.
func runNativeLowererQuery(t *testing.T, mux *http.ServeMux, query string, start, end time.Time, queryID string) int {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", formatPromTime(start))
	params.Set("end", formatPromTime(end))
	params.Set("step", nativeLowererStep.String())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?"+params.Encode(), nil)
	req = req.WithContext(chclient.WithQueryID(req.Context(), queryID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Logf("native lowerer query %q: HTTP %d body: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Code
}

// seedNativeLowererCounter seeds one monotonically increasing counter
// series (nativeLowererCounterMetric) at nativeLowererScrapeInterval
// cadence across [start, end], for the rate/resets families.
func seedNativeLowererCounter(ctx context.Context, t *testing.T, conn driver.Conn, start, end time.Time) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`INSERT INTO otel_metrics_sum
    (MetricName, Attributes, ServiceName, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES `)
	i := 0
	for ts := start; !ts.After(end); ts = ts.Add(nativeLowererScrapeInterval) {
		if i > 0 {
			sb.WriteString(",\n")
		}
		fmt.Fprintf(&sb,
			"('%s', map('host', '%s'), 'cerberus-smoke', toDateTime64('%s', 9), %g, 2, true)",
			nativeLowererCounterMetric, nativeLowererHost, ts.Format("2006-01-02 15:04:05.000"), float64(i+1))
		i++
	}
	if err := conn.Exec(ctx, sb.String()); err != nil {
		t.Fatalf("seed %s: %v", nativeLowererCounterMetric, err)
	}
	t.Logf("seeded %s: %d samples", nativeLowererCounterMetric, i)
}

// seedNativeLowererGauge seeds one oscillating gauge series
// (nativeLowererGaugeMetric) at nativeLowererScrapeInterval cadence across
// [start, end], for the changes/deriv/predict_linear/staleness families.
// The value alternates rather than holding steady so changes() sees real
// value transitions and deriv()/predict_linear() fit a non-degenerate
// (non-zero-slope) line.
func seedNativeLowererGauge(ctx context.Context, t *testing.T, conn driver.Conn, start, end time.Time) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`INSERT INTO otel_metrics_gauge
    (MetricName, Attributes, ServiceName, TimeUnix, Value) VALUES `)
	i := 0
	for ts := start; !ts.After(end); ts = ts.Add(nativeLowererScrapeInterval) {
		if i > 0 {
			sb.WriteString(",\n")
		}
		value := float64(i%nativeLowererGaugeModulus) + float64(i)*nativeLowererGaugeSlope
		fmt.Fprintf(&sb,
			"('%s', map('host', '%s'), 'cerberus-smoke', toDateTime64('%s', 9), %g)",
			nativeLowererGaugeMetric, nativeLowererHost, ts.Format("2006-01-02 15:04:05.000"), value)
		i++
	}
	if err := conn.Exec(ctx, sb.String()); err != nil {
		t.Fatalf("seed %s: %v", nativeLowererGaugeMetric, err)
	}
	t.Logf("seeded %s: %d samples", nativeLowererGaugeMetric, i)
}
