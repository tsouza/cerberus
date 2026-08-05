package grpc

import (
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// queryTelemetryInterceptor is the gRPC-transport counterpart of
// telemetry.QueryMiddleware, which wraps every Prom/Loki/Tempo HTTP
// route but has no reach into this package's gRPC StreamingQuerier
// service (#1452). Before this interceptor existed, none of the six
// implemented RPCs — Search, the four tag-list RPCs, the two metrics
// RPCs — recorded a query outcome at all: a client running exclusively
// through Grafana's gRPC/h2c "Streaming" toggle was invisible to
// cerberus_queries_total / cerberus_queries_duration_seconds, so any
// dashboard or alert built on those instruments silently excluded the
// whole streaming surface.
//
// Wired once at server construction (see NewServer) rather than inside
// each RPC method, mirroring QueryMiddleware's single-wiring-point
// pattern: a stream interceptor can't drift out of sync as new RPCs
// land, unlike a telemetry call scattered across every method body.
//
// ql is the AttrQL label value — always telemetry.QLTraceQL for this
// service; threaded as a parameter (rather than hard-coded) so the
// interceptor stays testable without standing up the full Service.
//
// The route label is info.FullMethod (e.g.
// "/tempopb.StreamingQuerier/Search") — a closed, seven-member set
// fixed by the tempopb.StreamingQuerier service definition, so
// cardinality is bounded the same way AttrRoute is for the HTTP mux
// pattern.
//
// Placed after service.Limiter.StreamInterceptor() in NewServer's
// ChainStreamInterceptor list, so an admission-rejected RPC (saturated
// head, codes.ResourceExhausted) never reaches this interceptor — the
// same behaviour the HTTP path gets from Limiter.Middleware wrapping
// QueryMiddleware, not the other way around. Only RPCs that actually
// ran the query pipeline are counted as a query outcome.
func queryTelemetryInterceptor(ql string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		t := telemetry.ObserveQuery(ql, info.FullMethod)
		err := handler(srv, ss)
		t.Done(ss.Context(), telemetry.ClassifyStatus(grpcCodeToHTTPStatus(status.Code(err))))
		return err
	}
}

// grpcCodeToHTTPStatus reverses grpcCodeFor's table (see errclass.go's
// HTTP↔gRPC correspondence comment) so a gRPC status code classifies
// through the exact same telemetry.ClassifyStatus the HTTP
// QueryMiddleware uses — guaranteeing an identical (result, reason,
// status_class) telemetry triple for the same underlying pipeline
// error regardless of which transport the client used, rather than
// maintaining a second, independently-drifting closed-enum mapping.
//
// Two HTTP statuses collapse onto a single gRPC code in the forward
// direction (400 Bad Request + 422 Unprocessable Entity both encode as
// InvalidArgument; 502 Bad Gateway + 503 Service Unavailable both
// encode as Unavailable), so this reverse mapping picks one
// representative status per code. That's lossless for telemetry
// purposes: both members of each collapsed pair land in the same
// reason bucket under telemetry.ClassifyStatus (4xx -> ReasonBadRequest,
// 5xx's 502/503 -> ReasonBackendUnavailable), so the choice of
// representative never changes the recorded Outcome.
//
// codes.OK needs no case — status.Code(nil) already returns codes.OK,
// and http.StatusOK classifies as ResultOK via ClassifyStatus's
// status < http.StatusBadRequest short-circuit — so the default arm
// (http.StatusInternalServerError) only ever fires for a genuine error
// code, never for success.
func grpcCodeToHTTPStatus(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.ResourceExhausted:
		return http.StatusUnprocessableEntity
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.Canceled:
		return tempo.StatusClientClosedRequest
	default:
		return http.StatusInternalServerError
	}
}
