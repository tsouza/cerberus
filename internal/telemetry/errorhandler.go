// Package telemetry: OpenTelemetry SDK error routing.
//
// The OTel SDK reports its own failures — a batch that could not be
// exported, a reader that could not collect, a shutdown that timed out
// — through a process-global error handler rather than by returning
// them to the caller. Left at the SDK default those errors bypass
// cerberus's structured logger entirely: they arrive as bare
// unstructured lines with no level and no component, which no log-based
// alert can select on and no `otel_logs` query can filter.
//
// That is exactly backwards for this class of failure. An export error
// means the gateway has LOST ITS OWN TELEMETRY: the metrics and traces
// an operator would use to diagnose the failure are the signals going
// missing. It has to be the loudest, most structured line cerberus
// writes, not the quietest.
package telemetry

import (
	"log/slog"

	"go.opentelemetry.io/otel"
)

// componentOTelSDK is the `component` log field stamped on every record
// the SDK error handler produces, matching the convention the rest of
// cerberus's subsystem loggers use (`logger.With("component", ...)`).
const componentOTelSDK = "otel"

// otelSDKErrorMsg is the message every SDK-reported failure carries.
// Fixed so an alert rule can match on it exactly.
const otelSDKErrorMsg = "otel: self-telemetry pipeline error"

// InstallErrorHandler routes OTel SDK-internal errors into logger at
// WARN, tagged `component=otel` and carrying the native error under the
// `err` key.
//
// WARN rather than INFO because the condition is actionable and
// degrading — telemetry is being dropped — and INFO is reserved in this
// codebase for lifecycle events, which nothing alerts on. WARN rather
// than ERROR because the export path is retried and a single failed
// batch does not mean a failed request; a sustained rate is what an
// operator should page on, and the fixed message plus component field
// make that rate expressible as a query.
//
// A nil logger falls back to the process default, resolved at call time
// so a later slog.SetDefault is picked up.
func InstallErrorHandler(logger *slog.Logger) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err == nil {
			return
		}
		l := logger
		if l == nil {
			l = slog.Default()
		}
		l.With("component", componentOTelSDK).Warn(otelSDKErrorMsg, "err", err)
	}))
}
