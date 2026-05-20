package grpc

import (
	"github.com/grafana/tempo/pkg/tempopb"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
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
//     handlers use, and gets the standard set of gRPC metrics
//     (`rpc.server.duration`, `rpc.server.request.size`, etc.).
//   - service.Limiter.StreamInterceptor() as the stream interceptor —
//     per-RPC admission control sharing the same per-head semaphore
//     the HTTP handlers use, so a saturated Tempo head rejects gRPC
//     and HTTP traffic symmetrically (gRPC sees codes.ResourceExhausted;
//     HTTP sees 503 + Retry-After).
//
// A nil service is a programmer error and panics; the gRPC server
// requires a registered implementation to dispatch RPCs to.
func NewServer(service *Service) *grpc.Server {
	if service == nil {
		panic("tempo/grpc: NewServer requires a non-nil Service")
	}
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(service.Limiter.StreamInterceptor()),
	)
	tempopb.RegisterStreamingQuerierServer(srv, service)
	return srv
}
