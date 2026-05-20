// Package grpc implements cerberus's Tempo StreamingQuerier gRPC
// service — the streaming sibling of the existing
// internal/api/tempo HTTP handler. Grafana's Tempo datasource opens a
// long-lived gRPC stream against cerberus when the user enables the
// "Streaming" toggle, then accumulates frames as incremental progress;
// the same TraceQL/metrics queries the HTTP handler answers eagerly
// stream incrementally over this surface.
//
// Wire-format generated stubs live in github.com/grafana/tempo/pkg/
// tempopb (routed through the tsouza/tempo:cerberus-accessors fork —
// see go.mod). This package only depends on the generated server
// interface + the cerberus query pipeline (Engine, schema, admit
// limiter); no protoc plumbing is needed in tree.
//
// This file ships the SCAFFOLD slice of the Tempo gRPC rollout (PR 1
// of a 4-PR sequence; see .claude/plans/tempo-grpc-streaming-design.md
// §6). All seven RPCs are advertised but every method returns
// codes.Unimplemented via the embedded UnimplementedStreamingQuerierServer.
// PR 2 fills in Search, PR 3 fills in the tag-list RPCs, PR 4 fills in
// the metrics RPCs. The scaffold is shippable on its own — Grafana's
// streaming-on toggle falls back to HTTP cleanly when the gRPC client
// receives Unimplemented, so the rollout can land incrementally without
// breaking the datasource.
package grpc

import (
	"log/slog"

	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/tempo"
)

// Service is the cerberus implementation of tempopb.StreamingQuerierServer.
// It embeds tempopb.UnimplementedStreamingQuerierServer so every RPC
// returns codes.Unimplemented by default — PRs 2/3/4 override
// individual methods to forward to the cerberus query pipeline.
//
// Service holds references to the HTTP-side Handler (for the shared
// Engine + schema + lang) and the per-head admit Limiter, but adds no
// caching or per-request state of its own. Goroutine-safe by
// construction: every field is read-only after construction.
type Service struct {
	tempopb.UnimplementedStreamingQuerierServer

	// Handler is the existing Tempo HTTP handler; its Engine,
	// Schema, and lang fields are the source of truth for the
	// query pipeline. Real RPC implementations (added in PR 2-4)
	// reuse the same parse + lower + emit + execute path the HTTP
	// handlers use, then marshal results into tempopb response
	// messages instead of cerberus's JSON envelope.
	Handler *tempo.Handler

	// Limiter is the per-head admission-control limiter — the same
	// one wired into Handler.Limiter for the HTTP surface. The
	// gRPC server wires it via StreamInterceptor at construction
	// time; this field is kept as a back-reference so per-RPC code
	// paths added in later PRs (custom Acquire/weighted slots)
	// have access to it without re-plumbing.
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
