package schema

import (
	"slices"
	"strings"
)

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
// These are the keys of the tables cerberus provisions. A table created
// outside cerberus keeps its own key, because cerberus's DDL is CREATE TABLE
// IF NOT EXISTS: a stock exporter's metrics tables are ordered by
// (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))
// before v0.157 and by (ServiceName, MetricName, toStartOfHour(TimeUnix),
// cityHash64(Attributes), TimeUnix) from v0.157. The boot requirements check
// (internal/preflight) therefore reads each table's deployed key from
// system.tables and reduces it with SortingKeyPrefixOf; the accessors below
// answer only for a table whose key was not read.

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

// SortingKeyPrefixOf reduces a sorting key as system.tables reports it
// (sorting_key: the ORDER BY elements, comma-separated, e.g.
// "ServiceName, MetricName, toStartOfHour(TimeUnix), TimeUnix") to its
// leading run of bare column names, the same contract as SortingKeyPrefix:
// the run stops at the first element that is not a bare (optionally
// back-quoted) identifier. An empty key yields an empty, non-nil prefix.
func SortingKeyPrefixOf(sortingKey string) []string {
	prefix := []string{}
	for _, elem := range splitTopLevel(sortingKey) {
		name, ok := bareIdentifier(strings.TrimSpace(elem))
		if !ok {
			break
		}
		prefix = append(prefix, name)
	}
	return prefix
}

// SortingKeyColumnsOf reduces a sorting key as system.tables reports it to
// the columns it sorts by, in key order: each element that reads exactly one
// column — a bare (optionally back-quoted) column, or that column wrapped in
// one or more single-argument calls such as toUnixTimestamp64Nano(TimeUnix)
// or toStartOfHour(TimeUnix) — contributes that column, once, at its first
// position. The run stops at the first element that does not reduce to one
// column. An empty key yields an empty, non-nil list.
//
// Unlike SortingKeyPrefixOf, a function-wrapped element does not end the
// run: a filter on the column it wraps still lets ClickHouse prune granules
// on that element, which is the order PREWHERE promotion ranks filters by.
func SortingKeyColumnsOf(sortingKey string) []string {
	columns := []string{}
	for _, elem := range splitTopLevel(sortingKey) {
		name, ok := singleColumnOf(strings.TrimSpace(elem))
		if !ok {
			break
		}
		if !slices.Contains(columns, name) {
			columns = append(columns, name)
		}
	}
	return columns
}

// singleColumnOf unwraps elem's single-argument calls, `fn(<arg>)`, and
// reports the bare column the innermost argument names.
func singleColumnOf(elem string) (string, bool) {
	for {
		if name, ok := bareIdentifier(elem); ok {
			return name, true
		}
		open := strings.IndexByte(elem, '(')
		if open <= 0 || !strings.HasSuffix(elem, ")") {
			return "", false
		}
		if _, ok := bareIdentifier(elem[:open]); !ok {
			return "", false
		}
		args := splitTopLevel(elem[open+1 : len(elem)-1])
		if len(args) != 1 {
			return "", false
		}
		elem = strings.TrimSpace(args[0])
	}
}

// splitTopLevel splits a sorting-key expression list on the commas outside
// any parentheses or quotes. An all-blank list yields no elements.
func splitTopLevel(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	var (
		elems []string
		depth int
		quote rune
		start int
	)
	for i, r := range list {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '`' || r == '\'' || r == '"':
			quote = r
		case r == '(' || r == '[':
			depth++
		case r == ')' || r == ']':
			depth--
		case r == ',' && depth == 0:
			elems = append(elems, list[start:i])
			start = i + 1
		}
	}
	return append(elems, list[start:])
}

// bareIdentifier reports whether elem is a bare column reference — an
// unquoted identifier or one back-quoted identifier — and returns the column
// name it names.
func bareIdentifier(elem string) (string, bool) {
	if len(elem) > len("``") && strings.HasPrefix(elem, "`") && strings.HasSuffix(elem, "`") {
		inner := elem[1 : len(elem)-1]
		if strings.Contains(inner, "`") {
			return "", false
		}
		return inner, true
	}
	if elem == "" {
		return "", false
	}
	for i, r := range elem {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		digit := r >= '0' && r <= '9'
		if !letter && (!digit || i == 0) {
			return "", false
		}
	}
	return elem, true
}
