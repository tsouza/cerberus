package property

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Dataset is the random data shape every property iteration starts with.
//
// The DDL is a multi-statement script (CREATE TABLE + INSERTs) the chDB
// helpers will replay against an ephemeral session. The Metrics / Logs /
// Traces mirrors are the same data in the in-memory shape the oracle reads —
// keeping them in sync with the DDL is the generator's responsibility.
//
// A generator populates exactly one of the typed mirrors: Metrics for the
// PromQL property test, Logs for the LogQL property test, Traces for the
// TraceQL property test. The other two stay nil. Run pivots on whichever of
// Metrics / Traces is populated; RunLogs always expects Logs.
type Dataset struct {
	// DDL is the multi-statement seed: `CREATE OR REPLACE TABLE …;
	// INSERT … VALUES …;`. The runner splits on top-level semicolons
	// before exec'ing.
	DDL string
	// Metrics is the in-memory mirror of a metrics dataset (otel_metrics_gauge).
	// Generator owns the invariant `Metrics ⇔ DDL`.
	Metrics *MetricsModel
	// Logs is the in-memory mirror of a logs dataset (otel_logs).
	// Generator owns the invariant `Logs ⇔ DDL`.
	Logs *LogsModel
	// Traces is the in-memory mirror of a traces dataset (otel_traces).
	// Generator owns the invariant `Traces ⇔ DDL`.
	Traces *TracesModel
}

// MetricsModel is the in-memory metrics mirror. It's intentionally tiny
// — the generator and the oracle both consume it directly, so there's
// no point in mirroring the full OTel CH schema.
type MetricsModel struct {
	Series []SeriesData
}

// SeriesData is one time series in the dataset.
type SeriesData struct {
	MetricName string
	// Labels are user-defined dimensions (job, instance, …). The
	// __name__ label is implied by MetricName and never appears here.
	Labels map[string]string
	Points []Point
}

// Point is one (timestamp, value) sample in a SeriesData. Histogram is
// an additive alternative payload: when set, the point represents an
// OTel native (exponential) histogram sample rather than a plain
// float, and Value is unused. A SeriesData mixing plain and histogram
// points isn't a shape any generator or fixture produces today.
type Point struct {
	// TimestampMs is unix milliseconds, matching Prometheus's internal
	// convention so the oracle's storage layer can consume it directly.
	TimestampMs int64
	Value       float64
	Histogram   *NativeHistogram
}

// NativeHistogram is an OTel/native exponential-histogram sample,
// mirroring the otel_metrics_exponential_histogram row shape (see
// internal/chsql/histogram_quantile_native.go for the bucket-walk
// semantics this feeds). ZeroThreshold isn't modeled: the default
// OTel-CH DDL doesn't persist it, so the zero bucket collapses to a
// point at 0 everywhere in cerberus, and this mirror follows suit.
type NativeHistogram struct {
	Count                uint64
	Sum                  float64
	Scale                int32
	ZeroCount            uint64
	PositiveOffset       int32
	PositiveBucketCounts []uint64
	NegativeOffset       int32
	NegativeBucketCounts []uint64
}

// LogsModel is the in-memory logs mirror. It holds the rows the
// generator inserted into otel_logs; the oracle reads each row's
// (resource attributes, line body) directly while the cerberus side
// re-reads them via SQL.
type LogsModel struct {
	Records []LogRecord
}

// LogRecord is one row in a LogsModel. ResourceAttributes carries the
// stream-identity labels (job, service_name, …); LogAttributes carries
// the structured-metadata map (the OTel-CH `LogAttributes` column —
// per-log-record level/severity keys cerberus's detected_level cascade
// reads); Body is the raw log line; SeverityText is the OTel
// SeverityText column; Timestamp is unix nanoseconds (DateTime64(9)
// precision in chDB).
type LogRecord struct {
	Body               string
	SeverityText       string
	ResourceAttributes map[string]string
	LogAttributes      map[string]string
	TimestampNanos     int64
}

// TracesModel is the in-memory traces mirror. It holds the rows the
// generator inserted into otel_traces; the oracle reads each span's
// identity, attributes, timing, and status directly, while cerberus
// re-reads the same rows via SQL.
type TracesModel struct {
	Spans []SpanRecord
}

// SpanRecord is one row in a TracesModel: one span, expressed as typed
// fields rather than folded into a MetricsModel.SeriesData under reserved
// string label keys. ParentSpanID is empty for a trace root and otherwise
// names another SpanRecord's SpanID within the same TraceID — see
// [ValidateTracesDataset] for the linkage invariant this must satisfy.
type SpanRecord struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	// ResourceAttributes carries resource-scope attributes (service.name,
	// cluster, …), matching the OTel-CH ResourceAttributes column.
	ResourceAttributes map[string]string
	// SpanAttributes carries span-scope attributes (http.method, …),
	// matching the OTel-CH SpanAttributes column.
	SpanAttributes map[string]string
	// TimestampMs is the span's start time, unix milliseconds (matching
	// Point.TimestampMs's convention).
	TimestampMs int64
	// DurationNs is the span's duration in nanoseconds, matching the
	// OTel-CH Duration column's unit.
	DurationNs int64
	// StatusCode is the CH-stored status literal (Ok / Error / Unset).
	StatusCode string
}

// NamesPresent returns the distinct span names in the dataset, sorted for
// determinism. The TraceQL query generator uses this so a `{ name = "…" }`
// filter can target a name the dataset actually carries.
func (m *TracesModel) NamesPresent() []string {
	if m == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, s := range m.Spans {
		seen[s.Name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// StreamLabelsPresent returns the union of all label names that appear
// on any record's ResourceAttributes map, sorted for determinism. Used
// by the LogQL query generator to bound matcher choices.
func (m *LogsModel) StreamLabelsPresent() map[string][]string {
	if m == nil {
		return nil
	}
	out := map[string]map[string]struct{}{}
	for _, r := range m.Records {
		for k, v := range r.ResourceAttributes {
			if _, ok := out[k]; !ok {
				out[k] = map[string]struct{}{}
			}
			out[k][v] = struct{}{}
		}
	}
	result := make(map[string][]string, len(out))
	for k, vs := range out {
		values := make([]string, 0, len(vs))
		for v := range vs {
			values = append(values, v)
		}
		sort.Strings(values)
		result[k] = values
	}
	return result
}

// BodyTokensPresent returns substrings that occur on at least one
// log line in the model, sorted for determinism. The LogQL query
// generator uses this so every `|= "literal"` filter has at least
// one record it could match.
func (m *LogsModel) BodyTokensPresent() []string {
	if m == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, r := range m.Records {
		for _, tok := range tokenizeBody(r.Body) {
			seen[tok] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for tok := range seen {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// tokenizeBody is a minimal whitespace tokeniser. It exists in this
// file (rather than in test/property/gen) so the model layer is
// self-contained for the BodyTokensPresent contract.
func tokenizeBody(body string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(body); i++ {
		atSpace := i == len(body) || body[i] == ' ' || body[i] == '\t'
		switch {
		case atSpace && start >= 0:
			out = append(out, body[start:i])
			start = -1
		case !atSpace && start < 0:
			start = i
		}
	}
	return out
}

// NamesPresent returns the distinct metric names in the dataset, sorted
// for determinism. The PromQL generator uses this so every generated
// query targets a metric the dataset actually carries.
func (m *MetricsModel) NamesPresent() []string {
	if m == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, s := range m.Series {
		seen[s.MetricName] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LabelsPresentFor returns the union of label sets for every series
// matching name. Used by the query generator to bound matcher choices.
func (m *MetricsModel) LabelsPresentFor(name string) map[string][]string {
	if m == nil {
		return nil
	}
	out := map[string]map[string]struct{}{}
	for _, s := range m.Series {
		if s.MetricName != name {
			continue
		}
		for k, v := range s.Labels {
			if _, ok := out[k]; !ok {
				out[k] = map[string]struct{}{}
			}
			out[k][v] = struct{}{}
		}
	}
	result := make(map[string][]string, len(out))
	for k, vs := range out {
		values := make([]string, 0, len(vs))
		for v := range vs {
			values = append(values, v)
		}
		sort.Strings(values)
		result[k] = values
	}
	return result
}

// Query is one randomly generated query. The framework keeps both the
// string (the form cerberus's HTTP entry point accepts) and the AST
// (the form the oracle and any debug logging consumes). The generator
// produces the AST first, then pretty-prints it via expr.String(); the
// two are guaranteed in lock-step by construction.
type Query struct {
	// ShapeID is the stable semantic-shape identifier selected by the
	// generator. It is deliberately independent of a generator's switch
	// order so adding or reordering shapes cannot silently relabel evidence.
	ShapeID ShapeID
	// String is the AST pretty-printed by parser.Expr.String(). Cerberus
	// re-parses it before lowering. The oracle's bridge re-parses it
	// inside Prometheus's engine as well.
	String string
	// EvalTs is the instant the query should be evaluated at, in unix
	// seconds. The generator chooses a timestamp from the dataset's
	// active window so the query has at least one matching sample to
	// see. Range queries are out of scope for PR 1 (instant only).
	EvalTs int64
}

// ShapeID is a stable, human-readable identifier for one generated semantic
// query shape. Generators publish exact rosters of these IDs and stamp every
// generated query with the selected member.
type ShapeID string

// ShapeExampleAttemptLimit bounds the stable oracle-guided search for a
// non-empty deterministic example. A roster case that cannot produce row
// evidence within this many seeds fails instead of becoming a hollow green.
const ShapeExampleAttemptLimit = 64

// ShapeExampleSeed derives a stable rapid.Example seed from a semantic shape
// ID. The seed does not depend on roster position, so reordering or inserting
// shapes cannot change another shape's deterministic enrollment case.
func ShapeExampleSeed(id ShapeID) int {
	const (
		fnvOffsetBasis   uint64 = 14695981039346656037
		fnvPrime         uint64 = 1099511628211
		shapeSeedModulus        = 2147483647
	)
	hash := fnvOffsetBasis
	for _, b := range []byte(id) {
		hash ^= uint64(b)
		hash *= fnvPrime
	}
	return int(hash % shapeSeedModulus)
}

// ShapeExampleAttemptSeed derives a stable seed for one bounded attempt. The
// first attempt preserves ShapeExampleSeed's original value; later attempts
// depend only on the shape ID and attempt number, never roster position.
func ShapeExampleAttemptSeed(id ShapeID, attempt int) int {
	if attempt == 0 {
		return ShapeExampleSeed(id)
	}
	return ShapeExampleSeed(ShapeID(fmt.Sprintf("%s#attempt-%d", id, attempt)))
}

// Outcome is the structured result of an oracle or cerberus invocation
// for one query. Empty Rows + nil Err means "no series matched" — a
// valid outcome both sides have to agree on.
type Outcome struct {
	// Rows is the result reshaped into shadow.VectorResult-friendly
	// form via the framework's CompareOutcomes helper. The generator
	// for PR 1 only produces instant queries, so each row is one
	// (label set, value) pair at EvalTs.
	Rows []OutcomeRow
	// Err is the error the side returned, if any. Both sides must
	// agree on err-vs-rows; a mismatch (e.g. oracle errs but cerberus
	// returns rows) is a real failure to report.
	Err error
}

// OutcomeRow is one labeled sample in an Outcome. Timestamp is unix
// milliseconds (matching Prom).
//
// The Value field is the canonical sample value for numeric outcomes
// (PromQL instant vectors, LogQL metric queries). Histogram carries the
// explicit Prometheus wire representation for native-histogram outcomes.
// The Line field
// carries log-stream content for LogQL log queries — both the oracle
// and cerberus must populate it for stream-shaped outcomes; the
// comparator's row matcher pairs entries by (label set, timestamp,
// line) so two rows with identical labels + ts but different lines
// won't collide. TraceID carries a TraceQL /api/search row's trace
// identity — both the oracle (test/property/oracle/traceql.Evaluate) and
// cerberus (traceql_test.go's runCerberusTraceQL) populate it from the
// same wire contract (Tempo's TraceSummary.TraceID), so
// CompareTraceIdentityOutcomes can catch a run that matched the wrong
// trace at the same row count — something the default label-keyed
// comparator, which never inspects TraceID, cannot.
//
// Exactly one of Histogram, Line, Value, or TraceID is the identity a row's
// family compares on. The comparator checks histogram structure and every
// numeric field before falling back to line or float comparison, so a
// histogram can never pass vacuously through Value's zero value;
// CompareTraceIdentityOutcomes is a separate, opt-in comparator (see
// CompareFn) that reads TraceID instead of Labels, so a TraceQL row never
// needs to smuggle its trace identity into the generic label map the way
// #3442 moved TraceQL's dataset model away from.
type OutcomeRow struct {
	Labels      map[string]string
	TimestampMs int64
	Value       float64
	Histogram   *Histogram
	Line        string
	TraceID     string
}

// Histogram is the decoded Prometheus HTTP representation of one native-
// histogram sample. Its buckets have explicit boundaries rather than the
// scale/offset representation used by the dataset and oracle internals,
// because explicit boundaries are the only histogram shape observable at
// the differential's HTTP seam.
type Histogram struct {
	Count   float64
	Sum     float64
	Buckets []HistogramBucket
}

// HistogramBucket is one populated native-histogram bucket in ascending
// value order. Boundaries uses Prometheus's integer vocabulary: 0 means
// lower-exclusive/upper-inclusive, 1 lower-inclusive/upper-exclusive, and
// 3 both-inclusive.
type HistogramBucket struct {
	Boundaries int
	Lower      float64
	Upper      float64
	Count      float64
}

// DatasetGen produces a random Dataset. Implementations should use
// rapid's Draw primitives so shrinking can minimise the dataset on
// failure.
type DatasetGen func(t *rapid.T) Dataset

// QueryGen produces a random Query targeting d. The generator's
// accept-set must match the oracle's accept-set — generating a query
// the oracle can't evaluate is a generator bug, not a cerberus bug.
type QueryGen func(t *rapid.T, d Dataset) Query

// OracleFn evaluates q against d using the independent specification
// (a from-scratch evaluator under oracle/, not delegating to the SUT).
type OracleFn func(d Dataset, q Query) Outcome

// CerberusFn runs the cerberus pipeline against the dataset (seeded
// into chDB on the caller side) and returns the same-shaped Outcome.
// Tests pass a closure that owns the chclienttest.Client + the mounted
// httptest.Server lifecycle.
type CerberusFn func(d Dataset, q Query) Outcome

// ShapeExampleFn builds the deterministic dataset/query pair for one exact
// roster member. The supplied seed is derived from the shape ID rather than
// its roster position.
type ShapeExampleFn[S ~string] func(shapeID S, seed int) (Dataset, Query)

// ShapeOutcomeFn evaluates one deterministic roster example inside its own
// subtest. The supplied *testing.T is the child created for that shape, so live
// clients, request contexts, and failures are all owned by the correct test.
type ShapeOutcomeFn func(t *testing.T, d Dataset, q Query) Outcome

// CompareFn is a family-specific successful-outcome comparator. Passed via
// Config.Compare (or RunShapeExamplesWithComparator), it replaces the
// default per-label-key row-count comparison (the unexported
// compareOutcomeRows) for a family whose outcome rows carry stronger,
// endpoint-specific identity evidence than a generic (labels, ts, value)
// triple can express — see CompareTraceIdentityOutcomes for the concrete
// TraceQL case.
//
// Compare only ever runs once the shared fail-closed error check has
// already passed both sides (see validateOutcomesWith) — an oracle or
// system error can never reach a family comparator, so no comparator can
// accidentally launder an error state into a pass.
type CompareFn func(oracle, system Outcome) string

// Config is a forward-looking knob bag. Compare is the one field the
// framework reads today — rapid's per-test iteration count is instead
// controlled via the `-rapid.checks=N` CLI flag (default 100), so a
// developer chasing a flake or running an overnight sweep cranks N up
// without touching the runner. The type stays exported so future
// fields (e.g., per-runner timeout, generator-specific knobs) can land
// without breaking the Run signature.
type Config struct {
	// Compare overrides the default row-count comparator Run uses once both
	// sides have produced a successful Outcome. Nil keeps the default
	// (label-keyed multiset row-count comparison) every non-TraceQL family
	// uses.
	Compare CompareFn
}

// ValidateGeneratedQuery rejects vacuous generator output before either side
// executes. A randomized property without a stable shape ID cannot contribute
// evidence to the exact roster, even when its query text happens to be valid.
func ValidateGeneratedQuery(query Query) string {
	var failures []string
	if strings.TrimSpace(string(query.ShapeID)) == "" {
		failures = append(failures, "missing shape ID")
	}
	if strings.TrimSpace(query.String) == "" {
		failures = append(failures, "empty query")
	}
	return strings.Join(failures, "; ")
}

// ValidateMetricsDataset and ValidateLogsDataset keep a generator defect from
// consuming a randomized check without executing either side of the fence.
func ValidateMetricsDataset(dataset Dataset) string {
	if dataset.Metrics == nil {
		return "missing metrics model"
	}
	if len(dataset.Metrics.Series) == 0 {
		return "empty metrics series"
	}
	if len(dataset.Metrics.NamesPresent()) == 0 {
		return "metrics dataset has no metric names"
	}
	hasSamplePoint := false
	for _, series := range dataset.Metrics.Series {
		if len(series.Points) > 0 {
			hasSamplePoint = true
			break
		}
	}
	if !hasSamplePoint {
		return "metrics dataset has no sample points"
	}
	if strings.TrimSpace(dataset.DDL) == "" {
		return "empty seed DDL"
	}
	return ""
}

func ValidateLogsDataset(dataset Dataset) string {
	if dataset.Logs == nil {
		return "missing logs model"
	}
	if len(dataset.Logs.Records) == 0 {
		return "empty log records"
	}
	if strings.TrimSpace(dataset.DDL) == "" {
		return "empty seed DDL"
	}
	return ""
}

// ValidateTracesDataset keeps a TraceQL generator defect from consuming a
// randomized check without executing either side of the fence. Beyond the
// DDL and non-empty checks its PromQL/LogQL siblings run, it validates the
// structural invariant every consumer (the oracle's structural-relation
// walk, the dataset-parent-chain generator test) depends on: every span has
// a trace/span identity, every non-root's ParentSpanID resolves to another
// span in the same trace, and every trace has exactly one root.
func ValidateTracesDataset(dataset Dataset) string {
	if dataset.Traces == nil {
		return "missing traces model"
	}
	if len(dataset.Traces.Spans) == 0 {
		return "empty traces spans"
	}
	for _, span := range dataset.Traces.Spans {
		if strings.TrimSpace(span.TraceID) == "" || strings.TrimSpace(span.SpanID) == "" {
			return fmt.Sprintf("span has a missing trace or span identity: trace=%q span=%q", span.TraceID, span.SpanID)
		}
	}
	if invalid := validateSpanParentLinkage(dataset.Traces.Spans); invalid != "" {
		return invalid
	}
	if strings.TrimSpace(dataset.DDL) == "" {
		return "empty seed DDL"
	}
	return ""
}

// validateSpanParentLinkage checks the structural precondition TraceQL's
// generator and the oracle's ancestor/descendant walk both depend on: an
// empty ParentSpanID marks a trace root, and any other value must name
// another span's SpanID within the SAME trace. A ParentSpanID that resolves
// nowhere, or a trace with zero or more than one root, is a generator bug —
// it produces structural queries the oracle cannot evaluate soundly.
func validateSpanParentLinkage(spans []SpanRecord) string {
	knownByTrace := make(map[string]map[string]struct{}, len(spans))
	for _, span := range spans {
		if knownByTrace[span.TraceID] == nil {
			knownByTrace[span.TraceID] = map[string]struct{}{}
		}
		knownByTrace[span.TraceID][span.SpanID] = struct{}{}
	}
	rootCounts := map[string]int{}
	for _, span := range spans {
		if span.ParentSpanID == "" {
			rootCounts[span.TraceID]++
			continue
		}
		if _, ok := knownByTrace[span.TraceID][span.ParentSpanID]; !ok {
			return fmt.Sprintf("span %q in trace %q references parent %q, which is not a span in the same trace",
				span.SpanID, span.TraceID, span.ParentSpanID)
		}
	}
	// Range over every trace ID that has a span at all, not just the ones
	// that show up in rootCounts: a trace whose spans form a pure cycle
	// (every ParentSpanID resolves within the trace, but none is empty)
	// has zero roots and would never earn a rootCounts entry, silently
	// passing a check that only inspected that map's keys.
	for traceID := range knownByTrace {
		if count := rootCounts[traceID]; count != 1 {
			return fmt.Sprintf("trace %q has %d roots (empty-ParentSpanID spans), want exactly one", traceID, count)
		}
	}
	return ""
}

// datasetModelFamily is one named, independently validatable model a
// Dataset may carry. ValidateGeneratedDataset enumerates these rather than
// hand-writing one pairwise check per combination, so a third (and now
// fourth) family only adds one entry here instead of a new branch for every
// existing family it can collide with.
type datasetModelFamily struct {
	name     string
	present  bool
	validate func() string
}

func datasetModelFamilies(dataset Dataset) []datasetModelFamily {
	return []datasetModelFamily{
		{name: "metrics", present: dataset.Metrics != nil, validate: func() string { return ValidateMetricsDataset(dataset) }},
		{name: "logs", present: dataset.Logs != nil, validate: func() string { return ValidateLogsDataset(dataset) }},
		{name: "traces", present: dataset.Traces != nil, validate: func() string { return ValidateTracesDataset(dataset) }},
	}
}

// ValidateGeneratedDataset selects the one model family a generated example
// carries — metrics XOR logs XOR traces, never more than one and never
// zero. Multiple populated models would make it ambiguous which oracle
// contract the example is proving; this is the shared seam [Run],
// [RunShapeCases], and [RunShapeExamples] all validate through, so every
// family (present or future) gets the same fail-closed exactly-one check.
func ValidateGeneratedDataset(dataset Dataset) string {
	var active []datasetModelFamily
	for _, family := range datasetModelFamilies(dataset) {
		if family.present {
			active = append(active, family)
		}
	}
	switch len(active) {
	case 0:
		return "missing metrics, logs, or traces model"
	case 1:
		return active[0].validate()
	default:
		names := make([]string, 0, len(active))
		for _, family := range active {
			names = append(names, family.name)
		}
		return fmt.Sprintf("dataset contains more than one model family: %s", strings.Join(names, ", "))
	}
}

func validateShapeRoster[S ~string](shapeIDs []S) string {
	if len(shapeIDs) == 0 {
		return "property shape roster is empty"
	}
	seen := make(map[ShapeID]struct{}, len(shapeIDs))
	for _, shapeID := range shapeIDs {
		propertyShapeID := ShapeID(shapeID)
		if strings.TrimSpace(string(propertyShapeID)) == "" {
			return "property shape roster contains an empty ID"
		}
		if _, duplicate := seen[propertyShapeID]; duplicate {
			return fmt.Sprintf("property shape roster contains duplicate ID %q", propertyShapeID)
		}
		seen[propertyShapeID] = struct{}{}
	}
	return ""
}

// RunShapeCases is the shared fail-closed orchestration seam for every
// deterministic property roster. It validates the complete roster before any
// example callback can run, then validates each example's dataset and stamped
// query identity before handing the payload to run.
//
// Q lets callers retain family-specific query geometry (for example a PromQL
// range grid or an instant-window case) without weakening the common dataset,
// roster, and query-shape contract.
func RunShapeCases[S ~string, Q any](
	t *testing.T,
	shapeIDs []S,
	example func(shapeID S, seed int) (Dataset, Q, ShapeID, string),
	run func(t *testing.T, dataset Dataset, generated Q),
) {
	t.Helper()
	if invalid := runShapeCases(t, shapeIDs, example, run); invalid != "" {
		t.Fatal(invalid)
	}
}

// runShapeCases keeps roster rejection observable without deliberately
// failing a parent test. Public runners turn its validation result into a
// fatal assertion; negative controls call this seam directly to prove no
// callback is reachable from an invalid roster.
func runShapeCases[S ~string, Q any](
	t *testing.T,
	shapeIDs []S,
	example func(shapeID S, seed int) (Dataset, Q, ShapeID, string),
	run func(t *testing.T, dataset Dataset, generated Q),
) string {
	t.Helper()
	if invalid := validateShapeRoster(shapeIDs); invalid != "" {
		return invalid
	}
	type preparedShapeCase struct {
		name      string
		dataset   Dataset
		generated Q
	}
	prepared := make([]preparedShapeCase, 0, len(shapeIDs))
	for _, shapeID := range shapeIDs {
		propertyShapeID := ShapeID(shapeID)
		dataset, generated, generatedShapeID, queryText := example(
			shapeID,
			ShapeExampleSeed(propertyShapeID),
		)
		if invalid := ValidateGeneratedDataset(dataset); invalid != "" {
			return fmt.Sprintf("shape %q generated an invalid dataset: %s", shapeID, invalid)
		}
		if generatedShapeID != propertyShapeID {
			return fmt.Sprintf("shape generator stamped %q, want %q", generatedShapeID, propertyShapeID)
		}
		if strings.TrimSpace(queryText) == "" {
			return fmt.Sprintf("shape %q generated an empty query", shapeID)
		}
		prepared = append(prepared, preparedShapeCase{
			name:      string(propertyShapeID),
			dataset:   dataset,
			generated: generated,
		})
	}
	for _, shapeCase := range prepared {
		shapeCase := shapeCase
		t.Run(shapeCase.name, func(t *testing.T) {
			run(t, shapeCase.dataset, shapeCase.generated)
		})
	}
	return ""
}

// RunShapeExamples executes exactly one deterministic live differential for
// every enrolled shape. This is the non-probabilistic floor beneath the rapid
// sweep: random iterations widen values and geometry, while this roster pass
// proves no semantic shape can disappear from a green run by chance.
func RunShapeExamples[S ~string](
	t *testing.T,
	shapeIDs []S,
	example ShapeExampleFn[S],
	oracle ShapeOutcomeFn,
	system ShapeOutcomeFn,
) {
	t.Helper()
	if invalid := runShapeExamples(t, shapeIDs, example, oracle, system, nil); invalid != "" {
		t.Fatal(invalid)
	}
}

// RunShapeExamplesWithComparator is RunShapeExamples's sibling for a family
// whose outcome rows carry evidence a generic label-keyed row count cannot
// interpret (see CompareFn) — TraceQL's TraceID-identity comparator is the
// motivating case. compare must be non-nil: a nil comparator would silently
// fall back to RunShapeExamples's default and mask the caller's explicit
// intent to use a stronger check.
func RunShapeExamplesWithComparator[S ~string](
	t *testing.T,
	shapeIDs []S,
	example ShapeExampleFn[S],
	oracle ShapeOutcomeFn,
	system ShapeOutcomeFn,
	compare CompareFn,
) {
	t.Helper()
	if compare == nil {
		t.Fatal("RunShapeExamplesWithComparator requires a non-nil comparator")
	}
	if invalid := runShapeExamples(t, shapeIDs, example, oracle, system, compare); invalid != "" {
		t.Fatal(invalid)
	}
}

func runShapeExamples[S ~string](
	t *testing.T,
	shapeIDs []S,
	example ShapeExampleFn[S],
	oracle ShapeOutcomeFn,
	system ShapeOutcomeFn,
	compare CompareFn,
) string {
	t.Helper()
	if invalid := validateShapeRoster(shapeIDs); invalid != "" {
		return invalid
	}
	if compare == nil {
		compare = compareOutcomeRows
	}
	for _, shapeID := range shapeIDs {
		shapeID := shapeID
		propertyShapeID := ShapeID(shapeID)
		t.Run(string(propertyShapeID), func(t *testing.T) {
			for attempt := range ShapeExampleAttemptLimit {
				dataset, query := example(shapeID, ShapeExampleAttemptSeed(propertyShapeID, attempt))
				if invalid := ValidateGeneratedDataset(dataset); invalid != "" {
					t.Fatalf("shape %q generated an invalid dataset: %s", shapeID, invalid)
				}
				if query.ShapeID != propertyShapeID {
					t.Fatalf("shape generator stamped %q, want %q", query.ShapeID, propertyShapeID)
				}
				if strings.TrimSpace(query.String) == "" {
					t.Fatalf("shape %q generated an empty query", shapeID)
				}

				oracleOutcome := oracle(t, dataset, query)
				if oracleOutcome.Err != nil {
					t.Fatalf("property shape %q oracle failed\n--- query ---\n%s\n--- dataset ---\n%s\n--- error ---\n%v",
						query.ShapeID, query.String, dumpDataset(dataset), oracleOutcome.Err)
				}
				if len(oracleOutcome.Rows) == 0 {
					continue
				}
				if diff := validateDeterministicOutcomesWith(oracleOutcome, system(t, dataset, query), compare); diff != "" {
					t.Fatalf("property shape %q failed\n--- query ---\n%s\n--- dataset ---\n%s\n--- diff ---\n%s",
						query.ShapeID, query.String, dumpDataset(dataset), diff)
				}
				return
			}
			t.Fatalf("property shape %q produced no oracle rows across %d stable attempts",
				propertyShapeID, ShapeExampleAttemptLimit)
		})
	}
	return ""
}

// Run is the top-level property runner. It walks the rapid.Check loop:
// each iteration draws a dataset and a query, evaluates both sides,
// and compares — on drift, calls t.Fatalf with enough context to
// reproduce. Shrinking is implicit (rapid will minimise the failing
// generators before this function returns).
//
// Run is shared by every non-log differential (PromQL's Metrics family and
// TraceQL's Traces family): ValidateGeneratedDataset accepts either, and
// rejects a draw that populates both or neither.
//
// The caller is responsible for closing over chDB / handler lifetime
// inside `ch` — the runner has no chDB knowledge of its own. This
// keeps the package free of chdb tags except in chdb.go.
//
// Iteration count is controlled by `-rapid.checks=N` (default 100).
// The deep CI lane overrides this to 500; local debug runs can pass
// `-rapid.checks=1000` for a wider sweep.
func Run(
	t *testing.T,
	cfg Config,
	dgen DatasetGen,
	qgen QueryGen,
	oracle OracleFn,
	ch CerberusFn,
) {
	t.Helper()

	compare := cfg.Compare
	if compare == nil {
		compare = compareOutcomeRows
	}

	rapid.Check(t, func(rt *rapid.T) {
		ds := dgen(rt)
		if invalid := ValidateGeneratedDataset(ds); invalid != "" {
			rt.Fatalf("invalid generated dataset: %s", invalid)
		}
		q := qgen(rt, ds)
		if invalid := ValidateGeneratedQuery(q); invalid != "" {
			rt.Fatalf("invalid generated query: %s", invalid)
		}

		oracleOut := oracle(ds, q)
		cerberusOut := ch(ds, q)

		if diff := validateOutcomesWith(oracleOut, cerberusOut, compare); diff != "" {
			rt.Fatalf("property drift\n--- query ---\n%s\nevalTs=%d\n--- dataset ---\n%s\n--- diff ---\n%s",
				q.String, q.EvalTs, dumpDataset(ds), diff)
		}
	})
}

// RunLogs is the LogQL equivalent of Run. It pivots on Dataset.Logs
// rather than Dataset.Metrics — the LogQL generator populates
// ds.Logs.Records and leaves ds.Metrics nil. Same shrinking contract
// applies; the rapid.Check loop minimises the failing (dataset,
// query) pair before reporting.
//
// Degenerate datasets and queries are harness failures: every counted
// iteration must execute both sides of the differential.
func RunLogs(
	t *testing.T,
	_ Config,
	dgen DatasetGen,
	qgen QueryGen,
	oracle OracleFn,
	ch CerberusFn,
) {
	t.Helper()

	rapid.Check(t, func(rt *rapid.T) {
		ds := dgen(rt)
		if invalid := ValidateLogsDataset(ds); invalid != "" {
			rt.Fatalf("invalid generated dataset: %s", invalid)
		}
		q := qgen(rt, ds)
		if invalid := ValidateGeneratedQuery(q); invalid != "" {
			rt.Fatalf("invalid generated query: %s", invalid)
		}

		oracleOut := oracle(ds, q)
		cerberusOut := ch(ds, q)

		if diff := ValidateOutcomes(oracleOut, cerberusOut); diff != "" {
			rt.Fatalf("property drift\n--- query ---\n%s\nevalTs=%d\n--- dataset ---\n%s\n--- diff ---\n%s",
				q.String, q.EvalTs, dumpDataset(ds), diff)
		}
	})
}

// ValidateOutcomes is the fail-closed property verdict. An oracle error is a
// harness failure and a system error is a product/substrate failure; neither
// can become a green agreement merely because both sides errored. When both
// sides produced rows, the verdict delegates to the value comparator.
//
// The function is pure so negative controls can prove every error state is
// rejected without booting chDB or an HTTP handler.
func ValidateOutcomes(oracle, system Outcome) string {
	return validateOutcomesWith(oracle, system, compareOutcomeRows)
}

// validateOutcomesWith is ValidateOutcomes parameterised over the successful-
// outcome comparator. The fail-closed error check runs unconditionally,
// before compare ever sees either side — see CompareFn's doc for why that
// ordering is load-bearing.
func validateOutcomesWith(oracle, system Outcome, compare CompareFn) string {
	var failures []string
	if oracle.Err != nil {
		failures = append(failures, fmt.Sprintf("oracle error: %v", oracle.Err))
	}
	if system.Err != nil {
		failures = append(failures, fmt.Sprintf("system error: %v", system.Err))
	}
	if len(failures) > 0 {
		return strings.Join(failures, "\n")
	}
	return compare(oracle, system)
}

// ValidateDeterministicOutcomes is the non-vacuity verdict for an enrolled
// roster case. Random properties may legitimately draw a query whose successful
// result is empty, but a deterministic floor must contribute observable row
// evidence on both sides. An empty agreement therefore fails after the normal
// fail-closed error and value comparison.
func ValidateDeterministicOutcomes(oracle, system Outcome) string {
	return validateDeterministicOutcomesWith(oracle, system, compareOutcomeRows)
}

// validateDeterministicOutcomesWith is ValidateDeterministicOutcomes
// parameterised over the successful-outcome comparator, mirroring
// validateOutcomesWith's relationship to ValidateOutcomes.
func validateDeterministicOutcomesWith(oracle, system Outcome, compare CompareFn) string {
	if diff := validateOutcomesWith(oracle, system, compare); diff != "" {
		return diff
	}
	if len(oracle.Rows) == 0 {
		return "deterministic property case produced no rows on either side"
	}
	return ""
}

// CompareOutcomes is the compatibility entry point used by the property and
// integration suites. It is intentionally fail-closed and therefore has the
// same verdict as [ValidateOutcomes].
func CompareOutcomes(want, got Outcome) string {
	return ValidateOutcomes(want, got)
}

// compareOutcomeRows returns "" when two successful outcomes agree and a
// multiline diff otherwise. The shape mirrors what shadow.Compare emits but
// is local to this package so the property test can render a failure without
// dragging shadow's VectorResult shape into the test code.
//
// Comparison is multiset-aware: row order doesn't matter, but every
// row on one side must have a same-(labels, ts, value) row on the
// other. Numeric tolerance follows shadow's defaults
// (abs=1e-9, rel=1e-9) so floating-point noise from a different
// evaluation order doesn't flag.
func compareOutcomeRows(want, got Outcome) string {
	wantIdx := indexOutcomeRows(want.Rows)
	gotIdx := indexOutcomeRows(got.Rows)

	var diff strings.Builder
	for key, ws := range wantIdx {
		gs, ok := gotIdx[key]
		if !ok {
			fmt.Fprintf(&diff, "missing series in got: %s\n", key)
			continue
		}
		if len(ws) != len(gs) {
			fmt.Fprintf(&diff, "series %s: sample count want=%d got=%d\n",
				key, len(ws), len(gs))
			continue
		}
		// Each series's sample list was sorted by indexOutcomeRows.
		for i := range ws {
			if ws[i].TimestampMs != gs[i].TimestampMs {
				fmt.Fprintf(&diff, "series %s: ts[%d] want=%d got=%d\n",
					key, i, ws[i].TimestampMs, gs[i].TimestampMs)
				continue
			}
			if ws[i].Histogram != nil || gs[i].Histogram != nil {
				if histDiff := compareHistograms(ws[i].Histogram, gs[i].Histogram); histDiff != "" {
					fmt.Fprintf(&diff, "series %s: histogram[%d] @ts=%d %s\n",
						key, i, ws[i].TimestampMs, histDiff)
				}
				continue
			}
			// Stream rows (Line non-empty on either side) check the
			// line content byte-for-byte; numeric rows fall through to
			// the float tolerance check. The two paths are mutually
			// exclusive — stream outcomes leave Value=0 and numeric
			// outcomes leave Line="".
			if ws[i].Line != "" || gs[i].Line != "" {
				if ws[i].Line != gs[i].Line {
					fmt.Fprintf(&diff, "series %s: line[%d] @ts=%d want=%q got=%q\n",
						key, i, ws[i].TimestampMs, ws[i].Line, gs[i].Line)
				}
				continue
			}
			if !valuesClose(ws[i].Value, gs[i].Value) {
				fmt.Fprintf(&diff, "series %s: value[%d] @ts=%d want=%g got=%g\n",
					key, i, ws[i].TimestampMs, ws[i].Value, gs[i].Value)
			}
		}
	}
	for key := range gotIdx {
		if _, ok := wantIdx[key]; !ok {
			fmt.Fprintf(&diff, "extra series in got: %s\n", key)
		}
	}

	return diff.String()
}

// CompareTraceIdentityOutcomes is the TraceQL-specific CompareFn plugged in
// through Config.Compare / RunShapeExamplesWithComparator. It replaces the
// default per-label-key ROW COUNT comparison with a per-TraceID multiset
// comparison, so two outcomes with the same total row count but a
// substituted, missing, duplicated, or scope-swapped trace identity are
// caught — something an empty-label row count can never distinguish (the
// gap TRACEQL-PROPERTY-EVIDENCE-TRACE-IDENTITY's evidence closes).
//
// Every TraceQL /api/search projection this repo generates — selector,
// structural, select(), and trace-scoped count()/avg|min|max|sum(duration)
// pipeline rows alike — carries a real TraceID (see
// test/property/oracle/traceql.Evaluate and traceql_test.go's
// runCerberusTraceQL); an empty TraceID on either side is therefore a
// comparator-usage bug, not a legitimate "no identity" outcome, and is
// reported as a mismatch rather than silently grouped under "".
func CompareTraceIdentityOutcomes(oracle, system Outcome) string {
	var diag strings.Builder
	if invalid := invalidTraceIDRows(oracle.Rows); invalid != "" {
		fmt.Fprintf(&diag, "oracle: %s\n", invalid)
	}
	if invalid := invalidTraceIDRows(system.Rows); invalid != "" {
		fmt.Fprintf(&diag, "system: %s\n", invalid)
	}
	if diag.Len() > 0 {
		return diag.String()
	}

	wantCounts := traceIDCounts(oracle.Rows)
	gotCounts := traceIDCounts(system.Rows)

	ids := make(map[string]struct{}, len(wantCounts)+len(gotCounts))
	for id := range wantCounts {
		ids[id] = struct{}{}
	}
	for id := range gotCounts {
		ids[id] = struct{}{}
	}
	sortedIDs := make([]string, 0, len(ids))
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	var diff strings.Builder
	for _, id := range sortedIDs {
		want, got := wantCounts[id], gotCounts[id]
		switch {
		case want == 0:
			fmt.Fprintf(&diff, "extra trace in system: %s (matched-span count=%d)\n", id, got)
		case got == 0:
			fmt.Fprintf(&diff, "missing trace in system: %s (want matched-span count=%d)\n", id, want)
		case want != got:
			fmt.Fprintf(&diff, "trace %s: matched-span count want=%d got=%d\n", id, want, got)
		}
	}
	return diff.String()
}

// invalidTraceIDRows reports the first row (if any) whose TraceID is empty
// — every TraceQL outcome this comparator is used for stamps a real
// dataset-generated TraceID on every row, so an empty one means the caller
// wired CompareTraceIdentityOutcomes to an Outcome that was never built for
// it (see CompareTraceIdentityOutcomes's doc).
func invalidTraceIDRows(rows []OutcomeRow) string {
	for i, r := range rows {
		if strings.TrimSpace(r.TraceID) == "" {
			return fmt.Sprintf("row[%d] has an empty TraceID", i)
		}
	}
	return ""
}

// traceIDCounts multiset-counts rows by TraceID. A selector/structural/
// select() shape's rows carry one entry per matched span (so a trace with
// N matching spans counts N times); a trace-scoped aggregate pipeline shape
// carries exactly one row per satisfying trace (see oracle/traceql.Evaluate
// and runCerberusTraceQL's traceIdentityRows) — the same counting logic
// serves both, since the row-emission side is what encodes the distinction.
func traceIDCounts(rows []OutcomeRow) map[string]int {
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.TraceID]++
	}
	return out
}

func compareHistograms(want, got *Histogram) string {
	if want == nil || got == nil {
		return fmt.Sprintf("shape mismatch: want=%v got=%v", want != nil, got != nil)
	}
	if !valuesClose(want.Count, got.Count) {
		return fmt.Sprintf("count want=%g got=%g", want.Count, got.Count)
	}
	if !valuesClose(want.Sum, got.Sum) {
		return fmt.Sprintf("sum want=%g got=%g", want.Sum, got.Sum)
	}
	if len(want.Buckets) != len(got.Buckets) {
		return fmt.Sprintf("bucket count want=%d got=%d", len(want.Buckets), len(got.Buckets))
	}
	for i := range want.Buckets {
		w, g := want.Buckets[i], got.Buckets[i]
		if w.Boundaries != g.Boundaries {
			return fmt.Sprintf("bucket[%d] boundaries want=%d got=%d", i, w.Boundaries, g.Boundaries)
		}
		if !valuesClose(w.Lower, g.Lower) || !valuesClose(w.Upper, g.Upper) || !valuesClose(w.Count, g.Count) {
			return fmt.Sprintf("bucket[%d] want=[%g,%g]=%g got=[%g,%g]=%g",
				i, w.Lower, w.Upper, w.Count, g.Lower, g.Upper, g.Count)
		}
	}
	return ""
}

func indexOutcomeRows(rows []OutcomeRow) map[string][]OutcomeRow {
	out := map[string][]OutcomeRow{}
	for _, r := range rows {
		key := labelKey(r.Labels)
		out[key] = append(out[key], r)
	}
	for _, samples := range out {
		sort.Slice(samples, func(i, j int) bool {
			// Sort by timestamp first, then line content for stream
			// outcomes (so two rows with the same ts but different
			// lines compare slot-for-slot rather than colliding
			// arbitrarily on map iteration order).
			if samples[i].TimestampMs != samples[j].TimestampMs {
				return samples[i].TimestampMs < samples[j].TimestampMs
			}
			return samples[i].Line < samples[j].Line
		})
	}
	return out
}

// labelKey is the stable string-form of a label set. Lifted in spirit
// from shadow/differ.go's labelKey so the comparator emits the same
// "{job=\"api\",instance=\"a\"}" notation a Prom user would recognise.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString("=\"")
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// valuesClose returns whether two float64 values match within the
// shadow harness's tolerances. The bridge oracle and cerberus take
// different paths through float arithmetic; a strict == would flake
// on small rounding noise.
func valuesClose(a, b float64) bool {
	const (
		absEpsilon = 1e-9
		relEpsilon = 1e-9
	)
	// IsNaN handling: PromQL gives NaN for division-by-zero and a few
	// other arithmetic shapes. Two NaNs are equal for our purposes.
	if a != a && b != b { // both NaN
		return true
	}
	// Infinity handling: PromQL produces +Inf / -Inf for x/0 and the
	// histogram_quantile phi-out-of-range cases. Two same-signed
	// infinities are equal; the subtraction below would otherwise yield
	// NaN (Inf-Inf) and flag a spurious mismatch.
	if isInf(a) || isInf(b) {
		return a == b
	}
	delta := a - b
	if delta < 0 {
		delta = -delta
	}
	if delta <= absEpsilon {
		return true
	}
	scale := a
	if scale < 0 {
		scale = -scale
	}
	if b > scale || -b > scale {
		scale = b
		if scale < 0 {
			scale = -scale
		}
	}
	return delta <= relEpsilon*scale
}

// isInf reports whether v is +Inf or -Inf. Defined locally so the
// comparator doesn't pull in math just for the one check.
func isInf(v float64) bool {
	return v > 1.7976931348623157e308 || v < -1.7976931348623157e308
}

// dumpDataset renders the dataset for a failure log. Compact enough
// for a single test failure to be greppable; verbose enough that the
// reader can reconstruct what the generator produced.
func dumpDataset(d Dataset) string {
	var b strings.Builder
	if d.Metrics != nil {
		fmt.Fprintf(&b, "series=%d\n", len(d.Metrics.Series))
		for _, s := range d.Metrics.Series {
			fmt.Fprintf(&b, "  %s%s points=%d\n", s.MetricName, labelKey(s.Labels), len(s.Points))
			// A native-histogram point's whole payload is its bucket
			// layout, so a count of points says nothing about what the
			// shrinker minimised to. Spell each one out; float points
			// stay summarised by the count above.
			for _, p := range s.Points {
				if p.Histogram == nil {
					continue
				}
				fmt.Fprintf(&b, "    ts=%d %s\n", p.TimestampMs, dumpNativeHistogram(p.Histogram))
			}
		}
		return b.String()
	}
	if d.Logs != nil {
		fmt.Fprintf(&b, "records=%d\n", len(d.Logs.Records))
		for _, r := range d.Logs.Records {
			fmt.Fprintf(&b, "  ts=%d %s severity=%q body=%q\n",
				r.TimestampNanos, labelKey(r.ResourceAttributes), r.SeverityText, r.Body)
		}
		return b.String()
	}
	if d.Traces != nil {
		fmt.Fprintf(&b, "spans=%d\n", len(d.Traces.Spans))
		for _, s := range d.Traces.Spans {
			fmt.Fprintf(&b, "  trace=%s span=%s parent=%q %s resource=%s span_attrs=%s status=%s duration=%dns ts=%d\n",
				s.TraceID, s.SpanID, s.ParentSpanID, s.Name,
				labelKey(s.ResourceAttributes), labelKey(s.SpanAttributes), s.StatusCode, s.DurationNs, s.TimestampMs)
		}
		return b.String()
	}
	return "(empty dataset)"
}

// dumpNativeHistogram renders one exponential-histogram payload on a
// single line, in the same field order as
// [NativeHistogram]'s declaration, so a failing property test's log is
// enough to reconstruct the sample by hand and re-derive the expected
// answer.
func dumpNativeHistogram(h *NativeHistogram) string {
	return fmt.Sprintf("count=%d sum=%g scale=%d zeroCount=%d pos=%+d%v neg=%+d%v",
		h.Count, h.Sum, h.Scale, h.ZeroCount,
		h.PositiveOffset, h.PositiveBucketCounts,
		h.NegativeOffset, h.NegativeBucketCounts)
}
