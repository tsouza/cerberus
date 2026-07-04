// Package info serves GET /info — a cerberus-native, unauthenticated
// metadata/health/connection fingerprint of the running process. It is a
// sibling to /healthz + /readyz (internal/api/health) and is deliberately NOT
// an upstream-compat surface: the Prometheus/Loki buildinfo endpoints
// (/api/v1/status/buildinfo, /loki/api/v1/status/buildinfo) mirror the
// reference backends byte-for-byte and must stay faithful, so cerberus's own
// build/config/optimization fingerprint lives here at the top level instead.
//
// The handler is a pure leaf: it holds only the static Snapshot captured at
// boot plus a small set of closures that read live state (ClickHouse
// reachability, the circuit-breaker phase, schema readiness, and overall
// readiness). cmd/cerberus builds the Snapshot from config + chopt + chclient
// and injects the live funcs, so this package depends on no other internal
// layer — exactly like internal/api/health.
package info

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Snapshot is the immutable, boot-captured portion of the /info fingerprint:
// build identity, enabled heads, the ClickHouse address/database, and the
// resolved optimization decision. The live portion (reachability, breaker
// phase, schema readiness, overall readiness) is supplied separately via the
// closures on Options and re-read on every request.
type Snapshot struct {
	// Service is the constant service identifier ("cerberus").
	Service string
	// Version is main.Version (the goreleaser ldflag; "dev" in dev builds).
	Version string
	// Revision is the VCS commit (runtime/debug ReadBuildInfo vcs.revision),
	// or "unknown" when the build carries no VCS stamp.
	Revision string
	// GoVersion is runtime.Version().
	GoVersion string
	// Heads is the set of ENABLED query heads (CERBERUS_ENABLED_HEADS),
	// e.g. ["prom","loki","tempo"], in a stable order.
	Heads []string

	// CHAddress is the configured ClickHouse endpoint (host:port).
	CHAddress string
	// CHDatabase is the configured ClickHouse database.
	CHDatabase string
	// ServerVersion is the resolved ClickHouse server version as
	// "<major>.<minor>" — either probed live at boot or, when the probe
	// failed, the assumed supported floor (see ServerVersionSource).
	ServerVersion string
	// ServerVersionSource is "probe" when the boot-time version probe
	// succeeded, or "fallback" when it failed and the 24.8 supported floor
	// was assumed.
	ServerVersionSource string

	// OptSelection is the raw CERBERUS_CH_OPTIMIZATIONS selection
	// (e.g. "auto", "auto,columnar_result_decode", "off").
	OptSelection string
	// OptMode is the resolution mode ("enforcing" | "permissive").
	OptMode string
	// OptResolvedAgainstVersion is the version the auto-picker resolved the
	// selection against ("<major>.<minor>"). It equals ServerVersion.
	OptResolvedAgainstVersion string
	// OptEnabled is the EFFECTIVELY ENABLED feature ids (chopt EnabledSet
	// IDs) — the headline field: it makes plain whether cerberus is running
	// the optimizations it should.
	OptEnabled []string
}

const (
	// ServerVersionSourceProbe marks a server version read live at boot.
	ServerVersionSourceProbe = "probe"
	// ServerVersionSourceFallback marks the assumed supported floor used when
	// the boot-time version probe failed.
	ServerVersionSourceFallback = "fallback"
)

// Options configure Handler.
type Options struct {
	// Snapshot is the static, boot-captured fingerprint. Required.
	Snapshot Snapshot

	// Reachable reports whether ClickHouse is reachable right now. It is the
	// same ping the /readyz probe issues, but reported as a plain bool here.
	// When nil, reachability is reported false.
	Reachable func(ctx context.Context) bool

	// Breaker reports the ClickHouse circuit-breaker phase right now — one of
	// "closed" | "open" | "half-open". When nil, "closed" is reported (the
	// zero-value breaker is always closed).
	Breaker func() string

	// SchemaReady reports whether the schema is provisioned + the auto-create
	// hook has completed. When nil, true is reported.
	SchemaReady func() bool

	// Ready reports overall readiness using the SAME condition /readyz uses
	// (CH reachable AND schema present AND schema ready). When nil, false is
	// reported.
	Ready func(ctx context.Context) bool

	// StartTime is the process start instant, captured at boot, used to
	// compute uptimeSeconds. When zero, uptime is reported as 0.
	StartTime time.Time

	// Autotune reports the self-driving solver's live decision state for
	// GET /info/autotune. The bool is false when autotune introspection is not
	// available; when nil, GET /info/autotune returns 404.
	Autotune func() (AutotuneStatus, bool)

	// PingTimeout caps the per-request reachability/ready probes. Defaults to
	// 1s, matching the health handler's ping budget.
	PingTimeout time.Duration
}

// Handler serves GET /info. Construct via New and register via Mount.
type Handler struct {
	snap        Snapshot
	reachable   func(ctx context.Context) bool
	breaker     func() string
	schemaReady func() bool
	ready       func(ctx context.Context) bool
	start       time.Time
	pingTimeout time.Duration
	autotune    func() (AutotuneStatus, bool)
}

// defaultPingTimeout bounds the live reachability/ready probes per request.
const defaultPingTimeout = time.Second

// New builds a Handler from opts. Nil live funcs degrade to safe defaults
// (reachable=false, breaker="closed", schemaReady=true, ready=false) so a
// partially-wired handler still serves a well-formed body.
func New(opts Options) *Handler {
	h := &Handler{
		snap:        opts.Snapshot,
		reachable:   opts.Reachable,
		breaker:     opts.Breaker,
		schemaReady: opts.SchemaReady,
		ready:       opts.Ready,
		start:       opts.StartTime,
		pingTimeout: opts.PingTimeout,
		autotune:    opts.Autotune,
	}
	if h.pingTimeout <= 0 {
		h.pingTimeout = defaultPingTimeout
	}
	return h
}

// Mount registers GET /info and GET /info/autotune on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /info", h.handleInfo)
	mux.HandleFunc("GET /info/autotune", h.handleAutotune)
}

// clickHouseInfo is the nested "clickhouse" object of the /info body.
type clickHouseInfo struct {
	Address             string `json:"address"`
	Database            string `json:"database"`
	ServerVersion       string `json:"serverVersion"`
	ServerVersionSource string `json:"serverVersionSource"`
	Reachable           bool   `json:"reachable"`
	Breaker             string `json:"breaker"`
	SchemaReady         bool   `json:"schemaReady"`
}

// optimizationsInfo is the nested "optimizations" object of the /info body.
// The "enabled" array (the resolved EnabledSet) is the headline field.
type optimizationsInfo struct {
	Selection              string   `json:"selection"`
	Mode                   string   `json:"mode"`
	ResolvedAgainstVersion string   `json:"resolvedAgainstVersion"`
	Enabled                []string `json:"enabled"`
}

// infoResponse is the single JSON fingerprint GET /info returns. Field casing
// (lowerCamelCase) mirrors the health handler's JSON conventions.
type infoResponse struct {
	Service       string            `json:"service"`
	Version       string            `json:"version"`
	Revision      string            `json:"revision"`
	GoVersion     string            `json:"goVersion"`
	UptimeSeconds int64             `json:"uptimeSeconds"`
	Heads         []string          `json:"heads"`
	ClickHouse    clickHouseInfo    `json:"clickhouse"`
	Optimizations optimizationsInfo `json:"optimizations"`
	Ready         bool              `json:"ready"`
}

// handleInfo writes the fingerprint. It always returns 200: /info is a
// metadata surface, not a probe — readiness is reported IN the body
// ("ready": bool, plus the live clickhouse sub-object), never via the status
// code, so a monitoring scrape can read the fingerprint of an unready process.
func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	resp := h.snapshotResponse(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// snapshotResponse assembles the response, reading the live state through the
// injected closures under a bounded ping budget.
func (h *Handler) snapshotResponse(ctx context.Context) infoResponse {
	pingCtx, cancel := context.WithTimeout(ctx, h.pingTimeout)
	defer cancel()

	return infoResponse{
		Service:       h.snap.Service,
		Version:       h.snap.Version,
		Revision:      h.snap.Revision,
		GoVersion:     h.snap.GoVersion,
		UptimeSeconds: int64(h.uptime().Seconds()),
		Heads:         h.snap.Heads,
		ClickHouse: clickHouseInfo{
			Address:             h.snap.CHAddress,
			Database:            h.snap.CHDatabase,
			ServerVersion:       h.snap.ServerVersion,
			ServerVersionSource: h.snap.ServerVersionSource,
			Reachable:           h.reachableNow(pingCtx),
			Breaker:             h.breakerNow(),
			SchemaReady:         h.schemaReadyNow(),
		},
		Optimizations: optimizationsInfo{
			Selection:              h.snap.OptSelection,
			Mode:                   h.snap.OptMode,
			ResolvedAgainstVersion: h.snap.OptResolvedAgainstVersion,
			Enabled:                h.snap.OptEnabled,
		},
		Ready: h.readyNow(pingCtx),
	}
}

// uptime is set by New via the StartTime closure; see startTime.
func (h *Handler) uptime() time.Duration {
	return time.Since(h.startTime())
}

// startTime resolves the process start instant. It is a method (not a field)
// so a nil start time degrades to "now" (uptime 0) rather than reporting a
// nonsensical multi-decade uptime against the zero Time.
func (h *Handler) startTime() time.Time {
	if h.start.IsZero() {
		return time.Now()
	}
	return h.start
}

func (h *Handler) reachableNow(ctx context.Context) bool {
	if h.reachable == nil {
		return false
	}
	return h.reachable(ctx)
}

func (h *Handler) breakerNow() string {
	if h.breaker == nil {
		return "closed"
	}
	return h.breaker()
}

func (h *Handler) schemaReadyNow() bool {
	if h.schemaReady == nil {
		return true
	}
	return h.schemaReady()
}

func (h *Handler) readyNow(ctx context.Context) bool {
	if h.ready == nil {
		return false
	}
	return h.ready(ctx)
}
