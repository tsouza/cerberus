//go:build integration

// handler_native_lowerers_integration_test.go proves the seven ts_grid_*
// native range-window lowerers (rate, increase, staleness/resample, changes,
// resets, deriv, predict_linear) can activate through this package's OWN
// *prom.Handler against a REAL ClickHouse server — not just that the
// request driving each one still returns HTTP 200.
//
// # Why this test exists
//
// Every real-CH integration test in this package builds its handler as
// prom.New(client, schema, nil) and leaves .Lowerers at its zero value —
// confirmed for handler_histogram_integration_test.go and
// handler_route_b_realch_integration_test.go.
// promql.RangeLowerers.withDefaults normalizes every nil field to its
// concrete fan-out impl at the lowering entry, so those handlers are
// permanently fan-out-only regardless of the connected server's version.
// Nothing else in CI outside a full binary boot (docker-compose / e2e /
// compose-smoke, none of which assert a specific native family activated)
// exercises NativeRateLowerer, NativeIncreaseLowerer, NativeStalenessLowerer,
// NativeChangesLowerer, NativeResetsLowerer, NativeDerivLowerer, or
// NativePredictLinearLowerer — issue #2487 (increase added by #2744).
//
// This test wires a *prom.Handler the way a real deployment's boot path
// does, via internal/chopttest — extracted from cmd/cerberus's own
// nativeRangeLowerers so the two dispatch tables can't silently drift —
// then, for each family, issues the query_range shape
// test/spec/promql/native_*_range_step.txtar itself pins as that family's
// native-eligible idiom, and reads the emitted SQL back out of
// system.query_log to assert the expected native ts_grid_* function name is
// actually in it. A passing HTTP 200 alone cannot distinguish "the native
// lowerer fired" from "it silently fell back to fan-out" — the "hollow
// green" failure mode this repo's own project memory warns about.
//
// Requests are dispatched in-process (httptest.NewRecorder +
// ServeMux.ServeHTTP), not over a real network round trip through
// httptest.Server: a real transport hop builds a fresh server-side request
// with a fresh context, losing the chclient.WithQueryID stamp
// system.query_log correlation below depends on.
//
// Gated behind the `integration` build tag (Docker required); wired into
// strict-scan.yml alongside this package's other real-CH integration steps.
// Run locally with:
//
//	go test -tags=integration -count=1 -run TestNativeRangeLowerers_RealCH ./internal/api/prom/...
package prom_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// nativeLowererCounterMetric / nativeLowererGaugeMetric are the two series
// this test seeds: a monotonic counter (rate, resets) and a gauge (changes,
// deriv, predict_linear, and the bare-selector staleness/resample shape).
const (
	nativeLowererCounterMetric = "native_lowerer_counter_total"
	nativeLowererGaugeMetric   = "native_lowerer_gauge"
)

// nativeLowererScrapeInterval / nativeLowererRange together guarantee >= 2
// samples in every anchor's window, clearing deriv/predict_linear's native
// >= 2-samples-per-window floor (test/spec/promql/native_deriv_range_step.txtar's
// own doc comment).
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
// series, so every family's `sum by(host)(...)` query — mirroring the idiom
// test/spec/promql's own native_*_range_step.txtar fixtures pin — groups
// down to exactly one series.
const nativeLowererHost = "native-lowerer-host"

// nativeLowererFamily describes one ts_grid_* native family this test
// activates: the PromQL query that reaches it, the chopt feature id gating
// it, and the ClickHouse function name proving activation in the emitted
// SQL.
type nativeLowererFamily struct {
	Name    string
	Feature string
	Query   string
	WantFn  string
}

// nativeLowererFamilies is deliberately just the six families issue #2487
// names plus increase (issue #2744, a rate() sibling that reuses the same
// timeSeriesRateToGrid aggregate). ClassicHistogram is out of scope here:
// this package's own handler_histogram_integration_test.go and
// test/perf/nightly's nightlyClassicHistogramLowerer already give it real
// activation coverage.
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

// nativeLowererCHImage matches the other real-CH integration lanes in this
// package (routeBCHImage in handler_route_b_realch_integration_test.go).
const nativeLowererCHImage = "clickhouse/clickhouse-server:25.9-alpine"

// nativeLowererSumDDL / nativeLowererGaugeDDL are the otel_metrics_sum /
// otel_metrics_gauge table shapes the real OTel-CH exporter creates — the
// same shape handler_histogram_integration_test.go's realExporterHistogramDDL
// uses for otel_metrics_sum, extracted to just the two tables this test
// needs.
const nativeLowererSumDDL = `CREATE TABLE otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`

const nativeLowererGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`

func TestNativeRangeLowerers_RealCH_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		nativeLowererCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
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
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for _, stmt := range []string{nativeLowererSumDDL, nativeLowererGaugeDDL} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("create table: %v\nstmt: %s", err, stmt)
		}
	}

	end := time.Now().UTC().Truncate(time.Minute).Add(-nativeLowererScrapeInterval)
	start := end.Add(-nativeLowererWindow)
	seedStart := start.Add(-nativeLowererRange)

	seedNativeLowererCounter(ctx, t, client, seedStart, end)
	seedNativeLowererGauge(ctx, t, client, seedStart, end)

	lowerers, enabled := chopttest.WireAllNativeLowerers(ctx, t, client)
	for _, family := range nativeLowererFamilies {
		if !enabled.Has(family.Feature) {
			t.Fatalf("chopt feature %q (family %s) did not resolve enabled against %s — "+
				"this lane's own activation assertions would be vacuous against a server too old "+
				"for the ts_grid_* floor", family.Feature, family.Name, nativeLowererCHImage)
		}
	}

	h := prom.New(client, schema.DefaultOTelMetrics(), nil)
	h.Lowerers = lowerers
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, family := range nativeLowererFamilies {
		t.Run(family.Name, func(t *testing.T) {
			queryID := "native-lowerer-" + family.Name
			code := runNativeLowererQuery(t, mux, family.Query, start, end, queryID)
			if code != http.StatusOK {
				t.Fatalf("%s: HTTP %d (want 200) for query %q", family.Name, code, family.Query)
			}
			chopttest.AssertNativeFunctionFired(ctx, t, client.Conn(), queryID, family.WantFn)
		})
	}
}

// runNativeLowererQuery issues one query_range GET over [start, end) at
// nativeLowererStep, with ctx pre-stamped to queryID (chclient.WithQueryID)
// so the ClickHouse dispatch it triggers carries EXACTLY this query_id in
// system.query_log.
func runNativeLowererQuery(t *testing.T, mux *http.ServeMux, query string, start, end time.Time, queryID string) int {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", fmt.Sprintf("%d", start.Unix()))
	params.Set("end", fmt.Sprintf("%d", end.Unix()))
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
// cadence across [start, end], for the rate/increase/resets families.
func seedNativeLowererCounter(ctx context.Context, t *testing.T, client *chclient.Client, start, end time.Time) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`INSERT INTO otel_metrics_sum
    (ResourceAttributes, ServiceName, MetricName, Attributes, StartTimeUnix, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES `)
	i := 0
	for ts := start; !ts.After(end); ts = ts.Add(nativeLowererScrapeInterval) {
		if i > 0 {
			sb.WriteString(",\n")
		}
		tsStr := ts.Format("2006-01-02 15:04:05.000")
		fmt.Fprintf(&sb,
			"(map('service.name','api'), 'api', '%s', map('host','%s'), toDateTime64('%s', 9), toDateTime64('%s', 9), %g, 2, true)",
			nativeLowererCounterMetric, nativeLowererHost, tsStr, tsStr, float64(i+1))
		i++
	}
	if err := client.Exec(ctx, sb.String()); err != nil {
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
func seedNativeLowererGauge(ctx context.Context, t *testing.T, client *chclient.Client, start, end time.Time) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`INSERT INTO otel_metrics_gauge
    (ResourceAttributes, ServiceName, MetricName, Attributes, StartTimeUnix, TimeUnix, Value) VALUES `)
	i := 0
	for ts := start; !ts.After(end); ts = ts.Add(nativeLowererScrapeInterval) {
		if i > 0 {
			sb.WriteString(",\n")
		}
		tsStr := ts.Format("2006-01-02 15:04:05.000")
		value := float64(i%nativeLowererGaugeModulus) + float64(i)*nativeLowererGaugeSlope
		fmt.Fprintf(&sb,
			"(map('service.name','api'), 'api', '%s', map('host','%s'), toDateTime64('%s', 9), toDateTime64('%s', 9), %g)",
			nativeLowererGaugeMetric, nativeLowererHost, tsStr, tsStr, value)
		i++
	}
	if err := client.Exec(ctx, sb.String()); err != nil {
		t.Fatalf("seed %s: %v", nativeLowererGaugeMetric, err)
	}
	t.Logf("seeded %s: %d samples", nativeLowererGaugeMetric, i)
}
