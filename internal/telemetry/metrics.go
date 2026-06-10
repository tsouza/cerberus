// Package telemetry owns cerberus's self-observability instruments — the
// OpenTelemetry counters and histograms that report how many queries are
// flowing through the gateway, how long each pipeline stage takes, and
// how much ClickHouse work each query did.
//
// Spans (per-request, owned by cerbtrace) and metrics (this package)
// are complementary: spans tell a per-request story (one trace per query),
// metrics aggregate the same events into cheap dashboard-friendly
// counters / histograms. The two sets share their attribute namespace
// (`cerberus.*`) so dashboards can pivot on the same fields.
//
// All instruments are created lazily on first call to Instruments(), so
// the package is safe to import from anywhere without paying a startup
// cost. Tests can install their own MeterProvider via
// otel.SetMeterProvider before the first call; subsequent calls reuse
// the cached instruments built off whichever provider was active at the
// first call. For test isolation use Reset() to drop the cache.
package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

// meterName is the instrumentation-scope identifier stamped on every
// metric this package records. Matches the package import path so the
// scope is greppable across cerberus's instruments.
const meterName = "github.com/tsouza/cerberus/internal/telemetry"

// Attribute keys shared with cerbtrace so dashboards pivot on the same
// names whether the source is a span attribute or a metric label.
const (
	// AttrQL is the query language: "promql", "logql", or "traceql".
	AttrQL = attribute.Key("cerberus.ql")
	// AttrRoute is the matched http.ServeMux pattern — e.g.
	// "GET /api/v1/query". Cardinality is bounded by the route set.
	AttrRoute = attribute.Key("cerberus.route")
	// AttrResult is "ok" or "error" — handler outcome bucket.
	AttrResult = attribute.Key("result")
	// AttrStage is the pipeline stage: parse / lower / optimize /
	// emit / execute. Cardinality is fixed at five.
	AttrStage = attribute.Key("stage")
)

// Stage names. Mirrored from cerbtrace's span names so a dashboard can
// group histogram buckets by the same stage attribute the trace view
// uses.
const (
	StageParse    = "parse"
	StageLower    = "lower"
	StageOptimize = "optimize"
	StageEmit     = "emit"
	StageExecute  = "execute"
)

// Result label values for AttrResult.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Instruments groups the cerberus self-metric set. One instance is
// cached per process; callers should fetch it via Get() rather than
// constructing their own.
type Instruments struct {
	// QueriesTotal counts every Prom/Loki/Tempo query the gateway
	// handles, regardless of result. Attributes: cerberus.ql,
	// cerberus.route, result.
	QueriesTotal metric.Int64Counter

	// QueryDuration is the wall-clock distribution per query, end to
	// end (handler entry to handler return). Seconds. Same attributes
	// as QueriesTotal.
	QueryDuration metric.Float64Histogram

	// StageDuration is the per-pipeline-stage timing distribution.
	// Attributes: stage. Seconds.
	StageDuration metric.Float64Histogram

	// RulesApplied is the distribution of how many optimizer rule
	// invocations reported a change for a given query. A rough proxy
	// for how much rewriting the optimizer actually did. No
	// attributes — the optimizer is QL-agnostic.
	RulesApplied metric.Int64Histogram

	// ClickHouseRowsRead is the distribution of rows ClickHouse
	// reported reading per query (sum across Progress events).
	// Attribute: cerberus.ql.
	ClickHouseRowsRead metric.Int64Histogram

	// ClickHouseBytesRead is the distribution of bytes ClickHouse
	// reported reading per query (sum across Progress events).
	// Attribute: cerberus.ql.
	ClickHouseBytesRead metric.Int64Histogram

	// QueryInflight is the count of currently-executing engine
	// queries — incremented at engine entry, decremented (via defer)
	// at engine return so panics + early-returns + cancellations
	// still balance. Attribute: cerberus.ql. The cerberus
	// dashboard panel queries `sum by (cerberus_ql)
	// (cerberus_query_inflight)`.
	QueryInflight metric.Int64UpDownCounter
}

// Histogram bucket boundaries, one ladder per instrument, matched to the
// unit + magnitude each instrument actually records.
//
// These exist because the OTel Go SDK's default explicit-bucket layout —
// [0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500,
// 10000] — is millisecond-shaped, while cerberus's duration instruments
// record seconds (see QueryTimer.Done / StageTimer.Done). Without
// explicit boundaries every real observation (2ms–1s) collapsed into the
// (0,5] bucket, so histogram_quantile(0.95) linearly interpolated a flat
// 0.95×5 = 4.75s for every query language. The row/byte instruments had
// the same defect: a millisecond ladder is meaningless for row and byte
// counts.
//
// Exported so tests assert against the single source of truth.
var (
	// QueryDurationBoundaries covers end-to-end query wall-clock in
	// seconds: Prometheus DefBuckets plus a 30s tail for pathological
	// slow queries.
	QueryDurationBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	// StageDurationBoundaries covers per-pipeline-stage wall-clock in
	// seconds. Stages (parse / lower / optimize / emit) are much faster
	// than whole queries, so the ladder starts at 1ms.
	StageDurationBoundaries = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

	// RulesAppliedBoundaries covers the optimizer's per-query change
	// count — small integers, so a near-unit ladder.
	RulesAppliedBoundaries = []float64{0, 1, 2, 3, 5, 8, 13, 21}

	// RowsReadBoundaries covers ClickHouse rows read per query —
	// decade-exponential from 100 rows to 100M rows.
	RowsReadBoundaries = []float64{100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8}

	// BytesReadBoundaries covers ClickHouse bytes read per query —
	// decade-exponential from 10KB to 10GB.
	BytesReadBoundaries = []float64{1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10}
)

var (
	cacheMu sync.Mutex
	cache   *Instruments
)

// Get returns the process-wide Instruments set, building it on first
// call from the currently-installed MeterProvider. Safe to call
// concurrently; subsequent callers see the same pointer. Failures from
// the SDK's instrument constructors are treated as fatal — they should
// only fire on a misconfigured provider, and a noop fallback would
// silently swallow telemetry.
func Get() *Instruments {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cache != nil {
		return cache
	}
	meter := otel.GetMeterProvider().Meter(meterName)
	cache = mustBuild(meter)
	return cache
}

// Reset drops the cached Instruments. Tests call this after swapping
// the global MeterProvider so the next Get() picks up the new one.
func Reset() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = nil
}

// mustBuild constructs every instrument off meter. The OTel SDK only
// returns an error if the instrument name/options fail validation —
// our names are constants known good, so the error path is
// theoretically unreachable but we still wire it via panic so a future
// rename that violates the syntax surfaces loudly at first use.
func mustBuild(meter metric.Meter) *Instruments {
	queriesTotal, err := meter.Int64Counter(
		"cerberus_queries_total",
		metric.WithDescription("Total queries handled, by language / route / result."),
		metric.WithUnit("{query}"),
	)
	if err != nil {
		panic("telemetry: build queries_total: " + err.Error())
	}
	queryDuration, err := meter.Float64Histogram(
		"cerberus_queries_duration_seconds",
		metric.WithDescription("End-to-end query wall-clock, seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(QueryDurationBoundaries...),
	)
	if err != nil {
		panic("telemetry: build queries_duration: " + err.Error())
	}
	stageDuration, err := meter.Float64Histogram(
		"cerberus_pipeline_stage_duration_seconds",
		metric.WithDescription("Per-stage pipeline wall-clock, seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(StageDurationBoundaries...),
	)
	if err != nil {
		panic("telemetry: build stage_duration: " + err.Error())
	}
	rulesApplied, err := meter.Int64Histogram(
		"cerberus_optimizer_rules_applied",
		metric.WithDescription("Optimizer rule invocations that changed the plan."),
		metric.WithUnit("{rule}"),
		metric.WithExplicitBucketBoundaries(RulesAppliedBoundaries...),
	)
	if err != nil {
		panic("telemetry: build rules_applied: " + err.Error())
	}
	chRows, err := meter.Int64Histogram(
		"cerberus_clickhouse_rows_read",
		metric.WithDescription("ClickHouse rows read per query (sum of Progress events)."),
		metric.WithUnit("{row}"),
		metric.WithExplicitBucketBoundaries(RowsReadBoundaries...),
	)
	if err != nil {
		panic("telemetry: build rows_read: " + err.Error())
	}
	chBytes, err := meter.Int64Histogram(
		"cerberus_clickhouse_bytes_read",
		metric.WithDescription("ClickHouse bytes read per query (sum of Progress events)."),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(BytesReadBoundaries...),
	)
	if err != nil {
		panic("telemetry: build bytes_read: " + err.Error())
	}
	queryInflight, err := meter.Int64UpDownCounter(
		"cerberus_query_inflight",
		metric.WithDescription("Currently-executing engine queries, by language."),
		metric.WithUnit("{query}"),
	)
	if err != nil {
		panic("telemetry: build query_inflight: " + err.Error())
	}
	return &Instruments{
		QueriesTotal:        queriesTotal,
		QueryDuration:       queryDuration,
		StageDuration:       stageDuration,
		RulesApplied:        rulesApplied,
		ClickHouseRowsRead:  chRows,
		ClickHouseBytesRead: chBytes,
		QueryInflight:       queryInflight,
	}
}

// Install replaces the process-global MeterProvider with mp. Passing
// nil installs a no-op provider — the safe default when no OTLP
// exporter is configured. Mirrors the spirit of cmd/cerberus's
// installOTel for tracer providers, and resets the instrument cache so
// the next Get() rebuilds against the new provider.
func Install(mp metric.MeterProvider) {
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	otel.SetMeterProvider(mp)
	Reset()
}

// StageTimer is a one-shot stopwatch returned by ObserveStage. Call
// Done() exactly once when the stage completes — typically via defer
// at the entry of the stage's call site.
type StageTimer struct {
	stage string
	start time.Time
}

// ObserveStage starts a stopwatch for the given pipeline stage. The
// returned timer records its duration on the StageDuration histogram
// when Done(ctx) is called. Idiomatic use:
//
//	t := telemetry.ObserveStage(telemetry.StageOptimize)
//	defer t.Done(ctx)
func ObserveStage(stage string) *StageTimer {
	return &StageTimer{stage: stage, start: time.Now()}
}

// Done records the stage's elapsed time. ctx propagates baggage /
// exemplars to the histogram if the SDK is configured for that. A nil
// receiver is a no-op so callers don't have to guard the defer.
func (t *StageTimer) Done(ctx context.Context) {
	if t == nil {
		return
	}
	Get().StageDuration.Record(
		ctx, time.Since(t.start).Seconds(),
		metric.WithAttributes(AttrStage.String(t.stage)),
	)
}

// QueryTimer is the per-request stopwatch returned by ObserveQuery. It
// owns the (counter, duration) pair for the API-handler middleware so
// callers don't have to remember to bump both.
type QueryTimer struct {
	ql    string
	route string
	start time.Time
}

// ObserveQuery starts a stopwatch for a Prom/Loki/Tempo request. ql is
// the language identifier ("promql" / "logql" / "traceql"); route is
// the matched http.ServeMux pattern (handler middleware pulls this
// from r.Pattern after the mux has resolved the request).
func ObserveQuery(ql, route string) *QueryTimer {
	return &QueryTimer{ql: ql, route: route, start: time.Now()}
}

// Done records the query's outcome on QueriesTotal and its wall-clock
// on QueryDuration. result is one of ResultOK / ResultError. ctx is
// passed through to the OTel SDK so exemplars (linked spans) attach
// correctly when the request span is on the context.
func (t *QueryTimer) Done(ctx context.Context, result string) {
	if t == nil {
		return
	}
	attrs := metric.WithAttributes(
		AttrQL.String(t.ql),
		AttrRoute.String(t.route),
		AttrResult.String(result),
	)
	inst := Get()
	inst.QueriesTotal.Add(ctx, 1, attrs)
	inst.QueryDuration.Record(ctx, time.Since(t.start).Seconds(), attrs)
}

// RecordRulesApplied records n (the optimizer's per-query change
// count) on the RulesApplied histogram. Pulled out as a free function
// because the optimizer.Driver.Run callsite is the only producer and
// it doesn't need a stopwatch wrapper.
func RecordRulesApplied(ctx context.Context, n int) {
	Get().RulesApplied.Record(ctx, int64(n))
}

// ObserveQueryInflight increments the QueryInflight gauge for ql and
// returns a closure that decrements it. The idiomatic call pattern is:
//
//	defer telemetry.ObserveQueryInflight(ctx, "promql")()
//
// The deferred decrement covers panics, early returns, and context
// cancellations so the gauge stays balanced across every engine code
// path. ql is one of "promql" / "logql" / "traceql"; the value lands on
// the cerberus.ql attribute so the dashboard's `sum by (cerberus_ql)`
// pivot resolves.
func ObserveQueryInflight(ctx context.Context, ql string) func() {
	attrs := metric.WithAttributes(AttrQL.String(ql))
	inst := Get()
	inst.QueryInflight.Add(ctx, 1, attrs)
	return func() {
		inst.QueryInflight.Add(ctx, -1, attrs)
	}
}

// RecordClickHouseProgress records the (rows, bytes) summary the
// ClickHouse driver reports for a single query. ql labels the
// histogram so dashboards can pivot per-language. Caller is the
// chclient query wrapper; it aggregates Progress callbacks into a
// single (rows, bytes) total before invoking this.
func RecordClickHouseProgress(ctx context.Context, ql string, rows, bytes uint64) {
	attrs := metric.WithAttributes(AttrQL.String(ql))
	inst := Get()
	// OTel Int64Histogram requires int64; CH reports uint64 totals.
	// Real CH row/byte counts never approach int64 overflow (≈9.2e18).
	inst.ClickHouseRowsRead.Record(ctx, int64(rows), attrs)   //nolint:gosec // G115
	inst.ClickHouseBytesRead.Record(ctx, int64(bytes), attrs) //nolint:gosec // G115
}
