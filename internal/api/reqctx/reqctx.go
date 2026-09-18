// Package reqctx hosts the request-context derivation shared across all
// three heads' HTTP handlers — prom, loki, and tempo. Envelope shaping and
// error writing stay per-handler (each upstream API has its own wire
// format); what this package owns is the neutral plumbing those handlers
// run identically — today, the `?timeout=` budget resolution + context
// wiring.
//
// Tempo's own wire format has no `?timeout=` convention of its own (unlike
// Prometheus's `?timeout=<duration>`), so its entrypoints call
// ApplyQueryTimeout purely for the static configured default: the Go-side
// watchdog that unblocks a hung handler and releases its admit slot +
// pooled connection even if the server-side ClickHouse cap doesn't fire.
// The `?timeout=` resolution below still applies if a caller sends one to
// a Tempo route — nothing Tempo-specific rejects it — but no Tempo client
// does today, so in practice only the default ever caps a Tempo request.
package reqctx

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// ApplyQueryTimeout derives the request context a query handler runs
// under, honouring the upstream `?timeout=` query parameter. It resolves
// the effective wall-clock budget — the configured default min'd with
// the request's `?timeout=` (the smaller wins; 0 on either side means
// "no cap from that source") — and, when positive, threads it onto the
// returned context as BOTH:
//
//   - a context deadline (context.WithTimeout), so a query that hangs
//     past the budget unblocks the handler and releases its admit slot +
//     pooled connection even if the server-side cap somehow doesn't fire;
//     and
//   - chclient.WithQueryTimeout, so the data-plane query's ClickHouse
//     max_execution_time is narrowed to the same budget and the server
//     aborts the query with TIMEOUT_EXCEEDED (code 159) → *QueryTimeoutError.
//
// The caller MUST defer the returned cancel (a no-op when no deadline
// was installed). A `?timeout=` that does not parse yields a non-nil
// error the caller reports as 400 bad_data, as reference Prometheus does
// (web/api/v1's invalidParamError); on that path the returned context is
// the bare request context and cancel is a no-op.
//
// A `?timeout=` that parses to ZERO or a NEGATIVE duration is not
// rejected — reference Prometheus's parseDuration accepts both ("0",
// "-1") and its handlers install the deadline as given,
// `context.WithDeadline(ctx, now.Add(timeout))`, which has already passed
// by the time the query runs: the request answers 503 errorType=timeout.
// The same already-expired deadline is installed here, so the two
// degenerate inputs answer what the reference answers rather than a 400
// (negative) or an uncapped query (zero) of cerberus's own.
func ApplyQueryTimeout(r *http.Request, def time.Duration) (context.Context, context.CancelFunc, error) {
	ctx := r.Context()
	budget := def
	if raw := r.FormValue("timeout"); raw != "" {
		reqTimeout, err := format.ParseDuration(raw)
		if err != nil {
			return ctx, func() {}, fmt.Errorf("invalid parameter 'timeout': %w", err)
		}
		if reqTimeout <= 0 {
			ctx, cancel := context.WithDeadline(ctx, time.Now().Add(reqTimeout))
			return ctx, cancel, nil
		}
		budget = format.MinPositiveDuration(budget, reqTimeout)
	}
	ctx, cancel := WithQueryBudget(ctx, budget)
	return ctx, cancel, nil
}

// WithQueryBudget installs budget on ctx the way ApplyQueryTimeout does
// once the budget is resolved — the context deadline that unblocks a hung
// handler and releases its admit slot, plus chclient.WithQueryTimeout so
// the ClickHouse-side max_execution_time is narrowed to the same value —
// for the entrypoints that have no `?timeout=` to resolve: the Tempo gRPC
// RPCs (a stream.Context() carries whatever deadline the client set, and
// Grafana sets none) and the Prom / Loki metadata handlers (reference
// Prometheus reads no `?timeout=` on /labels). A budget <= 0 installs
// nothing and returns a no-op cancel; the caller MUST defer cancel.
func WithQueryBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, func() {}
	}
	ctx = chclient.WithQueryTimeout(ctx, budget)
	return context.WithTimeout(ctx, budget)
}
