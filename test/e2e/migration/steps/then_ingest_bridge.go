package steps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"
	metricscollectorv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tsouza/cerberus/internal/migrate"
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
//
// The exponential histogram's name additionally carries cerberus's documented
// `_exp_hist` routing suffix (schema.Metrics.ExpHistogramSuffix). The exporter
// routes by OTLP data kind and would land it in
// otel_metrics_exponential_histogram whatever it were called; cerberus's READ
// path routes by that suffix and nothing else. Naming it so is what lets
// MIG-06 assert the two halves AGREE rather than only that the write half
// works — see thenBridgeExplainNamesLandedTables.
const (
	bridgeCounterName   = "cerberus.migration.bridge.counter"
	bridgeGaugeName     = "cerberus.migration.bridge.gauge"
	bridgeHistogramName = "cerberus.migration.bridge.histogram"
	bridgeExpHistName   = "cerberus.migration.bridge.exphistogram_exp_hist"
	bridgeSummaryName   = "cerberus.migration.bridge.summary"
)

// bridgeServiceName is the resource-attribute `service.name` every pushed
// metric carries, so the landing-row assertion can check resource-attribute
// placement (not just presence) alongside the metric name.
const bridgeServiceName = "cerberus-migration-bridge"

// bridgeRunAttr is the resource attribute every pushed data point carries a
// per-run random value under, and that every probe in this file filters on. A
// global `count() = 1` would turn the SECOND run against a live stack red for
// a reason that has nothing to do with the bridge — the first run's rows are
// still there — and would leave the wrong-table probe unable to tell this
// run's stray row from a previous run's legitimate one.
const bridgeRunAttr = "cerberus.migration.bridge.run"

// bridgeRunIDBytes is how much randomness the per-run id carries. It only has
// to separate the runs that share one live stack's retention window.
const bridgeRunIDBytes = 8

// bridgeCumulative is the OTLP aggregation temporality every shape that
// carries one is pushed with, and the value the landed row must carry back.
// Taken from the proto enum rather than its wire integer so a renumbering
// upstream cannot silently turn this into a comparison against the wrong
// constant.
const bridgeCumulative = int32(otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE)

// The companion series a classic histogram's bucket layout and observation
// count are addressed through in PromQL. In the OTel-CH layout both live as
// columns on the bare-name row, so resolving them is itself a reconstruction
// step rather than a lookup.
const (
	histogramBucketSuffix = "_bucket"
	histogramCountSuffix  = "_count"
)

// bridgeExplainQuantile is the phi the two histogram read queries carry. Any
// value works: `migrate explain` reports which physical tables an expression
// touches, never a sample value, and the quantile call only exists to select
// the histogram lowering at all.
const bridgeExplainQuantile = 0.5

// bridgeMetric names one pushed shape: the table it must land in, the
// function that reads the columns carrying its TYPE identity back, and the
// PromQL cerberus's own read path resolves it through.
//
// readQuery is empty for the summary. Cerberus's PromQL read path has no
// route to otel_metrics_summary at all, so no expression could make
// `migrate explain` name that table; the summary's landing and type are
// asserted by the direct ClickHouse probe alone. That limit is stated in
// docs/migration-testing.md section 6 rather than passed over in silence.
type bridgeMetric struct {
	kind       string // human-readable, used only in failure messages
	name       string
	table      func(schema.Metrics) string
	readQuery  string
	verifyType func(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error
}

// bridgeMetrics is the fixed set MIG-06 pushes and checks, in a stable order
// so failure messages are deterministic.
var bridgeMetrics = []bridgeMetric{
	{
		kind:       "counter",
		name:       bridgeCounterName,
		table:      func(m schema.Metrics) string { return m.SumTable },
		readQuery:  fmt.Sprintf("{__name__=%q}", bridgeCounterName),
		verifyType: verifyBridgeSum,
	},
	{
		kind:       "gauge",
		name:       bridgeGaugeName,
		table:      func(m schema.Metrics) string { return m.GaugeTable },
		readQuery:  fmt.Sprintf("{__name__=%q}", bridgeGaugeName),
		verifyType: verifyBridgeGauge,
	},
	{
		kind:  "classic histogram",
		name:  bridgeHistogramName,
		table: func(m schema.Metrics) string { return m.HistogramTable },
		readQuery: fmt.Sprintf("histogram_quantile(%v, {__name__=%q})",
			bridgeExplainQuantile, bridgeHistogramName+histogramBucketSuffix),
		verifyType: verifyBridgeHistogram,
	},
	{
		kind:  "exponential histogram",
		name:  bridgeExpHistName,
		table: func(m schema.Metrics) string { return m.ExpHistogramTable },
		// The exp-histogram lowering keys off the parser's bare metric NAME,
		// which a `{__name__="…"}` matcher never populates — written that way
		// the expression falls through to the classic histogram table. So this
		// one query spells the name as a bare identifier, in the all-underscore
		// form cerberus fans back out to every dot placement at read time.
		readQuery: fmt.Sprintf("histogram_quantile(%v, %s)",
			bridgeExplainQuantile, strings.ReplaceAll(bridgeExpHistName, ".", "_")),
		verifyType: verifyBridgeExpHistogram,
	},
	{
		kind:       "summary",
		name:       bridgeSummaryName,
		table:      func(m schema.Metrics) string { return m.SummaryTable },
		verifyType: verifyBridgeSummary,
	},
}

// bridgeState is MIG-06's one synthetic OTLP push: the run id scoping every
// probe to it, and the table each shape's row was actually FOUND in — not the
// table it was expected in, so the explain assertion downstream compares
// cerberus's read-path routing against what the live stack really did.
type bridgeState struct {
	pushed bool
	runID  string
	landed map[string]string
}

// registerIngestBridgeSteps binds MIG-06's Given/When/Then steps.
func (w *World) registerIngestBridgeSteps(ctx *godog.ScenarioContext) {
	ctx.Step(
		`^the operator pushes a counter, a gauge, a classic histogram, an exponential histogram and a summary through the collector$`,
		w.whenPushIngestBridgeBatch,
	)
	ctx.Step(`^each metric lands in its declared ClickHouse table and in no other$`,
		w.thenEachMetricLandsInDeclaredTable)
	ctx.Step(`^the declared type of each metric survives on the landed row$`,
		w.thenBridgeDeclaredTypeSurvives)
	ctx.Step(`^the metric name and its resource attributes are preserved on the landed rows$`,
		w.thenBridgeNameAndAttributesPreserved)
	ctx.Step(`^the offline explain names the same ClickHouse tables the landed rows were found in$`,
		w.thenBridgeExplainNamesLandedTables)
	ctx.Step(`^a query for the counter and the gauge and the classic histogram returns the pushed values through cerberus$`,
		w.thenBridgeFlatMetricsQueryThroughCerberus)
}

// bridgeAttrs renders a string map as OTLP KeyValue attributes, in key order
// so one push is byte-identical to the next but for the run id.
func bridgeAttrs(m map[string]string) []*commonv1.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonv1.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, &commonv1.KeyValue{
			Key:   k,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: m[k]}},
		})
	}
	return out
}

// Fixed data values every bridge point carries. Each shape's payload is read
// back off the landed row and compared against what was pushed here, so the
// numbers need only be distinct and non-zero — a zero would be
// indistinguishable from a column the exporter never wrote at all.
const (
	bridgeGaugeValue         = 7.0
	bridgeCounterValue       = 3.0
	bridgeHistCount          = uint64(4)
	bridgeHistSum            = 2.0
	bridgeExpHistCount       = uint64(4)
	bridgeExpHistSum         = 2.0
	bridgeExpHistScale       = int32(0)
	bridgeExpHistOffset      = int32(0)
	bridgeSummaryCount       = uint64(4)
	bridgeSummarySum         = 2.0
	bridgeSummaryQuantile    = 0.5
	bridgeSummaryQuantileVal = 0.5
)

// bridgeHistogramBounds is the classic histogram's explicit bucket edges;
// bridgeHistogramBuckets therefore carries len(bounds)+1 entries. Both arrays
// are read back off the landed row: a bridge that dropped a bound, reordered
// the counts or collapsed the layout to one bucket still lands a row in the
// right table, and would pass a presence-only check.
var (
	bridgeHistogramBounds  = []float64{0.5, 1.0}
	bridgeHistogramBuckets = []uint64{2, 1, 1}
)

// bridgeExpHistBuckets is the exponential histogram's positive-range bucket
// counts, read back the same way.
var bridgeExpHistBuckets = []uint64{1, 1, 1, 1}

// otlpPushTimeout bounds the single gRPC Export call MIG-06 makes.
const otlpPushTimeout = 30 * time.Second

// newBridgeRunID draws the per-run scope every probe filters on.
func newBridgeRunID() (string, error) {
	buf := make([]byte, bridgeRunIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("migration harness: draw an ingest-bridge run id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// whenPushIngestBridgeBatch builds one OTLP ResourceMetrics carrying all five
// shapes and exports it directly to the collector's OTLP/gRPC endpoint — the
// same ingest path the seeder's schema warm-up uses, but with a payload this
// scenario controls end to end, since the shared fixture builder declares no
// exponential-histogram or summary data.
func (w *World) whenPushIngestBridgeBatch() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	runID, err := newBridgeRunID()
	if err != nil {
		return err
	}
	ts := uint64(time.Now().UTC().UnixNano()) //nolint:gosec // wall clock, always positive

	resource := &resourcev1.Resource{Attributes: bridgeAttrs(map[string]string{
		"service.name": bridgeServiceName,
		bridgeRunAttr:  runID,
	})}
	scope := &commonv1.InstrumentationScope{Name: "cerberus-migration-bridge", Version: "v1"}

	metrics := []*otlpmetrics.Metric{
		{
			Name: bridgeCounterName,
			Data: &otlpmetrics.Metric_Sum{Sum: &otlpmetrics.Sum{
				AggregationTemporality: otlpmetrics.AggregationTemporality(bridgeCumulative),
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
				AggregationTemporality: otlpmetrics.AggregationTemporality(bridgeCumulative),
				DataPoints: []*otlpmetrics.HistogramDataPoint{{
					TimeUnixNano:   ts,
					Count:          bridgeHistCount,
					Sum:            ptr(bridgeHistSum),
					ExplicitBounds: bridgeHistogramBounds,
					BucketCounts:   bridgeHistogramBuckets,
				}},
			}},
		},
		{
			Name: bridgeExpHistName,
			Data: &otlpmetrics.Metric_ExponentialHistogram{ExponentialHistogram: &otlpmetrics.ExponentialHistogram{
				AggregationTemporality: otlpmetrics.AggregationTemporality(bridgeCumulative),
				DataPoints: []*otlpmetrics.ExponentialHistogramDataPoint{{
					TimeUnixNano: ts,
					Count:        bridgeExpHistCount,
					Sum:          ptr(bridgeExpHistSum),
					Scale:        bridgeExpHistScale,
					Positive: &otlpmetrics.ExponentialHistogramDataPoint_Buckets{
						Offset:       bridgeExpHistOffset,
						BucketCounts: bridgeExpHistBuckets,
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

	w.bridge.pushed, w.bridge.runID = true, runID
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

// bridgeWrongTableWatch is how long the wrong-table probe waits AFTER every
// right-table row has appeared before concluding nothing landed anywhere
// else. A duplicate produced by a mis-reconstruction flushes on the
// exporter's own schedule, not on the right table's, so a probe fired the
// instant the right-table row shows up cannot see one that is still in
// flight. It sits comfortably above the exporter's `timeout: 5s` in
// tiers/tier1-dual/otel-collector-config.yaml, which is what bounds one
// export attempt.
const bridgeWrongTableWatch = 20 * time.Second

// bridgeRowWhere is the WHERE clause every landed-row probe shares: this
// metric, from THIS run.
const bridgeRowWhere = " WHERE MetricName = ? AND ResourceAttributes[?] = ?"

// bridgeSelect renders a run-scoped read of cols from table. table is always
// one of the five harness-owned table names in schema.DefaultOTelMetrics, and
// cols are that schema's own column names — never user input.
func bridgeSelect(table string, cols ...string) string {
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + table + bridgeRowWhere
}

// countBridgeRows returns how many rows table carries for name under this
// run's scope.
func countBridgeRows(ctx context.Context, conn driver.Conn, table, name, runID string) (uint64, error) {
	var n uint64
	row := conn.QueryRow(ctx, bridgeSelect(table, "count()"), name, bridgeRunAttr, runID)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("migration harness: count %s WHERE MetricName = %q for run %s: %w", table, name, runID, err)
	}
	return n, nil
}

// waitForBridgeRow polls table until it carries a row for name under this
// run's scope, so the landing assertion never races the collector's flush.
func waitForBridgeRow(ctx context.Context, conn driver.Conn, table, name, runID string) error {
	deadline := time.Now().Add(bridgeLandingWait)
	for {
		n, err := countBridgeRows(ctx, conn, table, name, runID)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration harness: %q never landed in %s within %s for run %s",
				name, table, bridgeLandingWait, runID)
		}
		time.Sleep(bridgePollInterval)
	}
}

// metricTables lists every OTel-CH metrics table in a stable order, so a
// wrong-table failure names the same set every time.
func metricTables(m schema.Metrics) []string {
	return []string{m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable, m.SummaryTable}
}

// thenEachMetricLandsInDeclaredTable asserts every pushed shape landed
// EXACTLY in its documented table, and nowhere else — the "no pass on a row
// landed somewhere" half of MIG-06's PASS assertion. A counter that landed in
// otel_metrics_gauge (a monotonicity bit dropped) or a summary that landed
// nowhere both fail this, even though a naive "did a row appear anywhere"
// check would pass the first.
func (w *World) thenEachMetricLandsInDeclaredTable() error {
	if err := w.requireBridgePush(); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tables := schema.DefaultOTelMetrics()
	for _, bm := range bridgeMetrics {
		if err := waitForBridgeRow(ctx, conn, bm.table(tables), bm.name, w.bridge.runID); err != nil {
			return fmt.Errorf("migration harness: %s: %w", bm.kind, err)
		}
	}

	// Only now is the negative half meaningful: every shape has arrived, so a
	// stray duplicate elsewhere has had the whole landing window plus this
	// watch to show up.
	time.Sleep(bridgeWrongTableWatch)

	w.bridge.landed = make(map[string]string, len(bridgeMetrics))
	for _, bm := range bridgeMetrics {
		var found []string
		for _, table := range metricTables(tables) {
			n, err := countBridgeRows(ctx, conn, table, bm.name, w.bridge.runID)
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			found = append(found, table)
			if n != 1 {
				return fmt.Errorf("migration harness: %s %q landed %d rows in %s for run %s, want exactly one",
					bm.kind, bm.name, n, table, w.bridge.runID)
			}
		}
		want := bm.table(tables)
		if len(found) != 1 || found[0] != want {
			return fmt.Errorf("migration harness: %s %q landed in %v for run %s, want exactly [%s] — a mis-reconstructed type either missed its table or fanned out to another",
				bm.kind, bm.name, found, w.bridge.runID, want)
		}
		w.bridge.landed[bm.kind] = want
	}
	return nil
}

// requireBridgePush guards every Then on a scenario that really pushed a
// batch through a live stack.
func (w *World) requireBridgePush() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	if !w.bridge.pushed {
		return fmt.Errorf("migration harness: no ingest-bridge batch was pushed for this scenario")
	}
	return nil
}

// requireBridgeLanded returns the table a shape's row was located in, failing
// when the landing step that locates it never ran.
func (w *World) requireBridgeLanded(kind string) (string, error) {
	table, ok := w.bridge.landed[kind]
	if !ok {
		return "", fmt.Errorf("migration harness: %s has no located row; the landing step must run before this one", kind)
	}
	return table, nil
}

// thenBridgeDeclaredTypeSurvives reads back the columns that CARRY each
// shape's type — the aggregation temporality, the monotonicity bit, the
// bucket layout, the quantile pairs — and compares them against what the
// batch pushed. Without it, "landed in its declared table" proves only the
// table: a sum reconstructed with IsMonotonic=false, or a cumulative
// histogram rewritten as a delta one, still lands in otel_metrics_sum and
// otel_metrics_histogram respectively, and every query built on top of it is
// silently wrong.
func (w *World) thenBridgeDeclaredTypeSurvives() error {
	if err := w.requireBridgePush(); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	m := schema.DefaultOTelMetrics()
	for _, bm := range bridgeMetrics {
		table, err := w.requireBridgeLanded(bm.kind)
		if err != nil {
			return err
		}
		if err := bm.verifyType(ctx, conn, m, table, bm.name, w.bridge.runID); err != nil {
			return fmt.Errorf("migration harness: %s %q: %w", bm.kind, bm.name, err)
		}
	}
	return nil
}

// requireCumulative fails when a landed row carries a temporality other than
// the one pushed. A DELTA row in a table cerberus reads as cumulative makes
// every rate() over it wrong by a whole window.
func requireCumulative(got int32) error {
	if got != bridgeCumulative {
		return fmt.Errorf("landed with AggregationTemporality %d (%s), want %d (%s)",
			got, otlpmetrics.AggregationTemporality(got),
			bridgeCumulative, otlpmetrics.AggregationTemporality(bridgeCumulative))
	}
	return nil
}

// verifyBridgeSum reads the counter's type columns: the temporality, the
// monotonicity bit that is the whole difference between a counter and a gauge
// stored in the sum table, and the value.
func verifyBridgeSum(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error {
	var (
		temporality int32
		monotonic   bool
		value       float64
	)
	q := bridgeSelect(table, m.AggregationTemporalityColumn, m.IsMonotonicColumn, m.ValueColumn)
	if err := conn.QueryRow(ctx, q, name, bridgeRunAttr, runID).Scan(&temporality, &monotonic, &value); err != nil {
		return fmt.Errorf("read the type columns back from %s: %w", table, err)
	}
	if err := requireCumulative(temporality); err != nil {
		return err
	}
	if !monotonic {
		return fmt.Errorf("landed with IsMonotonic false — a counter reconstructed as a non-monotonic sum is a gauge sitting in the counter's table")
	}
	if value != bridgeCounterValue {
		return fmt.Errorf("landed with Value %v, want %v", value, bridgeCounterValue)
	}
	return nil
}

// verifyBridgeGauge reads the gauge's value. The gauge table carries neither
// a temporality nor a monotonicity column — being in that table with no such
// column IS the gauge type, and the wrong-table half of the landing step is
// what proves it.
func verifyBridgeGauge(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error {
	var value float64
	q := bridgeSelect(table, m.ValueColumn)
	if err := conn.QueryRow(ctx, q, name, bridgeRunAttr, runID).Scan(&value); err != nil {
		return fmt.Errorf("read the value column back from %s: %w", table, err)
	}
	if value != bridgeGaugeValue {
		return fmt.Errorf("landed with Value %v, want %v", value, bridgeGaugeValue)
	}
	return nil
}

// verifyBridgeHistogram reads the classic histogram's temporality and its
// whole bucket layout. The bounds and the per-bucket counts ARE the type: a
// row carrying the right count and sum but a collapsed or reordered layout
// makes every histogram_quantile over it wrong while landing in exactly the
// right table.
func verifyBridgeHistogram(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error {
	var (
		temporality int32
		count       uint64
		sum         float64
		bounds      []float64
		buckets     []uint64
	)
	q := bridgeSelect(table,
		m.AggregationTemporalityColumn, m.CountColumn, m.SumColumn, m.ExplicitBoundsColumn, m.BucketCountsColumn)
	if err := conn.QueryRow(ctx, q, name, bridgeRunAttr, runID).
		Scan(&temporality, &count, &sum, &bounds, &buckets); err != nil {
		return fmt.Errorf("read the type columns back from %s: %w", table, err)
	}
	if err := requireCumulative(temporality); err != nil {
		return err
	}
	if count != bridgeHistCount || sum != bridgeHistSum {
		return fmt.Errorf("landed with Count %d / Sum %v, want %d / %v", count, sum, bridgeHistCount, bridgeHistSum)
	}
	if !equalFloat64s(bounds, bridgeHistogramBounds) {
		return fmt.Errorf("landed with ExplicitBounds %v, want %v — the bucket layout did not survive", bounds, bridgeHistogramBounds)
	}
	if !equalUint64s(buckets, bridgeHistogramBuckets) {
		return fmt.Errorf("landed with BucketCounts %v, want %v — the bucket layout did not survive", buckets, bridgeHistogramBuckets)
	}
	return nil
}

// verifyBridgeExpHistogram reads the exponential histogram's temporality and
// the scale/offset/counts triple that defines its bucket geometry. Scale is
// the whole reason a native histogram is not a classic one; a row that lost
// it is not the type it claims to be.
func verifyBridgeExpHistogram(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error {
	var (
		temporality int32
		count       uint64
		sum         float64
		scale       int32
		offset      int32
		buckets     []uint64
	)
	q := bridgeSelect(table,
		m.AggregationTemporalityColumn, m.CountColumn, m.SumColumn,
		m.ScaleColumn, m.PositiveOffsetColumn, m.PositiveBucketCountsColumn)
	if err := conn.QueryRow(ctx, q, name, bridgeRunAttr, runID).
		Scan(&temporality, &count, &sum, &scale, &offset, &buckets); err != nil {
		return fmt.Errorf("read the type columns back from %s: %w", table, err)
	}
	if err := requireCumulative(temporality); err != nil {
		return err
	}
	if count != bridgeExpHistCount || sum != bridgeExpHistSum {
		return fmt.Errorf("landed with Count %d / Sum %v, want %d / %v", count, sum, bridgeExpHistCount, bridgeExpHistSum)
	}
	if scale != bridgeExpHistScale || offset != bridgeExpHistOffset {
		return fmt.Errorf("landed with Scale %d / PositiveOffset %d, want %d / %d — the native bucket geometry did not survive",
			scale, offset, bridgeExpHistScale, bridgeExpHistOffset)
	}
	if !equalUint64s(buckets, bridgeExpHistBuckets) {
		return fmt.Errorf("landed with PositiveBucketCounts %v, want %v", buckets, bridgeExpHistBuckets)
	}
	return nil
}

// verifyBridgeSummary reads the summary's quantile pairs. A summary carries
// no temporality column at all — its type IS the pre-computed quantile array,
// so an empty one is a summary reconstructed as something else.
func verifyBridgeSummary(ctx context.Context, conn driver.Conn, m schema.Metrics, table, name, runID string) error {
	var (
		count     uint64
		sum       float64
		quantiles []float64
		values    []float64
	)
	q := bridgeSelect(table, m.CountColumn, m.SumColumn,
		m.ValueAtQuantilesColumn+".Quantile", m.ValueAtQuantilesColumn+".Value")
	if err := conn.QueryRow(ctx, q, name, bridgeRunAttr, runID).
		Scan(&count, &sum, &quantiles, &values); err != nil {
		return fmt.Errorf("read the type columns back from %s: %w", table, err)
	}
	if count != bridgeSummaryCount || sum != bridgeSummarySum {
		return fmt.Errorf("landed with Count %d / Sum %v, want %d / %v", count, sum, bridgeSummaryCount, bridgeSummarySum)
	}
	wantQuantiles, wantValues := []float64{bridgeSummaryQuantile}, []float64{bridgeSummaryQuantileVal}
	if !equalFloat64s(quantiles, wantQuantiles) || !equalFloat64s(values, wantValues) {
		return fmt.Errorf("landed with ValueAtQuantiles %v -> %v, want %v -> %v — the pre-computed quantiles are the summary type",
			quantiles, values, wantQuantiles, wantValues)
	}
	return nil
}

// equalFloat64s compares two bucket-edge / quantile arrays exactly. Every
// value compared through it round-trips a Float64 column or a Prometheus
// `le` label without loss, so no tolerance applies — a tolerance here would
// be an epsilon on a LAYOUT, which has no meaning.
func equalFloat64s(a, b []float64) bool { return slices.Equal(a, b) }

// equalUint64s compares two bucket-count arrays exactly.
func equalUint64s(a, b []uint64) bool { return slices.Equal(a, b) }

// thenBridgeNameAndAttributesPreserved asserts the landed row's MetricName is
// byte-identical to the pushed OTel name (dots included — the dot/underscore
// mapping is a cerberus READ-time PromQL concern, never a write-time one),
// and that the resource attribute the batch carried is present in the landed
// ResourceAttributes map under the exact value pushed.
func (w *World) thenBridgeNameAndAttributesPreserved() error {
	if err := w.requireBridgePush(); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	m := schema.DefaultOTelMetrics()
	for _, bm := range bridgeMetrics {
		table := bm.table(m)
		var name, svc string
		q := bridgeSelect(table, m.MetricNameColumn, m.ResourceAttributesColumn+"['service.name']")
		if err := conn.QueryRow(ctx, q, bm.name, bridgeRunAttr, w.bridge.runID).Scan(&name, &svc); err != nil {
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

// The workspace names MIG-06's explain run writes under, and the corpus
// fields its synthetic entries carry. The corpus is synthesised rather than
// harvested: MIG-06 asserts where cerberus's READ path sends the five shapes
// it just pushed, and no archetype fixture mentions them.
const (
	bridgeWorkDir        = "ingest-bridge"
	bridgeExplainCorpus  = "bridge-corpus.json"
	bridgeExplainReport  = "bridge-explain.txt"
	bridgeExplainSource  = "probe:ingest-bridge/"
	bridgeExplainKind    = "panel"
	bridgeExplainLang    = "promql"
	bridgeCorpusFileMode = 0o600
)

// thenBridgeExplainNamesLandedTables closes MIG-06's loop from the other
// side. The direct ClickHouse probe proved where the collector PUT each
// shape; `cerberus migrate explain` — the CLI docs/migration-testing.md
// section 6 names for the touched-tables half — reports offline which
// physical tables cerberus's read path would GO TO for those same shapes. If
// the two sets differ, one of the halves is wrong and every query an operator
// builds on the bridged data reads a table the data is not in, which no
// amount of "a row landed somewhere" can detect.
//
// The summary is absent from both sides: cerberus's PromQL read path has no
// route to otel_metrics_summary, so no expression could make explain name it.
// That gap is recorded in docs/migration-testing.md section 6.
func (w *World) thenBridgeExplainNamesLandedTables() error {
	if err := w.requireBridgePush(); err != nil {
		return err
	}

	probe := map[string]struct{}{}
	queries := make([]migrate.CorpusQuery, 0, len(bridgeMetrics))
	for _, bm := range bridgeMetrics {
		if bm.readQuery == "" {
			continue
		}
		table, err := w.requireBridgeLanded(bm.kind)
		if err != nil {
			return err
		}
		probe[table] = struct{}{}
		queries = append(queries, migrate.CorpusQuery{
			Expr:   bm.readQuery,
			Source: bridgeExplainSource + bm.kind,
			Kind:   bridgeExplainKind,
			Lang:   bridgeExplainLang,
		})
	}
	if len(queries) == 0 {
		return fmt.Errorf("migration harness: no bridge shape declares a read query; explain would be handed an empty corpus")
	}

	path, err := w.workPath(bridgeWorkDir, bridgeExplainCorpus)
	if err != nil {
		return err
	}
	body, err := json.Marshal(migrate.Corpus{
		Version: migrate.CorpusVersion,
		Queries: queries,
		Skipped: []migrate.SkippedEntry{},
	})
	if err != nil {
		return fmt.Errorf("migration harness: encode the ingest-bridge explain corpus: %w", err)
	}
	if err := os.WriteFile(path, body, bridgeCorpusFileMode); err != nil {
		return fmt.Errorf("migration harness: write the ingest-bridge explain corpus: %w", err)
	}

	raw, err := w.runArtifact(bridgeWorkDir, bridgeExplainReport, "migrate", "explain", "--corpus", path)
	if err != nil {
		return err
	}
	rep, err := parseExplainReport(raw)
	if err != nil {
		return fmt.Errorf("migration harness: the ingest-bridge explain report: %w", err)
	}
	if len(rep.Queries) != len(queries) {
		return fmt.Errorf("migration harness: explain reported %d of the %d bridge read queries",
			len(rep.Queries), len(queries))
	}

	named := map[string]struct{}{}
	for _, q := range rep.Queries {
		if q.Unsupported != "" {
			return fmt.Errorf("migration harness: explain rejected the bridge read query %q (%s): %s",
				q.Expr, q.Source, q.Unsupported)
		}
		if len(q.Tables) == 0 {
			return fmt.Errorf("migration harness: explain named no table for the bridge read query %q (%s)", q.Expr, q.Source)
		}
		for _, t := range q.Tables {
			named[t] = struct{}{}
		}
	}
	if !equalStringSets(sortedSet(named), sortedSet(probe)) {
		return fmt.Errorf("migration harness: explain names tables %v for the bridge's read queries, but the landed rows were found in %v — cerberus's read path and the ingest bridge disagree about where a metric type lives",
			sortedSet(named), sortedSet(probe))
	}
	return nil
}

// sortedSet renders a set as a sorted slice, so both sides of a set
// comparison and every failure message are deterministic.
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bridgeReadback pairs a shape cerberus's PromQL read path can resolve with
// the value that shape pushed, so the read-back assertion compares a live
// answer against the batch rather than against itself.
type bridgeReadback struct {
	name string
	want float64
}

// bridgeReadbacks are the three flat (single-value) shapes MIG-06 reads back
// through cerberus. The exponential histogram and summary shapes are
// deliberately left to MIG-12/MIG-17's dedicated quantile scenarios.
var bridgeReadbacks = []bridgeReadback{
	{bridgeCounterName, bridgeCounterValue},
	{bridgeGaugeName, bridgeGaugeValue},
	{bridgeHistogramName + histogramCountSuffix, float64(bridgeHistCount)},
}

// thenBridgeFlatMetricsQueryThroughCerberus queries cerberus's own PromQL
// read path for the three flat shapes and compares each answer against the
// value the batch pushed. This is the "no pass on a row landed somewhere"
// honesty check from the OTHER side: a row that landed in ClickHouse but that
// cerberus's read path reconstructs wrongly — reading the sum column for a
// count, or resolving the companion series against the wrong arm of the union
// — returns a number here, just not the right one, and a presence-only check
// would call that a pass.
func (w *World) thenBridgeFlatMetricsQueryThroughCerberus() error {
	if err := w.requireBridgePush(); err != nil {
		return err
	}
	for _, rb := range bridgeReadbacks {
		env, err := instantQuery(w.live.CerberusURL, fmt.Sprintf(`{__name__=%q}`, rb.name))
		if err != nil {
			return fmt.Errorf("migration harness: read %s back through cerberus: %w", rb.name, err)
		}
		got, err := instantSampleValue(env)
		if err != nil {
			return fmt.Errorf("migration harness: read %s back through cerberus: %w", rb.name, err)
		}
		if got != rb.want {
			return fmt.Errorf("migration harness: cerberus returns %v for %s, but the batch pushed %v",
				got, rb.name, rb.want)
		}
	}
	return nil
}
