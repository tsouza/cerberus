// Command cerberus is the three-headed query gateway server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Version is set at build time by goreleaser.
var Version = "dev"

// isVersionFlag reports whether argv requests a version dump. cerberus
// is otherwise env-driven and ignores argv, but `--version` / `-v` /
// `version` are wired so docker + k8s healthchecks can probe the
// binary cheaply: the distroless runtime image has no shell, no wget,
// and no curl, so invoking the binary itself is the only viable
// healthcheck path. Exported via a function (not inlined in main) so
// the same dispatch is verified by main_test.go.
func isVersionFlag(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "--version", "-v", "version":
		return true
	}
	return false
}

// admitCap translates a per-head admission toggle into the concurrency
// cap handed to admit.New. An enabled head uses its default cap; a
// disabled head returns 0, which admit.New maps to a nil (pass-through)
// limiter. Keeping the enabled/disabled cases symmetric here means the
// only knob the operator sees is a plain boolean.
func admitCap(enabled bool, defaultCap int) int {
	if !enabled {
		return 0
	}
	return defaultCap
}

// newAdmitLimiters builds the per-head admission-control limiters. When
// CERBERUS_ADMIT_DISABLED=true every limiter is nil and the middleware
// short-circuits to a pass-through wrapper. Otherwise each per-head toggle
// CERBERUS_ADMIT_{PROM,LOKI,TEMPO} (boolean) selects the head's default cap
// when truthy, or leaves the head unlimited (nil limiter) when falsy.
// admit.New returns nil for a non-positive cap, so a disabled head and a
// zero cap collapse to the same pass-through path.
func newAdmitLimiters(cfg config.Config, logger *slog.Logger) (*admit.Limiter, *admit.Limiter, *admit.Limiter) {
	if cfg.Admit.Disabled {
		logger.Info("admission control disabled (CERBERUS_ADMIT_DISABLED=true)")
		return nil, nil, nil
	}
	promCap := admitCap(cfg.Admit.Prom, config.DefaultAdmitProm)
	lokiCap := admitCap(cfg.Admit.Loki, config.DefaultAdmitLoki)
	tempoCap := admitCap(cfg.Admit.Tempo, config.DefaultAdmitTempo)
	logger.Info(
		"admission control enabled",
		"prom", promCap,
		"loki", lokiCap,
		"tempo", tempoCap,
	)
	return admit.New("prom", promCap), admit.New("loki", lokiCap), admit.New("tempo", tempoCap)
}

func main() {
	if isVersionFlag(os.Args) {
		fmt.Fprintln(os.Stdout, Version)
		return
	}

	// Bootstrap logger used only until config.FromEnv returns and the
	// configured logger replaces it. Text + info matches the configured
	// defaults so the upgrade is invisible when env vars are unset.
	bootstrap := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(bootstrap)

	if err := run(); err != nil {
		slog.Default().Error("cerberus exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Stage-1 logger: stderr-only, used until telemetry providers
	// are built below. The startup log lines that come next describe
	// the OTLP target itself, so they have to land before the OTel
	// bridge is wired (and would be useless once the bridge is wired
	// — they can't ship before the exporter is up).
	logger := config.NewLogger(os.Stderr, cfg.Log)
	slog.SetDefault(logger)

	logger.Info(
		"cerberus starting",
		"version", Version,
		"http_addr", cfg.HTTPAddr,
		"ch_addr", cfg.ClickHouse.Addr,
		"ch_db", cfg.ClickHouse.Database,
		"log_format", cfg.Log.Format,
		"log_level", cfg.Log.Level.String(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Construction is lazy — chclient.New never dials, it only
	// validates options. An error here is misconfiguration that can
	// never succeed (fail-fast is correct); connectivity problems
	// surface on the best-effort Ping below and on /readyz.
	client, err := chclient.New(cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("configure clickhouse client: %w", err)
	}
	defer func() {
		_ = client.Close()
	}()

	warnIfClickHouseUnreachable(ctx, logger, client, cfg.ClickHouse)

	// schemaReady reports whether the auto-create-schema startup hook
	// has finished at least once; /readyz consults it on every probe.
	schemaReady := setupSchema(ctx, logger, client, cfg.ClickHouse, schemaApplyConfig(cfg), cfg.AutoCreateSchema, cfg.AutoCreateDatabase)

	// Boot-time requirements preflight (ON by default). It MUST run AFTER
	// the schema-create step above — on a fresh DB cerberus has just
	// created the tables, so introspecting them before the create would
	// fail gate 2 against tables that don't exist yet. The check fails
	// startup (returns an error → exit 1) when the connected server is
	// older than the config-derived floor or the deployed schema is
	// WRONG-SHAPE (a table exists but its columns diverge) — neither
	// self-heals, so failing fast converts an opaque query-time failure into
	// a precise boot-time one. A schema that is ENTIRELY ABSENT (not yet
	// provisioned — the cerberus+collector startup race) is NOT fatal: the
	// returned schemaPresent func reports NOT READY on /readyz and a
	// background re-probe flips it ready once an external writer creates the
	// schema, with no restart. CERBERUS_REQUIREMENTS_CHECK=false skips it.
	schemaPresent, err := runRequirementsCheck(ctx, logger, client, cfg)
	if err != nil {
		return err
	}

	// Install the W3C+Baggage propagator and build OTel providers from
	// the OTLP env config. When CERBERUS_OTLP_ENDPOINT is empty the
	// telemetry package returns noop providers, so cerberus stays a
	// zero-collector-dependency binary by default. The providers are
	// installed BEFORE handler.Mount so the per-head admit limiters
	// build their rejected-counter against the right meter provider.
	providers, err := telemetry.New(ctx, telemetry.Config{
		Endpoint:       cfg.OTLP.Endpoint,
		Insecure:       cfg.OTLP.Insecure,
		Headers:        cfg.OTLP.Headers,
		Timeout:        cfg.OTLP.Timeout,
		ExportInterval: cfg.OTLP.ExportInterval,
		ServiceName:    "cerberus",
		ServiceVersion: Version,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	installOTel(providers.TracerProvider)
	otel.SetMeterProvider(providers.MeterProvider)

	// Stage-2 logger: now that the OTLP log exporter is built, fan
	// every slog record out to BOTH the stderr handler (12-factor
	// stream / `kubectl logs` readability) AND the OTel slog bridge
	// (records ship via OTLP gRPC to the collector → CH `otel_logs`,
	// landing alongside the same binary's traces and metrics so a
	// self-dashboard works against a running cluster). With the
	// endpoint empty, providers.LoggerProvider is the no-op
	// LoggerProvider — bridge is a no-op, only stderr is written.
	logger = config.NewTelemetryLogger(os.Stderr, cfg.Log, providers.LoggerProvider)
	slog.SetDefault(logger)

	if cfg.OTLP.Endpoint != "" {
		logger.Info(
			"OTLP exporters enabled",
			"endpoint", cfg.OTLP.Endpoint,
			"insecure", cfg.OTLP.Insecure,
		)
	}

	// Build per-head admission-control limiters (see newAdmitLimiters).
	promLimiter, lokiLimiter, tempoLimiter := newAdmitLimiters(cfg, logger)

	// The trace mux carries the three Prom/Loki/Tempo APIs and is
	// wrapped with otelhttp so every request becomes a server span.
	// Wrapping at the mux level — instead of per-handler — keeps the
	// propagator code path uniform across all three APIs and lets the
	// span name formatter pull r.Pattern after the mux has resolved
	// the route.
	traceMux := http.NewServeMux()

	// Sharded-pushdown solver (ON by default — Mode=auto, the phase-2 flip).
	// Built from the CERBERUS_EVAL_ROUTE knobs and fail-fast validated, then
	// wired with the data-plane hooks: a GLOBAL connection gate sized from the
	// chclient pool (MaxOpenConns − reserve) and SHARED across heads, the
	// breaker peek, the chsql emitter adapter, and the prom admit limiter for
	// the (P-1) top-up. Under the default Mode=auto the Planner classifies
	// every plan and routes the ELIGIBLE, above-threshold ones through the
	// Executor (route B); everything else fails toward the byte-identical
	// route A. Operators pin CERBERUS_EVAL_ROUTE=single to disable routing
	// (the Planner still classifies for the shadow header, but never routes).
	// We always wire the solver so the additive X-Cerberus-Route-Decision
	// shadow header reports the classification regardless of mode.
	//
	// Per-head Client VIEWS (#94) are built FIRST so the solver's breaker
	// peek and the prom data plane share the SAME prom breaker: the solver's
	// routed fan-out is prom-only (it carries the prom admit limiter), so a
	// tripped prom breaker must fast-fail the solver's prom fan-out exactly
	// as it fast-fails the prom handler's route-A queries. ForHead hands each
	// head its OWN circuit breaker over the SAME connection pool, so a query
	// storm that trips one head's breaker (503s that head's queries) can
	// never cascade to the other two — and the readiness probe gets its own
	// HeadProbe breaker below so it stays green throughout.
	promClient := client.ForHead(chclient.HeadProm)
	lokiClient := client.ForHead(chclient.HeadLoki)
	tempoClient := client.ForHead(chclient.HeadTempo)

	evalSolver, err := buildSolver(logger, cfg.ClickHouse, promClient, promLimiter)
	if err != nil {
		return fmt.Errorf("configure solver: %w", err)
	}

	// All three heads run on the shared engine.Engine pipeline; each
	// engine is constructed below from a per-head Client VIEW + a seed
	// optimizer and assigned onto the per-head handler.
	promHandler := newPromHandler(promClient, cfg, evalSolver, promLimiter, logger)
	promHandler.Mount(traceMux)

	lokiHandler := loki.New(lokiClient, cfg.Logs, logger.With("api", "loki"))
	lokiHandler.Limiter = lokiLimiter
	lokiHandler.Version = Version
	lokiHandler.QueryTimeout = cfg.ClickHouse.QueryTimeout
	lokiHandler.TailWriteTimeout = cfg.LokiTailWriteTimeout
	lokiHandler.Mount(traceMux)

	tempoHandler := tempo.New(tempoClient, cfg.Traces, Version, logger.With("api", "tempo"))
	tempoHandler.Limiter = tempoLimiter
	tempoHandler.Mount(traceMux)

	tracedAPI := wrapWithOTel(traceMux, "cerberus")

	// /healthz and /readyz live on a separate sub-mux that bypasses
	// otelhttp: k8s probes hit at multi-Hz rates and would otherwise
	// flood the trace backend with no-op spans. The readiness handler
	// memoises results behind a TTL cache so concurrent probes coalesce
	// into a single ClickHouse ping per window.
	// Readiness pings flow through the dedicated HeadProbe breaker (#94), NOT
	// any data head's. That decouples "can cerberus reach ClickHouse at all"
	// (the only question readiness should ask) from "is one head's workload
	// melting ClickHouse": a prom-only query storm trips the prom breaker and
	// 503s prom queries while /readyz stays GREEN, so a single head's
	// transient CH storm never evicts a pod that is still serving the other
	// two heads. A genuine total-CH outage still fails the pings themselves
	// and trips the probe breaker, flipping /readyz red — correct eviction.
	healthHandler := health.New(health.Options{
		Pinger:        client.ForHead(chclient.HeadProbe),
		SchemaReady:   schemaReady,
		SchemaPresent: schemaPresent,
	})

	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
	maybeMountPProf(rootMux, cfg.DebugPProf, logger)
	rootMux.Handle("/", tracedAPI)

	// Tempo gRPC StreamingQuerier — PR 1 (scaffold) of the Tempo gRPC
	// rollout. The service shares the Tempo HTTP handler's Engine +
	// schema + admit limiter so the eventual streaming RPC bodies (PRs
	// 2-4) and the existing HTTP handlers run the same parse + lower +
	// emit pipeline against the same backend. Today every RPC returns
	// codes.Unimplemented via the embedded
	// UnimplementedStreamingQuerierServer; PRs 2-4 fill in real bodies
	// one RPC group at a time.
	tempoGRPCService := tempogrpc.NewService(tempoHandler, tempoLimiter, logger.With("api", "tempo-grpc"))
	grpcServer := tempogrpc.NewServer(tempoGRPCService)

	srv := buildDualStackServer(cfg.HTTPAddr, cfg.HTTPServer, rootMux, grpcServer)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP listener ready")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("signal received, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// Drain any in-flight gRPC streams before tearing telemetry down.
	// GracefulStop blocks until every active RPC returns or its
	// stream is closed by the HTTP/2 transport (which srv.Shutdown
	// has already done). With no in-flight streams it returns
	// immediately, so this is a no-op on the happy path.
	grpcServer.GracefulStop()
	// Flush any pending OTLP batches before the process exits. Noop
	// when telemetry was disabled (Endpoint == "").
	if err := providers.Shutdown(shutdownCtx); err != nil {
		logger.Warn("telemetry shutdown returned error", "err", err)
	}
	logger.Info("cerberus stopped")
	return nil
}

// solverGateReserve is the number of pooled connections the solver's GLOBAL
// shard gate leaves untouched so route-A traffic (the overwhelming majority)
// always has headroom even when a routed fan-out is holding gate slots. The
// gate is sized MaxOpenConns − reserve; the Executor additionally caps any
// single routed request at gate/2 so >=2 routed requests can always progress.
const solverGateReserve = 2

// buildSolver constructs the sharded-pushdown solver from the CERBERUS_*
// environment and wires its data-plane hooks. The Config is validated
// fail-fast (an invalid CERBERUS_EVAL_ROUTE / threshold aborts startup). The
// GLOBAL gate is sized from the chclient pool (MaxOpenConns − reserve) and
// shared across heads via the single returned *solver.Solver. Under the
// phase-2 default (Mode=auto) eligible, above-threshold plans route B through
// the Executor; everything else fails toward route A. Operators pin
// CERBERUS_EVAL_ROUTE=single to keep the Executor dormant (the Planner still
// classifies for the shadow header, but never routes).
// newPromHandler builds the prom head's handler with its engine (per-head
// Client view + seed optimizer + solver), limiter, and runtime knobs wired in.
func newPromHandler(client *chclient.Client, cfg config.Config, evalSolver *solver.Solver, limiter *admit.Limiter, logger *slog.Logger) *prom.Handler {
	h := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	h.Engine = &engine.Engine{Optimizer: h.Optimizer, Client: client, Solver: evalSolver}
	h.Limiter = limiter
	h.Version = Version
	h.ExperimentalTSGridRange = cfg.ExperimentalTSGridRange
	h.QueryTimeout = cfg.ClickHouse.QueryTimeout
	return h
}

func buildSolver(
	logger *slog.Logger,
	chCfg chclient.Config,
	client *chclient.Client,
	promLimiter *admit.Limiter,
) (*solver.Solver, error) {
	cfg, err := solver.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// GLOBAL shard gate: MaxOpenConns − reserve, floored at 2 so the
	// Executor's gate/2 cap never collapses to zero. The pool size is the
	// validated, already-positive value config.FromEnv resolved.
	gateCap := int64(chCfg.MaxOpenConns - solverGateReserve)
	if gateCap < 2 {
		gateCap = 2
	}
	gate := semaphore.NewWeighted(gateCap)

	// The admit top-up is only meaningful when admission control is enabled.
	// A nil *admit.Limiter (CERBERUS_ADMIT_DISABLED=true) leaves ExecDeps.Admit
	// nil, which the Executor treats as "no cap" (it runs at full P). Passing
	// the typed-nil *admit.Limiter directly would defeat the Executor's
	// nil-interface guard, so gate the assignment on a non-nil limiter.
	deps := solver.ExecDeps{
		Client:  client,
		Gate:    gate,
		GateCap: gateCap,
		Breaker: client,
	}
	if promLimiter != nil {
		deps.Admit = promLimiter
	}

	s := solver.New(cfg, engine.ChsqlEmitter{}, deps)

	logger.Info(
		"sharded-pushdown solver wired",
		"mode", cfg.Mode,
		"gate_cap", gateCap,
		"parallel", cfg.Parallel,
		"min_fanout", cfg.MinFanout,
		"min_anchor_pairs", cfg.MinAnchorPairs,
	)
	return s, nil
}

// warnIfClickHouseUnreachable performs the best-effort startup
// connectivity validation, demoted to a WARN. A replica that boots
// while ClickHouse is saturated or still starting (HPA scale-up during
// a load burst — CI run 27272406583 crash-looped on exactly this) must
// come up "started but unready": the HTTP listener binds regardless and
// /readyz keeps the pod out of the Service endpoints until the CH ping
// succeeds. That is what Kubernetes readiness gating is for — exiting
// here would just convert a transient dependency outage into a
// CrashLoopBackOff.
func warnIfClickHouseUnreachable(ctx context.Context, logger *slog.Logger, client *chclient.Client, cfg chclient.Config) {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := client.Ping(pingCtx); err != nil {
		logger.Warn(
			"clickhouse not reachable at startup; serving unready until it appears (/readyz gates traffic)",
			"addr", cfg.Addr,
			"err", err,
		)
	}
}

// schemaApplyConfig maps the runtime config into the typed internal/schema/ddl
// Config the auto-create hook applies. The database name comes from the
// ClickHouse connection config; the cluster / table-engine / TTL / Replicated
// database-engine knobs come from CERBERUS_SCHEMA_* (SchemaProvisioning); and
// the per-signal TABLE NAMES are threaded from the SAME resolved schema structs
// the query heads read (cfg.Schema / cfg.Logs / cfg.Traces), so a
// CERBERUS_SCHEMA_*_TABLE override creates and queries the same table instead of
// silently diverging.
func schemaApplyConfig(cfg config.Config) ddl.Config {
	p := cfg.SchemaProvisioning
	// Per-signal TTL: a non-zero per-signal override wins; otherwise the
	// signal inherits the global CERBERUS_SCHEMA_TTL default (which is itself
	// 0 = no retention unless the operator sets it).
	signalTTL := func(override time.Duration) time.Duration {
		if override > 0 {
			return override
		}
		return p.TTL
	}
	return ddl.Config{
		Database: cfg.ClickHouse.Database,
		Cluster:  p.Cluster,
		Engine:   p.TableEngine,
		TTL: ddl.TTL{
			Metrics: signalTTL(p.TTLMetrics),
			Logs:    signalTTL(p.TTLLogs),
			Traces:  signalTTL(p.TTLTraces),
		},
		DatabaseEngine: ddl.DatabaseEngine{
			Replicated:        p.DatabaseReplicated,
			ReplicatedZooPath: p.DatabaseReplicatedPath,
			ReplicatedShard:   p.DatabaseReplicatedShard,
			ReplicatedReplica: p.DatabaseReplicatedReplica,
		},
		Tables: ddl.Tables{
			Logs:                cfg.Logs.LogsTable,
			Traces:              cfg.Traces.SpansTable,
			MetricsGauge:        cfg.Schema.GaugeTable,
			MetricsSum:          cfg.Schema.SumTable,
			MetricsHistogram:    cfg.Schema.HistogramTable,
			MetricsExpHistogram: cfg.Schema.ExpHistogramTable,
			MetricsSummary:      cfg.Schema.SummaryTable,
		},
	}
}

// setupSchema runs the auto-create-schema startup hook (when enabled)
// and returns the SchemaReadyFunc the /readyz handler consults. When
// auto-create is off, readiness must not gate on it, so the returned
// func reports true immediately. When the first apply fails — the same
// incident class as the startup ping above: the DDL templates are
// static and covered by integration tests, so a failure here is
// overwhelmingly "ClickHouse isn't up yet" — the apply retries in the
// background and /readyz reports schema "pending" instead of the
// process exiting.
func setupSchema(
	ctx context.Context,
	logger *slog.Logger,
	client *chclient.Client,
	chCfg chclient.Config,
	applyCfg ddl.Config,
	autoCreateSchema, autoCreateDatabase bool,
) health.SchemaReadyFunc {
	ready := new(atomic.Bool)
	if !autoCreateSchema {
		ready.Store(true)
		return ready.Load
	}

	// Pick the connection the DDL runs over. When cerberus creates the
	// database, the CREATE DATABASE must run from a session whose default
	// database EXISTS — and the configured database may not yet — so it goes
	// over a bootstrap connection bound to ClickHouse's always-present
	// `default` database (the fully-qualified `<db>.<table>` table creates work
	// from there too). When the database is externally managed
	// (CERBERUS_AUTO_CREATE_DATABASE=false) the table creates run over the
	// normal target-bound connection and the CREATE DATABASE is skipped.
	applyConn := client.Conn()
	cleanup := func() {} // no-op unless a bootstrap client is opened
	applyCfg.SkipDatabaseCreate = !autoCreateDatabase
	if autoCreateDatabase {
		bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg))
		if err != nil {
			// chclient.New is lazy (no dial) and only validates options the
			// target client already validated, so this is effectively
			// unreachable; if it ever fires, fall back to the target connection
			// (the apply will surface the real error via the retry + /readyz).
			logger.Warn("could not open bootstrap connection for database create; using the configured connection", "err", err)
		} else {
			applyConn = bootClient.Conn()
			cleanup = func() { _ = bootClient.Close() }
		}
	}

	logger.Info(
		"auto-creating OTel ClickHouse schema",
		"database", applyCfg.Database,
		"create_database", autoCreateDatabase,
		"cluster", applyCfg.Cluster,
		"replicated_db", applyCfg.DatabaseEngine.Replicated,
		"signals", "metrics,logs,traces",
	)
	apply := func(ctx context.Context) error {
		return ddl.ApplyWithConfig(ctx, applyConn, applyCfg, ddl.All)
	}
	if err := apply(ctx); err != nil {
		logger.Warn(
			"auto-create schema failed at startup; retrying in background (/readyz reports schema pending)",
			"err", err,
		)
		go retrySchemaApply(ctx, logger, ready, schemaRetryInterval, apply, cleanup)
		return ready.Load
	}
	cleanup()
	logger.Info("OTel ClickHouse schema ready")
	ready.Store(true)
	return ready.Load
}

// bootstrapDatabase is ClickHouse's always-present database. The auto-create
// hook issues CREATE DATABASE <target> over a connection bound to it, because
// the configured target database may not exist yet — and ClickHouse rejects
// every statement (even CREATE DATABASE) on a session whose default database
// is absent (code 81, UNKNOWN_DATABASE).
const bootstrapDatabase = "default"

// bootstrapClickHouseConfig returns chCfg rebound to the always-present
// `default` database, for the one-time auto-create DDL. Everything else
// (address, auth, TLS, pool sizing) is unchanged.
func bootstrapClickHouseConfig(chCfg chclient.Config) chclient.Config {
	chCfg.Database = bootstrapDatabase
	return chCfg
}

// runRequirementsCheck runs the boot-time requirements check (gated ON by
// default via CERBERUS_REQUIREMENTS_CHECK). It validates the connected
// ClickHouse server version against the config-derived minimum AND the
// deployed schema shape of the configured (override-resolved) tables. The
// check is parameterised by the active config: the native-rate knob raises
// the version floor, and every table/column name comes from the resolved
// schema structs so CERBERUS_SCHEMA_* overrides are respected.
//
// Findings split two ways. A FATAL finding (too-old/unreadable server, or a
// table that EXISTS but is WRONG-SHAPE) never self-heals, so the returned
// error aggregates every such requirement and the caller exits non-zero —
// the precise boot-time failure replaces the opaque query-time error a
// too-old server or divergent schema would otherwise produce. Two cases are
// instead TRANSIENT and do NOT fail startup: a schema that is ENTIRELY ABSENT
// (not yet provisioned — the cerberus+collector race), and a ClickHouse that
// is ENTIRELY UNREACHABLE (a dial / connection-refused error — cerberus
// booted ahead of the database). In both the returned
// health.SchemaPresentFunc reports NOT READY on /readyz with a precise
// reason, and a background re-probe (reusing the auto-create retry cadence)
// flips it ready once the server appears and the schema is provisioned, with
// no restart.
//
// When the check is disabled, both gates are bypassed (one log line) and a
// nil SchemaPresentFunc is returned — readiness does not gate on the schema.
func runRequirementsCheck(
	ctx context.Context,
	logger *slog.Logger,
	client *chclient.Client,
	cfg config.Config,
) (health.SchemaPresentFunc, error) {
	if !cfg.RequirementsCheck {
		logger.Info("requirements check disabled (CERBERUS_REQUIREMENTS_CHECK=false)")
		return nil, nil
	}
	req := preflight.Requirements{
		Database:          cfg.ClickHouse.Database,
		NativeRateEnabled: cfg.ExperimentalTSGridRange,
		Metrics:           cfg.Schema,
		Logs:              cfg.Logs,
		Traces:            cfg.Traces,
	}
	res := preflight.RunIfEnabled(ctx, cfg.RequirementsCheck, client, req)
	if res.Fatal != nil {
		// Wrong-shape / too-old / unreadable — never self-heals. Exit even if
		// some tables are also absent: a too-old server won't fix itself by
		// waiting, and a wrong-shape table is a genuine misconfiguration.
		return nil, res.Fatal
	}

	if res.Unreachable {
		// Transient: ClickHouse is not accepting connections yet (cerberus
		// booted ahead of the database). A dial / connection-refused error is
		// NOT a misconfiguration and self-heals once the server appears, so
		// boot but stay NOT READY; the same background re-probe that waits on
		// an absent schema also waits out an unreachable server. No restart.
		reason := res.UnreachableReason()
		logger.Warn(
			"clickhouse not reachable at startup; serving unready until it appears (/readyz gates traffic)",
			"reason", reason,
		)
		present := newSchemaPresentSignal(reason)
		go reprobeSchema(ctx, logger, client, req, present, schemaRetryInterval)
		return present.Func(), nil
	}

	if res.DatabaseAbsent {
		// Transient: ClickHouse is up but the configured database does not exist
		// yet (UNKNOWN_DATABASE / code 81). The connection carries the database
		// as its session default, so even SELECT version() fails until the
		// database is created — by the collector that owns schema creation, or by
		// the auto-create hook once it can reach the server. This is the same
		// class of cold-cluster race as an absent schema, NOT a misconfiguration,
		// so boot but stay NOT READY; the background re-probe flips readiness once
		// the database (and its tables) appear. No restart.
		reason := res.DatabaseAbsentReason(cfg.ClickHouse.Database)
		logger.Warn(
			"clickhouse database not yet provisioned; serving unready until it is created (/readyz gates traffic)",
			"reason", reason,
		)
		present := newSchemaPresentSignal(reason)
		go reprobeSchema(ctx, logger, client, req, present, schemaRetryInterval)
		return present.Func(), nil
	}

	if !res.SchemaProvisioned() {
		// Transient: the schema has not been provisioned yet (cerberus booted
		// ahead of the collector that owns schema creation). Boot but stay
		// NOT READY; a background re-probe flips readiness once the writer
		// creates the schema. No restart.
		reason := res.AbsentReason()
		logger.Warn(
			"schema not yet provisioned; serving unready until an external writer creates it (/readyz gates traffic)",
			"reason", reason,
		)
		present := newSchemaPresentSignal(reason)
		go reprobeSchema(ctx, logger, client, req, present, schemaRetryInterval)
		return present.Func(), nil
	}

	logger.Info(
		"requirements check passed",
		"database", cfg.ClickHouse.Database,
		"native_rate", cfg.ExperimentalTSGridRange,
	)
	return nil, nil
}

// schemaPresentSignal is the concurrency-safe carrier behind the
// health.SchemaPresentFunc the readiness probe consults. present is flipped
// once the background re-probe sees a fully-provisioned schema; reason holds
// the current absent-tables explanation until then. The mutex guards reason
// (a string can't be stored atomically) and keeps the present/reason pair
// consistent for a probe that reads both.
type schemaPresentSignal struct {
	mu      sync.Mutex
	present bool
	reason  string
}

// newSchemaPresentSignal seeds the signal in the not-present state with the
// initial absent reason.
func newSchemaPresentSignal(reason string) *schemaPresentSignal {
	return &schemaPresentSignal{reason: reason}
}

// Func returns the health.SchemaPresentFunc view the readiness handler
// calls on every probe.
func (s *schemaPresentSignal) Func() health.SchemaPresentFunc {
	return func() (bool, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.present, s.reason
	}
}

// markPresent flips the signal to provisioned; once present the readiness
// probe stops gating on the schema.
func (s *schemaPresentSignal) markPresent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.present = true
	s.reason = ""
}

// setReason updates the absent-tables explanation while still not-present
// (e.g. fewer tables remain absent on a later probe).
func (s *schemaPresentSignal) setReason(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reason = reason
}

// reprobeSchema re-runs the requirements check on the auto-create retry
// cadence until the configured schema is fully provisioned AND ClickHouse is
// reachable (or ctx is cancelled). It only ever transitions a not-present
// schema to present: a re-probe that turns up a FATAL finding (e.g. an
// external writer created a wrong-shape table) is logged and retried rather
// than crashing an already-serving process — the boot-time fail-fast contract
// covers the cold-start case, and a running replica must not exit on a
// transient introspection blip. A still-unreachable server keeps the
// unreachable reason fresh and waits. Once the schema is present it flips
// readiness and returns.
func reprobeSchema(
	ctx context.Context,
	logger *slog.Logger,
	client *chclient.Client,
	req preflight.Requirements,
	signal *schemaPresentSignal,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res := preflight.Run(ctx, client, req)
		if res.Fatal != nil {
			logger.Warn("schema re-probe found a fatal requirement; staying unready and retrying", "err", res.Fatal)
			continue
		}
		if res.Unreachable {
			// Still no ClickHouse: keep the unreachable reason fresh and wait.
			signal.setReason(res.UnreachableReason())
			continue
		}
		if res.DatabaseAbsent {
			// Database still not created: keep the reason fresh and wait.
			signal.setReason(res.DatabaseAbsentReason(req.Database))
			continue
		}
		if !res.SchemaProvisioned() {
			signal.setReason(res.AbsentReason())
			continue
		}
		logger.Info("schema now provisioned; reporting ready", "database", req.Database)
		signal.markPresent()
		return
	}
}

// schemaRetryInterval is the cadence of background auto-create-schema
// retries after a failed startup attempt. 5s sits between the /readyz
// probe period (3s) and the readiness TTL cache (2s): a recovering
// ClickHouse is picked up within roughly two probe periods without
// hammering a server that is still coming up.
const schemaRetryInterval = 5 * time.Second

// retrySchemaApply re-runs the auto-create-schema hook until it
// succeeds once or ctx is cancelled (SIGTERM / process shutdown). On
// success it flips ready, which the /readyz handler consults — until
// then the pod reports schema "pending" and stays out of the Service
// endpoints. Failures stay WARNs: a booting replica must not exit(1)
// because ClickHouse isn't accepting connections yet (CI run
// 27272406583 crash-looped a scale-up replica on exactly that).
func retrySchemaApply(
	ctx context.Context,
	logger *slog.Logger,
	ready *atomic.Bool,
	interval time.Duration,
	apply func(context.Context) error,
	cleanup func(),
) {
	defer cleanup() // close the bootstrap connection on success or shutdown
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := apply(ctx); err != nil {
			logger.Warn("auto-create schema retry failed", "err", err)
			continue
		}
		logger.Info("OTel ClickHouse schema ready")
		ready.Store(true)
		return
	}
}

// buildDualStackServer wires an http.Server that serves HTTP/1.1 (for
// existing Prom/Loki/Tempo HTTP handlers + /healthz + /readyz) AND
// unencrypted HTTP/2 (for the Tempo gRPC StreamingQuerier) on the same
// listener. A content-type dispatcher routes HTTP/2 + application/grpc
// requests to the gRPC server; everything else flows to the HTTP mux.
//
// Cerberus accepts:
//
//   - HTTP/1.1 clients (Grafana HTTP datasource, curl, /healthz)
//   - HTTP/2 clients via prior-knowledge (grpc-go default)
//   - HTTP/2 upgrades from HTTP/1.1 (h2c-aware proxies)
//
// maybeMountPProf registers the standard net/http/pprof debug handlers under
// /debug/pprof/ on mux when enabled is set (CERBERUS_DEBUG_PPROF, see
// config.Config.DebugPProf) — a no-op otherwise, so the profiling surface
// never ships open by default. The explicit per-route registration (rather
// than relying on `net/http/pprof`'s init-time DefaultServeMux side effect)
// keeps the handlers on cerberus's own mux and makes the surface auditable in
// one place. /debug/pprof/heap is the one the e2e OOM diagnostics capture
// before pod teardown.
func maybeMountPProf(mux *http.ServeMux, enabled bool, logger *slog.Logger) {
	if !enabled {
		return
	}
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	logger.Warn("pprof debug endpoints enabled (CERBERUS_DEBUG_PPROF) — /debug/pprof/* is reachable on the HTTP listener")
}

// Go 1.24+ `http.Server.Protocols` supersedes the deprecated
// `golang.org/x/net/http2/h2c.NewHandler` wrap — same wire behaviour,
// no extra dep. Behind a TLS-terminating proxy (ingress-nginx, Envoy,
// Cloud Run) the proxy negotiates h2 with the client and forwards
// h2c upstream — the standard pattern. See
// docs/operations.md#port-binding.
func buildDualStackServer(addr string, httpCfg config.HTTPServerConfig, rootMux, grpcServer http.Handler) *http.Server {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		rootMux.ServeHTTP(w, r)
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	// All five timeout / size knobs come from CERBERUS_HTTP_* (internal/config).
	// ReadHeaderTimeout defaults to the promoted 5s; ReadTimeout / WriteTimeout
	// default to 0 (unlimited) so the Loki /tail WebSocket and long query_range
	// matrix streams are never severed mid-response; IdleTimeout bounds an idle
	// keep-alive connection; MaxHeaderBytes 0 leaves Go's 1 MiB default.
	return &http.Server{
		Addr:              addr,
		Handler:           dispatcher,
		Protocols:         protocols,
		ReadTimeout:       httpCfg.ReadTimeout,
		ReadHeaderTimeout: httpCfg.ReadHeaderTimeout,
		WriteTimeout:      httpCfg.WriteTimeout,
		IdleTimeout:       httpCfg.IdleTimeout,
		MaxHeaderBytes:    httpCfg.MaxHeaderBytes,
	}
}
