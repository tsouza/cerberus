// Package chclient is a thin wrapper around clickhouse-go/v2 that the API
// layer uses to execute emitted SQL.
package chclient

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
)

// tracer emits the `execute` pipeline-stage span on every ClickHouse
// round-trip.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/chclient")

// OpenTelemetry semantic-conventions attribute keys for the execute
// span. cerberus uses the v1.0-vintage db.system / db.statement keys
// for compatibility with dashboards already pivoting on them.
//
// peer.service / server.address are the canonical OTel signals the
// `servicegraph` connector (and Tempo's built-in service-graph
// metrics-generator) read off client-kind spans to derive the
// caller -> callee edge. Stamping `peer.service="clickhouse"` on
// every execute span gives Grafana's Tempo Service Graph tab the
// cerberus -> clickhouse hop with no extra trace post-processing.
var (
	attrDBSystem    = attribute.Key("db.system")
	attrDBStatement = attribute.Key("db.statement")
	attrPeerService = attribute.Key("peer.service")
	attrServerAddr  = attribute.Key("server.address")
	attrNetPeerName = attribute.Key("net.peer.name")
)

// peerServiceClickHouse is the constant `peer.service` value stamped on
// every execute span. It is the logical service name the servicegraph
// connector uses as the "server" side of the edge. Constant because
// every CH host cerberus talks to is the same logical service from the
// caller's perspective — sharding / replication is below the trace's
// abstraction layer.
const peerServiceClickHouse = "clickhouse"

// startExecuteSpan opens an `execute` span carrying the standard
// db.system + db.statement semantic-conventions attributes plus the
// cerberus.sql_length counter. The span is opened as SpanKindClient
// with peer.service + server.address so the OTel-Collector
// `servicegraph` connector picks up the cerberus -> clickhouse edge.
// Returns the derived context and span.
func startExecuteSpan(ctx context.Context, sql, addr string) (context.Context, trace.Span) {
	stmt := cerbtrace.Truncate(sql, cerbtrace.MaxStatementLen)
	attrs := []attribute.KeyValue{
		attrDBSystem.String("clickhouse"),
		attrDBStatement.String(stmt),
		attrPeerService.String(peerServiceClickHouse),
		cerbtrace.AttrSQLLength.Int(len(sql)),
	}
	if addr != "" {
		// server.address is the modern semconv key; net.peer.name is the
		// pre-v1.21 alias still consumed by older dashboards. Stamp both
		// so neither generation breaks.
		attrs = append(
			attrs,
			attrServerAddr.String(addr),
			attrNetPeerName.String(addr),
		)
	}
	return tracer.Start(
		ctx, cerbtrace.SpanExecute,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
}

// Config describes a single ClickHouse connection.
type Config struct {
	Addr     string // host:port, e.g. "clickhouse:9000"
	Database string
	Username string
	Password string

	// DialTimeout caps the initial connection dial. Zero falls back to 5s.
	DialTimeout time.Duration

	// MaxOpenConns caps the total number of pooled connections (busy +
	// idle) the driver will hold open to ClickHouse. clickhouse-go's
	// implicit default is MaxIdleConns+5 (≈10); cerberus makes it
	// explicit and configurable so the sharded-pushdown solver can raise
	// the ceiling for fan-out without silently inheriting the driver
	// default. When a query needs a connection and all MaxOpenConns are
	// in use, the acquire blocks up to DialTimeout and then fails with
	// clickhouse.ErrAcquireConnTimeout — a local pool-sizing signal, not
	// a CH-health failure (the breaker treats it neutrally; see
	// breaker.record). Zero falls back to defaultMaxOpenConns.
	MaxOpenConns int

	// MaxIdleConns caps the number of idle connections kept warm in the
	// pool for reuse. clickhouse-go's implicit default is 5; cerberus
	// makes it explicit. Zero falls back to defaultMaxIdleConns.
	MaxIdleConns int

	// ConnMaxLifetime caps how long a single pooled connection may live
	// before the driver recycles it. clickhouse-go's implicit default is
	// 1h; cerberus makes it explicit. Zero falls back to
	// defaultConnMaxLifetime.
	//
	// It doubles as the recovery-speed ceiling for a restarted ClickHouse
	// backend (ch-pod-kill, run 27509796946). clickhouse-go (v2.46.0) has
	// no separate idle-time knob: a pooled conn to the OLD pod is dropped
	// at acquire only once acquire()'s isBad() check trips — either the
	// non-blocking socket read in conn_check.go notices the dead peer, OR
	// the conn crosses ConnMaxLifetime (isBad checks both). A force-killed
	// pod often leaves the socket in ESTABLISHED with no FIN/RST, so the
	// socket check passes and the conn is only retired once it ages past
	// ConnMaxLifetime — which at the 1h default is far too long. The
	// transport-retry on the data path (see retry.go) makes a stale conn
	// transparent on the FIRST query regardless of this value; this cap is
	// the backstop that bounds how long a never-queried idle stale conn can
	// otherwise loiter, so a modest value (minutes, not an hour) keeps the
	// pool self-healing without churning conns under healthy load.
	ConnMaxLifetime time.Duration

	// MaxQuerySamples caps the number of Sample rows a single query may
	// load into memory. When a cursor drain crosses the budget,
	// iteration aborts and Cursor.Err() returns a *TooManySamplesError
	// (errors.Is ErrTooManySamples). 0 disables the budget. Mirrors
	// upstream Prometheus's --query.max-samples knob; cmd/cerberus
	// wires it from CERBERUS_QUERY_MAX_SAMPLES.
	MaxQuerySamples int64

	// BreakerThreshold is the number of consecutive CH-health failures
	// (within BreakerWindow) that trip the circuit breaker from CLOSED to
	// OPEN. 0 falls back to the breaker default (5). cmd/cerberus wires it
	// from CERBERUS_CH_BREAKER_THRESHOLD; see breaker.go for the state
	// machine. Defaults reproduce the pre-#95 hardcoded constants exactly,
	// so out-of-the-box breaker behaviour is byte-unchanged.
	BreakerThreshold int

	// BreakerWindow is the rolling window over which BreakerThreshold
	// consecutive failures must occur to trip the breaker. 0 falls back to
	// the breaker default (10s). cmd/cerberus wires it from
	// CERBERUS_CH_BREAKER_WINDOW.
	BreakerWindow time.Duration

	// BreakerOpenInterval is the OPEN-state backoff: after it elapses the
	// breaker admits a single HALF-OPEN probe. 0 falls back to the breaker
	// default (5s). cmd/cerberus wires it from
	// CERBERUS_CH_BREAKER_OPEN_INTERVAL.
	BreakerOpenInterval time.Duration

	// BreakerDisabled, when true, turns the circuit breaker into a no-op:
	// every call is admitted and the breaker can never trip, so a saturated
	// or dead CH surfaces as ordinary dial/query errors rather than
	// ErrCircuitOpen. Default false (breaker enabled). cmd/cerberus wires
	// it from CERBERUS_CH_BREAKER_ENABLED (enabled=false → disabled=true).
	BreakerDisabled bool

	// MaxQueryMemoryBytes caps ClickHouse's server-side memory use for
	// a single data-plane query: it is stamped as the per-query
	// `max_memory_usage` setting on every read-path query (QueryCursor
	// / Query / QueryStrings / QueryTimestampedLines / QueryMetricMeta
	// / QueryIndexStats / QueryIndexVolume / QueryExemplars /
	// QueryLabelSets). DDL / DML through Exec is exempt — schema
	// creation legitimately has different memory needs than the query
	// path. 0 = don't set the setting (ClickHouse server defaults
	// apply). cmd/cerberus wires it from CERBERUS_CH_QUERY_MAX_MEMORY.
	//
	// MaxQuerySamples bounds cerberus-process memory (rows drained
	// into Go); this bounds ClickHouse-process memory (the working set
	// the server materialises while evaluating the SQL). The k3d
	// dashboard run 27277793810 showed why both are needed: a 24h/15s
	// matrix query stayed under the sample budget client-side but blew
	// ClickHouse's server-total cap mid-stream (code 241), 502-ing the
	// panel. A query crossing this cap gets a *MemoryLimitError
	// rejection (errors.Is ErrMemoryLimitExceeded), classified by the
	// API heads as resource-exhausted, not internal.
	MaxQueryMemoryBytes int64

	// QueryTimeoutSeconds caps the server-side wall-clock duration of a
	// single data-plane query: it is stamped as the per-query
	// `max_execution_time` setting (with `timeout_overflow_mode=throw`)
	// on every read-path query, so a pathological query is ABORTED by
	// ClickHouse with TIMEOUT_EXCEEDED (code 159) instead of holding a
	// pooled connection + admit slot for its full unbounded duration.
	// DDL / DML through Exec is exempt — schema creation legitimately
	// takes longer than the query path. 0 = don't set the setting
	// (ClickHouse server defaults apply). cmd/cerberus wires it from
	// CERBERUS_QUERY_TIMEOUT (a duration string, e.g. "2m"; 0 disables).
	//
	// This is the wall-clock sibling of MaxQueryMemoryBytes: that bounds
	// the working set the server materialises, this bounds how long it
	// may run. A query crossing this cap gets a *QueryTimeoutError
	// rejection (errors.Is ErrQueryTimeout), classified by the API heads
	// as a head-idiomatic timeout (prom 503 errorType=timeout), not a
	// 5xx — CH is healthy when it enforces a cap. The standard
	// Prometheus ?timeout= query param min's with this default per
	// request (see WithQueryTimeout).
	QueryTimeout time.Duration
}

// Client is a stateless wrapper over a clickhouse-go/v2 connection pool.
//
// Every CH-touching method (Ping, Exec, Query, QueryCursor, QueryStrings,
// QueryMetricMeta, QueryIndexStats, QueryIndexVolume, QueryLabelSets) is
// guarded by a circuit breaker (see breaker.go). When CH goes dark the
// breaker trips after a short failure budget and methods return
// [ErrCircuitOpen] without dialling — the handler layer maps that into
// HTTP 503 with a `Retry-After: 5` header so clients back off cleanly
// instead of stacking inner-stage retries against a dead upstream.
//
// PER-HEAD ISOLATION (#94). A single *Client holds a registry of N
// breakers, one per [Head] (prom / loki / tempo / probe), all sharing the
// ONE driver.Conn pool. The data heads each get a distinct breaker via
// [Client.ForHead]: a query storm that trips the prom head's breaker
// fast-fails ONLY prom queries — loki and tempo keep their own CLOSED
// breakers and serve normally, and the readiness probe runs through its own
// HeadProbe breaker so /readyz stays GREEN. The shared driver.Conn means
// this isolates the 503-CASCADE + pod-eviction blast radius, NOT pool / CH
// server-side saturation (a fan-out that saturates CH can still slow the
// other heads — pool-acquire-timeouts are breaker-neutral by design) and NOT
// memory-cap (code-241) storms (those count as breaker SUCCESS). The breaker
// `br` field selected by a Client view determines which head's breaker its
// methods gate on; the bare *Client returned by [New] uses an unscoped
// breaker so direct (non-ForHead) callers — schema preflight, tests —
// behave exactly as a single-breaker Client did.
type Client struct {
	conn driver.Conn
	addr string // CH addr (host:port) — stamped on execute spans as server.address

	// br is the breaker the CH-touching methods on THIS view gate on. New
	// sets it to an unscoped breaker; ForHead returns a shallow copy of the
	// Client with br swapped for that head's registry entry. A pointer (not
	// an embedded value) so a ForHead copy shares the SAME *breaker the
	// registry holds — the copy is a lightweight view over the shared pool +
	// the head's own breaker, never a second breaker.
	br *breaker

	// breakers is the immutable per-head breaker registry, built once in
	// buildBreakers and never mutated afterward (so concurrent reads need no
	// mutex; each *breaker keeps its own mu). Shared by every ForHead view
	// of this Client so a head's breaker state is consistent across views.
	breakers map[Head]*breaker

	// maxSamples is Config.MaxQuerySamples, threaded into every cursor
	// QueryCursor opens (and therefore into Query, which drains a
	// cursor). 0 = unlimited.
	maxSamples int64
	// maxMemory is Config.MaxQueryMemoryBytes — the per-query
	// `max_memory_usage` ClickHouse setting applied to every data-plane
	// query via queryContext. 0 = setting not sent.
	maxMemory int64
	// queryTimeout is Config.QueryTimeoutSeconds as a time.Duration —
	// the per-query `max_execution_time` ClickHouse setting applied to
	// every data-plane query via queryContext (overridable per-request,
	// min'd, via WithQueryTimeout). 0 = setting not sent.
	queryTimeout time.Duration
}

// buildBreakers constructs the per-head breaker registry shared by all of a
// Client's ForHead views, plus the unscoped default breaker the bare Client
// gates on. Both New and the test-only newWithConn route through here so
// neither can ship a Client with a nil breaker (a nil br nil-derefs on the
// first allow()). The telemetry set is built once and SHARED across every
// breaker — one instrument pair, N head-labelled streams — so the zero-init
// pass in newBreakerMetrics seeds all four heads from a single construction.
//
// Every head breaker inherits the same #95 tuning + disable config: per-head
// disable / per-head thresholds are a future map-population detail, not a
// structural change. metrics may be nil (the no-telemetry path) — the default
// breaker and each head breaker then record nothing.
func buildBreakers(
	disabled bool,
	threshold int,
	window, openInterval time.Duration,
	metrics *breakerMetrics,
) (def *breaker, registry map[Head]*breaker) {
	mk := func(h Head) *breaker {
		th := threshold
		// The readiness probe breaker trips on a tighter default budget so a
		// total-CH outage flips /readyz red inside the k8s readinessProbe
		// eviction window — the probe ping stream is low-rate (TTL-coalesced),
		// so the looser data-head budget would trip too slowly. An explicit
		// operator override (threshold != 0) wins for every head, including
		// probe; this only fills the zero-value default.
		if h == HeadProbe && th == 0 {
			th = probeBreakerThreshold
		}
		return &breaker{
			disabled:     disabled,
			threshold:    th,
			window:       window,
			openInterval: openInterval,
			head:         h,
			metrics:      metrics,
		}
	}
	registry = make(map[Head]*breaker, len(allHeads))
	observed := make([]*breaker, 0, len(allHeads))
	for _, h := range allHeads {
		br := mk(h)
		registry[h] = br
		observed = append(observed, br)
	}
	// The default (unscoped) breaker fronts a bare *Client used without
	// ForHead — schema preflight, tests, the startup ping. It carries no
	// head label so it never pollutes a per-head series; direct callers see
	// exactly the pre-#94 single-breaker behaviour. It is deliberately left
	// OUT of the observed set so the state gauge emits exactly one series per
	// real head (no head="" sample).
	def = mk("")
	// Register the observable-gauge callback now that the live per-head
	// breakers exist (they post-date newBreakerMetrics). The callback reads
	// each breaker's CURRENT state every collection interval, so the gauge
	// always reflects reality and can never report a stale half-open after a
	// breaker has closed. A nil metrics set (the no-telemetry path) makes this
	// a no-op.
	metrics.registerStateCallback(observed...)
	return def, registry
}

// ForHead returns a lightweight Client VIEW that gates its CH-touching
// methods on the breaker for head h while sharing this Client's ONE
// connection pool (and the rest of its config). It is the seam that isolates
// a head's fast-fail blast radius (#94): cmd/cerberus hands each API head its
// own ForHead view, so prom.New(client.ForHead(HeadProm)), loki.New(...
// HeadLoki ...), tempo.New(... HeadTempo ...), and health.New(Pinger:
// client.ForHead(HeadProbe)) each get a DISTINCT breaker over the SAME pool.
//
// The returned *Client satisfies every head's narrow Querier interface
// unchanged (same method set as the parent) — no interface churn. An unknown
// head is a wiring bug: ForHead panics rather than minting a garbage-keyed
// breaker, so a typo can never silently route a head to a nil / shared
// breaker at request time.
func (c *Client) ForHead(h Head) *Client {
	br, ok := c.breakers[h]
	if !ok {
		panic("chclient: ForHead: unknown head " + string(h))
	}
	view := *c // shallow copy: shares conn + breakers registry + config
	view.br = br
	return &view
}

// New opens a connection pool to ClickHouse. Construction is lazy:
// clickhouse.Open only validates options and never dials, so New
// succeeds even when ClickHouse is unreachable — the first Ping/Query
// performs the actual dial. That is deliberate: a cerberus replica that
// boots while ClickHouse is saturated or still starting (e.g. an HPA
// scale-up during a load burst — CI run 27272406583) must come up
// "started but unready" and let /readyz gate traffic, not exit(1) and
// crash-loop on `dial tcp …:9000: connect: connection refused`.
//
// Fail-fast is preserved for misconfiguration that can never succeed:
// clickhouse.Open's option validation errors are returned as-is.
// Connectivity validation belongs to the caller (cmd/cerberus does a
// best-effort startup Ping demoted to a WARN log) and to the readiness
// probe (internal/api/health pings via this Client).
//
// The returned Client is safe for concurrent use.
func New(cfg Config) (*Client, error) {
	dial := cfg.DialTimeout
	if dial == 0 {
		dial = 5 * time.Second
	}
	opts := &clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: dial,
	}
	// Pool sizing is explicit and configurable (#81). A zero field is
	// left unset so clickhouse-go's own default applies — that keeps the
	// non-sharded path behaviour-compatible for callers (notably tests)
	// that build a bare Config. cmd/cerberus always supplies positive
	// values derived once in internal/config, so the production pool is
	// never implicit.
	if cfg.MaxOpenConns > 0 {
		opts.MaxOpenConns = cfg.MaxOpenConns
	}
	if cfg.MaxIdleConns > 0 {
		opts.MaxIdleConns = cfg.MaxIdleConns
	}
	if cfg.ConnMaxLifetime > 0 {
		opts.ConnMaxLifetime = cfg.ConnMaxLifetime
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("chclient: open: %w", err)
	}
	// Per-head breaker registry (#94) sharing one telemetry set (#95 tuning
	// + disable config flows to every head). Zero tuning fields resolve to
	// the GA defaults inside each breaker (resolveThreshold / resolveWindow /
	// resolveOpenInterval), so a bare Config — notably the ones tests build —
	// keeps the pre-#95 hardcoded behaviour byte-for-byte. The telemetry set
	// is wired off the global MeterProvider and zero-initialised at
	// construction for all four heads so a healthy replica exports a flat
	// closed/0 series per head instead of "No data" — see breaker_metrics.go.
	def, registry := buildBreakers(
		cfg.BreakerDisabled,
		cfg.BreakerThreshold,
		cfg.BreakerWindow,
		cfg.BreakerOpenInterval,
		newGlobalBreakerMetrics(),
	)
	return &Client{
		conn:         conn,
		addr:         cfg.Addr,
		br:           def,
		breakers:     registry,
		maxSamples:   cfg.MaxQuerySamples,
		maxMemory:    cfg.MaxQueryMemoryBytes,
		queryTimeout: cfg.QueryTimeout,
	}, nil
}

// querySettings returns the per-query ClickHouse settings map applied
// to every data-plane query, or nil when no setting is configured.
// It carries:
//
//   - `max_memory_usage` — ClickHouse's per-query memory cap, from
//     Config.MaxQueryMemoryBytes (when > 0).
//   - `max_execution_time` + `timeout_overflow_mode=throw` — the
//     per-query wall-clock cap from Config.QueryTimeoutSeconds (when the
//     effective timeout, after any per-request WithQueryTimeout override,
//     is > 0). `throw` aborts an over-long query with TIMEOUT_EXCEEDED
//     (code 159) rather than returning partial results.
//   - SettingExperimentalTSGridAggregate=1 — ONLY when ctx was marked by
//     WithTSGridSetting (the engine marks it when the emitted plan
//     contains a chplan.RangeWindowNative node). The experimental knob
//     is added to the SAME map as max_memory_usage, never via a second
//     independent clickhouse.WithSettings wrap — a second wrap REPLACES
//     rather than unions the settings map (clickhouse-go context.go:
//     `c.settings = maps.Clone(q.settings)`), which would silently drop
//     the memory cap. Merging here keeps both knobs on the one map.
//
// Kept as its own method (rather than inlined into queryContext) so
// tests can assert the settings content directly — the driver stores
// QueryOptions under an unexported context key with no public getter.
func (c *Client) querySettings(ctx context.Context) clickhouse.Settings {
	wantTSGrid := wantTSGridSetting(ctx)
	timeout := c.effectiveQueryTimeout(ctx)
	blockSize := maxBlockSizeFromContext(ctx)
	if c.maxMemory <= 0 && timeout <= 0 && !wantTSGrid && blockSize == 0 {
		return nil
	}
	s := clickhouse.Settings{}
	if c.maxMemory > 0 {
		s["max_memory_usage"] = c.maxMemory
	}
	if timeout > 0 {
		// ClickHouse's max_execution_time is a Float64 in seconds; send
		// the effective timeout as seconds so a sub-second ?timeout=
		// override (or a non-integer config) is honoured exactly rather
		// than truncated to whole seconds.
		s[settingMaxExecutionTime] = timeout.Seconds()
		s[settingTimeoutOverflowMode] = timeoutOverflowModeThrow
	}
	if wantTSGrid {
		s[SettingExperimentalTSGridAggregate] = 1
	}
	if blockSize > 0 {
		// Per-request override (WithMaxBlockSize) — only ever set by the
		// chaos_sleep build so its injected sleepEachRow source is read as
		// small blocks and max_execution_time can abort it mid-scan.
		s[settingMaxBlockSize] = blockSize
	}
	return s
}

// queryContext derives the context every data-plane query runs under:
// the caller's ctx plus the per-query ClickHouse settings from
// querySettings. clickhouse.Context merges with any QueryOptions
// already on ctx (e.g. the progress callback installed by
// WithProgressFor), so stacking is safe. When no settings are
// configured the ctx is returned unchanged.
//
// Exec (DDL / DML) deliberately does NOT go through this — see
// Config.MaxQueryMemoryBytes.
func (c *Client) queryContext(ctx context.Context) context.Context {
	s := c.querySettings(ctx)
	if s == nil {
		return ctx
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(s))
}

// Conn returns the underlying clickhouse-go/v2 driver connection. It is
// exposed so packages that need the raw driver — notably
// internal/schema/ddl, which calls driver.Conn.Exec on the upstream OTel
// exporter DDL templates — can share the same pool the API layer uses,
// instead of opening a second connection. Callers should treat the
// returned connection as read-only (do not close it; rely on Client.Close).
func (c *Client) Conn() driver.Conn {
	return c.conn
}

// newWithConn returns a *Client wrapping the supplied driver.Conn. It is
// a test-only seam used by the chaos / failure-mode tests in this package
// to drive the cursor / Exec / Query paths against a fault-injecting fake
// driver.Conn without standing up a real ClickHouse server.
//
// Production callers MUST use New, which goes through clickhouse.Open's
// option validation. This constructor bypasses it — it is unexported and
// intentionally narrow.
//
//nolint:revive // test-only seam; production code must use New.
func newWithConn(conn driver.Conn) *Client {
	// Route through buildBreakers so the test seam gets the SAME per-head
	// registry + a non-nil default breaker production New does — otherwise
	// br is nil and the first allow() nil-derefs in the chaos / integration
	// tests. nil metrics = the no-telemetry path (these tests assert breaker
	// state via currentState(), not via the metric label).
	def, registry := buildBreakers(false, 0, 0, 0, nil)
	return &Client{conn: conn, br: def, breakers: registry}
}

// Close releases all pooled connections.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Ping verifies the underlying ClickHouse connection is reachable. It
// forwards to the clickhouse-go/v2 driver's Ping, which performs a
// lightweight round-trip on a pooled connection. The readiness probe
// (internal/api/health) uses it as the downstream-dependency check.
//
// Guarded by the circuit breaker: when the breaker is OPEN, Ping
// returns ErrCircuitOpen instantly without touching CH. That is the
// behavior /readyz depends on to report "red" within the cache TTL
// after the breaker trips — the ping IS the readiness probe, so a
// short-circuited ping is a short-circuited readiness check.
func (c *Client) Ping(ctx context.Context) error {
	if c.conn == nil {
		return fmt.Errorf("chclient: ping: nil connection")
	}
	if !c.br.allow() {
		return fmt.Errorf("chclient: ping: %w", ErrCircuitOpen)
	}
	err := c.pingOpen(ctx)
	c.br.record(ctx, err)
	if err != nil {
		return fmt.Errorf("chclient: ping: %w", err)
	}
	return nil
}

// Sample is one row of metrics data returned by Query. It's the shape the
// /api/v1/query and /api/v1/query_range handlers expect — see api/prom.
//
// Labels sharing contract: the cursor interns decoded label maps by
// canonical key, so every Sample belonging to the same series carries
// the SAME map instance — that is what keeps a multi-thousand-row
// matrix drain at one retained map per series instead of one per row.
// Consumers MUST treat Labels as read-only; copy before mutating
// (internal/api/format.WithMetricName / NormalizeLabelMap and the
// loki/tempo label pivots already allocate fresh output maps).
type Sample struct {
	MetricName string
	Labels     map[string]string
	Timestamp  time.Time
	Value      float64
	// Metadata carries per-row structured metadata for Loki log-stream
	// queries — the OTel-CH LogAttributes map surfaced as the third
	// element of Loki's `[ts, line, {metadata}]` value tuple. It is
	// populated only when the projection emits a fifth `Metadata` column
	// (the log-stream path), and stays nil for every metric query and
	// for the prom / tempo heads, whose four-column projections leave the
	// shared cursor's 4-column scan path untouched.
	Metadata map[string]string
}

// PeekBreakerState reports the circuit-breaker lifecycle phase as a stable
// string — "closed", "open", or "half-open" — WITHOUT mutating breaker
// state. In particular it never reserves a HALF-OPEN probe slot, unlike the
// internal allow() path that the data-plane methods use.
//
// It is the read-only pre-flight hook the sharded solver uses (satisfies
// solver.breakerPeeker): a routed K-shard fan-out checks this before
// emitting and fails fast when the breaker is not CLOSED, so a doomed routed
// request never burns the single recovery probe — recovery probing is left
// to lighter route-A traffic.
func (c *Client) PeekBreakerState() string {
	return c.br.peek()
}

// Exec runs sql with positional args against ClickHouse and returns any
// error. Use for DDL (CREATE TABLE, ...) and DML (INSERT, ...) that don't
// produce a result set.
//
// Guarded by the circuit breaker: when the breaker is OPEN this returns
// ErrCircuitOpen without touching CH and without opening an execute span.
func (c *Client) Exec(ctx context.Context, sql string, args ...any) error {
	if !c.br.allow() {
		return fmt.Errorf("chclient: exec: %w", ErrCircuitOpen)
	}
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	err := c.conn.Exec(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("chclient: exec: %w", err)
	}
	return nil
}

// Query runs sql with positional args against ClickHouse and decodes each
// row into a Sample. The SQL must project MetricName, Attributes, TimeUnix,
// Value in that order — Scan binds positionally.
//
// For v0.1 the API layer ensures this projection shape via the chplan
// Project node wrapped around lowered PromQL output.
//
// Query is a thin wrapper around QueryCursor that drains the cursor into
// a slice. Callers that may return millions of rows (notably
// /api/v1/query_range) should use QueryCursor directly to keep memory
// bounded.
func (c *Client) Query(ctx context.Context, sql string, args ...any) ([]Sample, error) {
	cursor, err := c.QueryCursor(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = cursor.Close()
	}()

	var out []Sample
	for cursor.Next() {
		out = append(out, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// flushProgress records the cerberus.clickhouse.{rows,bytes}_read
// histograms for ctx if a progressRecorder was attached via
// WithProgressFor. No-op otherwise. Wired into each synchronous
// non-cursor query method (QueryStrings, QueryMetricMeta,
// QueryLabelSets, QueryIndexStats, QueryIndexVolume); the cursor path
// flushes from the cursor's Close.
func flushProgress(ctx context.Context) {
	if rec := recorderFromContext(ctx); rec != nil {
		rec.flush()
	}
}

// QueryStrings runs sql and decodes a single-string-column result into a
// flat slice. Used by metadata endpoints (/api/v1/labels, label values,
// metadata) that return a list of names.
//
// Guarded by the circuit breaker: returns ErrCircuitOpen instantly when
// the breaker is OPEN, no execute span opened.
func (c *Client) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// DetectedFieldRow is one (Body, LogAttributes, ResourceAttributes)
// tuple from the peek-window SQL backing /loki/api/v1/detected_fields.
// Line carries the raw log body the handler runs the JSON / logfmt
// detection over; Attributes carries the record-level attribute map
// (Loki's structured-metadata analogue in the OTel-CH schema);
// Resource carries the stream-identity label map the parser uses for
// collision renaming (a parsed key that shadows a stream label is
// surfaced as `<key>_extracted`, mirroring upstream Loki).
type DetectedFieldRow struct {
	Line       string
	Attributes map[string]string
	Resource   map[string]string
}

// QueryDetectedFieldRows runs sql and decodes a (String,
// Map(String,String), Map(String,String)) three-column result set into
// a flat slice. Used by /loki/api/v1/detected_fields to feed the
// field-detection heuristic — the handler needs the body for parsing
// plus both attribute maps for structured-metadata fields and
// stream-label collision handling.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryDetectedFieldRows(ctx context.Context, sql string, args ...any) ([]DetectedFieldRow, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []DetectedFieldRow
	for rows.Next() {
		var (
			line     string
			attrs    map[string]string
			resource map[string]string
		)
		if err := rows.Scan(&line, &attrs, &resource); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, DetectedFieldRow{Line: line, Attributes: attrs, Resource: resource})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// TimestampedLine is one (Timestamp, Body) tuple from the peek-window
// SQL backing /loki/api/v1/patterns. The timestamp is the row's
// DateTime64 value verbatim; the body is the raw log line. The drain
// template miner consumes the pair via [drain.Drain.Train], which takes
// the timestamp as unix nanoseconds.
type TimestampedLine struct {
	Timestamp time.Time
	Body      string
}

// QueryTimestampedLines runs sql and decodes a (DateTime64, String)
// two-column result set into a flat slice. Used by /loki/api/v1/patterns
// to feed the drain template miner — drain needs both the line body and
// a timestamp to bucket per-cluster samples.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryTimestampedLines(ctx context.Context, sql string, args ...any) ([]TimestampedLine, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []TimestampedLine
	for rows.Next() {
		var ts time.Time
		var body string
		if err := rows.Scan(&ts, &body); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, TimestampedLine{Timestamp: ts, Body: body})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// MetricMetaRow is one row from the metadata-discovery query — a metric
// name plus its OTel description and unit text and the cerberus-derived
// Prom-style type (gauge / counter / histogram).
type MetricMetaRow struct {
	Name        string
	Description string
	Unit        string
	Type        string
}

// QueryMetricMeta runs sql and decodes each row as a (name, description,
// unit) triple. The caller supplies the `metricType` (gauge / counter /
// histogram) since the table the row came from determines that — the SQL
// itself only returns the OTel columns.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryMetricMeta(ctx context.Context, sql, metricType string, args ...any) ([]MetricMetaRow, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []MetricMetaRow
	for rows.Next() {
		var r MetricMetaRow
		r.Type = metricType
		if err := rows.Scan(&r.Name, &r.Description, &r.Unit); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// IndexStatsRow is the single aggregate row returned by the Loki
// /loki/api/v1/index/stats SQL — counts of distinct streams, log entries
// and total byte volume (sum(length(Body))) for the matched selector.
//
// Cerberus has no chunk model (it's sample-backed, not chunk-backed), so
// the chunks count is reported as 0 by the API handler — it is not part
// of this struct.
type IndexStatsRow struct {
	Streams uint64
	Entries uint64
	Bytes   uint64
}

// QueryIndexStats runs sql expecting a single row of three UInt64
// aggregates (streams, entries, bytes) and decodes it. An empty result
// set is treated as the all-zeros row.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryIndexStats(ctx context.Context, sql string, args ...any) (IndexStatsRow, error) {
	if !c.br.allow() {
		return IndexStatsRow{}, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return IndexStatsRow{}, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out IndexStatsRow
	if rows.Next() {
		if err := rows.Scan(&out.Streams, &out.Entries, &out.Bytes); err != nil {
			return IndexStatsRow{}, fmt.Errorf("chclient: scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return IndexStatsRow{}, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// IndexVolumeRow is one grouped (label-set, bytes) tuple from the Loki
// /loki/api/v1/index/volume SQL. The label set is the GROUP BY key — by
// default the full ResourceAttributes map, or a filtered subset when the
// caller supplied `targetLabels`.
type IndexVolumeRow struct {
	Labels map[string]string
	Bytes  uint64
}

// QueryIndexVolume runs sql expecting rows of (Map(String,String),
// UInt64) and decodes them into IndexVolumeRow.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryIndexVolume(ctx context.Context, sql string, args ...any) ([]IndexVolumeRow, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []IndexVolumeRow
	for rows.Next() {
		var r IndexVolumeRow
		var labels map[string]string
		if err := rows.Scan(&labels, &r.Bytes); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		r.Labels = labels
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// ExemplarRow is one fanned-out exemplar tuple decoded from the SQL
// produced by chsql.EmitQueryExemplars. The eight columns project the
// per-data-point series identity (MetricName / Attributes /
// ServiceName), the per-exemplar `Exemplars.<field>` array elements
// (Timestamp / Value / TraceID / SpanID / ExemplarAttributes), and are
// scanned positionally in the order the emitter projects them.
//
// Wire-shape consumers (see [internal/api/prom.handleQueryExemplars])
// group these rows by `(MetricName, Attributes, ServiceName)` into
// ExemplarSeries; the per-exemplar columns become the inner Exemplar
// entries with `trace_id` / `span_id` merged into Labels via the
// reserved-key precedence rules documented on the PromQL exemplars
// endpoint plan.
type ExemplarRow struct {
	MetricName         string
	Attributes         map[string]string
	ServiceName        string
	Timestamp          time.Time
	Value              float64
	TraceID            string
	SpanID             string
	ExemplarAttributes map[string]string
}

// QueryExemplars runs sql expecting the eight-column row shape
// chsql.EmitQueryExemplars produces and decodes each row into an
// [ExemplarRow]. Scan binds positionally; the SQL column order is the
// emitter's contract.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryExemplars(ctx context.Context, sql string, args ...any) ([]ExemplarRow, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []ExemplarRow
	for rows.Next() {
		var r ExemplarRow
		if err := rows.Scan(
			&r.MetricName,
			&r.Attributes,
			&r.ServiceName,
			&r.Timestamp,
			&r.Value,
			&r.TraceID,
			&r.SpanID,
			&r.ExemplarAttributes,
		); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// QueryLabelSets runs sql and decodes each row into a Map(String,String)
// label set. Used by /api/v1/series.
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []map[string]string
	for rows.Next() {
		var m map[string]string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}

// NameTypePair is one (name, type) row decoded from a two-string-column
// result set — the shape `system.columns` returns when projected as
// (name, type). Used by the startup schema preflight to introspect the
// deployed column layout of the configured tables.
type NameTypePair struct {
	Name string
	Type string
}

// QueryNameTypePairs runs sql and decodes a two-string-column result
// (name, type) into a flat slice. Used by the startup preflight to read
// `system.columns` for the configured tables — the projection is
// (name, type) and Scan binds positionally.
//
// Guarded by the circuit breaker: returns ErrCircuitOpen instantly when
// the breaker is OPEN, no execute span opened.
func (c *Client) QueryNameTypePairs(ctx context.Context, sql string, args ...any) ([]NameTypePair, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []NameTypePair
	for rows.Next() {
		var p NameTypePair
		if err := rows.Scan(&p.Name, &p.Type); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}
