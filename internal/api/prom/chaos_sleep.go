//go:build chaos_sleep

// This file is compiled ONLY into the chaos e2e lane's cerberus image
// (built with the `chaos_sleep` tag — see Dockerfile.local's GO_BUILD_TAGS
// arg). Production, compose-smoke, and every other CI lane link the no-op
// sibling (chaos_sleep_stub.go), so the header below is never read and the
// query semantics are byte-identical to a normal build.
//
// It turns the chaos `ch-slow-query-timeout` scenario DETERMINISTIC: the
// scenario sends an undocumented request header naming a server-side sleep
// duration; this hook threads it into the query context so the chsql
// emitter splices a genuinely-blocking ClickHouse sleep (see
// internal/chsql/chaos_sleep.go). A real server-side block is substrate-
// independent — unlike a "naturally slow" query, it blocks for a fixed
// duration regardless of compose-vs-k3d data volume or CPU.

package prom

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
)

// chaosSleepHeader is the undocumented request header the chaos scenario
// sets to request a server-side ClickHouse sleep of N seconds. It is read
// ONLY in the chaos_sleep build; in any other build this file is absent so
// the header is inert. A header (vs. a magic label matcher) keeps the
// scenario's PromQL clean and leaves the query semantics untouched.
const chaosSleepHeader = "X-Cerberus-Chaos-Sleep-Seconds"

// chaosCHExecutionCap is the ClickHouse-side max_execution_time the chaos
// hook narrows the data-plane query to when a sleep is injected. It must be
// STRICTLY LESS than the handler's Go context deadline (CERBERUS_QUERY_TIMEOUT,
// 5s on the chaos overlay) so ClickHouse aborts the blocking sleep with its
// own TIMEOUT_EXCEEDED (code 159) BEFORE the Go deadline fires.
//
// Why this ordering matters for the scenario's two assertions:
//   - code 159 is mapped by chclient to *QueryTimeoutError and the prom
//     handler renders it as 503 errorType=timeout (queryTimeoutAPIError) —
//     and the breaker counts it as a SUCCESS (breaker.record treats a typed
//     CH resource cap as proof CH is alive), so the breaker stays CLOSED
//     through the slow-query burst.
//   - the Go context.DeadlineExceeded path ALSO renders 503 errorType=timeout,
//     but breaker.record counts deadline expiry as a FAILURE — a burst of
//     those would trip the breaker and break the breaker-CLOSED assertion.
//
// Forcing CH (code 159) to win deterministically gives BOTH the asserted
// 503 errorType=timeout AND a breaker that stays CLOSED.
const chaosCHExecutionCap = 3 * time.Second

// applyChaosSleep reads the undocumented chaos sleep header and, when it
// names a positive duration:
//   - stamps the sleep seconds onto ctx so chsql.Emit splices a blocking
//     server-side ClickHouse sleep into the emitted SQL, and
//   - narrows the data-plane query's ClickHouse max_execution_time to
//     chaosCHExecutionCap (strictly below the Go deadline) so ClickHouse
//     aborts with code 159 (breaker-neutral) before the Go deadline fires.
//
// When the header is absent or non-positive the ctx is returned unchanged,
// so a trivial `up` query (which the scenario also fires) takes neither the
// sleep nor the narrowed cap and returns 200.
func (h *Handler) applyChaosSleep(ctx context.Context, r *http.Request) context.Context {
	raw := r.Header.Get(chaosSleepHeader)
	if raw == "" {
		return ctx
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return ctx
	}
	ctx = chsql.WithChaosSleepSeconds(ctx, seconds)
	ctx = chclient.WithQueryTimeout(ctx, chaosCHExecutionCap)
	return ctx
}
