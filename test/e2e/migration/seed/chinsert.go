package seed

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The column lists below name exactly the columns the OTel-CH exporter
// populates for each signal, and no more. Naming a column the exporter's
// current DDL does not define is a hard INSERT failure ("No such column"),
// which is the correct outcome: it means this seeder and the collector have
// drifted, and the seeder must follow the exporter rather than the reverse.
const (
	insertGaugeSQL = `INSERT INTO otel_metrics_gauge (
		ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
		Attributes, StartTimeUnix, TimeUnix, Value, Flags
	)`
	insertSumSQL = `INSERT INTO otel_metrics_sum (
		ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
		Attributes, StartTimeUnix, TimeUnix, Value, Flags,
		AggregationTemporality, IsMonotonic
	)`
	insertHistogramSQL = `INSERT INTO otel_metrics_histogram (
		ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
		Attributes, StartTimeUnix, TimeUnix, Count, Sum,
		BucketCounts, ExplicitBounds, Flags, AggregationTemporality
	)`
	insertLogsSQL = `INSERT INTO otel_logs (
		Timestamp, TraceId, SpanId, TraceFlags,
		SeverityText, SeverityNumber, ServiceName, Body,
		ResourceSchemaUrl, ResourceAttributes,
		ScopeSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
		LogAttributes
	)`
	insertTracesSQL = `INSERT INTO otel_traces (
		Timestamp, TraceId, SpanId, ParentSpanId,
		SpanName, SpanKind, ServiceName,
		ScopeName, ScopeVersion,
		ResourceAttributes, SpanAttributes,
		Duration, StatusCode, StatusMessage
	)`
)

// Descriptions and units the fixture declares for its metrics. They are
// written because the OTel-CH columns exist and an empty value there would be
// a silent difference from what a real collector writes, not because any
// assertion reads them.
const (
	fixtureMetricDescription = "cerberus migration Tier-1 fixture series"
	fixtureMetricUnit        = "1"
	// noTraceFlags / noSpanFlags are the OTLP flag words for "no flags set".
	noTraceFlags = uint8(0)
	noSpanFlags  = uint32(0)
)

// InsertFixture writes the whole fixture into ClickHouse: metrics, logs and
// traces, in that order. Any failure aborts — a half-written ClickHouse side
// would surface later as a divergence against the reference rather than as the
// write failure it is.
func InsertFixture(ctx context.Context, conn driver.Conn, f Fixture) error {
	if err := insertGauge(ctx, conn, f.Gauge); err != nil {
		return fmt.Errorf("insert gauge: %w", err)
	}
	if err := insertSum(ctx, conn, f.Counter); err != nil {
		return fmt.Errorf("insert sum: %w", err)
	}
	if err := insertHistogram(ctx, conn, f.Histogram); err != nil {
		return fmt.Errorf("insert histogram: %w", err)
	}
	if err := insertLogs(ctx, conn, f.LogStreams); err != nil {
		return fmt.Errorf("insert logs: %w", err)
	}
	if err := insertTraces(ctx, conn, f.Traces); err != nil {
		return fmt.Errorf("insert traces: %w", err)
	}
	return nil
}

// resourceAttributes is the ClickHouse-side resource map for a service. It
// carries the OTel `service.name` key the exporter writes and cerberus's
// read-side resolves the dedicated ServiceName column from.
func resourceAttributes(service string) *orderedAttrs {
	return sortedAttrs(map[string]string{otelServiceNameKey: service})
}

// orderedAttrs is a ClickHouse Map column value with a DETERMINISTIC key
// order.
//
// A plain Go map cannot be used here. ClickHouse stores a Map's pairs in
// insertion order and the driver writes them in Go's map-iteration order,
// which is randomised per range statement — so the same logical label set
// lands as `{'a':…,'b':…}` on one row and `{'b':…,'a':…}` on the next. That
// alone breaks the seeder's own determinism contract (two runs of the same
// declaration must produce the same rows), and it also splits one logical
// series across two stored representations. Sorting the keys makes every row
// byte-identical for a given label set.
type orderedAttrs struct {
	keys   []string
	values map[string]string
}

// sortedAttrs wraps a label map for insertion with its keys in sorted order.
func sortedAttrs(m map[string]string) *orderedAttrs {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return &orderedAttrs{keys: keys, values: m}
}

// Put satisfies the driver's IterableOrderedMap interface. The seeder only
// ever writes these values, never scans into them, so a read path would be
// dead code — an unsupported call fails loudly rather than silently dropping
// the pair.
func (a *orderedAttrs) Put(key, value any) {
	k, kOK := key.(string)
	v, vOK := value.(string)
	if !kOK || !vOK {
		panic(fmt.Sprintf("orderedAttrs.Put: want string key and value, got %T/%T", key, value))
	}
	if _, exists := a.values[k]; !exists {
		a.keys = append(a.keys, k)
		sort.Strings(a.keys)
	}
	a.values[k] = v
}

// Iterator satisfies the driver's IterableOrderedMap interface.
func (a *orderedAttrs) Iterator() column.MapIterator {
	return &orderedAttrsIterator{attrs: a, index: -1}
}

type orderedAttrsIterator struct {
	attrs *orderedAttrs
	index int
}

func (i *orderedAttrsIterator) Next() bool {
	i.index++
	return i.index < len(i.attrs.keys)
}

func (i *orderedAttrsIterator) Key() any { return i.attrs.keys[i.index] }

func (i *orderedAttrsIterator) Value() any { return i.attrs.values[i.attrs.keys[i.index]] }

// InsertCounterSeries writes hand-built counter (Sum) series directly,
// bypassing the fixture-declaration path InsertFixture drives. It is the
// same insertSum every Counter sample in a declared fixture goes through, so
// a hand-built series lands byte-for-byte the way a declared one would —
// exported for a caller that needs one specific series a fixture declaration
// does not carry (see test/e2e/migration/steps/then_ruler.go, MIG-09's
// recording-rule fixture, whose expression depends on a metric name the
// three-signal fixture the Tier-2 stack seeds never declares).
func InsertCounterSeries(ctx context.Context, conn driver.Conn, series []MetricSeries) error {
	return insertSum(ctx, conn, series)
}

func insertGauge(ctx context.Context, conn driver.Conn, series []MetricSeries) error {
	batch, err := conn.PrepareBatch(ctx, insertGaugeSQL)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, s := range series {
		resource := resourceAttributes(s.ServiceName)
		for _, sample := range s.Samples {
			if err := batch.Append(
				resource, s.ServiceName, s.MetricName, fixtureMetricDescription, fixtureMetricUnit,
				sortedAttrs(s.Attributes), sample.Time, sample.Time, sample.Value, noSpanFlags,
			); err != nil {
				return fmt.Errorf("append %s: %w", s.MetricName, err)
			}
		}
	}
	return sendBatch(batch)
}

func insertSum(ctx context.Context, conn driver.Conn, series []MetricSeries) error {
	batch, err := conn.PrepareBatch(ctx, insertSumSQL)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, s := range series {
		resource := resourceAttributes(s.ServiceName)
		for _, sample := range s.Samples {
			if err := batch.Append(
				resource, s.ServiceName, s.MetricName, fixtureMetricDescription, fixtureMetricUnit,
				sortedAttrs(s.Attributes), sample.Time, sample.Time, sample.Value, noSpanFlags,
				otelCumulativeTemporality, true,
			); err != nil {
				return fmt.Errorf("append %s: %w", s.MetricName, err)
			}
		}
	}
	return sendBatch(batch)
}

func insertHistogram(ctx context.Context, conn driver.Conn, series []HistogramSeries) error {
	batch, err := conn.PrepareBatch(ctx, insertHistogramSQL)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, s := range series {
		resource := resourceAttributes(s.ServiceName)
		for _, p := range s.Points {
			if err := batch.Append(
				resource, s.ServiceName, s.MetricName, fixtureMetricDescription, fixtureMetricUnit,
				sortedAttrs(s.Attributes), p.Time, p.Time, p.Count, p.Sum,
				p.BucketCounts, s.Bounds, noSpanFlags, otelCumulativeTemporality,
			); err != nil {
				return fmt.Errorf("append %s: %w", s.MetricName, err)
			}
		}
	}
	return sendBatch(batch)
}

// severityNumbers maps the fixture's declared levels onto the OTLP severity
// numbers the exporter writes. The fixture only ever emits declared levels, so
// an unmapped level is a fixture bug and fails the write rather than landing a
// row with severity 0.
var severityNumbers = map[string]uint8{
	"trace": 1,
	"debug": 5,
	"info":  9,
	"warn":  13,
	"error": 17,
	"fatal": 21,
}

func insertLogs(ctx context.Context, conn driver.Conn, streams []LogStream) error {
	batch, err := conn.PrepareBatch(ctx, insertLogsSQL)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, s := range streams {
		for _, r := range s.Records {
			severity, ok := severityNumbers[r.Level]
			if !ok {
				return fmt.Errorf("log record carries undeclared level %q", r.Level)
			}
			if err := batch.Append(
				r.Time,
				r.TraceID,
				r.SpanID,
				noTraceFlags,
				strings.ToUpper(r.Level),
				severity,
				s.ServiceName,
				r.Line,
				"",
				// Stream identity lives in ResourceAttributes — the same keys
				// the Loki push sends as stream labels, so a LogQL matcher
				// selects the same records on both sides.
				sortedAttrs(s.Labels),
				"",
				otlpScopeName,
				otlpScopeVersion,
				sortedAttrs(map[string]string{}),
				// Per-record structured metadata is restricted to
				// detected_level on BOTH sides. A ClickHouse-only key here
				// would surface as a permanent structured-metadata diff.
				sortedAttrs(map[string]string{detectedLevelKey: r.Level}),
			); err != nil {
				return fmt.Errorf("append log record: %w", err)
			}
		}
	}
	return sendBatch(batch)
}

// spanKindCH maps the OTLP span-kind enum onto the string literals the OTel-CH
// exporter writes into the SpanKind column, which is what cerberus's TraceQL
// emitter matches on.
var spanKindCH = map[otlptrace.Span_SpanKind]string{
	otlptrace.Span_SPAN_KIND_UNSPECIFIED: "Unspecified",
	otlptrace.Span_SPAN_KIND_INTERNAL:    "Internal",
	otlptrace.Span_SPAN_KIND_SERVER:      "Server",
	otlptrace.Span_SPAN_KIND_CLIENT:      "Client",
	otlptrace.Span_SPAN_KIND_PRODUCER:    "Producer",
	otlptrace.Span_SPAN_KIND_CONSUMER:    "Consumer",
}

// statusCodeCH maps the OTLP status enum onto the exporter's StatusCode
// literals.
var statusCodeCH = map[otlptrace.Status_StatusCode]string{
	otlptrace.Status_STATUS_CODE_UNSET: "Unset",
	otlptrace.Status_STATUS_CODE_OK:    "Ok",
	otlptrace.Status_STATUS_CODE_ERROR: "Error",
}

func insertTraces(ctx context.Context, conn driver.Conn, traces []Trace) error {
	batch, err := conn.PrepareBatch(ctx, insertTracesSQL)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, t := range traces {
		resource := resourceAttributes(t.ServiceName)
		for _, s := range t.Spans {
			kind, ok := spanKindCH[s.Kind]
			if !ok {
				return fmt.Errorf("span %s carries unmapped kind %v", s.Name, s.Kind)
			}
			status, ok := statusCodeCH[s.StatusCode]
			if !ok {
				return fmt.Errorf("span %s carries unmapped status %v", s.Name, s.StatusCode)
			}
			parent := ""
			if s.ParentSpanID != ([8]byte{}) {
				parent = hex.EncodeToString(s.ParentSpanID[:])
			}
			duration := s.End.Sub(s.Start)
			if duration < 0 {
				return fmt.Errorf("span %s ends before it starts", s.Name)
			}
			if err := batch.Append(
				s.Start,
				hex.EncodeToString(s.TraceID[:]),
				hex.EncodeToString(s.SpanID[:]),
				parent,
				s.Name,
				kind,
				t.ServiceName,
				otlpScopeName,
				otlpScopeVersion,
				resource,
				sortedAttrs(s.Attributes),
				uint64(duration.Nanoseconds()), //nolint:gosec // guarded non-negative above
				status,
				"",
			); err != nil {
				return fmt.Errorf("append span %s: %w", s.Name, err)
			}
		}
	}
	return sendBatch(batch)
}

func sendBatch(batch driver.Batch) error {
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}
