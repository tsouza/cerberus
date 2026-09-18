// Package grpc implements cerberus's Tempo StreamingQuerier gRPC
// service — the streaming sibling of the existing
// internal/api/tempo HTTP handler. Grafana's Tempo datasource opens a
// long-lived gRPC stream against cerberus when the user enables the
// "Streaming" toggle, then accumulates frames as incremental progress;
// the same TraceQL/metrics queries the HTTP handler answers eagerly
// stream incrementally over this surface.
//
// Wire-format generated stubs live in github.com/grafana/tempo/pkg/
// tempopb — the plain Apache-2.0 upstream wire types, a direct require
// in go.mod (no fork). This package only depends on the generated server
// interface + the cerberus query pipeline (Engine, schema, admit
// limiter); no protoc plumbing is needed in tree.
//
// Every RPC answers in a SINGLE frame: the result is drained eagerly
// through the same pipeline the HTTP handler runs and sent once, because
// the matrix / instant pivots and the search summaries need the full row
// set in hand before they can produce a well-formed envelope. Service
// embeds tempopb.UnimplementedStreamingQuerierServer so an RPC this
// package has not implemented answers codes.Unimplemented, which Grafana's
// streaming-on toggle falls back from to HTTP cleanly.
package grpc

import (
	"context"
	"log/slog"

	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/reqctx"
	"github.com/tsouza/cerberus/internal/api/tempo"
)

// Service is the cerberus implementation of tempopb.StreamingQuerierServer.
// It embeds tempopb.UnimplementedStreamingQuerierServer so an RPC it does
// not implement returns codes.Unimplemented; the implemented RPCs
// (search.go, tags.go, metrics.go) forward to the cerberus query pipeline.
//
// Service holds references to the HTTP-side Handler (for the shared
// Engine + schema + lang) and the per-head admit Limiter, but adds no
// caching or per-request state of its own. Goroutine-safe by
// construction: every field is read-only after construction.
type Service struct {
	tempopb.UnimplementedStreamingQuerierServer

	// Handler is the existing Tempo HTTP handler; its Engine,
	// Schema, lang and QueryTimeout fields are the source of truth for
	// the query pipeline. The RPCs reuse the same parse + lower + emit +
	// execute path the HTTP handlers use, then marshal results into
	// tempopb response messages instead of cerberus's JSON envelope.
	Handler *tempo.Handler

	// Limiter is the per-head admission-control limiter — the same
	// one wired into Handler.Limiter for the HTTP surface. The
	// gRPC server wires it via StreamInterceptor at construction
	// time; this field is kept as a back-reference so a per-RPC code
	// path (custom Acquire/weighted slots) has access to it without
	// re-plumbing.
	Limiter *admit.Limiter

	// Logger fans out to stderr + the OTLP slog bridge; carries
	// the `api=tempo-grpc` attribute so dashboards can split gRPC
	// log volume from HTTP.
	Logger *slog.Logger
}

// NewService constructs a Service wired to the given Tempo HTTP
// Handler. The Service shares the Handler's Engine + schema so the
// HTTP and gRPC surfaces produce identical query results for the same
// TraceQL input — gRPC is purely a wire-format alternative, not a
// behavioural fork.
//
// limiter and logger may be nil — a nil limiter falls through to
// pass-through (admission control disabled), and a nil logger is
// replaced with slog.Default() so the Service is always safe to call.
func NewService(handler *tempo.Handler, limiter *admit.Limiter, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		Handler: handler,
		Limiter: limiter,
		Logger:  logger,
	}
}

// queryContext derives the context an RPC's query pipeline runs under from
// the stream's own context: the Handler's configured QueryTimeout is
// installed on it exactly as every Tempo HTTP entrypoint installs it
// (reqctx.WithQueryBudget — the Go-side watchdog that unblocks a hung
// ClickHouse call and releases the admit slot, plus the narrowed
// ClickHouse max_execution_time). A stream.Context() carries only the
// deadline the CLIENT set, and Grafana's streaming datasource sets none,
// so without this a hung backend held a slot of the limiter this service
// shares with the HTTP surface until the driver's own read timeout fired.
// The caller MUST defer cancel.
func (s *Service) queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return reqctx.WithQueryBudget(ctx, s.Handler.QueryTimeout)
}
