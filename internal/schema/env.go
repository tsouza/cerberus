package schema

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Env var names recognised by the FromEnv factories. Listed as exported
// constants so docs / tests can reference them without re-typing the
// string and risking drift.
const (
	// EnvMetricsGaugeTable overrides Metrics.GaugeTable.
	EnvMetricsGaugeTable = "CERBERUS_SCHEMA_METRICS_GAUGE_TABLE"
	// EnvMetricsSumTable overrides Metrics.SumTable.
	EnvMetricsSumTable = "CERBERUS_SCHEMA_METRICS_SUM_TABLE"
	// EnvMetricsHistogramTable overrides Metrics.HistogramTable.
	EnvMetricsHistogramTable = "CERBERUS_SCHEMA_METRICS_HISTOGRAM_TABLE"
	// EnvMetricsExpHistogramTable overrides Metrics.ExpHistogramTable.
	EnvMetricsExpHistogramTable = "CERBERUS_SCHEMA_METRICS_EXP_HISTOGRAM_TABLE"
	// EnvMetricsSummaryTable overrides Metrics.SummaryTable.
	EnvMetricsSummaryTable = "CERBERUS_SCHEMA_METRICS_SUMMARY_TABLE"
	// EnvLogsTable overrides Logs.LogsTable.
	EnvLogsTable = "CERBERUS_SCHEMA_LOGS_TABLE"
	// EnvTracesTable overrides Traces.SpansTable.
	EnvTracesTable = "CERBERUS_SCHEMA_TRACES_TABLE"
	// EnvTracesTsLookup opts into the trace_id_ts window pre-filter
	// (Traces.TraceIDTsEnabled). Truthy values ("1", "true", "yes", "on")
	// enable it; unset/empty/falsey leaves it off. The operator sets it
	// only after confirming the `<spans>_trace_id_ts` MV is populated.
	EnvTracesTsLookup = "CERBERUS_SCHEMA_TRACES_TS_LOOKUP"
	// EnvPromResourceLabels is the comma-separated allowlist of OTel
	// ResourceAttributes keys (dotted form) projected as Prometheus
	// labels. Empty / unset promotes every resource-attribute key.
	// Populates Metrics.PromResourceLabels.
	EnvPromResourceLabels = "CERBERUS_PROM_RESOURCE_LABELS"
)

// traceIDTsSuffix is the fixed suffix the OTel-CH exporter's DDL template
// appends to the spans table name for the trace-id→timestamp lookup table
// (`<spans>_trace_id_ts`). It is baked into the upstream template, so
// cerberus derives the lookup-table name the same way when the spans
// table is overridden.
const traceIDTsSuffix = "_trace_id_ts"

// envBool reports whether key is set to a truthy value ("1", "true",
// "yes", "on"; case-insensitive). Unset, empty, or any other value is
// false — the opt-in gate stays off unless the operator affirmatively
// enables it.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// envOverride returns the trimmed value of key when set to a non-empty
// string, else def. An env var set to whitespace-only is treated as
// unset — operators paste values with stray newlines often enough that
// silently honouring them would produce table names like
// "otel_metrics_sum\n" that fail at query time with cryptic CH errors.
func envOverride(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// DefaultOTelMetricsFromEnv returns DefaultOTelMetrics() with any
// CERBERUS_SCHEMA_METRICS_*_TABLE env overrides applied. Unset or
// whitespace-only values leave the corresponding field at its default.
// Non-table fields (column names, rollups, suffixes) are not exposed
// as overrides — extend the surface here if a deployment demonstrates
// the need.
func DefaultOTelMetricsFromEnv() Metrics {
	m := DefaultOTelMetrics()
	m.GaugeTable = envOverride(EnvMetricsGaugeTable, m.GaugeTable)
	m.SumTable = envOverride(EnvMetricsSumTable, m.SumTable)
	m.HistogramTable = envOverride(EnvMetricsHistogramTable, m.HistogramTable)
	m.ExpHistogramTable = envOverride(EnvMetricsExpHistogramTable, m.ExpHistogramTable)
	m.SummaryTable = envOverride(EnvMetricsSummaryTable, m.SummaryTable)
	m.PromResourceLabels = envCSVList(EnvPromResourceLabels)
	return m
}

// envCSVList splits a comma-separated env var into a trimmed,
// empty-dropped slice. Unset / whitespace-only returns nil so callers
// can treat nil as the documented "all" / "none" sentinel. Mirrors
// envOverride's trim-and-skip-empty discipline for the list shape.
func envCSVList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// KV is one ordered `key = value` entry of a ClickHouse MergeTree SETTINGS
// tail — the value-carrier the schema-provisioning escape hatch threads from
// the CERBERUS_SCHEMA_SETTINGS env var through to chsql.TableSettings. The
// slice order is preserved end-to-end so the emitted DDL is deterministic
// (a stable golden / byte-identical re-apply). Value carries its Go type: an
// int64 / float64 / bool renders BARE (the form a numeric or boolean CH
// setting takes — e.g. `min_bytes_for_wide_part = 0`), a string renders
// single-quoted (e.g. `storage_policy = 's3_tiered'`) — the RHS quoting is
// inferred from the value's dynamic type by chsql.InlineLit. It lives here
// rather than in chsql because chsql already imports this package, so a
// chsql-owned KV would form an import cycle.
type KV struct {
	Key   string
	Value any
}

// ParseKVList parses a `k=v,k2=v2` string into the ordered KV slice the
// schema-provisioning SETTINGS escape hatch consumes. It mirrors envCSVList's
// trim-and-skip-empty discipline for the comma split, but is FAIL-FAST on a
// token that carries no `=` (a malformed setting must surface at startup, not
// be silently dropped). Order is preserved so the rendered DDL is
// deterministic. Each value's Go type is inferred — a bare integer parses to
// int64, a bare float to float64, `true`/`false` to bool, anything else stays a
// string — so chsql.TableSettings renders numerics/bools bare and strings
// single-quoted. Unset / whitespace-only returns nil.
func ParseKVList(raw string) ([]KV, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []KV
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed setting %q: expected k=v", part)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("malformed setting %q: empty key", part)
		}
		out = append(out, KV{Key: k, Value: inferKVValue(strings.TrimSpace(v))})
	}
	return out, nil
}

// inferKVValue maps a raw setting value to the Go type chsql.InlineLit renders
// in the right CH form: a bare integer / float / boolean stays numeric/bool (so
// it renders unquoted), and anything else is a string (single-quoted). This is
// what lets `storage_policy=s3_tiered` render quoted while
// `min_bytes_for_wide_part=0` renders bare.
func inferKVValue(v string) any {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return v
}

// DefaultOTelLogsFromEnv returns DefaultOTelLogs() with the
// CERBERUS_SCHEMA_LOGS_TABLE override applied (if set).
func DefaultOTelLogsFromEnv() Logs {
	l := DefaultOTelLogs()
	l.LogsTable = envOverride(EnvLogsTable, l.LogsTable)
	return l
}

// DefaultOTelTracesFromEnv returns DefaultOTelTraces() with the
// CERBERUS_SCHEMA_TRACES_TABLE override applied (if set).
func DefaultOTelTracesFromEnv() Traces {
	t := DefaultOTelTraces()
	t.SpansTable = envOverride(EnvTracesTable, t.SpansTable)
	// The lookup table name tracks the spans table: the OTel-CH DDL
	// template hard-codes the `_trace_id_ts` suffix, so when the operator
	// overrides the spans table the lookup table is `<spans>_trace_id_ts`.
	t.TraceIDTsTable = t.SpansTable + traceIDTsSuffix
	t.TraceIDTsEnabled = envBool(EnvTracesTsLookup)
	return t
}
