package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"
	metricscollectorv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// The five metric shapes MIG-06 pushes through the collector, one per OTel
// metric data kind, and the OTel-CH landing table each one is documented to
// route to (internal/schema.DefaultOTelMetrics — the same table set the
// collector's own clickhouseexporter creates). Every name carries a literal
// dot, the OTel convention (`http.server.duration`), so the landing-row
// assertion can tell a bridge that has silently pre-mangled the name at
// WRITE time (wrong — the dot/underscore mapping is a cerberus READ-time
// concern) from one that preserved it.
const (
	bridgeCounterName   = "cerberus.migration.bridge.counter"
	bridgeGaugeName     = "cerberus.migration.bridge.gauge"
	bridgeHistogramName = "cerberus.migration.bridge.histogram"
	bridgeExpHistName   = "cerberus.migration.bridge.exphistogram"
	bridgeSummaryName   = "cerberus.migration.bridge.summary"
)

// bridgeServiceName is the resource-attribute `service.name` every pushed
// metric carries, so the landing-row assertion can check resource-attribute
// placement (not just presence) alongside the metric name.
const bridgeServiceName = "cerberus-migration-bridge"

// bridgeMetric names one pushed shape and the table it must land in.
type bridgeMetric struct {
	kind  string // human-readable, used only in failure messages
	name  string
	table func(schema.Metrics) string
}

// bridgeMetrics is the fixed set MIG-06 pushes and checks, in a stable order
// so failure messages are deterministic.
var bridgeMetrics = []bridgeMetric{
	{"counter", bridgeCounterName, func(m schema.Metrics) string { return m.SumTable }},
	{"gauge", bridgeGaugeName, func(m schema.Metrics) string { return m.GaugeTable }},
	{"classic histogram", bridgeHistogramName, func(m schema.Metrics) string { return m.HistogramTable }},
	{"exponential histogram", bridgeExpHistName, func(m schema.Metrics) string { return m.ExpHistogramTable }},
	{"summary", bridgeSummaryName, func(m schema.Metrics) string { return m.SummaryTable }},
}

// bridgeState is MIG-06's one synthetic OTLP push, plus the instant every
// data point in it carries — the landing-row and cerberus-readback
// assertions both need it.
type bridgeState struct {
	pushed bool
	at     time.Time
}

// registerIngestBridgeSteps binds MIG-06's Given/When/Then steps.
func (w *World) registerIngestBridgeSteps(ctx *godog.ScenarioContext) {
	ctx.Step(
		`^the operator pushes a counter, a gauge, a classic histogram, an exponential histogram and a summary through the collector$`,
		w.whenPushIngestBridgeBatch,
	)
	ctx.Step(`^each metric lands in its declared ClickHouse table with the declared type$`,
		w.thenEachMetricLandsInDeclaredTable)
	ctx.Step(`^the metric name and its resource attributes are preserved on the landed rows$`,
		w.thenBridgeNameAndAttributesPreserved)
	ctx.Step(`^a query for the counter and the gauge and the classic histogram returns data through cerberus$`,
		w.thenBridgeFlatMetricsQueryThroughCerberus)
}

// bridgeAttrs renders a string map as OTLP KeyValue attributes.
func bridgeAttrs(m map[string]string) []*commonv1.KeyValue {
	out := make([]*commonv1.KeyValue, 0, len(m))
	for k, v := range m {
		out = append(out, &commonv1.KeyValue{
			Key:   k,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}},
		})
	}
	return out
}

// Fixed data values every bridge point carries. They exercise no arithmetic
// downstream — MIG-06 asserts landing + type, not sample values — so any
// distinct, non-zero constants suffice.
const (
	bridgeGaugeValue         = 7.0
	bridgeCounterValue       = 3.0
	bridgeHistCount          = uint64(4)
	bridgeHistSum            = 2.0
	bridgeExpHistCount       = uint64(4)
	bridgeExpHistSum         = 2.0
	bridgeExpHistScale       = int32(0)
	bridgeSummaryCount       = uint64(4)
	bridgeSummarySum         = 2.0
	bridgeSummaryQuantile    = 0.5
	bridgeSummaryQuantileVal = 0.5
)

// bridgeHistogramBounds is the classic histogram's explicit bucket edges;
// BucketCounts therefore carries len(bounds)+1 entries.
var bridgeHistogramBounds = []float64{0.5, 1.0}

// otlpPushTimeout bounds the single gRPC Export call MIG-06 makes.
const otlpPushTimeout = 30 * time.Second

// whenPushIngestBridgeBatch builds one OTLP ResourceMetrics carrying all five
// shapes and exports it directly to the collector's OTLP/gRPC endpoint — the
// same ingest path the seeder's schema warm-up uses, but with a payload this
// scenario controls end to end, since the shared fixture builder declares no
// exponential-histogram or summary data.
func (w *World) whenPushIngestBridgeBatch() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	at := time.Now().UTC()
	ts := uint64(at.UnixNano()) //nolint:gosec // wall clock, always positive

	resource := &resourcev1.Resource{Attributes: bridgeAttrs(map[string]string{"service.name": bridgeServiceName})}
	scope := &commonv1.InstrumentationScope{Name: "cerberus-migration-bridge", Version: "v1"}

	metrics := []*otlpmetrics.Metric{
		{
			Name: bridgeCounterName,
			Data: &otlpmetrics.Metric_Sum{Sum: &otlpmetrics.Sum{
				AggregationTemporality: otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
				DataPoints: []*otlpmetrics.NumberDataPoint{{
					TimeUnixNano: ts,
					Value:        &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: bridgeCounterValue},
				}},
			}},
		},
		{
			Name: bridgeGaugeName,
			Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{
				DataPoints: []*otlpmetrics.NumberDataPoint{{
					TimeUnixNano: ts,
					Value:        &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: bridgeGaugeValue},
				}},
			}},
		},
		{
			Name: bridgeHistogramName,
			Data: &otlpmetrics.Metric_Histogram{Histogram: &otlpmetrics.Histogram{
				AggregationTemporality: otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*otlpmetrics.HistogramDataPoint{{
					TimeUnixNano:   ts,
					Count:          bridgeHistCount,
					Sum:            ptr(bridgeHistSum),
					ExplicitBounds: bridgeHistogramBounds,
					BucketCounts:   []uint64{2, 1, 1},
				}},
			}},
		},
		{
			Name: bridgeExpHistName,
			Data: &otlpmetrics.Metric_ExponentialHistogram{ExponentialHistogram: &otlpmetrics.ExponentialHistogram{
				AggregationTemporality: otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*otlpmetrics.ExponentialHistogramDataPoint{{
					TimeUnixNano: ts,
					Count:        bridgeExpHistCount,
					Sum:          ptr(bridgeExpHistSum),
					Scale:        bridgeExpHistScale,
					Positive: &otlpmetrics.ExponentialHistogramDataPoint_Buckets{
						Offset:       0,
						BucketCounts: []uint64{1, 1, 1, 1},
					},
				}},
			}},
		},
		{
			Name: bridgeSummaryName,
			Data: &otlpmetrics.Metric_Summary{Summary: &otlpmetrics.Summary{
				DataPoints: []*otlpmetrics.SummaryDataPoint{{
					TimeUnixNano: ts,
					Count:        bridgeSummaryCount,
					Sum:          bridgeSummarySum,
					QuantileValues: []*otlpmetrics.SummaryDataPoint_ValueAtQuantile{{
						Quantile: bridgeSummaryQuantile,
						Value:    bridgeSummaryQuantileVal,
					}},
				}},
			}},
		},
	}

	conn, err := grpc.NewClient(w.live.CollectorOTLPAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("migration harness: dial the collector's OTLP endpoint %s: %w", w.live.CollectorOTLPAddr, err)
	}
	defer func() { _ = conn.Close() }()

	callCtx, cancel := context.WithTimeout(context.Background(), otlpPushTimeout)
	defer cancel()

	client := metricscollectorv1.NewMetricsServiceClient(conn)
	if _, err := client.Export(callCtx, &metricscollectorv1.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpmetrics.ResourceMetrics{{
			Resource: resource,
			ScopeMetrics: []*otlpmetrics.ScopeMetrics{{
				Scope:   scope,
				Metrics: metrics,
			}},
		}},
	}); err != nil {
		return fmt.Errorf("migration harness: export the ingest-bridge batch to the collector: %w", err)
	}

	w.bridge.pushed, w.bridge.at = true, at
	return nil
}

// ptr returns a pointer to v — HistogramDataPoint.Sum and friends are
// *float64 so an explicit-but-absent sum is distinguishable from zero.
func ptr[T any](v T) *T { return &v }

// bridgeCHConn dials ClickHouse directly with the live stack's own
// credentials, closing over the caller's defer.
func (w *World) bridgeCHConn(ctx context.Context) (driver.Conn, error) {
	return seed.DialCH(ctx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
}

// bridgeLandingWait bounds how long a landing-row probe waits for the
// collector to have flushed the pushed batch through to ClickHouse.
const bridgeLandingWait = 60 * time.Second

// bridgePollInterval paces the landing-row poll.
const bridgePollInterval = 500 * time.Millisecond

// countRows returns how many rows table carries for name, polling until
// bridgeLandingWait so a scenario never races the collector's flush.
func countRows(ctx context.Context, conn driver.Conn, table, name string) (uint64, error) {
	var n uint64
	deadline := time.Now().Add(bridgeLandingWait)
	for {
		row := conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM %s WHERE MetricName = ?", table), name) //nolint:gosec // table is one of five harness-owned constants, never user input
		if err := row.Scan(&n); err != nil {
			return 0, fmt.Errorf("migration harness: count %s WHERE MetricName = %q: %w", table, name, err)
		}
		if n > 0 || time.Now().After(deadline) {
			return n, nil
		}
		time.Sleep(bridgePollInterval)
	}
}

// thenEachMetricLandsInDeclaredTable asserts every pushed shape landed
// EXACTLY in its documented table, and nowhere else — the "no pass on a row
// landed somewhere" half of MIG-06's PASS assertion. A counter that landed in
// otel_metrics_gauge (a monotonicity bit dropped) or a summary that landed
// nowhere both fail this, even though a naive "did a row appear anywhere"
// check would pass the first.
func (w *World) thenEachMetricLandsInDeclaredTable() error {
	if !w.bridge.pushed {
		return fmt.Errorf("migration harness: no ingest-bridge batch was pushed for this scenario")
	}
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	ctx := context.Background()
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tables := schema.DefaultOTelMetrics()
	allTables := []string{tables.GaugeTable, tables.SumTable, tables.HistogramTable, tables.ExpHistogramTable, tables.SummaryTable}

	for _, bm := range bridgeMetrics {
		want := bm.table(tables)
		n, err := countRows(ctx, conn, want, bm.name)
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("migration harness: %s %q landed %d row(s) in %s, want exactly one",
				bm.kind, bm.name, n, want)
		}
		for _, other := range allTables {
			if other == want {
				continue
			}
			n, err := countRowsNoWait(ctx, conn, other, bm.name)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("migration harness: %s %q ALSO landed %d row(s) in %s — mis-reconstructed type fanned out to the wrong table",
					bm.kind, bm.name, n, other)
			}
		}
	}
	return nil
}

// countRowsNoWait is countRows without the landing-wait poll, for the
// negative "did NOT land in the wrong table" checks — there is nothing to
// wait for converging on here.
func countRowsNoWait(ctx context.Context, conn driver.Conn, table, name string) (uint64, error) {
	var n uint64
	row := conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM %s WHERE MetricName = ?", table), name) //nolint:gosec // table is one of five harness-owned constants, never user input
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("migration harness: count %s WHERE MetricName = %q: %w", table, name, err)
	}
	return n, nil
}

// thenBridgeNameAndAttributesPreserved asserts the landed row's MetricName is
// byte-identical to the pushed OTel name (dots included — the dot/underscore
// mapping is a cerberus READ-time PromQL concern, never a write-time one),
// and that the resource attribute the batch carried is present in the landed
// ResourceAttributes map under the exact value pushed.
func (w *World) thenBridgeNameAndAttributesPreserved() error {
	if !w.bridge.pushed {
		return fmt.Errorf("migration harness: no ingest-bridge batch was pushed for this scenario")
	}
	ctx := context.Background()
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tables := schema.DefaultOTelMetrics()
	for _, bm := range bridgeMetrics {
		table := bm.table(tables)
		var name, svc string
		row := conn.QueryRow(ctx, //nolint:gosec // table is one of five harness-owned constants, never user input
			fmt.Sprintf("SELECT MetricName, ResourceAttributes['service.name'] FROM %s WHERE MetricName = ? LIMIT 1", table),
			bm.name)
		if err := row.Scan(&name, &svc); err != nil {
			return fmt.Errorf("migration harness: read the landed %s row back from %s: %w", bm.kind, table, err)
		}
		if name != bm.name {
			return fmt.Errorf("migration harness: %s landed as MetricName %q, want %q (dots must survive to read time)",
				bm.kind, name, bm.name)
		}
		if svc != bridgeServiceName {
			return fmt.Errorf("migration harness: %s landed with ResourceAttributes['service.name'] = %q, want %q",
				bm.kind, svc, bridgeServiceName)
		}
	}
	return nil
}

// promInstantQueryResult is the shape of a cerberus /api/v1/query response
// this scenario needs: enough to see whether any series came back.
type promInstantQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

// thenBridgeFlatMetricsQueryThroughCerberus queries cerberus's own PromQL
// read path for the three flat (single-value) shapes — this is the "no pass
// on a row landed somewhere" honesty check from the OTHER side: a row that
// landed in ClickHouse but that cerberus's read path cannot reconstruct
// (wrong temporality, wrong monotonicity bit) would fail HERE even though
// the direct CH probe above already passed. The exponential histogram and
// summary shapes are deliberately left to MIG-12/MIG-17's dedicated quantile
// scenarios, not re-covered here.
func (w *World) thenBridgeFlatMetricsQueryThroughCerberus() error {
	if !w.bridge.pushed {
		return fmt.Errorf("migration harness: no ingest-bridge batch was pushed for this scenario")
	}
	for _, name := range []string{bridgeCounterName, bridgeGaugeName, bridgeHistogramName + "_count"} {
		if err := requireInstantQueryHasResult(w.live.CerberusURL, name); err != nil {
			return err
		}
	}
	return nil
}

// requireInstantQueryHasResult asserts cerberus's /api/v1/query for the exact
// `{__name__="<name>"}` selector returns at least one series.
func requireInstantQueryHasResult(baseURL, name string) error {
	query := fmt.Sprintf(`{__name__=%q}`, name)
	reqURL := baseURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("migration harness: build the instant-query request for %s: %w", name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus for %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("migration harness: read cerberus's response for %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("migration harness: cerberus returned HTTP %d for %s: %s", resp.StatusCode, name, string(body))
	}
	var out promInstantQueryResult
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("migration harness: decode cerberus's response for %s: %w (body: %s)", name, err, string(body))
	}
	if out.Status != "success" {
		return fmt.Errorf("migration harness: cerberus's query status for %s = %q, want success (body: %s)",
			name, out.Status, string(body))
	}
	if len(out.Data.Result) == 0 {
		return fmt.Errorf("migration harness: cerberus's query for %s returned zero series — the row did not land somewhere queryable",
			name)
	}
	return nil
}
