// Package telemetry: slog → OTel log bridge.
//
// `NewSlogHandler` returns the slog.Handler cerberus installs as the
// process default. It fans every record out to two sinks:
//
//  1. The local handler the operator configured (text or JSON to
//     stderr) — keeps `kubectl logs` / `docker logs` readable and
//     preserves the "factor XI" stream the 12-factor doc commits to.
//  2. An OTel slog bridge backed by the provided LoggerProvider —
//     ships the same record via OTLP gRPC to the collector, which
//     writes it to ClickHouse `otel_logs` alongside the metrics and
//     traces this binary emits.
//
// Without (2), cerberus would rely on the k8s container-log path
// (stderr → kubelet → filelog receiver → OTLP → CH) which requires a
// DaemonSet sidecar and round-trips through plain text, losing slog's
// structured attributes. The OTLP bridge keeps the attribute namespace
// (`cerberus.ql`, `cerberus.route`, etc.) intact end-to-end.
//
// When the LoggerProvider is the no-op (telemetry disabled), the
// bridge is a no-op too: every record still hits the local handler,
// nothing is exported.
package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
)

// slogScope is the otel.Logger instrumentation-scope name stamped on
// every record emitted via the bridge. Matches the package import path
// so a downstream query against `otel_logs` can filter on
// `ScopeName = 'github.com/tsouza/cerberus/internal/telemetry'`.
const slogScope = "github.com/tsouza/cerberus/internal/telemetry"

// NewSlogHandler returns the slog.Handler cerberus installs as the
// process default. `local` is the handler that writes to stderr (text
// or JSON, level-filtered per LogConfig). `provider` is the OTel
// LoggerProvider built by telemetry.New — pass the no-op provider if
// you only want the local handler.
//
// The returned handler runs both sinks unconditionally; level filter
// happens inside each sink, so a record that the local handler drops
// (e.g. below `info`) still reaches the OTLP bridge if it accepts the
// level. Callers who want symmetric filtering should set the same
// level on both sinks — config.NewLogger already does this via
// `LogConfig.Level`.
func NewSlogHandler(local slog.Handler, provider otellog.LoggerProvider) slog.Handler {
	if local == nil {
		// Defensive: a nil local handler would crash slog.New; install
		// a discard fallback so callers can pass `nil` to mean
		// "OTel-only".
		local = discardHandler{}
	}
	if provider == nil {
		return local
	}
	bridge := otelslog.NewHandler(slogScope, otelslog.WithLoggerProvider(provider))
	return fanoutHandler{handlers: []slog.Handler{local, bridge}}
}

// fanoutHandler dispatches every record to each wrapped handler in
// order, returning the first non-nil error.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, sub := range h.handlers {
		if sub.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var firstErr error
	for _, sub := range h.handlers {
		// Re-check enablement per sub-handler: a sub-handler may
		// filter below the level the top-level Enabled accepted.
		if !sub.Enabled(ctx, record.Level) {
			continue
		}
		// slog.Record's documented sharing semantics require a Clone
		// before mutating; sub-handlers may mutate (e.g. attribute
		// rewriting). Clone is cheap (a single slice copy).
		if err := sub.Handle(ctx, record.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	subs := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		subs[i] = sub.WithAttrs(attrs)
	}
	return fanoutHandler{handlers: subs}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	subs := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		subs[i] = sub.WithGroup(name)
	}
	return fanoutHandler{handlers: subs}
}

// discardHandler is the slog equivalent of io.Discard. Used when the
// caller passes a nil `local` to NewSlogHandler to signal "OTel-only".
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
