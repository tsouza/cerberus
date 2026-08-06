// Package reqctx hosts the request-context derivation shared across the
// prom / loki / tempo HTTP handlers. Envelope shaping and error writing
// stay per-handler (each upstream API has its own wire format); what
// this package owns is the neutral plumbing every handler runs
// identically — today, the `?timeout=` budget resolution + context
// wiring.
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
// was installed). A malformed `?timeout=` yields a non-nil
// error the caller reports as 400 bad_data (matching upstream); on that
// path the returned context is the bare request context and cancel is a
// no-op.
func ApplyQueryTimeout(r *http.Request, def time.Duration) (context.Context, context.CancelFunc, error) {
	ctx := r.Context()
	budget := def
	if raw := r.FormValue("timeout"); raw != "" {
		reqTimeout, err := format.ParseDuration(raw)
		if err != nil || reqTimeout < 0 {
			return ctx, func() {}, fmt.Errorf("invalid parameter 'timeout': %w", err)
		}
		budget = format.MinPositiveDuration(budget, reqTimeout)
	}
	if budget <= 0 {
		return ctx, func() {}, nil
	}
	ctx = chclient.WithQueryTimeout(ctx, budget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	return ctx, cancel, nil
}
