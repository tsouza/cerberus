package seed

import (
	"context"
	"fmt"
	"time"

	logscollectorv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscollectorv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollectorv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The warm-up record set. The collector's clickhouseexporter is the sole
// schema authority in the Tier-1 stack, so the seeder's first action is to
// send it exactly one record per signal. That makes table creation
// unconditional — it no longer depends on the exporter's start-time behaviour
// — and it costs one row per table.
//
// Every warm-up name sits outside the fixture namespace, and every Tier-1
// corpus query names a fixture metric, stream or span explicitly, so these
// rows are unreachable from a comparison. They exist on the ClickHouse side
// only (the collector writes to ClickHouse, never to a reference backend), so
// a query that could reach them would read as a permanent divergence.
const (
	warmupMetric  = "cerberus_migration_schema_warmup"
	warmupJob     = "cerberus-migration-warmup"
	warmupService = "cerberus-migration-warmup"
	warmupSpan    = "schema_warmup"
	warmupBody    = "cerberus migration schema warm-up"
	// warmupValue is the single gauge point the warm-up metric carries. Its
	// magnitude is meaningless — only the row's existence matters.
	warmupValue = 1.0
	// warmupSpanDuration is the warm-up span's length.
	warmupSpanDuration = time.Millisecond
	// warmupSeverityNumber is OTLP severity INFO.
	warmupSeverityNumber = 9
	warmupSeverityText   = "INFO"
)

// WarmUpSchema emits one OTLP record per signal to the collector so its
// exporter provisions every table. It does NOT wait — callers follow it with
// WaitForTables, which is the assertion that the warm-up actually worked.
func WarmUpSchema(ctx context.Context, otlpAddr string, at time.Time) error {
	conn, err := grpc.NewClient(otlpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc client for %s: %w", otlpAddr, err)
	}
	defer func() { _ = conn.Close() }()

	resource := &resourcev1.Resource{
		Attributes: otlpAttributes(map[string]string{otelServiceNameKey: warmupService}),
	}
	scope := &commonv1.InstrumentationScope{Name: otlpScopeName, Version: otlpScopeVersion}

	callCtx, cancel := context.WithTimeout(ctx, otlpPushTimeout)
	defer cancel()

	metrics := metricscollectorv1.NewMetricsServiceClient(conn)
	if _, err := metrics.Export(callCtx, &metricscollectorv1.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpmetrics.ResourceMetrics{{
			Resource: resource,
			ScopeMetrics: []*otlpmetrics.ScopeMetrics{{
				Scope: scope,
				Metrics: []*otlpmetrics.Metric{{
					Name: warmupMetric,
					Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{
						DataPoints: []*otlpmetrics.NumberDataPoint{{
							TimeUnixNano: uint64(at.UnixNano()), //nolint:gosec // seeder-controlled wall clock, always positive
							Value:        &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: warmupValue},
						}},
					}},
				}},
			}},
		}},
	}); err != nil {
		return fmt.Errorf("otlp warm-up metrics export: %w", err)
	}

	logs := logscollectorv1.NewLogsServiceClient(conn)
	if _, err := logs.Export(callCtx, &logscollectorv1.ExportLogsServiceRequest{
		ResourceLogs: []*otlplogs.ResourceLogs{{
			Resource: resource,
			ScopeLogs: []*otlplogs.ScopeLogs{{
				Scope: scope,
				LogRecords: []*otlplogs.LogRecord{{
					TimeUnixNano:   uint64(at.UnixNano()), //nolint:gosec // seeder-controlled wall clock, always positive
					SeverityNumber: warmupSeverityNumber,
					SeverityText:   warmupSeverityText,
					Body: &commonv1.AnyValue{
						Value: &commonv1.AnyValue_StringValue{StringValue: warmupBody},
					},
					Attributes: otlpAttributes(map[string]string{jobLabel: warmupJob}),
				}},
			}},
		}},
	}); err != nil {
		return fmt.Errorf("otlp warm-up logs export: %w", err)
	}

	traces := tracecollectorv1.NewTraceServiceClient(conn)
	warmupTraceID := deriveTraceID(warmupService, 0)
	warmupSpanID := deriveSpanID(warmupService, 0, 0)
	if _, err := traces.Export(callCtx, &tracecollectorv1.ExportTraceServiceRequest{
		ResourceSpans: []*otlptrace.ResourceSpans{{
			Resource: resource,
			ScopeSpans: []*otlptrace.ScopeSpans{{
				Scope: scope,
				Spans: otlpSpans([]Span{{
					TraceID:    warmupTraceID,
					SpanID:     warmupSpanID,
					Name:       warmupSpan,
					Kind:       otlptrace.Span_SPAN_KIND_INTERNAL,
					StatusCode: otlptrace.Status_STATUS_CODE_OK,
					Start:      at,
					End:        at.Add(warmupSpanDuration),
				}}),
			}},
		}},
	}); err != nil {
		return fmt.Errorf("otlp warm-up traces export: %w", err)
	}
	return nil
}
