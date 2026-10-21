package schema

// This file exposes each OTel-CH table's ORDER BY (sorting key) as
// structured data, narrowed to the leading run of PLAIN COLUMN keys.
//
// The OTel ClickHouse Exporter DDL (the sqltemplates this repo vendors via
// internal/schema/ddl) declares each table's ORDER BY tuple. Cerberus does
// not otherwise need the tuple at runtime, so it is not parsed out of the
// DDL; instead the canonical OTel-CH layout is reflected here as a typed
// accessor keyed off the same column-name fields the rest of the schema
// package already carries. Callers that want to reason about sort-order (the
// optimize_aggregation_in_order eligibility check in internal/engine) read
// these instead of re-deriving the ORDER BY from SQL text.
//
// IMPORTANT: only the LEADING run of bare column references is returned.
// A sorting-key element that is a function of a column
// (`toUnixTimestamp64Nano(TimeUnix)`, `toStartOfFiveMinutes(Timestamp)`,
// `toDateTime(Timestamp)`) is NOT a bare column and TERMINATES the prefix:
// optimize_aggregation_in_order can only exploit a GROUP BY that is a prefix
// of the sort key expressed over the SAME bare columns, so a GROUP BY can
// never legitimately match past the first function-wrapped key element. This
// keeps every consumer conservative by construction.
//
// The OTel-CH ORDER BY tuples reflected here (the cerberus-ddl fork's
// templates, docs/upstream-forks.md):
//
//	otel_metrics_{gauge,sum,histogram,exp_histogram,summary}, nanos precision
//	    ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
//	    -> bare prefix: (MetricName, Attributes, ServiceName)
//	otel_metrics_{gauge,sum,histogram,exp_histogram,summary}, seconds precision
//	    ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp(TimeUnix))
//	    -> bare prefix: (MetricName, Attributes, ServiceName)
//	otel_traces
//	    ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
//	    -> bare prefix: (ServiceName, SpanName)
//	otel_logs
//	    ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)
//	    -> bare prefix: () the first key element is function-wrapped
//
// These are the keys of the tables cerberus provisions. A metrics table a
// stock exporter created carries a different key — before v0.157
// (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix)),
// from v0.157 (ServiceName, MetricName, toStartOfHour(TimeUnix),
// cityHash64(Attributes), TimeUnix) — and CREATE TABLE IF NOT EXISTS keeps
// it. Nothing reads the deployed key, so Metrics.SortingKeyPrefix answers
// the provisioned prefix over such a table too: the eligibility check can
// stamp optimize_aggregation_in_order on a GROUP BY that is no prefix of the
// deployed key, where ClickHouse finds no matching key and aggregates as it
// would without the setting, and withholds it from one that is. Either
// changes only how ClickHouse aggregates, never the rows a query returns.
// Reading the deployed key is cerberus issue #3948.

// SortingKeyPrefix returns the leading run of bare-column sorting-key
// columns for the metrics tables, in ORDER BY order. Every metrics table
// (gauge / sum / histogram / exp_histogram / summary) shares the same
// ORDER BY, so the prefix is identical for all of them, at either timestamp
// precision. The fourth key element — toUnixTimestamp64Nano(TimeUnix) or
// toUnixTimestamp(TimeUnix) — is function-wrapped and so is excluded.
// Returns a fresh slice each call (callers may retain it).
func (m Metrics) SortingKeyPrefix() []string {
	return []string{m.MetricNameColumn, m.AttributesColumn, m.ServiceNameColumn}
}

// SortingKeyPrefix returns the leading run of bare-column sorting-key
// columns for the spans table. The third ORDER BY element,
// toDateTime(Timestamp), is function-wrapped and so is excluded.
func (t Traces) SortingKeyPrefix() []string {
	return []string{t.ServiceNameColumn, t.SpanNameColumn}
}

// SortingKeyPrefix returns the leading run of bare-column sorting-key
// columns for the logs table. The OTel-CH logs ORDER BY leads with a
// function-wrapped key element (toStartOfFiveMinutes(Timestamp)), so there
// is no bare-column prefix and the result is empty: no GROUP BY can be a
// bare-column prefix of this sort key, so optimize_aggregation_in_order is
// never eligible for logs.
func (Logs) SortingKeyPrefix() []string {
	return nil
}
