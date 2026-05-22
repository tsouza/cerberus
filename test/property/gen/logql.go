package gen

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// LogsTableName is the OTel-CH default logs table. The DDL the
// generator emits targets this name; the chDB session and the SQL
// cerberus emits both expect it.
const LogsTableName = "otel_logs"

// LogStreamLabelPool is the fixed stream-identity label pool for the
// LogQL property generator. `job` and `service_name` are the two label
// names cerberus's LogQL handler routes through the
// ResourceAttributes map (OTel-CH convention). Two names keep the
// label-set count predictable so the rapid shrinker doesn't have to
// search through a wide combinatorial space.
var LogStreamLabelPool = []string{"job", "service_name"}

// LogStreamLabelValues is the per-name value pool. Values are kept
// small + integer-suffix-free so two records share a label value
// often — the line-filter property only catches drift if matched
// records exist on each side.
var LogStreamLabelValues = map[string][]string{
	"job":          {"api", "web", "batch"},
	"service_name": {"checkout", "auth", "billing"},
}

// LogSeverityPool is the SeverityText pool. Values mirror the OTel
// SeverityText vocabulary the CH-exporter writes. Not yet used as a
// matcher target (the M3.x lowering surfaces severity through
// SeverityNumber, not stream labels), but stored on every record so
// the dataset shape matches production.
var LogSeverityPool = []string{"INFO", "WARN", "ERROR", "DEBUG"}

// LogBodyTokenPool is the per-line word pool. Each generated body is
// a space-joined sequence of 2-4 tokens drawn from this pool. Keeping
// the pool small means every `|= "<token>"` line-filter query has a
// non-trivial accept-set against random datasets — without that the
// generator would burn iterations on filters that match zero records.
var LogBodyTokenPool = []string{"error", "ok", "timeout", "retry", "cache", "miss", "hit", "auth"}

// logAnchorTime is the timestamp the generator anchors all log records
// to. Fixed (2026-05-13T12:00:00Z) so each rapid iteration produces
// the same wall-clock baseline; the failure log's `evalTs` value is
// comparable across runs.
var logAnchorTime = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// LogAnchorTime is the exported handle on logAnchorTime — the LogQL
// query generator reads it to pin the request's [start, end] window
// around the dataset's active span.
func LogAnchorTime() time.Time { return logAnchorTime }

// LogsDataset returns a rapid generator that draws a random
// property.Dataset whose Logs mirror carries the in-memory records.
// The generator's accept-set is intentionally narrow:
//
//   - 1-5 log records per draw.
//   - Each record carries 1-2 stream-identity labels from
//     LogStreamLabelPool, with values drawn from
//     LogStreamLabelValues.
//   - Body is a space-joined sequence of 2-4 tokens from
//     LogBodyTokenPool, so generated `|= "<token>"` filters have a
//     non-trivial accept-set.
//   - Timestamps are anchor + 15s * i so the per-record ordering is
//     deterministic and well within a one-hour query window.
//
// The Dataset's DDL renders one `CREATE OR REPLACE TABLE otel_logs`
// statement plus a single `INSERT … VALUES …` batched insert for all
// records.
func LogsDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numRecords := rapid.IntRange(1, 5).Draw(t, "numRecords")

		records := make([]property.LogRecord, 0, numRecords)
		step := 15 * time.Second
		for i := 0; i < numRecords; i++ {
			lset := drawStreamLabels(t, fmt.Sprintf("labels_%d", i))
			body := drawBody(t, fmt.Sprintf("body_%d", i))
			severity := rapid.SampledFrom(LogSeverityPool).Draw(t, fmt.Sprintf("severity_%d", i))
			ts := logAnchorTime.Add(time.Duration(i) * step).UnixNano()
			records = append(records, property.LogRecord{
				Body:               body,
				SeverityText:       severity,
				ResourceAttributes: lset,
				TimestampNanos:     ts,
			})
		}

		return property.Dataset{
			DDL:  renderLogsDDL(records),
			Logs: &property.LogsModel{Records: records},
		}
	})
}

// drawStreamLabels picks a 1-2 label subset from LogStreamLabelPool
// and assigns each a random value. Labels are picked in sorted order
// so shrinking focuses on count rather than reshuffles.
func drawStreamLabels(t *rapid.T, id string) map[string]string {
	count := rapid.IntRange(1, len(LogStreamLabelPool)).Draw(t, id+"_count")
	names := append([]string(nil), LogStreamLabelPool...)
	sort.Strings(names)
	picked := names[:count]
	out := make(map[string]string, len(picked))
	for _, name := range picked {
		values := LogStreamLabelValues[name]
		v := rapid.SampledFrom(values).Draw(t, id+"_"+name)
		out[name] = v
	}
	return out
}

// drawBody picks 2-4 tokens from LogBodyTokenPool and joins them with
// single spaces. The shape mirrors a structured log message
// reasonably well — short, alphabetic, no special characters that
// would force the chDB string literal escaper to run.
func drawBody(t *rapid.T, id string) string {
	count := rapid.IntRange(2, 4).Draw(t, id+"_count")
	tokens := make([]string, 0, count)
	for i := 0; i < count; i++ {
		tokens = append(tokens, rapid.SampledFrom(LogBodyTokenPool).Draw(t, fmt.Sprintf("%s_tok_%d", id, i)))
	}
	return strings.Join(tokens, " ")
}

// renderLogsDDL produces the multi-statement seed script for records.
//
// Statements:
//   - One `CREATE OR REPLACE TABLE otel_logs (...) ENGINE = MergeTree
//     ORDER BY Timestamp;`
//   - One batched `INSERT INTO otel_logs (Timestamp, SeverityText,
//     Body, ResourceAttributes) VALUES (...), (...);`
//
// `CREATE OR REPLACE TABLE` keeps re-runs inside the same chDB
// process idempotent. MergeTree (not Memory) matches the metrics-
// side rationale: the optimizer's PREWHERE promotion fires
// unconditionally and chDB's Memory engine refuses PREWHERE.
//
// The DDL must declare every top-level OTel-CH scalar column the
// LogQL lowering routes through `topLevelLogColumnFor` — even though
// the property generator only populates `ResourceAttributes`,
// `SeverityText`, `Body`, and `Timestamp`. The matcher lowering
// emits `coalesce(nullIf(ServiceName, ”),
// ResourceAttributes['service_name'])` (and the matching shape for
// each other top-level label), so chDB rejects the query with
// `Unknown expression identifier ServiceName` if the column is
// missing. The empty-string / zero-value defaults keep the `nullIf`
// branch falsy, so the coalesce still falls through to the
// ResourceAttributes map the generator populated.
func renderLogsDDL(records []property.LogRecord) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(LogsTableName)
	b.WriteString(` (
    Timestamp DateTime64(9),
    SeverityText LowCardinality(String),
    SeverityNumber UInt8 DEFAULT 0,
    Body String,
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String) DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT '',
    EventName LowCardinality(String) DEFAULT '',
    TraceId String DEFAULT '',
    SpanId String DEFAULT '',
    TraceFlags UInt8 DEFAULT 0
) ENGINE = MergeTree ORDER BY Timestamp;
`)
	if len(records) == 0 {
		return b.String()
	}
	b.WriteString(`INSERT INTO `)
	b.WriteString(LogsTableName)
	b.WriteString(` (Timestamp, SeverityText, Body, ResourceAttributes) VALUES `)
	for i, r := range records {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(renderLogRow(r))
	}
	b.WriteString(";\n")
	return b.String()
}

// renderLogRow renders one
// `(toDateTime64('YYYY-MM-DD HH:MM:SS.fff', 9), 'severity', 'body',
// map(...))` literal row.
func renderLogRow(r property.LogRecord) string {
	var b strings.Builder
	b.WriteString("(toDateTime64('")
	// chdb-go accepts 'YYYY-MM-DD HH:MM:SS.nnn' wall-clock literals
	// with the toDateTime64(..., 9) cast. 15s spacing in the
	// generator means millisecond precision is enough.
	ts := time.Unix(0, r.TimestampNanos).UTC().Format("2006-01-02 15:04:05.000")
	b.WriteString(ts)
	b.WriteString("', 9), '")
	b.WriteString(escapeSQLString(r.SeverityText))
	b.WriteString("', '")
	b.WriteString(escapeSQLString(r.Body))
	b.WriteString("', ")
	b.WriteString(renderResourceAttrs(r.ResourceAttributes))
	b.WriteByte(')')
	return b.String()
}

// renderResourceAttrs renders a label set as a CH
// `map('k1','v1', 'k2','v2')` expression. Sorted keys for
// determinism.
func renderResourceAttrs(labels map[string]string) string {
	if len(labels) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('\'')
		b.WriteString(escapeSQLString(k))
		b.WriteString("', '")
		b.WriteString(escapeSQLString(labels[k]))
		b.WriteByte('\'')
	}
	b.WriteByte(')')
	return b.String()
}

// escapeSQLString minimal-escapes a string for inclusion inside a
// single-quoted CH literal. The generator only produces alphabetic
// + space content (LogBodyTokenPool, LogSeverityPool, etc.), so the
// only escape it ever has to do is single-quote → ”. Defensive
// against future pool growth.
func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// LogQLQuery returns a rapid generator that produces a random
// property.Query targeted at d. The generator builds the LogQL query
// as a string (Loki's syntax struct constructors are unexported, so
// re-parsing via syntax.ParseExpr is the documented round-trip
// boundary). The string surfaces on the Query value verbatim; the
// cerberus handler re-parses it via the same syntax.ParseExpr and
// the oracle parses it inline.
//
// Accept-set (deliberately narrow — start with what the from-scratch
// oracle implements):
//
//   - Bare stream selector:   `{job="api"}`
//   - Line filter (contains): `{job="api"} |= "error"`
//   - Line filter (not):      `{job="api"} != "debug"`
//   - Label format (rename):  `{job="api"} | label_format renamed=job`
//
// All shapes are log-stream queries (resultType=streams). Metric-form
// (rate / count_over_time / aggregations) is not generated; the oracle's
// evaluator does not cover those shapes.
//
// EvalTs is anchored to an hour past the dataset's first record so
// the cerberus handler's instant-lookback (5min) doesn't clip the
// generated records out of the result.
func LogQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		if d.Logs == nil || len(d.Logs.Records) == 0 {
			return property.Query{}
		}

		streamLabels := d.Logs.StreamLabelsPresent()
		if len(streamLabels) == 0 {
			return property.Query{}
		}

		sel := drawLogQLSelector(t, streamLabels)
		query := drawLogQLShape(t, sel, d.Logs)

		// EvalTs lives at the end of the dataset's active window plus
		// a buffer, mirroring the prom generator's strategy. The
		// LogQL handler treats /query as `[ts-5m, ts]`, so 60s after
		// the last record keeps every record inside the instant
		// lookback.
		lastRecord := d.Logs.Records[len(d.Logs.Records)-1]
		evalTs := time.Unix(0, lastRecord.TimestampNanos).Add(60 * time.Second).Unix()
		return property.Query{
			String: query,
			EvalTs: evalTs,
		}
	})
}

// drawLogQLSelector picks one stream-selector matcher and renders
// it as a `{name="value"}` literal. Single-matcher shape keeps the
// accept-set narrow; the AND-matcher case is well-covered by spec/
// fixtures already.
func drawLogQLSelector(t *rapid.T, present map[string][]string) string {
	names := make([]string, 0, len(present))
	for k := range present {
		names = append(names, k)
	}
	sort.Strings(names)
	name := rapid.SampledFrom(names).Draw(t, "matcherLabel")
	value := rapid.SampledFrom(present[name]).Draw(t, "matcherValue")
	return fmt.Sprintf(`{%s=%q}`, name, value)
}

// drawLogQLShape picks the random expression shape per the LogQL
// generator accept-set. Uniform draw over the four shapes:
//
//	0: bare selector                — exercises the matcher path
//	1: selector |= "<tok>"          — line-filter contains
//	2: selector != "<tok>"          — line-filter not-contains
//	3: selector | label_format renamed=<src>
//	                                — rename label, post-process path
//
// The token for shapes 1 / 2 is drawn from the dataset's body
// tokens so the filter has at least one record it could match. For
// shape 3 the source label is drawn from the stream-label pool so
// the rename actually fires.
func drawLogQLShape(t *rapid.T, sel string, logs *property.LogsModel) string {
	shape := rapid.IntRange(0, 3).Draw(t, "shape")
	switch shape {
	case 0:
		return sel
	case 1, 2:
		tokens := logs.BodyTokensPresent()
		if len(tokens) == 0 {
			return sel
		}
		tok := rapid.SampledFrom(tokens).Draw(t, "filterToken")
		op := "|="
		if shape == 2 {
			op = "!="
		}
		return fmt.Sprintf(`%s %s %q`, sel, op, tok)
	case 3:
		srcLabel := rapid.SampledFrom(LogStreamLabelPool).Draw(t, "renameSrc")
		return fmt.Sprintf(`%s | label_format renamed=%s`, sel, srcLabel)
	}
	return sel
}
