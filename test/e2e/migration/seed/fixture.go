package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Declaration is an archetype's `seed/fixture.json`: the service list, the
// log-format list and the metric table the fixture is built from. It declares
// DATA, never a generator per backend — one declaration produces one in-memory
// fixture, and that fixture is written to both sides of the parity diff. Two
// independent sample paths cannot land identical timestamps, and the migration
// comparator keys samples by exact timestamp, so a second generator would
// inject sample-level skew into the one place ground truth exists.
type Declaration struct {
	// Services and StatusCodes are crossed to produce the metric label sets.
	// The cross is a real CROSS JOIN over both declared lists, never a
	// `index % len` derivation: a correlated derivation silently collapses
	// len(Services)*len(StatusCodes) series into len(Services).
	Services    []string `json:"services"`
	StatusCodes []string `json:"http_status_codes"`
	// LogFormats is crossed with Services to produce the log streams.
	LogFormats []string `json:"log_formats"`
	// LogJob is the `job` stream label every fixture stream carries — the
	// handle a LogQL corpus query selects the fixture by.
	LogJob string `json:"log_job"`
	// GaugeMetric, CounterMetric and HistogramMetric name the three metric
	// tables the fixture populates. The histogram is a classic one: PromQL
	// lowering routes any `<base>_count` / `<base>_sum` reference onto
	// otel_metrics_histogram under the bare base name, so the family is only
	// exercisable when a real histogram row exists.
	GaugeMetric     string `json:"gauge_metric"`
	CounterMetric   string `json:"counter_metric"`
	HistogramMetric string `json:"histogram_metric"`
	// HistogramBounds are the explicit bucket edges; a trailing +Inf bucket is
	// implicit, so the bucket count is len(HistogramBounds)+1.
	HistogramBounds []float64 `json:"histogram_bounds"`
	// TracesPerService is how many traces each service emits.
	TracesPerService int `json:"traces_per_service"`
	// SpansPerTrace is the rotation of trace sizes; trace i of a service holds
	// SpansPerTrace[i % len(SpansPerTrace)] spans.
	SpansPerTrace []int `json:"spans_per_trace"`
}

// LoadDeclaration reads and validates an archetype's fixture declaration. A
// declaration with an empty list is rejected rather than silently producing a
// fixture with zero series — an empty fixture makes every parity comparison
// vacuously agree.
func LoadDeclaration(path string) (Declaration, error) {
	buf, err := os.ReadFile(path) //nolint:gosec // harness-supplied fixture path
	if err != nil {
		return Declaration{}, fmt.Errorf("read fixture declaration %s: %w", path, err)
	}
	var d Declaration
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Declaration{}, fmt.Errorf("parse fixture declaration %s: %w", path, err)
	}
	if err := d.validate(); err != nil {
		return Declaration{}, fmt.Errorf("fixture declaration %s: %w", path, err)
	}
	return d, nil
}

func (d Declaration) validate() error {
	for _, f := range []struct {
		name string
		n    int
	}{
		{"services", len(d.Services)},
		{"http_status_codes", len(d.StatusCodes)},
		{"log_formats", len(d.LogFormats)},
		{"histogram_bounds", len(d.HistogramBounds)},
		{"spans_per_trace", len(d.SpansPerTrace)},
	} {
		if f.n == 0 {
			return fmt.Errorf("%s is empty; an empty list produces a fixture no comparison can disagree about", f.name)
		}
	}
	for _, f := range []struct {
		name, value string
	}{
		{"log_job", d.LogJob},
		{"gauge_metric", d.GaugeMetric},
		{"counter_metric", d.CounterMetric},
		{"histogram_metric", d.HistogramMetric},
	} {
		if f.value == "" {
			return fmt.Errorf("%s is empty", f.name)
		}
	}
	if d.TracesPerService <= 0 {
		return fmt.Errorf("traces_per_service is %d, want a positive count", d.TracesPerService)
	}
	for i, n := range d.SpansPerTrace {
		if n <= 0 {
			return fmt.Errorf("spans_per_trace[%d] is %d, want a positive count", i, n)
		}
	}
	return nil
}

// Fixture-window geometry. The fixture's SHAPE is byte-identical on every run;
// only its offset from the epoch moves.
const (
	// SeedWindow is the span of fixture data. It stays well under reference
	// Tempo's wal ingestion_time_range_slack (1h) so no span timestamp is
	// clamped — a clamped span produces a BlockMeta whose time range excludes
	// the fixture, and /api/search then returns zero blocks.
	SeedWindow = 30 * time.Minute
	// SampleStep is the fixture's metric-sample and log-record spacing.
	SampleStep = 15 * time.Second
	// VerifyStep is the step the parity lane queries at. It is a whole
	// multiple of SampleStep so both sides land on the same grid.
	VerifyStep = 60 * time.Second
	// RangeLookbackMargin is the leading slice of the seed window excluded
	// from the verify window, so every range selector in a corpus has full
	// lookback on both sides at the very first queried step.
	RangeLookbackMargin = 10 * time.Minute
)

// Window is the fixture's time geometry: where its samples sit, and which
// sub-interval the parity lane queries. The lane never queries a live window
// ending at `now` — a window whose end moves while the two sides are being
// read cannot be compared.
type Window struct {
	// SeedStart is the first fixture sample.
	SeedStart time.Time
	// VerifyStart is the first instant the parity lane queries.
	VerifyStart time.Time
	// VerifyEnd and AnchorEnd coincide: the last fixture sample is the last
	// queried instant, so no step falls past the data.
	VerifyEnd time.Time
	AnchorEnd time.Time
	// Step is the query step; every fixture sample timestamp is a whole
	// multiple of SampleStep from SeedStart, and SeedStart is Step-aligned.
	Step time.Duration
}

// NewWindow derives the fixture window from a wall-clock instant, truncating
// the anchor to the step grid.
//
// The anchor rolls rather than being pinned to a fixed date, and one anchor
// serves all three signals. A fixed date is unusable: Tempo's live store
// clamps span timestamps outside its ingestion slack, and the clamped block
// metadata makes search return nothing. Per-signal anchors are equally
// unusable: cross-signal correlation needs the three signals inside one
// window. Rolling costs no determinism — the fixture's shape is derived from
// the declaration and a fixed RNG seed, never from the date.
func NewWindow(now time.Time) Window {
	anchorEnd := now.UTC().Truncate(VerifyStep)
	seedStart := anchorEnd.Add(-SeedWindow)
	return Window{
		SeedStart:   seedStart,
		VerifyStart: seedStart.Add(RangeLookbackMargin),
		VerifyEnd:   anchorEnd,
		AnchorEnd:   anchorEnd,
		Step:        VerifyStep,
	}
}

// SampleTimes returns every instant a fixture sample is written at:
// SeedStart, SeedStart+SampleStep, … , AnchorEnd inclusive.
func (w Window) SampleTimes() []time.Time {
	out := make([]time.Time, 0, int(SeedWindow/SampleStep)+1)
	for t := w.SeedStart; !t.After(w.AnchorEnd); t = t.Add(SampleStep) {
		out = append(out, t)
	}
	return out
}

// VerifySteps returns every instant the parity lane queries: VerifyStart,
// VerifyStart+Step, … , VerifyEnd inclusive.
func (w Window) VerifySteps() []time.Time {
	out := make([]time.Time, 0, int(w.VerifyEnd.Sub(w.VerifyStart)/w.Step)+1)
	for t := w.VerifyStart; !t.After(w.VerifyEnd); t = t.Add(w.Step) {
		out = append(out, t)
	}
	return out
}

// TraceSearchRange is the [start, end] a trace search covers. The end bound is
// pushed one step past the last fixture span so a span starting exactly at the
// anchor is inside the range on both sides, whose end-bound inclusivity
// differs.
func (w Window) TraceSearchRange() (time.Time, time.Time) {
	return w.SeedStart, w.AnchorEnd.Add(w.Step)
}

// MetricSeries is one fixture metric series carried in both shapes at once:
// Attributes is simultaneously the ClickHouse `Attributes` map and — plus
// `__name__` — the reference Prometheus label set, so the two sides cannot
// drift into different label surfaces.
type MetricSeries struct {
	MetricName  string
	ServiceName string
	Attributes  map[string]string
	Samples     []Sample
}

// PromSeries renders the series in the reference-Prometheus shape.
func (m MetricSeries) PromSeries() Series {
	labels := make(map[string]string, len(m.Attributes)+1)
	for k, v := range m.Attributes {
		labels[k] = v
	}
	labels[metricNameLabel] = m.MetricName
	return Series{Labels: labels, Samples: m.Samples}
}

// HistogramPoint is one classic-histogram observation set at an instant.
// BucketCounts is per-bucket (NOT cumulative) with len(Bounds)+1 entries — the
// shape the OTel-CH exporter writes. Cumulation into `le` buckets happens only
// on the reference-Prometheus side, where the classic-histogram wire format
// requires it.
type HistogramPoint struct {
	Time         time.Time
	Count        uint64
	Sum          float64
	BucketCounts []uint64
}

// HistogramSeries is one classic-histogram series in both shapes.
type HistogramSeries struct {
	MetricName  string
	ServiceName string
	Attributes  map[string]string
	Bounds      []float64
	Points      []HistogramPoint
}

// Classic-histogram wire-format suffixes and the `le` label, as the
// Prometheus exposition format defines them.
const (
	metricNameLabel   = "__name__"
	bucketSuffix      = "_bucket"
	countSuffix       = "_count"
	sumSuffix         = "_sum"
	leLabel           = "le"
	positiveInfinityL = "+Inf"
)

// PromSeries renders the classic histogram as the reference-Prometheus wire
// format demands: one cumulative `_bucket` series per explicit bound plus a
// `+Inf` bucket, one `_count`, one `_sum`.
func (h HistogramSeries) PromSeries() []Series {
	out := make([]Series, 0, len(h.Bounds)+3)
	for i, bound := range h.Bounds {
		out = append(out, Series{
			Labels: h.bucketLabels(formatBound(bound)),
			Samples: h.mapPoints(func(p HistogramPoint) float64 {
				return float64(cumulativeThrough(p.BucketCounts, i))
			}),
		})
	}
	out = append(out, Series{
		Labels:  h.bucketLabels(positiveInfinityL),
		Samples: h.mapPoints(func(p HistogramPoint) float64 { return float64(p.Count) }),
	})
	out = append(out, h.CountSeries().PromSeries(), h.SumSeries().PromSeries())
	return out
}

// CountSeries and SumSeries are the classic histogram's `_count` and `_sum`
// companions in plain metric-series shape. PromQL lowering routes a reference
// to either of them onto otel_metrics_histogram under the bare base name, so
// they are the handles a corpus query uses — and the handles a parity oracle
// has to reproduce.
func (h HistogramSeries) CountSeries() MetricSeries {
	return MetricSeries{
		MetricName:  h.MetricName + countSuffix,
		ServiceName: h.ServiceName,
		Attributes:  h.Attributes,
		Samples:     h.mapPoints(func(p HistogramPoint) float64 { return float64(p.Count) }),
	}
}

func (h HistogramSeries) SumSeries() MetricSeries {
	return MetricSeries{
		MetricName:  h.MetricName + sumSuffix,
		ServiceName: h.ServiceName,
		Attributes:  h.Attributes,
		Samples:     h.mapPoints(func(p HistogramPoint) float64 { return p.Sum }),
	}
}

func (h HistogramSeries) suffixLabels(suffix string) map[string]string {
	labels := make(map[string]string, len(h.Attributes)+1)
	for k, v := range h.Attributes {
		labels[k] = v
	}
	labels[metricNameLabel] = h.MetricName + suffix
	return labels
}

func (h HistogramSeries) bucketLabels(le string) map[string]string {
	labels := h.suffixLabels(bucketSuffix)
	labels[leLabel] = le
	return labels
}

func (h HistogramSeries) mapPoints(value func(HistogramPoint) float64) []Sample {
	out := make([]Sample, 0, len(h.Points))
	for _, p := range h.Points {
		out = append(out, Sample{Time: p.Time, Value: value(p)})
	}
	return out
}

// cumulativeThrough sums buckets 0..idx inclusive.
func cumulativeThrough(counts []uint64, idx int) uint64 {
	var total uint64
	for i := 0; i <= idx && i < len(counts); i++ {
		total += counts[i]
	}
	return total
}

// formatBound renders a bucket edge the way Prometheus's exposition format
// does, so the `le` label matches between the two sides byte-for-byte.
func formatBound(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}

// LogRecord is one fixture log line plus the fields both sides must carry
// identically: the level (structured metadata on the Loki side, LogAttributes
// plus SeverityText on the ClickHouse side) and the trace correlation ids.
type LogRecord struct {
	Time    time.Time
	Line    string
	Level   string
	TraceID string
	SpanID  string
}

// LogStream is one fixture log stream. Labels is simultaneously the Loki
// stream-label set and the ClickHouse `ResourceAttributes` map, so a LogQL
// matcher selects the same records on both sides with no naming bridge.
type LogStream struct {
	ServiceName string
	Format      string
	Labels      map[string]string
	Records     []LogRecord
}

// Fixture is the whole in-memory dataset: what gets written to ClickHouse and,
// unchanged, to the reference backends.
type Fixture struct {
	Window     Window
	Gauge      []MetricSeries
	Counter    []MetricSeries
	Histogram  []HistogramSeries
	LogStreams []LogStream
	Traces     []Trace
}

// Determinism devices. The fixture is byte-identical run to run apart from its
// offset from the epoch.
const (
	// seedValue seeds the one RNG the fixture draws from.
	seedValue = 42
	// gaugeCeiling bounds the gauge's drawn values. Values are drawn as whole
	// numbers so every later sum is exact in IEEE-754 and a parity comparison
	// needs no float tolerance.
	gaugeCeiling = 500
	// counterMaxIncrement bounds one step's counter increase.
	counterMaxIncrement = 20
	// histogramStepCount is how many observations a histogram series records
	// per step, split across buckets by histogramBucketWeights.
	histogramStepCount = 10
	// histogramMeanSeconds is the per-observation contribution to the
	// histogram's Sum. It is a negative power of two, so Sum stays exactly
	// representable and comparable without tolerance.
	histogramMeanSeconds = 0.25
	// spanMinMillis / spanJitterMillis bound a span's duration, so
	// duration-quantile queries have a non-degenerate distribution.
	spanMinMillis    = 5
	spanJitterMillis = 300
	// otelCumulativeTemporality is the OTLP AggregationTemporality enum value
	// for CUMULATIVE, which is what both the counter and the histogram are.
	otelCumulativeTemporality = int32(2)
)

// histogramBucketWeights splits each step's observations across the buckets.
// It must hold len(HistogramBounds)+1 entries and sum to histogramStepCount;
// Build enforces both rather than silently producing a histogram whose
// BucketCounts do not add up to Count.
var histogramBucketWeights = []uint64{4, 3, 2, 1}

// Label and attribute keys the fixture writes. Every one is shared by both
// sides of the diff.
const (
	serviceNameLabel   = "service_name"
	statusCodeLabel    = "http_status_code"
	jobLabel           = "job"
	formatLabel        = "format"
	detectedLevelKey   = "detected_level"
	traceIDLogKey      = "trace_id"
	otelServiceNameKey = "service.name"
	spanStatusAttr     = "http.status_code"
)

// Build produces the fixture from a declaration and a window. Every value it
// draws comes from one RNG seeded with seedValue and consumed in a fixed
// order, so two Builds with the same declaration produce byte-identical output
// apart from the timestamps the window carries.
func Build(d Declaration, w Window) (Fixture, error) {
	if len(histogramBucketWeights) != len(d.HistogramBounds)+1 {
		return Fixture{}, fmt.Errorf(
			"histogram bucket weights hold %d entries but the declaration has %d bounds (+1 implicit +Inf bucket)",
			len(histogramBucketWeights), len(d.HistogramBounds),
		)
	}
	var weightSum uint64
	for _, weight := range histogramBucketWeights {
		weightSum += weight
	}
	if weightSum != histogramStepCount {
		return Fixture{}, fmt.Errorf(
			"histogram bucket weights sum to %d, want %d; BucketCounts must add up to Count",
			weightSum, histogramStepCount,
		)
	}

	rng := rand.New(rand.NewSource(seedValue)) //nolint:gosec // fixture determinism, not cryptography
	times := w.SampleTimes()

	traces := buildTraces(d, w, rng)
	fixture := Fixture{
		Window:     w,
		Gauge:      buildGauge(d, times, rng),
		Counter:    buildCounter(d, times, rng),
		Histogram:  buildHistogram(d, times),
		LogStreams: buildLogStreams(d, times, traces),
		Traces:     traces,
	}
	return fixture, nil
}

// crossJoin materialises the declared service × status-code label sets. It is
// a real cross of both lists: deriving the second dimension from the first
// (`codes[i % len(codes)]`) would produce len(services) series where the
// declaration asks for len(services)*len(codes).
func crossJoin(d Declaration) []map[string]string {
	out := make([]map[string]string, 0, len(d.Services)*len(d.StatusCodes))
	for _, svc := range d.Services {
		for _, code := range d.StatusCodes {
			out = append(out, map[string]string{
				serviceNameLabel: svc,
				statusCodeLabel:  code,
			})
		}
	}
	return out
}

func buildGauge(d Declaration, times []time.Time, rng *rand.Rand) []MetricSeries {
	out := make([]MetricSeries, 0, len(d.Services)*len(d.StatusCodes))
	for _, attrs := range crossJoin(d) {
		samples := make([]Sample, 0, len(times))
		for _, t := range times {
			samples = append(samples, Sample{Time: t, Value: float64(rng.Intn(gaugeCeiling))})
		}
		out = append(out, MetricSeries{
			MetricName:  d.GaugeMetric,
			ServiceName: attrs[serviceNameLabel],
			Attributes:  attrs,
			Samples:     samples,
		})
	}
	return out
}

func buildCounter(d Declaration, times []time.Time, rng *rand.Rand) []MetricSeries {
	out := make([]MetricSeries, 0, len(d.Services)*len(d.StatusCodes))
	for _, attrs := range crossJoin(d) {
		samples := make([]Sample, 0, len(times))
		var total float64
		for _, t := range times {
			total += float64(rng.Intn(counterMaxIncrement))
			samples = append(samples, Sample{Time: t, Value: total})
		}
		out = append(out, MetricSeries{
			MetricName:  d.CounterMetric,
			ServiceName: attrs[serviceNameLabel],
			Attributes:  attrs,
			Samples:     samples,
		})
	}
	return out
}

// buildHistogram draws no random values: a classic histogram's Count, Sum and
// BucketCounts have to stay mutually consistent (Count == sum(BucketCounts),
// all three monotonically non-decreasing), so they are derived from the sample
// index instead.
func buildHistogram(d Declaration, times []time.Time) []HistogramSeries {
	out := make([]HistogramSeries, 0, len(d.Services)*len(d.StatusCodes))
	for _, attrs := range crossJoin(d) {
		points := make([]HistogramPoint, 0, len(times))
		for i, t := range times {
			observations := uint64(i + 1)
			buckets := make([]uint64, len(histogramBucketWeights))
			var count uint64
			for b, weight := range histogramBucketWeights {
				buckets[b] = weight * observations
				count += buckets[b]
			}
			points = append(points, HistogramPoint{
				Time:         t,
				Count:        count,
				Sum:          float64(count) * histogramMeanSeconds,
				BucketCounts: buckets,
			})
		}
		out = append(out, HistogramSeries{
			MetricName:  d.HistogramMetric,
			ServiceName: attrs[serviceNameLabel],
			Attributes:  attrs,
			Bounds:      d.HistogramBounds,
			Points:      points,
		})
	}
	return out
}

// buildTraces derives every identifier from a SHA-256 of (service, trace
// index, span index), so trace and span ids are byte-stable across runs
// without being drawn from the RNG — an id drawn from the shared RNG would
// shift the moment any earlier draw changed.
func buildTraces(d Declaration, w Window, rng *rand.Rand) []Trace {
	out := make([]Trace, 0, len(d.Services)*d.TracesPerService)
	// Traces are spread evenly across the seed window so a trace-search over
	// the fixture window returns all of them, and so TraceQL range windows see
	// a non-degenerate arrival distribution.
	spacing := SeedWindow / time.Duration(d.TracesPerService)
	kinds := []otlptrace.Span_SpanKind{
		// SPAN_KIND_UNSPECIFIED is pinned in the rotation on purpose: it maps
		// to the CH `Unspecified` literal, the one span kind an emitter is
		// most likely to drop on the floor.
		otlptrace.Span_SPAN_KIND_UNSPECIFIED,
		otlptrace.Span_SPAN_KIND_SERVER,
		otlptrace.Span_SPAN_KIND_CLIENT,
		otlptrace.Span_SPAN_KIND_INTERNAL,
		otlptrace.Span_SPAN_KIND_PRODUCER,
		otlptrace.Span_SPAN_KIND_CONSUMER,
	}
	statuses := []otlptrace.Status_StatusCode{
		otlptrace.Status_STATUS_CODE_OK,
		otlptrace.Status_STATUS_CODE_ERROR,
		otlptrace.Status_STATUS_CODE_UNSET,
	}

	for _, svc := range d.Services {
		for i := range d.TracesPerService {
			traceStart := w.SeedStart.Add(time.Duration(i) * spacing)
			spanCount := d.SpansPerTrace[i%len(d.SpansPerTrace)]
			traceID := deriveTraceID(svc, i)
			spans := make([]Span, 0, spanCount)
			var parent [8]byte
			for j := range spanCount {
				spanID := deriveSpanID(svc, i, j)
				start := traceStart.Add(time.Duration(j) * time.Millisecond * spanMinMillis)
				dur := time.Duration(spanMinMillis+rng.Intn(spanJitterMillis)) * time.Millisecond
				spans = append(spans, Span{
					TraceID:      traceID,
					SpanID:       spanID,
					ParentSpanID: parent,
					Name:         spanName(svc, j),
					Kind:         kinds[(i+j)%len(kinds)],
					StatusCode:   statuses[(i+j)%len(statuses)],
					Start:        start,
					End:          start.Add(dur),
					Attributes: map[string]string{
						spanStatusAttr:   d.StatusCodes[(i+j)%len(d.StatusCodes)],
						serviceNameLabel: svc,
					},
				})
				parent = spanID
			}
			out = append(out, Trace{ServiceName: svc, Spans: spans})
		}
	}
	return out
}

// spanNameStages are the span names a trace rotates through; the name of span
// j is stage j mod len(stages), prefixed by the service so a TraceQL name
// matcher can select one service's spans.
var spanNameStages = []string{"handle", "authorize", "query", "render", "publish"}

func spanName(service string, index int) string {
	return service + "." + spanNameStages[index%len(spanNameStages)]
}

func deriveTraceID(service string, index int) [16]byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("trace|%s|%d", service, index)))
	var id [16]byte
	copy(id[:], sum[:len(id)])
	return id
}

func deriveSpanID(service string, traceIndex, spanIndex int) [8]byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("span|%s|%d|%d", service, traceIndex, spanIndex)))
	var id [8]byte
	copy(id[:], sum[:len(id)])
	return id
}

// logLevels rotates the level a record carries. Every level is declared, never
// inferred: reference Loki's level discovery synthesises `detected_level` for
// a record that arrives without one, and a synthesised value on one side only
// would read as a divergence.
var logLevels = []string{"info", "warn", "error"}

// buildLogStreams crosses services with log formats. Every record carries a
// trace id drawn from its own service's trace set, so the cross-signal
// correlation the three-signal archetype asserts is present in the substrate
// itself rather than assumed.
func buildLogStreams(d Declaration, times []time.Time, traces []Trace) []LogStream {
	byService := map[string][]Trace{}
	for _, t := range traces {
		byService[t.ServiceName] = append(byService[t.ServiceName], t)
	}

	out := make([]LogStream, 0, len(d.Services)*len(d.LogFormats))
	for _, svc := range d.Services {
		serviceTraces := byService[svc]
		for _, format := range d.LogFormats {
			records := make([]LogRecord, 0, len(times))
			for i, t := range times {
				level := logLevels[i%len(logLevels)]
				code := d.StatusCodes[i%len(d.StatusCodes)]
				var traceID, spanID string
				if len(serviceTraces) > 0 {
					tr := serviceTraces[i%len(serviceTraces)]
					traceID = hex.EncodeToString(tr.Spans[0].TraceID[:])
					spanID = hex.EncodeToString(tr.Spans[0].SpanID[:])
				}
				records = append(records, LogRecord{
					Time:    t,
					Line:    renderLine(format, svc, level, code, traceID, i),
					Level:   level,
					TraceID: traceID,
					SpanID:  spanID,
				})
			}
			out = append(out, LogStream{
				ServiceName: svc,
				Format:      format,
				Labels: map[string]string{
					jobLabel:         d.LogJob,
					serviceNameLabel: svc,
					formatLabel:      format,
				},
				Records: records,
			})
		}
	}
	return out
}

// Unwrappable numeric fields every rendered line carries, so LogQL `unwrap`
// metric queries have something real to aggregate on either side.
const (
	// durationBaseMillis / durationSpreadMillis shape the `duration_ms` field.
	durationBaseMillis   = 12
	durationSpreadMillis = 7
	// bytesPerRecord scales the `bytes` field with the record index.
	bytesPerRecord = 128
)

// renderLine renders one record in the declared format. The formats are the
// three shapes a LogQL corpus parses with `json`, `logfmt` and `pattern`
// respectively; an unrecognised format is a declaration error surfaced as a
// line no parser accepts, which fails the parity comparison loudly.
func renderLine(format, service, level, statusCode, traceID string, index int) string {
	durationMS := durationBaseMillis + index%durationSpreadMillis
	bytesOut := (index + 1) * bytesPerRecord
	switch format {
	case "json":
		payload := map[string]any{
			"level":       level,
			"service":     service,
			"status":      statusCode,
			"duration_ms": durationMS,
			"bytes":       bytesOut,
			traceIDLogKey: traceID,
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			// json.Marshal of a map of strings and ints cannot fail; a change
			// that makes it fail must not silently emit an empty line.
			panic(fmt.Sprintf("render json log line: %v", err))
		}
		return string(encoded)
	case "logfmt":
		return fmt.Sprintf("level=%s service=%s status=%s duration_ms=%d bytes=%d %s=%s",
			level, service, statusCode, durationMS, bytesOut, traceIDLogKey, traceID)
	default:
		return fmt.Sprintf("%s %s served status %s in %d ms with %d bytes %s=%s",
			level, service, statusCode, durationMS, bytesOut, traceIDLogKey, traceID)
	}
}

// SortedTraceIDs returns the hex trace ids of every fixture trace for one
// service, sorted, so a search-result comparison is order-independent.
func (f Fixture) SortedTraceIDs(service string) []string {
	var out []string
	for _, t := range f.Traces {
		if t.ServiceName != service || len(t.Spans) == 0 {
			continue
		}
		out = append(out, hex.EncodeToString(t.Spans[0].TraceID[:]))
	}
	sort.Strings(out)
	return out
}

// ServiceTraces is one service's fixture traces, rendered the way reference
// Tempo returns them, sorted so a search-result comparison is order-
// independent.
type ServiceTraces struct {
	Service  string
	TraceIDs []string
}

// TraceIDsByService groups every fixture trace under the service that emitted
// it, in reference-Tempo wire rendering and in a stable service order. The
// settle gate compares a search result against this, so it waits for the exact
// set the push produced rather than for a payload-independent counter.
func (f Fixture) TraceIDsByService() []ServiceTraces {
	byService := map[string][]string{}
	for _, t := range f.Traces {
		if len(t.Spans) == 0 {
			continue
		}
		id := hex.EncodeToString(t.Spans[0].TraceID[:])
		byService[t.ServiceName] = append(byService[t.ServiceName], RenderTempoTraceID(id))
	}
	services := make([]string, 0, len(byService))
	for service := range byService {
		services = append(services, service)
	}
	sort.Strings(services)
	out := make([]ServiceTraces, 0, len(services))
	for _, service := range services {
		ids := byService[service]
		sort.Strings(ids)
		out = append(out, ServiceTraces{Service: service, TraceIDs: ids})
	}
	return out
}

// SpanCount is the total number of fixture spans across every trace.
func (f Fixture) SpanCount() int {
	var n int
	for _, t := range f.Traces {
		n += len(t.Spans)
	}
	return n
}
