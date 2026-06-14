//go:build !chaos_sleep

package prom

import (
	"context"
	"net/http"
)

// applyChaosSleep is the production (default-build) no-op: it returns the
// request ctx unchanged and never inspects the request. The deterministic-
// chaos sleep trigger (an undocumented request header read into the query
// context) lives only in the `chaos_sleep`-tagged sibling (chaos_sleep.go),
// so production and every non-chaos CI lane compile this hook out to a
// pass-through — no header is read and no query semantics change.
func (h *Handler) applyChaosSleep(ctx context.Context, _ *http.Request) context.Context {
	return ctx
}
