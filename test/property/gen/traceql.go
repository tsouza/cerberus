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

// TraceQLDataset returns a rapid generator that draws a random
// property.Dataset of OTel-CH traces rows. The dataset is intentionally
// narrow for the first TraceQL property test:
//
//   - 1–5 spans.
//   - Each span carries a service.name from TraceQLServicePool stored
//     under ResourceAttributes['service.name'].
//   - Span names draw from TraceQLSpanNamePool plus a per-span ordinal
//     suffix so (SpanName, Timestamp) pairs are unique across the
//     dataset — Tempo's /api/search collapses traces by name+ts, and
//     the property test wants one TraceSummary per span so the count
//     comparator is exact.
//   - Per-span StartTime is anchor + i*1s; Duration is fixed
//     50 000 000 ns (50ms). The values don't matter to the selector +
//     count() queries the generator produces.
//   - StatusCode = "Unset" for every span (the only intrinsic the
//     generator touches today).
//
// The returned Dataset's DDL is a multi-statement script
// (`CREATE OR REPLACE TABLE otel_traces (...); INSERT ...;`) the chDB
// runner replays before each query. The MetricsModel mirror carries
// the same data in a shape the oracle reads — it stores spans as
// SeriesData entries with MetricName=SpanName, Labels carrying the
// span-level + resource attributes via reserved key prefixes:
//
//   - "resource.<key>"  → ResourceAttributes[<key>]
//   - "span.<key>"      → SpanAttributes[<key>] (unused today; reserved
//     for span-scope matchers when the generator widens)
//   - "__name__"        → SpanName (mirrors the PromQL convention; the
//     oracle's labels-minus-__name__ identity rule reuses it)
//   - "__traceID__"     → TraceID (unique 32-hex per span)
//   - "__spanID__"      → SpanID (unique 16-hex per span)
//   - "__duration_ns__" → string-formatted Duration (so the oracle
//     can read it without changing SeriesData's float64 Point shape)
//   - "__status__"      → "Unset" (the seeded value)
//
// MergeTree is the chosen engine (matches PromQL property test
// rationale: Memory engine refuses PREWHERE the cerberus optimizer
// emits).
func TraceQLDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numSpans := rapid.IntRange(1, 5).Draw(t, "numSpans")
		spans := make([]traceQLSpan, 0, numSpans)
		for i := 0; i < numSpans; i++ {
			service := rapid.SampledFrom(TraceQLServicePool).Draw(t, fmt.Sprintf("service_%d", i))
			baseName := rapid.SampledFrom(TraceQLSpanNamePool).Draw(t, fmt.Sprintf("spanName_%d", i))
			// Unique suffix → unique (SpanName, Timestamp) across spans:
			// Tempo's toTraceSummaries() keys by name+timestamp and collapses
			// duplicates. Suffixing the span index is the cheapest way to
			// guarantee uniqueness without adding a second random draw.
			name := fmt.Sprintf("%s /api/%d", baseName, i)
			spans = append(spans, traceQLSpan{
				traceID:    deterministicTraceID(i, 0xa1),
				spanID:     deterministicSpanID(i, 0xb2),
				parentID:   "0000000000000000",
				service:    service,
				name:       name,
				startTime:  traceQLAnchor.Add(time.Duration(i) * time.Second),
				durationNs: 50_000_000,
				statusCode: "Unset",
			})
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
// SeriesData entries keyed by SpanName; this struct is the bridge.
type traceQLSpan struct {
	traceID    string // 32-hex
	spanID     string // 16-hex
	parentID   string // 16-hex (all-zero for root)
	service    string
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
// OTel-CH default), DurationNs (column name `Duration`), StatusCode.
//
// `CREATE OR REPLACE TABLE` keeps re-runs idempotent (chdb-go shares
// one catalog across sessions).
func renderTraceQLDDL(spans []traceQLSpan) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(SpansTableName)
	b.WriteString(` (
    Timestamp DateTime64(9),
    TraceId FixedString(32),
    SpanId FixedString(16),
    ParentSpanId FixedString(16),
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(String, String),
    SpanAttributes Map(String, String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String
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
	b.WriteString(renderTraceQLMap(map[string]string{"service.name": s.service}))
	b.WriteString(", ")
	// SpanAttributes (empty for now; reserved for span.<attr> matchers)
	b.WriteString("map()")
	b.WriteString(", ")
	// Duration
	b.WriteString(fmt.Sprintf("%d", s.durationNs))
	b.WriteString(", ")
	// StatusCode
	b.WriteString(quoteSQL(s.statusCode))
	b.WriteString(", ")
	// StatusMessage
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
// from fixed pools (TraceQLServicePool, TraceQLSpanNamePool, hex IDs),
// none of which contain quotes — but the escape is here so future
// generator widening can stay safe.
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

// TraceQLQuery returns a rapid generator that draws a property.Query
// targeted at dataset d. The accept-set for this first sweep:
//
//   - Selector only:           `{ resource.service.name = "<value>" }`
//   - Selector + count filter: `{ resource.service.name = "<v>" } | count() OP N`
//     where OP ∈ {>, >=, <, <=, =} and N ∈ [0, len(d.Series)].
//
// The selector's RHS is always a value drawn from the dataset's
// observed services so half of the iterations exercise a matching
// service (genuine non-empty result) and the other half exercise a
// service not present (empty result). The mix is implicit — rapid
// draws across the pool uniformly, and the dataset's spans cover only
// a subset of TraceQLServicePool on any iteration.
//
// EvalTs is irrelevant for TraceQL search (no time range threading)
// so it's stamped at TraceQLAnchorTime() + 1h purely for log
// completeness.
func TraceQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		// Draw service value — always from the global pool so half the
		// queries hit "absent service" (zero matches), exercising the
		// empty-result path on both sides.
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")

		// Optional `| count() OP N` filter.
		hasCount := rapid.Bool().Draw(t, "hasCount")
		query := fmt.Sprintf(`{ resource.service.name = "%s" }`, service)
		if hasCount {
			op := rapid.SampledFrom([]string{">", ">=", "<", "<=", "="}).Draw(t, "countOp")
			// Bound N at [0, len(spans)] so the threshold is sometimes
			// satisfied and sometimes not. len(d.Series) is the upper
			// bound; using a slightly wider range exercises the
			// "threshold above ceiling" path too.
			n := rapid.IntRange(0, len(d.Metrics.Series)+1).Draw(t, "countN")
			query = fmt.Sprintf("%s | count() %s %d", query, op, n)
		}

		evalTs := traceQLAnchor.Add(time.Hour).Unix()
		return property.Query{
			String: query,
			EvalTs: evalTs,
		}
	})
}
