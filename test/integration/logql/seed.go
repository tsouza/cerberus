//go:build chdb_agpl_oracle

package logql

import (
	"sort"
	"strings"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

var seedAnchor = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// RichSeed returns one deterministic OTel logs fixture and its independent
// in-memory mirror. The rows intentionally overlap on stream labels while
// varying bodies, structured metadata, severity, and IP tokens so every
// matrix category has both accepting and rejecting records.
func RichSeed() (string, *property.LogsModel) {
	records := []property.LogRecord{
		logRecord(0, "INFO", "request ok cache hit 10.1.2.3", labels("job", "api", "service_name", "checkout"), labels("trace.id", "t-1")),
		logRecord(1, "WARN", "request timeout retry 10.200.0.9", labels("job", "api", "service_name", "checkout"), labels("level", "ERR", "trace.id", "t-2")),
		logRecord(2, "ERROR", "auth error retry 192.168.1.5", labels("job", "api", "service_name", "auth"), labels("detected_level", "Warn", "tenant", "blue")),
		logRecord(3, "DEBUG", "cache miss ok 172.16.0.1", labels("job", "web", "service_name", "checkout"), labels("log.level", "debug", "tenant", "green")),
		logRecord(4, "", "billing error timeout 8.8.8.8", labels("job", "batch", "service_name", "billing"), labels("severity", "critical")),
		logRecord(5, "INFO", "request ok cache miss", labels("job", "web", "service_name", "auth"), nil),
		logRecord(6, "ERROR", "auth timeout from 10.1.2.3 retry", labels("job", "api", "service_name", "auth"), labels("severity_text", "fatal", "tenant", "blue")),
		logRecord(7, "", "worker heartbeat ok", labels("job", "batch", "service_name", "billing"), nil),
	}
	return renderDDL(records), &property.LogsModel{Records: records}
}

func logRecord(step int, severity, body string, resource, attrs map[string]string) property.LogRecord {
	return property.LogRecord{
		Body:               body,
		SeverityText:       severity,
		ResourceAttributes: resource,
		LogAttributes:      attrs,
		TimestampNanos:     seedAnchor.Add(time.Duration(step) * 15 * time.Second).UnixNano(),
	}
}

func labels(kv ...string) map[string]string {
	out := make(map[string]string, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func renderDDL(records []property.LogRecord) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE otel_logs (
    Timestamp DateTime64(9),
    SeverityText LowCardinality(String),
    SeverityNumber UInt8 DEFAULT 0,
    Body String,
    ResourceAttributes Map(LowCardinality(String), String),
    LogAttributes Map(String, String),
    ServiceName LowCardinality(String) DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT '',
    EventName LowCardinality(String) DEFAULT '',
    TraceId String DEFAULT '',
    SpanId String DEFAULT '',
    TraceFlags UInt8 DEFAULT 0
) ENGINE = MergeTree ORDER BY Timestamp;
INSERT INTO otel_logs (Timestamp, SeverityText, Body, ResourceAttributes, LogAttributes) VALUES `)
	for i, record := range records {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(toDateTime64('")
		b.WriteString(time.Unix(0, record.TimestampNanos).UTC().Format("2006-01-02 15:04:05.000"))
		b.WriteString("', 9), '")
		b.WriteString(escapeSQL(record.SeverityText))
		b.WriteString("', '")
		b.WriteString(escapeSQL(record.Body))
		b.WriteString("', ")
		b.WriteString(renderMap(record.ResourceAttributes))
		b.WriteString(", ")
		b.WriteString(renderMap(record.LogAttributes))
		b.WriteByte(')')
	}
	b.WriteString(";\n")
	return b.String()
}

func renderMap(values map[string]string) string {
	if len(values) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map(")
	for i, key := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('\'')
		b.WriteString(escapeSQL(key))
		b.WriteString("', '")
		b.WriteString(escapeSQL(values[key]))
		b.WriteByte('\'')
	}
	b.WriteByte(')')
	return b.String()
}

func escapeSQL(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
