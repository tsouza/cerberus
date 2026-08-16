//go:build chdb

package traceql

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

var seedAnchor = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

const rootParentID = ""

type spanSeed struct {
	traceID    string
	spanID     string
	parentID   string
	service    string
	cluster    string
	httpMethod string
	name       string
	startTime  time.Time
	durationNs int64
	statusCode string
}

// RichSeed returns a fixed OTel traces fixture with three traces and multiple
// parent chains. Its service, cluster, method, duration, and status spread
// gives selectors and per-trace aggregations non-trivial positive, negative,
// and boundary answers; the chains make child and descendant differ.
func RichSeed() (string, *property.MetricsModel) {
	spans := []spanSeed{
		span(0, "11111111111111111111111111111111", "1111111111111111", rootParentID, "api", "east", "GET", "GET /checkout/0", 10, "Ok"),
		span(1, "11111111111111111111111111111111", "1111111111111112", "1111111111111111", "web", "east", "POST", "POST /checkout/1", 120, "Error"),
		span(2, "11111111111111111111111111111111", "1111111111111113", "1111111111111112", "batch", "west", "PUT", "PUT /worker/2", 300, "Unset"),
		span(3, "22222222222222222222222222222222", "2222222222222221", rootParentID, "api", "west", "POST", "POST /auth/3", 50, "Error"),
		span(4, "22222222222222222222222222222222", "2222222222222222", "2222222222222221", "api", "west", "GET", "GET /auth/4", 300, "Ok"),
		span(5, "33333333333333333333333333333333", "3333333333333331", rootParentID, "web", "east", "PUT", "PUT /billing/5", 120, "Unset"),
		span(6, "33333333333333333333333333333333", "3333333333333332", "3333333333333331", "batch", "east", "GET", "GET /billing/6", 50, "Error"),
		span(7, "33333333333333333333333333333333", "3333333333333333", "3333333333333332", "web", "west", "POST", "POST /billing/7", 10, "Ok"),
	}
	return renderDDL(spans), spansModel(spans)
}

func span(step int, traceID, spanID, parentID, service, cluster, method, name string, durationMs int64, status string) spanSeed {
	return spanSeed{
		traceID: traceID, spanID: spanID, parentID: parentID,
		service: service, cluster: cluster, httpMethod: method, name: name,
		startTime:  seedAnchor.Add(time.Duration(step) * time.Second),
		durationNs: durationMs * int64(time.Millisecond), statusCode: status,
	}
}

func spansModel(spans []spanSeed) *property.MetricsModel {
	series := make([]property.SeriesData, 0, len(spans))
	for _, item := range spans {
		series = append(series, property.SeriesData{
			MetricName: item.name,
			Labels: map[string]string{
				"resource.service.name": item.service,
				"resource.cluster":      item.cluster,
				"span.http.method":      item.httpMethod,
				"__name__":              item.name,
				"__traceID__":           item.traceID,
				"__spanID__":            item.spanID,
				"__parentSpanID__":      item.parentID,
				"__duration_ns__":       fmt.Sprintf("%d", item.durationNs),
				"__status__":            item.statusCode,
			},
			Points: []property.Point{{TimestampMs: item.startTime.UnixMilli(), Value: float64(item.durationNs)}},
		})
	}
	return &property.MetricsModel{Series: series}
}

func renderDDL(spans []spanSeed) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE otel_traces (
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
INSERT INTO otel_traces VALUES `)
	for i, item := range spans {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(toDateTime64('%s', 9), %s, %s, %s, %s, 'Internal', %s, %s, %s, %d, %s, '', '', '')",
			item.startTime.UTC().Format("2006-01-02 15:04:05.000"),
			quoteSQL(item.traceID), quoteSQL(item.spanID), quoteSQL(item.parentID),
			quoteSQL(item.name), quoteSQL(item.service),
			renderMap(map[string]string{"service.name": item.service, "cluster": item.cluster}),
			renderMap(map[string]string{"http.method": item.httpMethod}),
			item.durationNs, quoteSQL(item.statusCode))
	}
	b.WriteString(";\n")
	return b.String()
}

func renderMap(values map[string]string) string {
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
		b.WriteString(quoteSQL(key))
		b.WriteString(", ")
		b.WriteString(quoteSQL(values[key]))
	}
	b.WriteByte(')')
	return b.String()
}

func quoteSQL(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "\\'") + "'"
}
