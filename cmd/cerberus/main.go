// Command cerberus is the three-headed query gateway server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	schemaReady := setupSchema(ctx, logger, client, cfg.ClickHouse.Database, cfg.AutoCreateSchema)

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

	// Build per-head admission-control limiters. When
	// CERBERUS_ADMIT_DISABLED=true every limiter is nil and the
	// middleware short-circuits to a pass-through wrapper. Otherwise
	// each cap comes from CERBERUS_ADMIT_{PROM,LOKI,TEMPO} (or the
	// conservative defaults — see internal/config.admitFromEnv).
	var promLimiter, lokiLimiter, tempoLimiter *admit.Limiter
	if !cfg.Admit.Disabled {
		promLimiter = admit.New("prom", cfg.Admit.MaxInflightProm)
		lokiLimiter = admit.New("loki", cfg.Admit.MaxInflightLoki)
		tempoLimiter = admit.New("tempo", cfg.Admit.MaxInflightTempo)
		logger.Info(
			"admission control enabled",
			"prom", cfg.Admit.MaxInflightProm,
			"loki", cfg.Admit.MaxInflightLoki,
			"tempo", cfg.Admit.MaxInflightTempo,
		)
	} else {
		logger.Info("admission control disabled (CERBERUS_ADMIT_DISABLED=true)")
	}

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
	evalSolver, err := buildSolver(logger, cfg.ClickHouse, client, promLimiter)
	if err != nil {
		return fmt.Errorf("configure solver: %w", err)
	}

	// All three heads run on the shared engine.Engine pipeline; each
	// engine is constructed below from the shared Client + a seed
	// optimizer and assigned onto the per-head handler.
	promHandler := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	promHandler.Engine = &engine.Engine{Optimizer: promHandler.Optimizer, Client: client, Solver: evalSolver}
	promHandler.Limiter = promLimiter
	promHandler.Version = Version
	promHandler.ExperimentalTSGridRange = cfg.ExperimentalTSGridRange
	promHandler.QueryTimeout = cfg.ClickHouse.QueryTimeout
	promHandler.Mount(traceMux)

	lokiHandler := loki.New(client, cfg.Logs, logger.With("api", "loki"))
	lokiHandler.Limiter = lokiLimiter
	lokiHandler.Version = Version
	lokiHandler.QueryTimeout = cfg.ClickHouse.QueryTimeout
	lokiHandler.Mount(traceMux)

	tempoHandler := tempo.New(client, cfg.Traces, Version, logger.With("api", "tempo"))
	tempoHandler.Limiter = tempoLimiter
	tempoHandler.Mount(traceMux)

	tracedAPI := wrapWithOTel(traceMux, "cerberus")

	// /healthz and /readyz live on a separate sub-mux that bypasses
	// otelhttp: k8s probes hit at multi-Hz rates and would otherwise
	// flood the trace backend with no-op spans. The readiness handler
	// memoises results behind a TTL cache so concurrent probes coalesce
	// into a single ClickHouse ping per window.
	healthHandler := health.New(health.Options{
		Pinger:      client,
		SchemaReady: schemaReady,
	})

	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
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

	srv := buildDualStackServer(cfg.HTTPAddr, rootMux, grpcServer)

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
	database string,
	autoCreate bool,
) health.SchemaReadyFunc {
	ready := new(atomic.Bool)
	if !autoCreate {
		ready.Store(true)
		return ready.Load
	}
	logger.Info(
		"auto-creating OTel ClickHouse schema",
		"database", database,
		"signals", "metrics,logs,traces",
	)
	applyCfg := ddl.Config{Database: database}
	apply := func(ctx context.Context) error {
		return ddl.ApplyWithConfig(ctx, client.Conn(), applyCfg, ddl.All)
	}
	if err := apply(ctx); err != nil {
		logger.Warn(
			"auto-create schema failed at startup; retrying in background (/readyz reports schema pending)",
			"err", err,
		)
		go retrySchemaApply(ctx, logger, ready, schemaRetryInterval, apply)
		return ready.Load
	}
	logger.Info("OTel ClickHouse schema ready")
	ready.Store(true)
	return ready.Load
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
) {
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
// Go 1.24+ `http.Server.Protocols` supersedes the deprecated
// `golang.org/x/net/http2/h2c.NewHandler` wrap — same wire behaviour,
// no extra dep. Behind a TLS-terminating proxy (ingress-nginx, Envoy,
// Cloud Run) the proxy negotiates h2 with the client and forwards
// h2c upstream — the standard pattern. See
// docs/operations.md#port-binding.
func buildDualStackServer(addr string, rootMux, grpcServer http.Handler) *http.Server {
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
	return &http.Server{
		Addr:              addr,
		Handler:           dispatcher,
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
