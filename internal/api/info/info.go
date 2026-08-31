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

	// OptSelection is the raw CERBERUS_CH_OPTIMIZATIONS selection
	// (e.g. "auto", "auto,columnar_result_decode", "off").
	OptSelection string
	// OptMode is the resolution mode ("enforcing" | "permissive").
	OptMode string
}

// OptState is the outcome of the CURRENT ClickHouse capability resolution: the
// server version cerberus resolved its optimization selection against, where
// that version came from, and the feature ids the resolution enabled.
//
// It is deliberately NOT part of [Snapshot]. The selection and the mode are
// configuration and cannot change under a running process; the resolution is a
// reading of a LIVE server, which a rolling ClickHouse upgrade moves without
// cerberus restarting. Reporting a boot-time copy of it would tell an operator
// watching an upgrade the one thing they must not be told: that nothing changed.
type OptState struct {
	// ServerVersion is the ClickHouse server version the selection resolved
	// against, as "<major>.<minor>".
	ServerVersion string
	// ServerVersionSource is [ServerVersionSourceProbe] when the version was
	// read live, or [ServerVersionSourceFallback] when the probe failed and the
	// supported floor was assumed.
	ServerVersionSource string
	// Enabled is the EFFECTIVELY ENABLED feature ids (chopt EnabledSet IDs) —
	// the headline field: it makes plain whether cerberus is running the
	// optimizations it should.
	Enabled []string
	// QueryWorkload is the EFFECTIVE ClickHouse `workload` name cerberus is
	// CURRENTLY stamping on its own queries (cerberus issue #2785), or ""
	// when CERBERUS_CH_QUERY_WORKLOAD is unset, or was configured but the
	// live capability probe found the connected server rejects the
	// `workload` setting (or, under throw_on_unknown_workload, rejects the
	// named workload itself). Like ServerVersion/Enabled and unlike
	// Snapshot, this is read from the LIVE re-probed resolution: a workload
	// name entered CERBERUS_CH_QUERY_WORKLOAD reports "" here until (and
	// unless) the boot or a later re-probe finds the connected server
	// actually accepts it — see docs/operations.md#workload-scheduling.
	QueryWorkload string
}

const (
	// ServerVersionSourceProbe marks a server version read live from the server.
	ServerVersionSourceProbe = "probe"
	// ServerVersionSourceFallback marks the assumed supported floor used when
	// the version probe failed.
	ServerVersionSourceFallback = "fallback"
)

// ResultCacheState is the current query-result-cache hit/miss tally (cerberus
// issue #2781): the process-wide totals accumulated since boot, read from
// chclient.ResultCacheStats. Unlike OptState it carries no "did this even
// resolve" ambiguity — Hits/Misses are honest zero-value counters whether or
// not result_cache ever resolved in, so a deployment running on an older
// ClickHouse (or one whose capability probe came back Forbidden) reports
// 0/0 rather than an absent field.
type ResultCacheState struct {
	// Hits is the ClickHouse-reported QueryCacheHits ProfileEvent total,
	// summed across every dispatch that carried use_query_cache=1.
	Hits uint64
	// Misses is the ClickHouse-reported QueryCacheMisses ProfileEvent total,
	// summed the same way. A query cerberus stamped eligible but the server
	// still recomputed (a cold entry, an evicted one, or a byte-differing
	// settings fingerprint) counts here, never silently dropped.
	Misses uint64
}

// FilesystemCacheState is the current server-side filesystem cache reading
// (cerberus issue #2780): whether an operator has configured a named
// filesystem cache disk at all (see docs/operations.md's "Local filesystem
// cache" section) plus its aggregate configured capacity and live
// occupancy, read from chclient.QueryFilesystemCacheState. Configured is
// the headline field — the per-query enable_filesystem_cache toggle already
// defaults to on across every ClickHouse version cerberus supports
// (verified live; see docs/clickhouse-optimizations.md), so the one thing
// standing between "cerberus" and "cerberus with a warm S3 read cache" is
// whether the operator has wired a cache disk into the server's
// storage_configuration — this field answers that directly instead of
// leaving it to be inferred from a zero byte count that could otherwise
// mean either "no cache" or "an empty configured one".
type FilesystemCacheState struct {
	// Configured reports whether at least one named filesystem cache is
	// configured on the connected server.
	Configured bool
	// Caches is the number of named filesystem caches configured.
	Caches uint64
	// MaxSizeBytes is the summed configured max_size across every
	// configured cache.
	MaxSizeBytes uint64
	// CurrentSizeBytes is the summed current_size (live occupied bytes)
	// across every configured cache.
	CurrentSizeBytes uint64
	// CurrentElements is the summed current_elements_num (live occupied
	// file segments) across every configured cache.
	CurrentElements uint64
}

// ViewRefreshState is the current scheduler status of the loki
// label-cardinality catalog's refreshable materialized view (cerberus
// issue #2770), read live off system.view_refreshes on every /info poll —
// the same posture FilesystemCacheState and ResultCacheState take, since
// it too is a property of the CONNECTED server, not of cerberus's own
// process. Reported verbatim, with no cerberus-side "healthy"/"unhealthy"
// verdict layered on: a failed refresh reads here as a non-empty Exception
// plus LastRefreshTime having advanced past LastSuccessTime, so an operator
// can see the catalog is serving a stale-but-real previous snapshot (rather
// than cerberus silently deciding that for them). The field set mirrors
// system.view_refreshes' REAL columns — verified live against ClickHouse
// 25.9 (`SELECT name FROM system.columns WHERE table='view_refreshes'`) —
// which notably carries NO "last refresh result" enum and NO refresh
// counter, unlike an earlier draft of this struct assumed; see
// chclient.ViewRefreshState's doc comment for the verification detail and
// internal/schema/ddl's TestLokiLabelCatalog_RefreshAndFailureMode for the
// live proof this shape's fields actually behave as documented.
type ViewRefreshState struct {
	// Configured reports whether the view exists on the connected server
	// at all — false when LokiLabelCatalogEnabled is off, the DDL has not
	// applied yet, or the server predates the chopt loki_catalog_mv
	// version floor. Every other field is the zero value when false.
	Configured bool
	// Status is the view's current scheduler state (e.g. "Scheduled",
	// "Running"), read verbatim from ClickHouse. Does NOT flip to an
	// "Error" value on a failed refresh (verified live) — Exception is
	// what carries the failure signal.
	Status string
	// Exception is the most recently COMPLETED attempt's error text, or ""
	// when that attempt succeeded (or none has completed yet).
	Exception string
	// LastSuccessTime is the last successful refresh's completion
	// timestamp, or "" when no refresh has EVER succeeded.
	LastSuccessTime string
	// LastRefreshTime is the most recent refresh ATTEMPT's completion
	// timestamp (successful or not), or "" when none has completed yet.
	// LastRefreshTime > LastSuccessTime means the most recent attempt
	// failed (Exception is then non-empty) — exactly the "still serving
	// the previous snapshot" case this pair of fields surfaces.
	LastRefreshTime string
	// Retry is the current backoff retry counter for a REPEATEDLY failing
	// refresh (reset to 0 by ClickHouse on a successful attempt).
	Retry uint64
}

// Options configure Handler.
type Options struct {
	// Snapshot is the static, boot-captured fingerprint. Required.
	Snapshot Snapshot

	// Optimizations reports the CURRENT ClickHouse capability resolution — the
	// state a periodic re-probe replaces when the server's capabilities move.
	// When nil the body reports an unknown server version and an empty enabled
	// set, which is the honest answer for a handler wired without the resolver.
	Optimizations func() OptState

	// ResultCache reports the CURRENT query-result-cache hit/miss tally
	// (cerberus issue #2781). When nil the body reports 0/0, the honest
	// answer for a handler wired without chclient.ResultCacheStats.
	ResultCache func() ResultCacheState

	// FilesystemCache reports the CURRENT server-side filesystem cache
	// state (cerberus issue #2780), read live under the handler's own ping
	// budget the same way Reachable/Ready are — cache configuration and
	// occupancy are properties of the connected server, not of cerberus's
	// own process, so a rolling ClickHouse config change is reflected on
	// the next poll without a restart. When nil the body reports
	// Configured=false and every counter at 0, the honest answer for a
	// handler wired without chclient.QueryFilesystemCacheState.
	FilesystemCache func(ctx context.Context) FilesystemCacheState

	// LokiCatalogViewRefresh reports the CURRENT scheduler status of the
	// loki label-catalog's refreshable materialized view (cerberus issue
	// #2770), read live under the handler's own ping budget the same way
	// FilesystemCache is — the refresh's success/failure state is a
	// property of the connected server, not of cerberus's own process, so
	// a refresh that starts failing mid-run shows up on the next poll
	// without a restart. When nil the body reports Configured=false and
	// every other field at its zero value, the honest answer for a
	// handler wired without chclient.QueryViewRefreshState.
	LokiCatalogViewRefresh func(ctx context.Context) ViewRefreshState

	// TempoTagCatalogViewRefresh reports the CURRENT scheduler status of
	// the Tempo tag-catalog's refreshable materialized view (cerberus
	// issue #2771), the exact same posture LokiCatalogViewRefresh takes
	// for its sibling — read live under the handler's own ping budget,
	// nil degrades to Configured=false and every other field at its zero
	// value.
	TempoTagCatalogViewRefresh func(ctx context.Context) ViewRefreshState

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

	// PingTimeout caps the per-request reachability/ready probes. Defaults to
	// 1s, matching the health handler's ping budget.
	PingTimeout time.Duration
}

// Handler serves GET /info. Construct via New and register via Mount.
type Handler struct {
	snap                       Snapshot
	opts                       func() OptState
	resultCache                func() ResultCacheState
	filesystemCache            func(ctx context.Context) FilesystemCacheState
	lokiCatalogViewRefresh     func(ctx context.Context) ViewRefreshState
	tempoTagCatalogViewRefresh func(ctx context.Context) ViewRefreshState
	reachable                  func(ctx context.Context) bool
	breaker                    func() string
	schemaReady                func() bool
	ready                      func(ctx context.Context) bool
	start                      time.Time
	pingTimeout                time.Duration
}

// defaultPingTimeout bounds the live reachability/ready probes per request.
const defaultPingTimeout = time.Second

// New builds a Handler from opts. Nil live funcs degrade to safe defaults
// (reachable=false, breaker="closed", schemaReady=true, ready=false,
// optimizations=zero OptState) so a partially-wired handler still serves a
// well-formed body.
func New(opts Options) *Handler {
	h := &Handler{
		snap:                       opts.Snapshot,
		opts:                       opts.Optimizations,
		resultCache:                opts.ResultCache,
		filesystemCache:            opts.FilesystemCache,
		lokiCatalogViewRefresh:     opts.LokiCatalogViewRefresh,
		tempoTagCatalogViewRefresh: opts.TempoTagCatalogViewRefresh,
		reachable:                  opts.Reachable,
		breaker:                    opts.Breaker,
		schemaReady:                opts.SchemaReady,
		ready:                      opts.Ready,
		start:                      opts.StartTime,
		pingTimeout:                opts.PingTimeout,
	}
	if h.pingTimeout <= 0 {
		h.pingTimeout = defaultPingTimeout
	}
	return h
}

// Mount registers GET /info on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /info", h.handleInfo)
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
	// QueryWorkload mirrors [OptState.QueryWorkload] — the effective,
	// live-re-probed ClickHouse `workload` name (cerberus issue #2785), or
	// "" when unconfigured or rejected by the connected server.
	QueryWorkload string `json:"queryWorkload"`
}

// resultCacheInfo is the nested "resultCache" object of the /info body
// (cerberus issue #2781) — the query-result-cache hit/miss tally.
type resultCacheInfo struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

// filesystemCacheInfo is the nested "filesystemCache" object of the /info
// body (cerberus issue #2780) — the server-side filesystem cache's
// configured-vs-occupied state.
type filesystemCacheInfo struct {
	Configured       bool   `json:"configured"`
	Caches           uint64 `json:"caches"`
	MaxSizeBytes     uint64 `json:"maxSizeBytes"`
	CurrentSizeBytes uint64 `json:"currentSizeBytes"`
	CurrentElements  uint64 `json:"currentElements"`
}

// lokiCatalogViewRefreshInfo is the nested "lokiCatalogViewRefresh" object
// of the /info body (cerberus issue #2770) — the refreshable materialized
// view's system.view_refreshes status, reported verbatim.
type lokiCatalogViewRefreshInfo struct {
	Configured      bool   `json:"configured"`
	Status          string `json:"status"`
	Exception       string `json:"exception"`
	LastSuccessTime string `json:"lastSuccessTime"`
	LastRefreshTime string `json:"lastRefreshTime"`
	Retry           uint64 `json:"retry"`
}

// tempoTagCatalogViewRefreshInfo is the nested "tempoTagCatalogViewRefresh"
// object of the /info body (cerberus issue #2771) — the Tempo tag-catalog
// refreshable materialized view's system.view_refreshes status, reported
// verbatim, the exact same shape lokiCatalogViewRefreshInfo reports for
// its sibling.
type tempoTagCatalogViewRefreshInfo struct {
	Configured      bool   `json:"configured"`
	Status          string `json:"status"`
	Exception       string `json:"exception"`
	LastSuccessTime string `json:"lastSuccessTime"`
	LastRefreshTime string `json:"lastRefreshTime"`
	Retry           uint64 `json:"retry"`
}

// infoResponse is the single JSON fingerprint GET /info returns. Field casing
// (lowerCamelCase) mirrors the health handler's JSON conventions.
type infoResponse struct {
	Service                    string                         `json:"service"`
	Version                    string                         `json:"version"`
	Revision                   string                         `json:"revision"`
	GoVersion                  string                         `json:"goVersion"`
	UptimeSeconds              int64                          `json:"uptimeSeconds"`
	Heads                      []string                       `json:"heads"`
	ClickHouse                 clickHouseInfo                 `json:"clickhouse"`
	Optimizations              optimizationsInfo              `json:"optimizations"`
	ResultCache                resultCacheInfo                `json:"resultCache"`
	FilesystemCache            filesystemCacheInfo            `json:"filesystemCache"`
	LokiCatalogViewRefresh     lokiCatalogViewRefreshInfo     `json:"lokiCatalogViewRefresh"`
	TempoTagCatalogViewRefresh tempoTagCatalogViewRefreshInfo `json:"tempoTagCatalogViewRefresh"`
	Ready                      bool                           `json:"ready"`
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

	opts := h.optsNow()
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
			ServerVersion:       opts.ServerVersion,
			ServerVersionSource: opts.ServerVersionSource,
			Reachable:           h.reachableNow(pingCtx),
			Breaker:             h.breakerNow(),
			SchemaReady:         h.schemaReadyNow(),
		},
		Optimizations: optimizationsInfo{
			Selection: h.snap.OptSelection,
			Mode:      h.snap.OptMode,
			// The selection is always resolved against the same version the
			// clickhouse sub-object reports; they are one reading, rendered
			// under both keys because each sub-object is read on its own.
			ResolvedAgainstVersion: opts.ServerVersion,
			Enabled:                opts.Enabled,
			QueryWorkload:          opts.QueryWorkload,
		},
		ResultCache:                h.resultCacheInfoNow(),
		FilesystemCache:            h.filesystemCacheInfoNow(pingCtx),
		LokiCatalogViewRefresh:     h.lokiCatalogViewRefreshInfoNow(pingCtx),
		TempoTagCatalogViewRefresh: h.tempoTagCatalogViewRefreshInfoNow(pingCtx),
		Ready:                      h.readyNow(pingCtx),
	}
}

// resultCacheInfoNow reads the current query-result-cache tally and renders
// it as the JSON-shaped resultCacheInfo. ResultCacheState and resultCacheInfo
// share an identical field set (only struct tags differ), so this is a plain
// type conversion rather than a field-by-field literal.
func (h *Handler) resultCacheInfoNow() resultCacheInfo {
	return resultCacheInfo(h.resultCacheNow())
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

// optsNow reads the current capability resolution. An unwired resolver yields
// the zero OptState — an empty server version and an empty enabled list — which
// reads as "cerberus has not resolved anything", never as "nothing is enabled
// on a known server".
func (h *Handler) optsNow() OptState {
	if h.opts == nil {
		return OptState{}
	}
	return h.opts()
}

// resultCacheNow reads the current query-result-cache tally. An unwired
// closure yields the zero ResultCacheState (0/0), the honest answer for a
// handler wired without chclient.ResultCacheStats.
func (h *Handler) resultCacheNow() ResultCacheState {
	if h.resultCache == nil {
		return ResultCacheState{}
	}
	return h.resultCache()
}

// filesystemCacheInfoNow reads the current server-side filesystem cache
// state and renders it as the JSON-shaped filesystemCacheInfo. An unwired
// closure yields the zero FilesystemCacheState (Configured=false, every
// counter 0), the honest answer for a handler wired without
// chclient.QueryFilesystemCacheState. FilesystemCacheState and
// filesystemCacheInfo share an identical field set (only struct tags
// differ), so this is a plain type conversion rather than a field-by-field
// literal — the same shape resultCacheInfoNow uses.
func (h *Handler) filesystemCacheInfoNow(ctx context.Context) filesystemCacheInfo {
	if h.filesystemCache == nil {
		return filesystemCacheInfo{}
	}
	return filesystemCacheInfo(h.filesystemCache(ctx))
}

// lokiCatalogViewRefreshInfoNow reads the current loki label-catalog view
// refresh status and renders it as the JSON-shaped
// lokiCatalogViewRefreshInfo. ViewRefreshState and lokiCatalogViewRefreshInfo
// share an identical field set in the SAME order (only struct tags differ),
// so this is a plain type conversion — the same shape
// filesystemCacheInfoNow uses. An unwired closure yields the zero
// ViewRefreshState (Configured=false, every other field at its zero
// value), the honest answer for a handler wired without
// chclient.QueryViewRefreshState.
func (h *Handler) lokiCatalogViewRefreshInfoNow(ctx context.Context) lokiCatalogViewRefreshInfo {
	if h.lokiCatalogViewRefresh == nil {
		return lokiCatalogViewRefreshInfo{}
	}
	return lokiCatalogViewRefreshInfo(h.lokiCatalogViewRefresh(ctx))
}

// tempoTagCatalogViewRefreshInfoNow reads the current Tempo tag-catalog
// view refresh status and renders it as the JSON-shaped
// tempoTagCatalogViewRefreshInfo — the exact same plain type-conversion
// shape lokiCatalogViewRefreshInfoNow uses for its sibling.
func (h *Handler) tempoTagCatalogViewRefreshInfoNow(ctx context.Context) tempoTagCatalogViewRefreshInfo {
	if h.tempoTagCatalogViewRefresh == nil {
		return tempoTagCatalogViewRefreshInfo{}
	}
	return tempoTagCatalogViewRefreshInfo(h.tempoTagCatalogViewRefresh(ctx))
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
