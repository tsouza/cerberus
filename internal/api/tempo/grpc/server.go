package grpc

import (
	"github.com/grafana/tempo/pkg/tempopb"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// NewServer builds the gRPC server that hosts cerberus's Tempo
// StreamingQuerier service. The returned *grpc.Server is ready to be
// passed to an h2c-wrapped HTTP listener (see cmd/cerberus/main.go) so
// gRPC and HTTP share one TCP port.
//
// The server is preconfigured with two cross-cutting hooks:
//
//   - otelgrpc.NewServerHandler() as the stats handler — every RPC
//     becomes an OTel server span on the same TracerProvider the HTTP
//     handlers use, and gets the standard gRPC server duration metric.
//     otelgrpc (pinned v0.69.0) defaults to semconv v1.41.0's STABLE
//     naming — `rpc.server.call.duration`, not the older
//     `rpc.server.duration` — unless OTEL_SEMCONV_STABILITY_OPT_IN
//     requests the legacy/dup mode (cerberus sets neither). The e2e
//     compose collector's `transform/metric_names` processor rewrites
//     dots to underscores before the point reaches ClickHouse, so the
//     PromQL-queryable series is `rpc_server_call_duration_count` /
//     `_sum` / `_bucket{le=...}` — see test/e2e/playwright/
//     tempo_grpc_streaming.spec.ts (#1454), which asserts against it.
//   - service.Limiter.StreamInterceptor() as the first stream
//     interceptor — per-RPC admission control sharing the same
//     per-head semaphore the HTTP handlers use, so a saturated Tempo
//     head rejects gRPC and HTTP traffic symmetrically (gRPC sees
//     codes.ResourceExhausted; HTTP sees 503 + Retry-After).
//   - queryTelemetryInterceptor(telemetry.QLTraceQL) as the second
//     stream interceptor — the gRPC counterpart of the
//     telemetry.QueryMiddleware every HTTP route already gets, so
//     cerberus_queries_total / cerberus_queries_duration_seconds cover
//     the streaming surface too (#1452). Listed after the limiter so
//     an admission-rejected RPC is not double-counted: only RPCs that
//     actually ran the query pipeline are recorded, mirroring
//     Limiter.Middleware wrapping QueryMiddleware on the HTTP side.
//
// A nil service is a programmer error and panics; the gRPC server
// requires a registered implementation to dispatch RPCs to.
func NewServer(service *Service) *grpc.Server {
	if service == nil {
		panic("tempo/grpc: NewServer requires a non-nil Service")
	}
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(
			service.Limiter.StreamInterceptor(),
			queryTelemetryInterceptor(telemetry.QLTraceQL),
		),
	)
	tempopb.RegisterStreamingQuerierServer(srv, service)
	return srv
}
