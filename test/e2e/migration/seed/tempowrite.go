package seed

import (
	"context"
	"fmt"
	"sort"
	"time"

	tracecollectorv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Span is one span of a fixture trace. Identifiers are fixed-width arrays so a
// caller cannot hand over a short id that a backend would silently pad.
type Span struct {
	TraceID      [16]byte
	SpanID       [8]byte
	ParentSpanID [8]byte
	Name         string
	Kind         otlptrace.Span_SpanKind
	StatusCode   otlptrace.Status_StatusCode
	Start        time.Time
	End          time.Time
	Attributes   map[string]string
}

// Trace is one trace: the spans plus the service that emitted them. The
// service name rides as the OTLP `service.name` resource attribute, which is
// what both Tempo and the OTel-CH schema key `resource.service.name` on.
type Trace struct {
	ServiceName string
	Spans       []Span
}

// otlpPushTimeout bounds one OTLP Export call.
const otlpPushTimeout = 30 * time.Second

// PushTraces exports traces to an OTLP/gRPC endpoint over plaintext, one
// ExportTraceServiceRequest per trace so each message stays far below the 4MB
// default limit. Plaintext is correct here: the Tier-1 Tempo exposes :4317
// with no TLS, matching upstream's getting-started config.
func PushTraces(ctx context.Context, addr string, traces []Trace) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc client for %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	client := tracecollectorv1.NewTraceServiceClient(conn)
	for _, t := range traces {
		req := &tracecollectorv1.ExportTraceServiceRequest{
			ResourceSpans: []*otlptrace.ResourceSpans{{
				Resource: &resourcev1.Resource{
					Attributes: otlpAttributes(map[string]string{"service.name": t.ServiceName}),
				},
				ScopeSpans: []*otlptrace.ScopeSpans{{
					Scope: &commonv1.InstrumentationScope{Name: otlpScopeName, Version: otlpScopeVersion},
					Spans: otlpSpans(t.Spans),
				}},
			}},
		}
		callCtx, cancel := context.WithTimeout(ctx, otlpPushTimeout)
		_, err := client.Export(callCtx, req)
		cancel()
		if err != nil {
			return fmt.Errorf("otlp export to %s: %w", addr, err)
		}
	}
	return nil
}

// The instrumentation scope every fixture span carries. The OTel-CH exporter
// writes it into ScopeName / ScopeVersion, so a ClickHouse-side write of the
// same fixture must mirror these exact values for TraceQL's
// `instrumentation:name` / `instrumentation:version` intrinsics to agree.
const (
	otlpScopeName    = "cerberus-migration-seed"
	otlpScopeVersion = "1"
)

func otlpSpans(in []Span) []*otlptrace.Span {
	out := make([]*otlptrace.Span, 0, len(in))
	for _, s := range in {
		span := &otlptrace.Span{
			TraceId:           append([]byte(nil), s.TraceID[:]...),
			SpanId:            append([]byte(nil), s.SpanID[:]...),
			Name:              s.Name,
			Kind:              s.Kind,
			StartTimeUnixNano: uint64(s.Start.UnixNano()), //nolint:gosec // fixture-controlled wall-clock ns, always positive
			EndTimeUnixNano:   uint64(s.End.UnixNano()),   //nolint:gosec // fixture-controlled wall-clock ns, always positive
			Attributes:        otlpAttributes(s.Attributes),
			Status:            &otlptrace.Status{Code: s.StatusCode},
		}
		if s.ParentSpanID != ([8]byte{}) {
			span.ParentSpanId = append([]byte(nil), s.ParentSpanID[:]...)
		}
		out = append(out, span)
	}
	return out
}

// otlpAttributes converts a string map into OTLP's repeated KeyValue shape,
// sorted by key so the encoding is byte-deterministic across runs.
func otlpAttributes(m map[string]string) []*commonv1.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonv1.KeyValue, 0, len(m))
	for _, k := range keys {
		out = append(out, &commonv1.KeyValue{
			Key:   k,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: m[k]}},
		})
	}
	return out
}
