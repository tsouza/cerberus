package gen

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// TraceQLServicePool is the fixed service.name pool the TraceQL
// dataset draws from. Small + overlapping so generator iterations
// produce spans whose `{ resource.service.name = "<X>" }` selector
// matches multiple spans (predictable count() arithmetic).
var TraceQLServicePool = []string{"api", "web", "batch"}

// TraceQLClusterPool is the resource-attribute pool for "cluster" — a
// second resource attribute alongside service.name, used to exercise
// attribute matchers beyond resource.service.name.
var TraceQLClusterPool = []string{"east", "west"}

// TraceQLHTTPMethodPool is the span-attribute pool for "http.method",
// used to exercise span-scope attribute matchers (`span.http.method`).
// Deliberately a different pool than TraceQLSpanNamePool so a
// coincidental value match between the two doesn't mask a real bug.
var TraceQLHTTPMethodPool = []string{"GET", "POST", "PUT"}

// TraceQLStatusPool is the intrinsic status pool, in the CH-stored
// (title-case) form. TraceQL query literals are lowercase
// (ok/error/unset); statusQueryLiteral bridges the two.
var TraceQLStatusPool = []string{"Ok", "Error", "Unset"}

// TraceQLDurationPoolNs is the fixed duration-bucket pool spans draw
// from, in nanoseconds. Multiple distinct buckets give the duration
// intrinsic and avg|min|max|sum(duration) filters real spread to
// discriminate on, rather than every span sharing one fixed value.
var TraceQLDurationPoolNs = []int64{10_000_000, 50_000_000, 120_000_000, 300_000_000}

// TraceQLDurationThresholdPoolMs is the pool of millisecond thresholds
// the query generator draws for duration comparisons (`duration OP
// Nms`, `avg(duration) OP Nms`, …). Includes values that land exactly
// on a TraceQLDurationPoolNs bucket and values that fall strictly
// between buckets, so the boundary case is exercised too.
var TraceQLDurationThresholdPoolMs = []int64{10, 50, 75, 120, 200, 300}

// traceQLComparisonOps is the scalar-comparison operator pool shared by
// every numeric filter the generator draws (count(), duration, and the
// avg|min|max|sum(duration) metrics).
var traceQLComparisonOps = []string{">", ">=", "<", "<=", "="}

// traceQLEqualityOps is the equality/inequality operator pool for
// intrinsics that only support (in)equality comparison (status).
var traceQLEqualityOps = []string{"=", "!="}

// TraceQLSpanNamePool is the fixed span-name pool. Each generated
// span draws a name from this pool plus a per-span ordinal so the
// (SpanName, Timestamp) pair is unique across the dataset (Tempo's
// /api/search collapses TraceSummary rows that share name+timestamp;
// the generator avoids that collapse by stamping a unique suffix on
// each span name).
var TraceQLSpanNamePool = []string{"GET", "POST", "PUT", "DELETE"}

// SpansTableName is the OTel-CH default traces table the DDL targets.
// Matches schema.DefaultOTelTraces().SpansTable.
const SpansTableName = "otel_traces"

// traceQLAnchor is the wall-clock baseline the dataset anchors span
// timestamps to. Picked far enough in the future to avoid colliding
// with any wall-clock assertion in the chDB seeds (same convention as
// the metrics generator's anchorTime).
var traceQLAnchor = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// TraceQLAnchorTime returns the fixed wall-clock baseline the dataset
// generator anchors span timestamps to.
func TraceQLAnchorTime() time.Time { return traceQLAnchor }

// traceQLRootParentID is the all-zero ParentSpanId literal the OTel-CH
// schema uses to mark a root span (no parent).
const traceQLRootParentID = "0000000000000000"

// traceQLMaxTraces / traceQLMaxChainDepth bound the dataset's trace
// count and per-trace parent-chain depth. A linear chain (rather than
// a branching tree) is enough to exercise both structural operators
// the generator draws — `>` (immediate child) and `>>` (descendant at
// any depth) — while keeping ancestor/descendant computation in the
// oracle a simple walk up a unique parent chain, no general tree
// algorithm required.
const (
	traceQLMaxTraces     = 3
	traceQLMaxChainDepth = 3
)

// TraceQLDataset returns a rapid generator that draws a random
// property.Dataset of OTel-CH traces rows.
//
//   - 1–3 traces, each a linear parent→child chain of 1–3 spans (root
//     first). The chain lets structural-operator queries (`>`, `>>`)
//     exercise a real ancestor/descendant relationship instead of
//     every span being trace-root.
//   - Each span carries a resource.service.name (TraceQLServicePool),
//     a second resource attribute resource.cluster (TraceQLClusterPool),
//     and a span attribute span.http.method (TraceQLHTTPMethodPool) —
//     the pool beyond service.name attribute matchers exercise.
//   - Span names draw from TraceQLSpanNamePool plus a per-span ordinal
//     suffix so (SpanName, Timestamp) pairs are unique across the
//     dataset — Tempo's /api/search collapses traces by name+ts, and
//     the property test wants one TraceSummary per span so the count
//     comparator is exact.
//   - Per-span StartTime is anchor + i*1s (i = the global span
//     ordinal, unique across every trace); Duration and StatusCode
//     draw from TraceQLDurationPoolNs / TraceQLStatusPool so the
//     duration/status intrinsic filters have real spread to
//     discriminate on.
//
// The returned Dataset's DDL is a multi-statement script
// (`CREATE OR REPLACE TABLE otel_traces (...); INSERT ...;`) the chDB
// runner replays before each query. The MetricsModel mirror carries
// the same data in a shape the oracle reads — it stores spans as
// SeriesData entries with MetricName=SpanName, Labels carrying the
// span-level + resource attributes via reserved key prefixes:
//
//   - "resource.<key>"  → ResourceAttributes[<key>] (service.name, cluster)
//   - "span.<key>"      → SpanAttributes[<key>] (http.method)
//   - "__name__"        → SpanName (mirrors the PromQL convention; the
//     oracle's labels-minus-__name__ identity rule reuses it)
//   - "__traceID__"     → TraceID (shared by every span in a trace's chain)
//   - "__spanID__"      → SpanID (unique 16-hex per span)
//   - "__parentSpanID__" → ParentSpanId (unique per chain: the
//     preceding span's SpanID, or traceQLRootParentID for the chain root)
//   - "__duration_ns__" → string-formatted Duration (so the oracle
//     can read it without changing SeriesData's float64 Point shape)
//   - "__status__"      → the CH-stored status literal (Ok/Error/Unset)
//
// MergeTree is the chosen engine (matches PromQL property test
// rationale: Memory engine refuses PREWHERE the cerberus optimizer
// emits).
func TraceQLDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numTraces := rapid.IntRange(1, traceQLMaxTraces).Draw(t, "numTraces")
		var spans []traceQLSpan
		spanOrdinal := 0
		for ti := 0; ti < numTraces; ti++ {
			traceID := deterministicTraceID(ti, 0xa1)
			chainDepth := rapid.IntRange(1, traceQLMaxChainDepth).Draw(t, fmt.Sprintf("chainDepth_%d", ti))
			parentID := traceQLRootParentID
			for ci := 0; ci < chainDepth; ci++ {
				service := rapid.SampledFrom(TraceQLServicePool).Draw(t, fmt.Sprintf("service_%d_%d", ti, ci))
				cluster := rapid.SampledFrom(TraceQLClusterPool).Draw(t, fmt.Sprintf("cluster_%d_%d", ti, ci))
				httpMethod := rapid.SampledFrom(TraceQLHTTPMethodPool).Draw(t, fmt.Sprintf("httpMethod_%d_%d", ti, ci))
				baseName := rapid.SampledFrom(TraceQLSpanNamePool).Draw(t, fmt.Sprintf("spanName_%d_%d", ti, ci))
				status := rapid.SampledFrom(TraceQLStatusPool).Draw(t, fmt.Sprintf("status_%d_%d", ti, ci))
				durationNs := rapid.SampledFrom(TraceQLDurationPoolNs).Draw(t, fmt.Sprintf("duration_%d_%d", ti, ci))
				// Unique suffix → unique (SpanName, Timestamp) across spans:
				// Tempo's toTraceSummaries() keys by name+timestamp and collapses
				// duplicates. Suffixing the global span ordinal is the cheapest way
				// to guarantee uniqueness without adding a second random draw.
				name := fmt.Sprintf("%s /api/%d", baseName, spanOrdinal)
				spanID := deterministicSpanID(spanOrdinal, 0xb2)
				spans = append(spans, traceQLSpan{
					traceID:    traceID,
					spanID:     spanID,
					parentID:   parentID,
					service:    service,
					cluster:    cluster,
					httpMethod: httpMethod,
					name:       name,
					startTime:  traceQLAnchor.Add(time.Duration(spanOrdinal) * time.Second),
					durationNs: durationNs,
					statusCode: status,
				})
				parentID = spanID
				spanOrdinal++
			}
		}
		series := traceQLSpansToSeries(spans)
		return property.Dataset{
			DDL:     renderTraceQLDDL(spans),
			Metrics: &property.MetricsModel{Series: series},
		}
	})
}

// traceQLSpan is the in-memory mirror of one row of otel_traces the
// generator emits. The Dataset.Metrics mirror stores spans as
// SeriesData entries; this struct is the bridge.
type traceQLSpan struct {
	traceID    string // 32-hex, shared by every span in a trace's chain
	spanID     string // 16-hex
	parentID   string // 16-hex (traceQLRootParentID for a chain root)
	service    string
	cluster    string
	httpMethod string
	name       string
	startTime  time.Time
	durationNs int64
	statusCode string
}

// traceQLSpansToSeries pivots the generator's spans into the
// property.SeriesData shape the framework persists on the Dataset.
// Each span becomes one SeriesData with a single Point at the span's
// StartTime carrying its Duration as the float value.
//
// The Labels map intentionally folds span-level metadata under
// reserved keys (see TraceQLDataset's doc) so the oracle can recover
// the original schema without needing a new framework type.
func traceQLSpansToSeries(spans []traceQLSpan) []property.SeriesData {
	out := make([]property.SeriesData, 0, len(spans))
	for _, s := range spans {
		labels := map[string]string{
			"resource.service.name": s.service,
			"resource.cluster":      s.cluster,
			"span.http.method":      s.httpMethod,
			"__name__":              s.name,
			"__traceID__":           s.traceID,
			"__spanID__":            s.spanID,
			"__parentSpanID__":      s.parentID,
			"__status__":            s.statusCode,
			"__duration_ns__":       fmt.Sprintf("%d", s.durationNs),
		}
		out = append(out, property.SeriesData{
			MetricName: s.name,
			Labels:     labels,
			Points: []property.Point{
				{TimestampMs: s.startTime.UnixMilli(), Value: float64(s.durationNs)},
			},
		})
	}
	return out
}

// renderTraceQLDDL produces the multi-statement seed script for an
// otel_traces table. The schema mirrors the OTel-CH traces table
// (the subset the generator + oracle care about) — TraceId, SpanId,
// ParentSpanId, ServiceName (kept for OTel parity; the generator
// queries via ResourceAttributes), SpanName, ResourceAttributes,
// SpanAttributes, StartTimeUnixNano (column name `Timestamp` per
// OTel-CH default), DurationNs (column name `Duration`), StatusCode,
// ScopeName, ScopeVersion — the last two are unvaried (always empty
// string) but must exist: schema.DefaultOTelTraces() names them, and
// the structural-join (`>`/`>>`) lowering projects them unconditionally
// (see the ScopeName/ScopeVersion comment in renderTraceQLRow).
//
// `CREATE OR REPLACE TABLE` keeps re-runs idempotent (chdb-go shares
// one catalog across sessions).
func renderTraceQLDDL(spans []traceQLSpan) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(SpansTableName)
	b.WriteString(` (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(String, String),
    SpanAttributes Map(String, String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String
) ENGINE = MergeTree ORDER BY (Timestamp, TraceId);
`)
	if len(spans) == 0 {
		return b.String()
	}
	b.WriteString(`INSERT INTO `)
	b.WriteString(SpansTableName)
	b.WriteString(` VALUES `)
	for i, s := range spans {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(renderTraceQLRow(s))
	}
	b.WriteString(";\n")
	return b.String()
}

// renderTraceQLRow renders one row literal for the INSERT script. The
// column order matches renderTraceQLDDL's CREATE TABLE.
func renderTraceQLRow(s traceQLSpan) string {
	var b strings.Builder
	b.WriteByte('(')
	// Timestamp
	b.WriteString("toDateTime64('")
	b.WriteString(s.startTime.UTC().Format("2006-01-02 15:04:05.000"))
	b.WriteString("', 9)")
	b.WriteString(", ")
	// TraceId / SpanId / ParentSpanId
	b.WriteString(quoteSQL(s.traceID))
	b.WriteString(", ")
	b.WriteString(quoteSQL(s.spanID))
	b.WriteString(", ")
	b.WriteString(quoteSQL(s.parentID))
	b.WriteString(", ")
	// SpanName
	b.WriteString(quoteSQL(s.name))
	b.WriteString(", ")
	// SpanKind (fixed "Internal" — generator doesn't vary this today)
	b.WriteString("'Internal'")
	b.WriteString(", ")
	// ServiceName (mirrors ResourceAttributes['service.name'])
	b.WriteString(quoteSQL(s.service))
	b.WriteString(", ")
	// ResourceAttributes
	b.WriteString(renderTraceQLMap(map[string]string{
		"service.name": s.service,
		"cluster":      s.cluster,
	}))
	b.WriteString(", ")
	// SpanAttributes
	b.WriteString(renderTraceQLMap(map[string]string{"http.method": s.httpMethod}))
	b.WriteString(", ")
	// Duration
	fmt.Fprintf(&b, "%d", s.durationNs)
	b.WriteString(", ")
	// StatusCode
	b.WriteString(quoteSQL(s.statusCode))
	b.WriteString(", ")
	// StatusMessage
	b.WriteString("''")
	b.WriteString(", ")
	// ScopeName / ScopeVersion — the generator doesn't vary instrumentation
	// scope, but the columns must exist: structuralExtraProjectionColumns
	// (internal/traceql/lower.go) hard-codes schema.Traces.ScopeNameColumn /
	// ScopeVersionColumn into every structural-join (`>`/`>>`) wrap
	// projection, so a table missing them 502s with UNKNOWN_IDENTIFIER the
	// moment a structural query runs.
	b.WriteString("''")
	b.WriteString(", ")
	b.WriteString("''")
	b.WriteByte(')')
	return b.String()
}

// renderTraceQLMap renders a CH `map('k','v', 'k2','v2')` literal.
// Sorted keys for determinism.
func renderTraceQLMap(m map[string]string) string {
	if len(m) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteSQL(k))
		b.WriteString(", ")
		b.WriteString(quoteSQL(m[k]))
	}
	b.WriteByte(')')
	return b.String()
}

// quoteSQL renders s as a single-quoted CH SQL string literal. Embedded
// single quotes get backslash-escaped. The generator only emits values
// from fixed pools (TraceQLServicePool, TraceQLClusterPool,
// TraceQLHTTPMethodPool, TraceQLSpanNamePool, hex IDs), none of which
// contain quotes — but the escape is here so future generator widening
// can stay safe.
func quoteSQL(s string) string {
	if !strings.Contains(s, "'") && !strings.Contains(s, "\\") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
	return b.String()
}

// deterministicTraceID renders a 32-hex string from an integer seed +
// salt. Deterministic so re-running with the same rapid seed produces
// the same SQL.
func deterministicTraceID(seed, salt int) string {
	var buf [16]byte
	for i := range buf {
		buf[i] = byte((seed*31 + salt + i) & 0xFF)
	}
	return hex.EncodeToString(buf[:])
}

// deterministicSpanID is the 8-byte variant of deterministicTraceID.
func deterministicSpanID(seed, salt int) string {
	var buf [8]byte
	for i := range buf {
		buf[i] = byte((seed*17 + salt + i) & 0xFF)
	}
	return hex.EncodeToString(buf[:])
}

// statusQueryLiteral maps a CH-stored StatusCode value (Ok/Error/Unset)
// to the lowercase literal TraceQL's `status` intrinsic expects on the
// query side (ok/error/unset).
func statusQueryLiteral(chStatus string) string {
	return strings.ToLower(chStatus)
}

// TraceQLQuery returns a rapid generator that draws a property.Query
// targeted at dataset d. The accept-set, widened from the first sweep's
// single selector shape (issue #1471) to a breadth comparable to the
// PromQL leg's drawExpr (test/property/gen/promql.go), spans 14 shapes
// drawn uniformly via rapid.IntRange(0, 13):
//
//   - 0: bare service-name selector          — `{ resource.service.name = "<v>" }`
//   - 1: selector + count() scalar filter    — `{ ... } | count() OP N`
//   - 2: resource attribute beyond service.name — `{ resource.cluster = "<v>" }`
//   - 3: span attribute matcher              — `{ span.http.method = "<v>" }`
//   - 4: duration intrinsic                  — `{ duration OP Nms }`
//   - 5: status intrinsic (eq or negated)    — `{ status OP <ok|error|unset> }`
//   - 6: name intrinsic (exact match)        — `{ name = "<v>" }`
//   - 7: regex attribute matcher             — `{ resource.service.name =~ "<pattern>" }`
//   - 8: negated attribute matcher           — `{ resource.service.name != "<v>" }`
//   - 9: multi-condition (&&) filter         — `{ resource.service.name = "<v>" && status = <lit> }`
//   - 10: structural child (`>`)             — `{ A } > { B }`
//   - 11: structural descendant (`>>`)       — `{ A } >> { B }`
//   - 12: metric beyond count()              — `{ ... } | avg|min|max|sum(duration) OP Nms`
//   - 13: select() pipeline stage            — `{ ... } | select(span.http.method)`
//
// Every shape's oracle counterpart lives in test/property/oracle/traceql
// — see that package's parseQuery/Evaluate for the independent
// specification each shape is checked against. A shape is added here
// only once the oracle can evaluate it (never the reverse), so the
// property test is never vacuous for a generated shape.
//
// The selector's RHS is always a value drawn from the dataset's
// observed pools so a good share of iterations exercise a matching
// value (genuine non-empty result) and the rest exercise an absent one
// (empty result). The mix is implicit — rapid draws across the pools
// uniformly, and any one dataset draw covers only a subset of a given
// pool.
//
// EvalTs is stamped at TraceQLAnchorTime() + 1h for log completeness.
// /api/search DOES thread a time range now (the harness sends an explicit
// window bracketing the anchor — see runCerberusTraceQL — so the
// DefaultSearchLookback clamp doesn't filter this historically-anchored
// dataset out), but the query value itself doesn't carry it.
func TraceQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		shape := rapid.IntRange(0, 13).Draw(t, "shape")
		query := drawTraceQLQueryString(t, d, shape)

		evalTs := traceQLAnchor.Add(time.Hour).Unix()
		return property.Query{
			String: query,
			EvalTs: evalTs,
		}
	})
}

// drawTraceQLQueryString renders the query string for the given shape
// index. Split out of TraceQLQuery so each shape's construction reads
// as one focused case rather than a single sprawling closure.
func drawTraceQLQueryString(t *rapid.T, d property.Dataset, shape int) string {
	switch shape {
	case 0:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name = "%s" }`, service)
	case 1:
		return drawTraceQLCountQuery(t, d)
	case 2:
		cluster := rapid.SampledFrom(TraceQLClusterPool).Draw(t, "cluster")
		return fmt.Sprintf(`{ resource.cluster = "%s" }`, cluster)
	case 3:
		method := rapid.SampledFrom(TraceQLHTTPMethodPool).Draw(t, "httpMethod")
		return fmt.Sprintf(`{ span.http.method = "%s" }`, method)
	case 4:
		op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "durationOp")
		thresholdMs := rapid.SampledFrom(TraceQLDurationThresholdPoolMs).Draw(t, "durationThresholdMs")
		return fmt.Sprintf(`{ duration %s %dms }`, op, thresholdMs)
	case 5:
		op := rapid.SampledFrom(traceQLEqualityOps).Draw(t, "statusOp")
		statusCH := rapid.SampledFrom(TraceQLStatusPool).Draw(t, "status")
		return fmt.Sprintf(`{ status %s %s }`, op, statusQueryLiteral(statusCH))
	case 6:
		return fmt.Sprintf(`{ name = %q }`, drawTraceQLNameValue(t, d))
	case 7:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		pattern := service[:1] + ".*"
		return fmt.Sprintf(`{ resource.service.name =~ %q }`, pattern)
	case 8:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name != "%s" }`, service)
	case 9:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		statusCH := rapid.SampledFrom(TraceQLStatusPool).Draw(t, "status")
		return fmt.Sprintf(`{ resource.service.name = "%s" && status = %s }`,
			service, statusQueryLiteral(statusCH))
	case 10:
		left := rapid.SampledFrom(TraceQLServicePool).Draw(t, "leftService")
		right := rapid.SampledFrom(TraceQLServicePool).Draw(t, "rightService")
		return fmt.Sprintf(`{ resource.service.name = "%s" } > { resource.service.name = "%s" }`, left, right)
	case 11:
		left := rapid.SampledFrom(TraceQLServicePool).Draw(t, "leftService")
		right := rapid.SampledFrom(TraceQLServicePool).Draw(t, "rightService")
		return fmt.Sprintf(`{ resource.service.name = "%s" } >> { resource.service.name = "%s" }`, left, right)
	case 12:
		return drawTraceQLMetricQuery(t)
	case 13:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name = "%s" } | select(span.http.method)`, service)
	}
	// Unreachable: rapid.IntRange(0, 13) never draws outside [0, 13].
	panic(fmt.Sprintf("gen/traceql: unhandled query shape %d", shape))
}

// drawTraceQLCountQuery renders shape 1: a service selector with an
// optional `| count() OP N` scalar filter — the accept-set the first
// sweep shipped, preserved verbatim as one shape among the widened set.
func drawTraceQLCountQuery(t *rapid.T, d property.Dataset) string {
	service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
	query := fmt.Sprintf(`{ resource.service.name = "%s" }`, service)
	if !rapid.Bool().Draw(t, "hasCount") {
		return query
	}
	op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "countOp")
	// Bound N at [0, len(spans)+1] so the threshold is sometimes
	// satisfied and sometimes not, including the "above ceiling" case.
	n := rapid.IntRange(0, len(d.Metrics.Series)+1).Draw(t, "countN")
	return fmt.Sprintf("%s | count() %s %d", query, op, n)
}

// drawTraceQLMetricQuery renders shape 12: a service selector piped
// into one of avg|min|max|sum(duration) with a scalar comparison —
// count()'s sibling aggregates, exercising the same trace-scoped
// aggregate pipeline with a different reducer.
func drawTraceQLMetricQuery(t *rapid.T) string {
	service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
	fn := rapid.SampledFrom([]string{"avg", "min", "max", "sum"}).Draw(t, "metricFn")
	op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "metricOp")
	thresholdMs := rapid.SampledFrom(TraceQLDurationThresholdPoolMs).Draw(t, "metricThresholdMs")
	return fmt.Sprintf(`{ resource.service.name = "%s" } | %s(duration) %s %dms`,
		service, fn, op, thresholdMs)
}

// drawTraceQLNameValue picks the RHS for shape 6's `{ name = "<v>" }`.
// Half the draws (when the dataset has spans) pick a name that's
// actually present so the equality has something to match; the rest
// pick a value that's guaranteed absent, exercising the empty-result
// path deliberately rather than by accident.
func drawTraceQLNameValue(t *rapid.T, d property.Dataset) string {
	names := d.Metrics.NamesPresent()
	if len(names) > 0 && rapid.Bool().Draw(t, "nameExists") {
		return rapid.SampledFrom(names).Draw(t, "existingName")
	}
	return "nonexistent-span-name"
}
