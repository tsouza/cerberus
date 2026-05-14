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

// Options configure Handler.
type Options struct {
	// Pinger is the ClickHouse health check. Required.
	Pinger Pinger

	// SchemaReady is consulted on every readiness check. When nil the
	// schema status is treated as ready (i.e. only the CH ping matters).
	SchemaReady SchemaReadyFunc

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
type Handler struct {
	pinger      Pinger
	schemaReady SchemaReadyFunc
	pingTimeout time.Duration
	cacheTTL    time.Duration
	now         func() time.Time

	mu         sync.Mutex
	cachedAt   time.Time
	cachedResp readyResponse
	cachedCode int
}

// readyResponse is the JSON shape /readyz returns.
type readyResponse struct {
	ClickHouse string `json:"clickhouse"`
	Schema     string `json:"schema"`
}

// New builds a Handler with the given options. A nil Pinger is allowed
// — the readiness probe will always report 503 in that case, which is
// the safe default if startup wiring forgot to plug a real client in.
func New(opts Options) *Handler {
	h := &Handler{
		pinger:      opts.Pinger,
		schemaReady: opts.SchemaReady,
		pingTimeout: opts.PingTimeout,
		cacheTTL:    opts.CacheTTL,
		now:         opts.Now,
	}
	if h.pingTimeout <= 0 {
		h.pingTimeout = time.Second
	}
	if h.cacheTTL == 0 {
		// 2s lines up with the typical k8s probe period (5Hz on the
		// hot path; ~3s for cerberus' own readinessProbe in
		// test/e2e/k3s/cerberus.yaml). Two seconds of coalescing keeps
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

// runCheck performs the actual ping + schema-ready evaluation.
func (h *Handler) runCheck(ctx context.Context) (readyResponse, int) {
	if h.pinger == nil {
		return readyResponse{
			ClickHouse: "error: no clickhouse client configured",
			Schema:     "unknown",
		}, http.StatusServiceUnavailable
	}

	pingCtx, cancel := context.WithTimeout(ctx, h.pingTimeout)
	defer cancel()

	if err := h.pinger.Ping(pingCtx); err != nil {
		return readyResponse{
			ClickHouse: "error: " + err.Error(),
			Schema:     "unknown",
		}, http.StatusServiceUnavailable
	}

	if h.schemaReady != nil && !h.schemaReady() {
		return readyResponse{
			ClickHouse: "ok",
			Schema:     "pending",
		}, http.StatusServiceUnavailable
	}

	return readyResponse{
		ClickHouse: "ok",
		Schema:     "ready",
	}, http.StatusOK
}
