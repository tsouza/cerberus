package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Pinger is the subset of *chclient.Client the readiness probe needs.
// Stubbing it makes the unit test pure — no live ClickHouse required.
type Pinger interface {
	Ping(ctx context.Context) error
}

// SchemaReadyFunc reports whether the auto-create-schema startup hook
// has finished at least one successful run. When auto-create is off
// the wiring passes a func that returns true so readiness only gates
// on the ClickHouse ping.
type SchemaReadyFunc func() bool

// SchemaPresentFunc reports whether the configured schema has been
// provisioned (every required table exists). When the schema is not yet
// present it returns present=false plus a precise reason for the /readyz
// body (e.g. "schema not yet provisioned: table otel_logs absent"). It
// models the absent-schema startup race: a cerberus deployed alongside the
// otel-collector that owns schema creation can boot before any table
// exists, so readiness waits for the external writer rather than the
// process crash-looping. When nil the schema is treated as present.
type SchemaPresentFunc func() (present bool, reason string)

// HeadBreakersFunc reports the circuit-breaker phase of every query head THIS
// process actually serves, keyed by head name ("prom" / "loki" / "tempo") and
// valued with the stable phase vocabulary "closed" / "open" / "half-open".
//
// The keys ARE the enablement set: cmd/cerberus builds the map from
// CERBERUS_ENABLED_HEADS, so a head this process does not serve is simply
// absent and can never hold the pod out of its Service. A nil func (or an empty
// map) leaves readiness on the ClickHouse + schema conditions alone.
type HeadBreakersFunc func() map[string]string

// breakerOpen is the one phase that makes a head unable to answer: an OPEN
// breaker fast-fails every query it fronts. "half-open" is a recovering head
// that admits a probe, and "closed" is healthy — neither is a serving failure,
// so only this value counts against readiness.
const breakerOpen = "open"

// Options configure Handler.
type Options struct {
	// Pinger is the ClickHouse health check. Required.
	Pinger Pinger

	// HeadBreakers is consulted on every readiness check. Its result is
	// reported verbatim in the `heads` object of the /readyz body — the
	// per-head circuit-breaker state that the aggregate ClickHouse ping
	// cannot express — and drives the head-exhaustion gate described on
	// Handler.
	//
	// When nil the body carries no `heads` object and readiness gates on the
	// ClickHouse ping + schema conditions only.
	HeadBreakers HeadBreakersFunc

	// SchemaReady is consulted on every readiness check. When nil the
	// schema status is treated as ready (i.e. only the CH ping matters).
	SchemaReady SchemaReadyFunc

	// SchemaPresent is consulted on every readiness check, BEFORE
	// SchemaReady. When it reports not-present the probe returns 503 with
	// the absent reason — the schema has not been provisioned yet and
	// cerberus waits (no restart) for the external writer to create it.
	// When nil the schema is treated as present.
	SchemaPresent SchemaPresentFunc

	// PingTimeout caps the per-probe ClickHouse ping. Defaults to 1s.
	PingTimeout time.Duration

	// CacheTTL coalesces probe results so high-frequency Kubernetes
	// probes (default 5Hz) do not run a fresh CH ping on every call.
	// Defaults to 2s. Set < 0 to disable caching (tests).
	CacheTTL time.Duration

	// Now is the time source. Defaults to time.Now. Tests inject a
	// fake clock to verify TTL behavior deterministically.
	Now func() time.Time
}

// Handler exposes /healthz (liveness) and /readyz (readiness) HTTP
// handlers. Construct via New and register via Mount.
//
// # Per-head readiness
//
// Each query head fronts ClickHouse through its OWN circuit breaker
// (chclient.Head), so "is cerberus able to serve" is a per-head question and
// the probe answers it as one. Two rules follow from that, and they are the
// whole of the head contract:
//
//   - The `heads` object of the /readyz body names every head this process
//     serves and its live breaker phase, so an OPEN head is visible on the
//     probe surface even when the pod stays in its Service. Without it a
//     tripped head is observable only in metrics.
//   - The pod goes NOT-ready when EVERY head it serves has an OPEN breaker —
//     the point at which it can answer nothing and belongs out of the Service.
//     Under the split deployment mode (one Deployment per head,
//     CERBERUS_ENABLED_HEADS naming a single head) that is exactly "this
//     head's breaker is OPEN", which is the signal Kubernetes needs to stop
//     routing to a degraded head. Under the combined mode one tripped head
//     leaves the pod ready, because evicting it would take the other,
//     healthy heads out of their Services with it — the same per-head
//     blast-radius isolation the breakers themselves exist to provide.
type Handler struct {
	pinger        Pinger
	schemaReady   SchemaReadyFunc
	schemaPresent SchemaPresentFunc
	headBreakers  HeadBreakersFunc
	pingTimeout   time.Duration
	cacheTTL      time.Duration
	now           func() time.Time

	mu         sync.Mutex
	cachedAt   time.Time
	cachedResp readyResponse
	cachedCode int
}

// readyResponse is the JSON shape /readyz returns.
type readyResponse struct {
	ClickHouse string `json:"clickhouse"`
	Schema     string `json:"schema"`
	// Heads maps each served head name to its circuit-breaker phase. Omitted
	// when no HeadBreakersFunc is wired (the probe then reports only the
	// aggregate ClickHouse + schema conditions).
	Heads map[string]string `json:"heads,omitempty"`
}

// New builds a Handler with the given options. A nil Pinger is allowed
// — the readiness probe will always report 503 in that case, which is
// the safe default if startup wiring forgot to plug a real client in.
func New(opts Options) *Handler {
	h := &Handler{
		pinger:        opts.Pinger,
		schemaReady:   opts.SchemaReady,
		schemaPresent: opts.SchemaPresent,
		headBreakers:  opts.HeadBreakers,
		pingTimeout:   opts.PingTimeout,
		cacheTTL:      opts.CacheTTL,
		now:           opts.Now,
	}
	if h.pingTimeout <= 0 {
		h.pingTimeout = time.Second
	}
	if h.cacheTTL == 0 {
		// 2s lines up with the typical k8s probe period (5Hz on the
		// hot path; ~3s for cerberus' own readinessProbe in
		// test/e2e/k3s/cerberus-values.yaml). Two seconds of coalescing keeps
		// CH load near zero while still surfacing outages within one
		// probe period.
		h.cacheTTL = 2 * time.Second
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Mount registers /healthz and /readyz on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
}

// handleHealthz is the liveness probe. It must not touch any external
// dependency: a failure here causes k8s to restart the pod.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is the readiness probe. Coalesces concurrent probes via
// a small TTL cache, then writes a JSON body describing the CH ping
// and the schema-startup invariant.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	resp, code := h.checkReady(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// checkReady returns the response body + HTTP status code. Cached for
// up to cacheTTL; an in-flight probe holds the mutex so concurrent
// callers see one CH ping per TTL window.
func (h *Handler) checkReady(ctx context.Context) (readyResponse, int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cacheTTL > 0 && !h.cachedAt.IsZero() {
		if h.now().Sub(h.cachedAt) < h.cacheTTL {
			return h.cachedResp, h.cachedCode
		}
	}

	resp, code := h.runCheck(ctx)

	h.cachedResp = resp
	h.cachedCode = code
	h.cachedAt = h.now()
	return resp, code
}

// runCheck performs the actual ping + head-breaker + schema-ready evaluation.
// The per-head breaker phases are read FIRST and stamped on every response
// shape below, including the failure ones: an operator debugging a red probe
// needs to see which heads are tripped regardless of which condition tipped it.
func (h *Handler) runCheck(ctx context.Context) (readyResponse, int) {
	heads := h.readHeadBreakers()

	if h.pinger == nil {
		return readyResponse{
			ClickHouse: "error: no clickhouse client configured",
			Schema:     "unknown",
			Heads:      heads,
		}, http.StatusServiceUnavailable
	}

	pingCtx, cancel := context.WithTimeout(ctx, h.pingTimeout)
	defer cancel()

	if err := h.pinger.Ping(pingCtx); err != nil {
		return readyResponse{
			ClickHouse: "error: " + err.Error(),
			Schema:     "unknown",
			Heads:      heads,
		}, http.StatusServiceUnavailable
	}

	// Head exhaustion: ClickHouse answers the probe, but every head this
	// process serves fast-fails on its own OPEN breaker, so the pod can
	// answer no query at all and belongs out of its Service. See Handler for
	// why the gate is "every head" rather than "any head".
	if allHeadsOpen(heads) {
		return readyResponse{
			ClickHouse: "ok",
			Schema:     "unknown",
			Heads:      heads,
		}, http.StatusServiceUnavailable
	}

	// Absent-schema gate: a schema that has not been provisioned yet
	// (the cerberus+collector startup race) reports the precise absent
	// reason so the operator sees WHY readiness is held, distinct from the
	// auto-create "pending" state below.
	if h.schemaPresent != nil {
		if present, reason := h.schemaPresent(); !present {
			schema := "absent"
			if reason != "" {
				schema = "absent: " + reason
			}
			return readyResponse{
				ClickHouse: "ok",
				Schema:     schema,
				Heads:      heads,
			}, http.StatusServiceUnavailable
		}
	}

	if h.schemaReady != nil && !h.schemaReady() {
		return readyResponse{
			ClickHouse: "ok",
			Schema:     "pending",
			Heads:      heads,
		}, http.StatusServiceUnavailable
	}

	return readyResponse{
		ClickHouse: "ok",
		Schema:     "ready",
		Heads:      heads,
	}, http.StatusOK
}

// readHeadBreakers snapshots the served heads' breaker phases, or nil when no
// HeadBreakersFunc is wired. An empty map is normalised to nil so the `heads`
// key is omitted rather than rendered as `{}`.
func (h *Handler) readHeadBreakers() map[string]string {
	if h.headBreakers == nil {
		return nil
	}
	states := h.headBreakers()
	if len(states) == 0 {
		return nil
	}
	return states
}

// allHeadsOpen reports whether every served head's breaker is OPEN. An empty /
// absent map is NOT exhaustion — a process that reports no heads has nothing to
// exhaust, and must not be evicted on the strength of an unwired probe.
func allHeadsOpen(heads map[string]string) bool {
	if len(heads) == 0 {
		return false
	}
	for _, state := range heads {
		if state != breakerOpen {
			return false
		}
	}
	return true
}
