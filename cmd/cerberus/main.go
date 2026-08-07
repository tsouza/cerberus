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
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/api/info"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/schemaboot"
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

// newAdmitLimiters builds the per-head admission-control limiters. When
// CERBERUS_ADMIT_DISABLED=true every limiter is nil and the middleware
// short-circuits to a pass-through wrapper. Otherwise each per-head cap
// CERBERUS_ADMIT_{PROM,LOKI,TEMPO} sizes its limiter directly (resolved by
// config.admitFromEnv from an explicit integer or a true/false alias).
// admit.New returns nil for a non-positive cap, so a disabled head and a
// zero cap collapse to the same pass-through path.
func newAdmitLimiters(cfg config.Config, logger *slog.Logger) (*admit.Limiter, *admit.Limiter, *admit.Limiter) {
	if cfg.Admit.Disabled {
		logger.Info("admission control disabled (CERBERUS_ADMIT_DISABLED=true)")
		return nil, nil, nil
	}
	promCap := cfg.Admit.Prom
	lokiCap := cfg.Admit.Loki
	tempoCap := cfg.Admit.Tempo
	logger.Info(
		"admission control enabled",
		"prom", promCap,
		"loki", lokiCap,
		"tempo", tempoCap,
	)
	return admit.New("prom", promCap), admit.New("loki", lokiCap), admit.New("tempo", tempoCap)
}

// apiHeads carries what run() needs back from mountAPIHeads: the Tempo gRPC
// server (nil when the tempo head is disabled, so the dual-stack dispatcher
// skips the gRPC branch and shutdown skips GracefulStop) and the capability-set
// consumers the periodic re-probe swaps a re-resolved set into.
type apiHeads struct {
	grpcServer *grpc.Server
	consumers  chOptConsumers
}

// mountAPIHeads builds and mounts ONLY the query heads enabled by
// CERBERUS_ENABLED_HEADS (default: all three) onto traceMux. A disabled head's
// handler, per-head Client view, engine, and admit limiter are NEVER built and
// its routes are NEVER mounted — so a single-head process carries none of the
// other heads' memory, the property that lets one head be isolated in its own
// deployment/cgroup (today all three share one process, so one head's OOM kills
// the others). The Tempo gRPC StreamingQuerier server is built (and wired to
// the tempo handler) only when tempo is enabled. The async query_log corpus
// reconciler is registered against exactly the engines that were built. The
// /healthz + /readyz probes are mounted by the caller, unconditionally, in
// every mode.
//
// At least one head is always enabled (config.enabledHeadsFromEnv rejects an
// empty set), so traceMux always gets at least one head's routes.
func mountAPIHeads(
	ctx context.Context,
	traceMux *http.ServeMux,
	client *chclient.Client,
	cfg config.Config,
	optSet chopt.EnabledSet,
	promLimiter, lokiLimiter, tempoLimiter *admit.Limiter,
	logger *slog.Logger,
) (apiHeads, error) {
	// engines accumulates the engines actually built so the corpus reconciler
	// observes only live heads (a disabled head has no engine to observe), and
	// so the capability re-probe swaps a re-resolved set into exactly the heads
	// this process serves.
	var engines []*engine.Engine
	// promHandler stays nil when the prom head is disabled; it is the only head
	// carrying a native-lowering dispatch table for the re-probe to swap.
	var promHandler *prom.Handler

	if cfg.HeadEnabled(config.HeadProm) {
		// Per-head Client VIEW (#94): own breaker over the shared pool. Built
		// only for the prom head — and the prom-only sharded-pushdown solver is
		// built from it, so when prom is disabled neither the view nor the
		// solver exists.
		promClient := client.ForHead(chclient.HeadProm)
		evalSolver, err := buildSolver(logger, cfg.ClickHouse, promClient, promLimiter)
		if err != nil {
			return apiHeads{}, fmt.Errorf("configure solver: %w", err)
		}
		promHandler = newPromHandler(promClient, cfg, optSet, evalSolver, promLimiter, logger)
		promHandler.Mount(traceMux)
		engines = append(engines, promHandler.Engine)
	}

	if cfg.HeadEnabled(config.HeadLoki) {
		lokiClient := client.ForHead(chclient.HeadLoki)
		lokiHandler := newLokiHandler(lokiClient, cfg, optSet, lokiLimiter, logger)
		lokiHandler.Mount(traceMux)
		engines = append(engines, lokiHandler.Engine)
	}

	var grpcServer *grpc.Server
	if cfg.HeadEnabled(config.HeadTempo) {
		tempoClient := client.ForHead(chclient.HeadTempo)
		tempoHandler := tempo.New(tempoClient, cfg.Traces, Version, logger.With("api", "tempo"))
		tempoHandler.Limiter = tempoLimiter
		tempoHandler.StructuralTwoPhase = cfg.TempoStructuralTwoPhase
		tempoHandler.Engine.Settings = settingsRules(cfg, optSet)
		tempoHandler.Mount(traceMux)
		engines = append(engines, tempoHandler.Engine)

		// Tempo gRPC StreamingQuerier — shares the Tempo HTTP handler's Engine
		// + schema + admit limiter so the streaming RPC bodies and the HTTP
		// handlers run the same parse + lower + emit pipeline. Built only when
		// tempo is enabled; nil otherwise.
		tempoGRPCService := tempogrpc.NewService(tempoHandler, tempoLimiter, logger.With("api", "tempo-grpc"))
		grpcServer = tempogrpc.NewServer(tempoGRPCService)
	}

	logger.Info("query heads enabled", "heads", strings.Join(enabledHeadNames(cfg), ","))

	// Async query_log performance-corpus reconciler (off by default). When
	// enabled it registers itself as each BUILT head engine's QueryObserver. A
	// no-op when disabled, leaving the engines' observer nil (byte-unchanged
	// hot path).
	startOptCorpus(ctx, logger, client, cfg, engines...)

	return apiHeads{
		grpcServer: grpcServer,
		consumers:  chOptConsumers{engines: engines, prom: promHandler},
	}, nil
}

// servedHead pairs a head's config identity (the CERBERUS_ENABLED_HEADS token,
// which is also the /readyz `heads` key) with the chclient registry key whose
// breaker fronts it.
type servedHead struct {
	name   config.Head
	broker chclient.Head
}

// allServedHeads is the config↔chclient head correspondence, in the canonical
// prom,loki,tempo order. It is the ONE place the two head vocabularies are
// mapped onto each other.
var allServedHeads = [...]servedHead{
	{config.HeadProm, chclient.HeadProm},
	{config.HeadLoki, chclient.HeadLoki},
	{config.HeadTempo, chclient.HeadTempo},
}

// enabledHeadBreakers builds the /readyz per-head breaker reporter for exactly
// the heads CERBERUS_ENABLED_HEADS turned on. The enablement set is immutable
// after boot, so which heads to report is resolved once here; the returned
// closure reads only LIVE breaker state, per probe.
//
// Scoping to the enabled set is what makes the probe's head-exhaustion gate
// correct under the chart's split mode: a Deployment that serves only tempo
// reports only tempo, so a tripped tempo breaker takes that pod out of its
// Service — while a prom/loki pod, whose own heads are healthy, stays in.
// Reporting a head this process never built would evict pods for a breaker no
// request can ever reach.
func enabledHeadBreakers(client *chclient.Client, cfg config.Config) health.HeadBreakersFunc {
	served := make([]servedHead, 0, len(allServedHeads))
	for _, h := range allServedHeads {
		if cfg.HeadEnabled(h.name) {
			served = append(served, h)
		}
	}
	if len(served) == 0 {
		return nil
	}
	return func() map[string]string {
		states := client.HeadBreakerStates()
		out := make(map[string]string, len(served))
		for _, h := range served {
			if state, ok := states[h.broker]; ok {
				out[string(h.name)] = state
			}
		}
		return out
	}
}

// enabledHeadNames returns the enabled heads in the canonical prom,loki,tempo
// order for a stable log line (the EnabledHeads set is unordered).
func enabledHeadNames(cfg config.Config) []string {
	var names []string
	for _, h := range allServedHeads {
		if cfg.HeadEnabled(h.name) {
			names = append(names, string(h.name))
		}
	}
	return names
}

func main() {
	// --version / -v / version is resolved BEFORE cobra is constructed and is the
	// SOLE authority for a version dump: the distroless container healthcheck
	// (PR #297) probes the binary this way and depends on the output being the
	// bare Version string. cobra's built-in .Version mechanism is deliberately
	// NOT used — it reformats the output and would claim -v for itself.
	if isVersionFlag(os.Args) {
		printVersion(os.Stdout)
		return
	}

	// Bootstrap logger used only until config.FromEnv returns and the
	// configured logger replaces it. Text + info matches the configured
	// defaults so the upgrade is invisible when env vars are unset.
	bootstrap := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(bootstrap)

	// Bare `cerberus` (no subcommand) starts the server via the root RunE; the
	// subcommands cover migrate + the doc/analysis generators. main() owns the
	// slog.Error + os.Exit: cobra runs with SilenceErrors so the error is logged
	// once here, and the typed migration errors are mapped to their exit codes.
	if err := newRootCmd(run).Execute(); err != nil {
		slog.Default().Error("cerberus exited with error", "err", err)
		os.Exit(exitCodeForError(err))
	}
}

// installStage2Logging replaces the stderr-only startup logger with the one
// that fans every record out to BOTH the stderr handler (12-factor stream /
// `kubectl logs` readability) AND the OTel slog bridge (records ship via OTLP
// gRPC to the collector → CH `otel_logs`, landing alongside the same binary's
// traces and metrics so a self-dashboard works against a running cluster).
// With the endpoint empty, providers.LoggerProvider is the no-op
// LoggerProvider — the bridge is a no-op and only stderr is written.
//
// It also routes the OTel SDK's own failures (export batches it could not
// ship, collect / shutdown errors) into that same structured logger at WARN,
// so a failure of the pipeline carrying cerberus's metrics, traces and logs is
// itself visible — on stderr always, and over OTLP whenever the bridge is up.
func installStage2Logging(cfg config.Config, providers *telemetry.Providers) *slog.Logger {
	logger := config.NewTelemetryLogger(os.Stderr, cfg.Log, providers.LoggerProvider)
	slog.SetDefault(logger)
	telemetry.InstallErrorHandler(logger)

	if cfg.OTLP.Endpoint != "" {
		logger.Info(
			"OTLP exporters enabled",
			"endpoint", cfg.OTLP.Endpoint,
			"insecure", cfg.OTLP.Insecure,
		)
	}
	return logger
}

// newTelemetryProviders builds the OTel provider set from the OTLP env config.
// When CERBERUS_OTLP_ENDPOINT is empty the telemetry package returns noop
// providers, so cerberus stays a zero-collector-dependency binary by default.
// Installing what it returns is the caller's job — run() does it before
// anything that mints an instrument.
func newTelemetryProviders(ctx context.Context, cfg config.Config) (*telemetry.Providers, error) {
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
		return nil, fmt.Errorf("init telemetry: %w", err)
	}
	return providers, nil
}

func run() error {
	// Captured first so the /info fingerprint's uptimeSeconds counts from the
	// earliest point in process lifetime, before any config/connection work.
	startTime := time.Now()

	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Stage-1 logger: stderr-only, and it covers exactly one line — the
	// startup banner, which announces the OTLP target itself and so cannot
	// ship over the bridge that target is on. Everything after the telemetry
	// block below runs on the stage-2 logger.
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

	// Install the W3C+Baggage propagator and the OTel providers built from the
	// OTLP env config (see newTelemetryProviders).
	//
	// This is the FIRST thing run() does after the signal context, and the
	// ordering is load-bearing rather than stylistic: OTel's global
	// MeterProvider is a delegating shim, and a synchronous counter Add
	// recorded before the real provider is installed is dropped outright —
	// there is no buffering. Every zero-seeded counter cerberus mints at
	// construction (the ClickHouse connection-lifecycle set, the per-head
	// breaker set, the admit limiters' rejected counters) would therefore
	// lose exactly the seed that makes a healthy replica export a flat 0
	// instead of "No data". Observable gauges re-register and survive, which
	// is what makes the loss so easy to miss: the pool census still appears
	// while the counters beside it silently do not.
	// Pinned by test/regression/telemetry_provider_ordering_test.go.
	providers, err := newTelemetryProviders(ctx, cfg)
	if err != nil {
		return err
	}
	installOTel(providers.TracerProvider)
	otel.SetMeterProvider(providers.MeterProvider)

	logger = installStage2Logging(cfg, providers)

	// Construction is lazy — chclient.New never dials, it only
	// validates options. An error here is misconfiguration that can
	// never succeed (fail-fast is correct); connectivity problems
	// surface on the best-effort Ping below and on /readyz.
	client, err := chclient.New(cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("configure clickhouse client: %w", err)
	}
	// Safety net for every early-return path below (schema boot, requirements
	// check, head mounting, …) that exits before the shutdownSteps below are
	// ever built. The graceful-shutdown path closes the client explicitly, via
	// steps.closeCH; Client.Close is documented idempotent, so this defer
	// firing again afterward is a deliberate no-op, not a double-close bug.
	defer func() {
		_ = client.Close()
	}()

	warnIfClickHouseUnreachable(ctx, logger, client, cfg.ClickHouse)

	// Resolve the ClickHouse-optimization auto-picker ONCE, here, after the
	// client is built and the runtime version is probed. The version probe runs
	// over a short-lived connection bound to ClickHouse's always-present
	// `default` database (NOT the configured otel one), because this runs before
	// setupSchema creates otel and a session whose default database is absent
	// rejects every statement, version() included (code 81). config.FromEnv has
	// no live connection, so it carried only the raw CERBERUS_CH_OPTIMIZATIONS
	// selection + parsed mode + the tri-state legacy alias; this is where they
	// become the EnabledSet every consumer reads. A fatal resolve
	// (unknown feature id in any mode, or an unsupported explicit id under
	// enforcing) aborts startup. The resolved set then back-fills
	// cfg.ExperimentalTSGridRange (the single source of truth for the legacy
	// ts-grid consumers), drives the per-query SettingsRules built below, and
	// the main client (passed here) gets the one boot-time columnar-decode swap.
	//
	// The resolution is a reading of a LIVE server, not a fact about this
	// process, so it is held in chOptLive and re-read on a fixed cadence: a
	// rolling ClickHouse upgrade that crosses a feature floor moves the answer
	// under a running pod, and reprobeCHOptimizations (started once the heads
	// are mounted) swaps the new one into the query path.
	optRes, err := resolveCHOptimizations(ctx, logger, client, &cfg)
	if err != nil {
		return err
	}
	optSet := optRes.Set
	chOpts := newCHOptLive(optRes)

	// schemaReady reports whether the auto-create-schema startup hook
	// has finished at least once; /readyz consults it on every probe.
	applyCfg, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		return err
	}
	schemaReady := setupSchema(ctx, logger, client, cfg.ClickHouse, applyCfg, cfg.AutoCreateSchema, cfg.AutoCreateDatabase)

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
	// Build + mount only the ENABLED heads (CERBERUS_ENABLED_HEADS; default
	// all three). A disabled head's handler/client/limiter is never built and
	// its routes are never mounted, so a single-head process holds no engine,
	// no per-head Client view, and no admit limiter for the other two — the
	// memory win that motivates splitting cerberus into per-head deployments
	// (one process = one OOM kills all heads today). The Tempo gRPC server is
	// likewise nil when tempo is off. /healthz + /readyz are mounted below,
	// unconditionally, in every mode.
	heads, err := mountAPIHeads(ctx, traceMux, client, cfg, optSet, promLimiter, lokiLimiter, tempoLimiter, logger)
	if err != nil {
		return err
	}
	grpcServer := heads.grpcServer

	// Periodic capability re-probe: re-resolves the optimization set against the
	// connected server and swaps a changed result into the heads mounted above,
	// so an upgraded ClickHouse is picked up without restarting cerberus. Bound
	// to the run ctx, so SIGTERM stops it.
	go reprobeCHOptimizations(ctx, logger, cfg, chOpts, heads.consumers, chOptReprobeInterval)

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
	// The per-head breaker report is scoped to the ENABLED heads, so the
	// probe's head-exhaustion gate means "this pod can serve nothing it was
	// deployed to serve" in every deployment mode — including the split mode
	// where one Deployment serves a single head.
	healthHandler := health.New(health.Options{
		Pinger:        client.ForHead(chclient.HeadProbe),
		SchemaReady:   schemaReady,
		SchemaPresent: schemaPresent,
		HeadBreakers:  enabledHeadBreakers(client, cfg),
	})

	// /info is cerberus's own metadata/health/connection fingerprint — a
	// top-level, unauthenticated sibling to /healthz + /readyz, deliberately
	// NOT under the upstream-compat buildinfo namespaces (which must mirror
	// Prometheus/Loki byte-for-byte). It reads the SAME HeadProbe breaker the
	// readiness probe uses for its live reachability/breaker fields, and
	// reuses the /readyz readiness condition for "ready". Like the health
	// probes it bypasses otelhttp (low-frequency metadata scrape, no spans).
	infoHandler := info.New(infoOptions(client, cfg, chOpts, schemaReady, schemaPresent, startTime))

	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
	infoHandler.Mount(rootMux)
	maybeMountPProf(rootMux, cfg.DebugPProf, logger)
	rootMux.Handle("/", tracedAPI)

	// The Tempo gRPC StreamingQuerier server was built (and wired to the
	// Tempo handler) inside mountAPIHeads when the tempo head is enabled; it
	// is nil when tempo is disabled, in which case buildDualStackServer skips
	// the gRPC dispatch branch entirely.
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()

	steps := shutdownSteps{
		drainHTTP: srv.Shutdown,
		flushOTLP: providers.Shutdown,
		closeCH:   client.Close,
	}
	// nil when the tempo head is disabled — no gRPC server was built.
	if grpcServer != nil {
		steps.stopGRPC = func(graceful bool) {
			if graceful {
				grpcServer.GracefulStop()
				return
			}
			grpcServer.Stop()
		}
	}
	if err := shutdown(shutdownCtx, steps, logger); err != nil {
		return err
	}
	logger.Info("cerberus stopped")
	return nil
}

// shutdownSteps is the teardown surface run() owns, narrowed to the four calls
// whose ORDER and whose run-even-on-failure contract are the thing worth
// asserting. Extracting it is what makes that contract reachable from a test at
// all: run() itself needs a listener, a ClickHouse pool and a signal.
//
// stopGRPC is nil when no gRPC server was built. Its argument is whether the
// drain may block: GracefulStop takes no context and waits for every active RPC
// to return, so calling it after the HTTP drain has already blown its deadline
// would inherit that hang with nothing left to bound it.
//
// closeCH is run() 's client.Close, which run() ALSO defers unconditionally so
// every early-return path (config load, schema boot, requirements check — all
// before this steps struct is ever built) still tears the pool down. Routing it
// through this struct too is safe rather than a double-teardown risk because
// Client.Close is documented idempotent (internal/chclient/client.go): the
// recovery-loop stop and the pool close both guard against a second call. This
// is the one path that lets a test observe the CLOSE HAPPENING on the graceful
// exit, not just on process teardown the test can't drive.
type shutdownSteps struct {
	drainHTTP func(context.Context) error
	stopGRPC  func(graceful bool)
	flushOTLP func(context.Context) error
	closeCH   func() error
}

// shutdown tears the process down in dependency order and runs EVERY step even
// when an earlier one fails, returning the first error.
//
// The order is a dependency chain: the HTTP drain closes the HTTP/2 transports
// the gRPC streams ride on, so the gRPC drain after it is a no-op on the happy
// path; the telemetry flush comes next because the two steps before it emit the
// spans and metrics it is flushing; the ClickHouse client closes LAST because
// the HTTP drain is what guarantees no in-flight query is still using it.
//
// Running the tail unconditionally is the whole point. Returning at the first
// error would drop exactly the telemetry that describes the failed shutdown —
// the one teardown an operator actually needs to see — and would leave the gRPC
// listener holding its sockets, or the ClickHouse pool holding its connections,
// until the process image went away.
func shutdown(ctx context.Context, steps shutdownSteps, logger *slog.Logger) error {
	var firstErr error
	drained := true
	if err := steps.drainHTTP(ctx); err != nil {
		drained = false
		firstErr = fmt.Errorf("graceful shutdown: %w", err)
		logger.Warn("http graceful shutdown returned error", "err", err)
	}
	if steps.stopGRPC != nil {
		steps.stopGRPC(drained)
	}
	// Noop when telemetry was disabled (Endpoint == "").
	if err := steps.flushOTLP(ctx); err != nil {
		logger.Warn("telemetry shutdown returned error", "err", err)
	}
	// Mirrors run()'s outer `_ = client.Close()` discard: a close failure here
	// is logged, never promoted to the process's exit status, and never masks
	// an earlier, more actionable failure already captured in firstErr.
	if err := steps.closeCH(); err != nil {
		logger.Warn("clickhouse client close returned error", "err", err)
	}
	return firstErr
}

// solverGateReserve is the number of pooled connections the solver's GLOBAL
// shard gate leaves untouched so route-A traffic (the overwhelming majority)
// always has headroom even when a routed fan-out is holding gate slots. The
// gate is sized MaxOpenConns − reserve; the Executor additionally caps any
// single routed request at gate/2 so >=2 routed requests can always progress.
const solverGateReserve = 2

// gracefulShutdownTimeout bounds in-flight request draining + provider
// teardown after a shutdown signal before the process exits regardless.
const gracefulShutdownTimeout = 10 * time.Second

// newPromHandler builds the prom head's handler with its engine (per-head
// Client view + seed optimizer + solver), limiter, and runtime knobs wired in.
func newPromHandler(client *chclient.Client, cfg config.Config, optSet chopt.EnabledSet, evalSolver *solver.Solver, limiter *admit.Limiter, logger *slog.Logger) *prom.Handler {
	h := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	h.Engine = &engine.Engine{
		Optimizer:       h.Optimizer,
		Client:          client,
		Solver:          evalSolver,
		Settings:        settingsRules(cfg, optSet),
		MaxQuerySamples: client.MaxQuerySamples(),
		RouteMemo:       buildRouteMemo(evalSolver, logger),
	}
	h.Limiter = limiter
	h.Version = Version
	h.Lowerers = nativeRangeLowerers(optSet)
	h.QueryTimeout = cfg.ClickHouse.QueryTimeout
	// CERBERUS_PROM_METADATA_LOOKBACK is the ONLY input to the windowless
	// metadata-discovery horizon; zero leaves the handler on its own
	// fallback. Nothing else in the config is evidence of what ClickHouse
	// physically retains — see config.Config.PromMetadataLookback.
	h.MetadataLookback = cfg.PromMetadataLookback
	return h
}

// buildRouteMemo wires the failure-driven route memo (internal/routememo,
// docs/solver.md §"Failure-driven route memo") when explicitly enabled via
// CERBERUS_SOLVER_ROUTE_MEMO_ENABLED (default false — see
// solver.Config.RouteMemoEnabled's doc for why this is opt-in rather than
// riding along with Mode=auto/sharded automatically). Returns nil (the
// engine's byte-unchanged, feature-off default) when disabled or when
// evalSolver is nil.
//
// The pressure damper's correlation window is the solver's OWN effective
// wall-clock deadline (Solver.EffectiveTimeout) — the horizon a single
// fan-out's resource pressure stays live server-side, per routememo.New's
// own doc — rather than a second, independently-configured duration that
// could drift out of step with it. RouteMemoEntryTTL / RouteMemoReValidationFraction
// (both zero-value-means-"use the routememo package default") are applied
// after construction so an operator can override either without this
// function needing to know the routememo package's own default constants.
func buildRouteMemo(evalSolver *solver.Solver, logger *slog.Logger) *routememo.Memo {
	if evalSolver == nil || !evalSolver.Cfg.RouteMemoEnabled {
		return nil
	}
	memo := routememo.New(evalSolver.EffectiveTimeout())
	// Both setters no-op on the Config zero value, so these calls are safe
	// unconditionally regardless of whether the operator set either var —
	// an unset RouteMemoEntryTTL / RouteMemoReValidationFraction leaves the
	// memo on the routememo package's own default.
	memo.SetEntryTTL(evalSolver.Cfg.RouteMemoEntryTTL)
	memo.SetReValidationFraction(evalSolver.Cfg.RouteMemoReValidationFraction)
	logger.Info(
		"failure-driven route memo wired",
		"pressure_window", evalSolver.EffectiveTimeout(),
		"entry_ttl", evalSolver.Cfg.RouteMemoEntryTTL,
		"revalidation_fraction", evalSolver.Cfg.RouteMemoReValidationFraction,
	)
	return memo
}

// nativeRangeLowerers builds the BOOT-WIRED polymorphic lowering dispatch table
// for the ClickHouse-native timeSeries*ToGrid family from the resolved
// optimization EnabledSet. The feature/version decision is made HERE, ONCE, at
// boot (optSet was produced by the single boot-time version probe in
// resolveCHOptimizations) and is the ONLY place the feature is read: each field
// is wired to a CONCRETE non-nil strategy —
//
//	rate      = enabled ? NativeRateLowerer{Fallback: FanoutRateLowerer{}} : FanoutRateLowerer{}
//	staleness = enabled ? NativeStalenessLowerer{Fallback: FanoutStalenessLowerer{}} : FanoutStalenessLowerer{}
//	changes   = enabled ? NativeChangesLowerer{Fallback: FanoutChangesLowerer{}} : FanoutChangesLowerer{}
//	resets    = enabled ? NativeResetsLowerer{Fallback: FanoutResetsLowerer{}} : FanoutResetsLowerer{}
//	deriv     = enabled ? NativeDerivLowerer{Fallback: FanoutDerivLowerer{}} : FanoutDerivLowerer{}
//	predict   = enabled ? NativePredictLinearLowerer{Fallback: FanoutPredictLinearLowerer{}} : FanoutPredictLinearLowerer{}
//
// The fan-out impl is the concrete DEFAULT (never nil), and the native impl
// embeds it as the fallback for shapes it cannot handle. The features are
// independent, so the table composes per-function — native rate can be on while
// native staleness / changes / resets / deriv / predict_linear are off, and vice
// versa (the whole family shares the 25.9 floor, but each member is probed and
// resolved on its own). The per-query
// lowering then
// dispatches through this table as a plain interface method call: NO
// feature/version read, NO nil/presence check.
//
// ts_grid_recollapse is the one NON-independent knob: it defers the
// label-shaping tower past the native rate grid, so it only means anything
// inside the ts_grid_range branch. The registry has no inter-feature dependency
// mechanism, so that narrowing is expressed HERE — reading it only where a
// native rate lowerer is being built makes "recollapse without a native grid"
// unrepresentable rather than merely unlikely.
func nativeRangeLowerers(optSet chopt.EnabledSet) promql.RangeLowerers {
	var l promql.RangeLowerers
	if optSet.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   promql.FanoutRateLowerer{},
			Recollapse: optSet.Has(chopt.FeatureTSGridRecollapse),
		}
	} else {
		l.Rate = promql.FanoutRateLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
	} else {
		l.Changes = promql.FanoutChangesLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
	} else {
		l.Resets = promql.FanoutResetsLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridDeriv) {
		l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
	} else {
		l.Deriv = promql.FanoutDerivLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridPredictLinear) {
		l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
	} else {
		l.PredictLinear = promql.FanoutPredictLinearLowerer{}
	}
	return l
}

// newLokiHandler builds the Loki head's handler with its limiter, version,
// timeouts, and the resolved per-query optimization SettingsRules wired in.
// Extracted (mirroring newPromHandler) so run's bootstrap stays within its
// maintainability budget as the optimization suite adds wiring.
func newLokiHandler(client *chclient.Client, cfg config.Config, optSet chopt.EnabledSet, limiter *admit.Limiter, logger *slog.Logger) *loki.Handler {
	h := loki.New(client, cfg.Logs, logger.With("api", "loki"))
	h.Limiter = limiter
	h.Version = Version
	h.QueryTimeout = cfg.ClickHouse.QueryTimeout
	h.TailWriteTimeout = cfg.LokiTailWriteTimeout
	h.Engine.Settings = settingsRules(cfg, optSet)
	return h
}

// settingsRules builds the per-query ClickHouse settings rules from the
// resolved optimization EnabledSet plus the CERBERUS_* config. The
// aggregation-in-order and condition-cache rules are now driven by the frozen
// EnabledSet (set.Has(...)), not raw env flags: under the default `auto` the
// stable 24.8-safe aggregation_in_order is on, and condition_cache is on when
// the probed server is >= 25.3. log_comment shape stays its own dark flag
// (CERBERUS_LOG_COMMENT_SHAPE), wired alongside the corpus reconciler. The
// schema instances are always supplied so the eligibility checks can map ANY
// scanned signal table to its sort-key prefix regardless of which head runs
// the query. Shared by all three heads' engines so the rules flip uniformly.
func settingsRules(cfg config.Config, set chopt.EnabledSet) engine.SettingsRules {
	return engine.SettingsRules{
		OptimizeAggregationInOrder: set.Has(chopt.FeatureAggregationInOrder),
		ConditionCache:             set.Has(chopt.FeatureConditionCache),
		LogCommentShape:            cfg.LogCommentShape,
		Metrics:                    cfg.Schema,
		Traces:                     cfg.Traces,
		Logs:                       cfg.Logs,
	}
}

// infoOptions assembles the /info handler options: the static boot Snapshot
// (build identity, enabled heads, CH address/database, and the raw optimization
// SELECTION, all of which are configuration) plus the live closures the handler
// re-reads per request. The live reachability + readiness funcs run over the
// HeadProbe breaker view — the SAME breaker /readyz uses — so /info's clickhouse
// fields agree with the readiness probe; "ready" mirrors the /readyz condition
// (CH reachable AND schema present AND schema ready).
//
// The RESOLVED capability decision is a live closure rather than a snapshot
// field: it is a reading of the connected server, and the periodic re-probe
// moves it when the cluster is upgraded (see reprobeCHOptimizations).
func infoOptions(
	client *chclient.Client,
	cfg config.Config,
	live *chOptLive,
	schemaReady health.SchemaReadyFunc,
	schemaPresent health.SchemaPresentFunc,
	startTime time.Time,
) info.Options {
	probe := client.ForHead(chclient.HeadProbe)

	schemaReadyNow := func() bool {
		return schemaReady == nil || schemaReady()
	}
	schemaPresentNow := func() bool {
		if schemaPresent == nil {
			return true
		}
		present, _ := schemaPresent()
		return present
	}

	return info.Options{
		Snapshot: info.Snapshot{
			Service:      "cerberus",
			Version:      Version,
			Revision:     buildRevision(),
			GoVersion:    runtime.Version(),
			Heads:        enabledHeadsList(cfg),
			CHAddress:    cfg.ClickHouse.Addr,
			CHDatabase:   cfg.ClickHouse.Database,
			OptSelection: cfg.CHOptimizations,
			OptMode:      cfg.CHOptimizationsMode.String(),
		},
		Optimizations: live.infoState,
		StartTime:     startTime,
		Reachable:     func(ctx context.Context) bool { return probe.Ping(ctx) == nil },
		Breaker:       probe.PeekBreakerState,
		SchemaReady:   schemaReadyNow,
		Ready: func(ctx context.Context) bool {
			return probe.Ping(ctx) == nil && schemaPresentNow() && schemaReadyNow()
		},
	}
}

// enabledHeadsList renders the ENABLED heads in the canonical prom/loki/tempo
// order for the /info fingerprint. The order is fixed (not map iteration) so
// the body is deterministic.
func enabledHeadsList(cfg config.Config) []string {
	order := []config.Head{config.HeadProm, config.HeadLoki, config.HeadTempo}
	heads := make([]string, 0, len(order))
	for _, h := range order {
		if cfg.HeadEnabled(h) {
			heads = append(heads, string(h))
		}
	}
	return heads
}

// buildRevision returns the VCS commit the binary was built from, read from
// the embedded build info (runtime/debug). It is "unknown" when the build
// carries no VCS stamp (e.g. `go test` binaries or a build with -buildvcs=false).
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}

// chOptResolution is a ClickHouse-optimization decision, captured for the
// consumers that need more than the EnabledSet: the engine/handler wiring reads
// Set, while the /info fingerprint also reports the version the auto-picker
// resolved against and whether that version was probed live or assumed from the
// supported floor (VersionFallback) after a failed probe.
type chOptResolution struct {
	Set             chopt.EnabledSet
	ResolvedVersion chopt.Version
	VersionFallback bool
}

// supportedFloorVersion is the oldest ClickHouse cerberus supports, and the
// version an unreachable server is assumed to be running: resolving against it
// keeps every floor-safe optimization on while holding back anything newer, so
// a failed probe degrades the feature set rather than the process.
var supportedFloorVersion = chopt.Version{Major: 24, Minor: 8}

// resolveCHOptimizations probes the connected ClickHouse server version and
// resolves the CERBERUS_CH_OPTIMIZATIONS auto-picker against it at boot,
// returning the EnabledSet the process starts on. It back-fills
// cfg.ExperimentalTSGridRange from the resolved set so the legacy ts-grid
// consumers (the PromQL lowering, the engine native gate, the preflight version
// floor) read a single source of truth, and logs the resolved set + the server
// version + any warnings (permissive skips and the legacy-alias deprecation).
//
// The version probe is best-effort with respect to CONNECTIVITY: cerberus is
// designed to boot even when ClickHouse is briefly unreachable (the
// cerberus + collector startup race, where the background re-probe flips
// /readyz once the schema lands). A probe that fails to reach the server is
// therefore NOT fatal here; it falls back to the documented supported floor
// (24.8) so the stable 24.8-safe optimizations still resolve under `auto`,
// while any newer feature (condition_cache, ts_grid_range) stays off until
// reprobeCHOptimizations reaches a server that answers. A genuine CONFIG fault
// (unknown feature id, or an unsupported explicit id under enforcing) is still
// fatal — that is a typo/operator error, independent of connectivity.
func resolveCHOptimizations(ctx context.Context, logger *slog.Logger, client *chclient.Client, cfg *config.Config) (chOptResolution, error) {
	resolvedVersion, err := probeVersionOverBootstrap(ctx, cfg.ClickHouse)
	versionFallback := err != nil
	if err != nil {
		// Connectivity fallback: assume the supported floor so 24.8-safe
		// stable features still resolve under auto; newer features stay off
		// until the periodic re-probe reaches a server that answers. Never
		// fatal on a probe read error.
		resolvedVersion = supportedFloorVersion
		logger.Warn(
			"clickhouse version probe failed; resolving optimizations against the supported floor until the next re-probe",
			"err", err,
			"assumed_version", resolvedVersion.String(),
		)
	}

	// Capability canary: a server may meet the native ts_grid version floor yet
	// still FORBID the experimental setting cerberus stamps on the native node (a
	// hardened/constrained profile, or a readonly user). Auto-selecting the
	// native node there would only earn a SETTING_CONSTRAINT_VIOLATION at query
	// time, breaking a deployment that worked on fan-out. The canary probes the
	// setting at boot over the same bootstrap/default-DB connection the version
	// probe uses, and the verdict gates the four RequiresExperimentalTSGrid
	// features in Resolve. A failed/unreachable canary is conservative (native
	// stays off), never fatal here.
	capability := probeTSGridCapabilityOverBootstrap(ctx, cfg.ClickHouse)

	set, warnings, err := chopt.Resolve(chopt.Config{
		Optimizations: cfg.CHOptimizations,
		Mode:          cfg.CHOptimizationsMode,
		LegacyTSGrid:  cfg.LegacyTSGridFlag,
		Capability:    capability,
	}, resolvedVersion)
	if err != nil {
		return chOptResolution{}, fmt.Errorf("resolve clickhouse optimizations: %w", err)
	}
	for _, w := range warnings {
		logger.Warn("ch_opt: " + w)
	}

	// Single source of truth: the legacy ts-grid bool is now derived from the
	// resolved set, not the raw env.
	cfg.ExperimentalTSGridRange = set.Has(chopt.FeatureTSGridRange)

	// Install the client-side columnar matrix decode when the resolved set
	// enables it. columnar_result_decode is a chopt feature (opt-in, never
	// auto), so its enable decision flows through the EnabledSet exactly like
	// every other optimization rather than a standalone env bool. The client
	// was built on the row path at New (the version probe above needed it); this
	// is the one boot-time swap, run before any handler serves. cfg.ClickHouse
	// is the Config the columnar strategy's second ch-go dial maps off of.
	client.UseColumnarMatrixDecode(set.Has(chopt.FeatureColumnarResultDecode), cfg.ClickHouse)

	logger.Info(
		"clickhouse optimizations resolved",
		"selection", cfg.CHOptimizations,
		"mode", cfg.CHOptimizationsMode.String(),
		"server_version", resolvedVersion.String(),
		"server_ts_grid_capability", capability.String(),
		"enabled", strings.Join(set.IDs(), ","),
	)
	return chOptResolution{
		Set:             set,
		ResolvedVersion: resolvedVersion,
		VersionFallback: versionFallback,
	}, nil
}

// probeVersionOverBootstrap issues the SELECT version() probe over a
// short-lived client bound to ClickHouse's always-present `default` database,
// not the configured (otel) one. The version probe must succeed on a fresh or
// freshly-upgraded server whose configured database does not exist yet: it runs
// at boot BEFORE setupSchema creates the target database, and ClickHouse rejects
// EVERY statement — version() included — on a session whose default database is
// absent (code 81, UNKNOWN_DATABASE). Binding the probe to `default` (which is
// always present, the same database the auto-create DDL targets) makes the probe
// independent of whether the configured database exists, so a CH upgrade takes
// effect on the next boot instead of being masked as a probe failure that pins
// the supported floor. The client is opened, probed, and closed here — it never
// outlives the probe; the breaker-guarded read surface still makes a genuinely
// unreachable server fail (not hang), preserving the connectivity fallback.
func probeVersionOverBootstrap(ctx context.Context, chCfg chclient.Config) (chopt.Version, error) {
	bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg, versionProbePool))
	if err != nil {
		return chopt.Version{}, fmt.Errorf("open bootstrap client for version probe: %w", err)
	}
	defer func() {
		_ = bootClient.Close()
	}()
	return bootClient.ProbeVersion(ctx)
}

// probeTSGridCapabilityOverBootstrap runs the experimental-setting capability
// canary over a short-lived client bound to ClickHouse's always-present
// `default` database, exactly like probeVersionOverBootstrap. The canary must
// not depend on the configured (otel) database existing -- it runs at boot
// BEFORE setupSchema creates it, and ClickHouse rejects every statement on a
// session whose default database is absent (code 81), which would masquerade as
// a capability rejection. Binding to `default` keeps the verdict about the
// SETTING, not the missing database. A failure to even open the client is
// itself an unreachable verdict (conservative: native stays off), never fatal.
func probeTSGridCapabilityOverBootstrap(ctx context.Context, chCfg chclient.Config) chopt.Capability {
	bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg, tsGridProbePool))
	if err != nil {
		return chopt.CapabilityUnreachable
	}
	defer func() {
		_ = bootClient.Close()
	}()
	return bootClient.ProbeTSGridCapability(ctx)
}

// maxOptCorpusSourceTimeout caps the per-scan wall-clock bound derived from
// the reconcile interval, so a long interval cannot hand one corpus SELECT
// minutes of runway.
const maxOptCorpusSourceTimeout = 30 * time.Second

// startOptCorpus starts the async system.query_log performance-corpus
// reconciler when CERBERUS_CH_OPT_CORPUS_ENABLED is set. It is production-only
// (system.query_log access) and returns a no-op Observe sink plus a nil
// reconciler when disabled, so the engine dispatch seam can call Observe
// unconditionally. Errors building the JSONL sink are logged and degrade to
// disabled — the reconciler never takes the binary down. The Run loop is
// started on its own goroutine and stops on ctx cancel.
func startOptCorpus(ctx context.Context, logger *slog.Logger, client *chclient.Client, cfg config.Config, engines ...*engine.Engine) {
	if !cfg.CHOptCorpus.Enabled {
		return
	}
	sink, sinkDesc, ok := buildCorpusSink(ctx, logger, client.Conn(), cfg)
	if !ok {
		return
	}
	// Bound each corpus SELECT in wall-clock to a fraction of the reconcile
	// interval (capped) so a stuck scan can never outlive its slot or pin the
	// reconciler goroutine; the server-side max_execution_time is the primary
	// cap, this is the belt-and-braces client deadline.
	srcTimeout := cfg.CHOptCorpus.Interval / 2
	if srcTimeout <= 0 || srcTimeout > maxOptCorpusSourceTimeout {
		srcTimeout = maxOptCorpusSourceTimeout
	}
	// Derive the query_log lookback from the reconcile interval so a longer
	// interval still covers more than one scan worth of dispatched queries
	// (instead of a fixed 1h window). The same window drives the reconciler's
	// TTL eviction of never-finished ids.
	window := optcorpus.QueryLogWindow(cfg.CHOptCorpus.Interval)
	src := optcorpus.NewCHQueryLogSource(client.Conn(), srcTimeout, window)
	rec := optcorpus.New(src, sink, optcorpus.Options{
		Interval:     cfg.CHOptCorpus.Interval,
		RingCapacity: cfg.CHOptCorpus.RingCapacity,
		TTL:          window,
		Logger:       logger.With("component", "optcorpus"),
	})
	attachQueryObserver(rec, engines...)
	go func() {
		rec.Run(ctx)
		// The sink flushes on close; a failure here means buffered corpus
		// rows never reached disk, so surface it rather than dropping it.
		if err := sink.Close(); err != nil {
			logger.Warn("ch_opt query_log corpus sink close failed", "err", err)
		}
	}()
	logger.Info(
		"ch_opt query_log performance-corpus reconciler started",
		"interval", cfg.CHOptCorpus.Interval.String(),
		"sink", sinkDesc,
	)
}

// corpusSinkModeCHTable selects the cerberus_router_corpus MergeTree sink.
const corpusSinkModeCHTable = "chtable"

// buildCorpusSink selects the durable corpus sink from CHOptCorpus.SinkMode:
// the CH-table MergeTree (which it creates IF NOT EXISTS and reconciles) or the
// JSONL file. It returns the Sink, a short description for the startup log, and
// ok=false (already logged) when the CONFIGURED sink cannot be built.
//
// The configured mode is honoured or nothing is: a chtable failure does NOT
// silently degrade to the JSONL file. An operator who asked for the CH table
// and got a local file instead would be told the corpus is healthy while
// nothing reads it — a worse outcome than a reconciler that is off and says so.
// Failure-open here means the DATA PLANE is untouched: no query path depends on
// the corpus sink, so a sink outage costs calibration data and nothing else.
func buildCorpusSink(ctx context.Context, logger *slog.Logger, conn optcorpus.CHTableConn, cfg config.Config) (optcorpus.Sink, string, bool) {
	if cfg.CHOptCorpus.SinkMode == corpusSinkModeCHTable {
		sink, err := optcorpus.NewCHTableSink(ctx, conn)
		if err != nil {
			logger.Warn("ch_opt corpus CH-table sink unavailable; reconciler disabled", "err", err)
			return nil, "", false
		}
		return sink, "chtable:" + optcorpus.CorpusTableName, true
	}
	if cfg.CHOptCorpus.SinkPath == "" {
		logger.Warn("ch_opt corpus enabled but CERBERUS_CH_OPT_CORPUS_SINK_PATH is empty; reconciler disabled")
		return nil, "", false
	}
	sink, err := optcorpus.NewJSONLSink(cfg.CHOptCorpus.SinkPath)
	if err != nil {
		logger.Warn("ch_opt corpus sink unavailable; reconciler disabled", "err", err)
		return nil, "", false
	}
	return sink, "jsonl:" + cfg.CHOptCorpus.SinkPath, true
}

// attachQueryObserver registers the corpus reconciler as the QueryObserver on
// each supplied engine, but ONLY when corpus is non-nil. Passing a nil
// *optcorpus.Reconciler through the engine.QueryObserver interface would create
// a non-nil interface wrapping a nil pointer (the classic Go nil-interface
// trap), so the engine's `QueryObserver == nil` guard would not fire and
// ObserveQuery would nil-deref. The explicit nil check keeps the default
// (corpus disabled) path a true nil interface, so the dispatch seam stays
// byte-unchanged.
func attachQueryObserver(corpus *optcorpus.Reconciler, engines ...*engine.Engine) {
	if corpus == nil {
		return
	}
	for _, eng := range engines {
		if eng != nil {
			eng.QueryObserver = corpus
		}
	}
}

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
		bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg, schemaApplyPool))
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
//
// pool names this bootstrap connection's census. Each bootstrap client is a
// pool of its own, live alongside the serving pool — the schema-apply one
// overlaps it outright — and the connection gauges are process-wide, so
// without distinct names two live pools would write one attribute set and one
// would silently win.
func bootstrapClickHouseConfig(chCfg chclient.Config, pool string) chclient.Config {
	chCfg.Database = bootstrapDatabase
	chCfg.PoolName = pool
	return chCfg
}

// Bootstrap pool names, one per short-lived startup connection.
const (
	versionProbePool = "version-probe"
	tsGridProbePool  = "tsgrid-probe"
	schemaApplyPool  = "schema-apply"
)

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
	// Logged BEFORE the fatal check: a warning describes the server cerberus is
	// pointed at, and an operator debugging a failed boot needs it just as much
	// as one whose boot succeeds.
	for _, w := range res.Warnings {
		logger.Warn(w)
	}

	outcome := decideRequirementsOutcome(res, cfg.Schema, cfg.ClickHouse.Database)
	if outcome.fatalErr != nil {
		return nil, outcome.fatalErr
	}
	if outcome.notReadyReason != "" {
		// Transient: boot but stay NOT READY; the background re-probe (reusing
		// the auto-create retry cadence) flips readiness once the condition
		// decideRequirementsOutcome named clears. No restart.
		logger.Warn(outcome.logMsg, "reason", outcome.notReadyReason)
		present := newSchemaPresentSignal(outcome.notReadyReason)
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

// requirementsOutcome is the boot decision decideRequirementsOutcome derives
// from a preflight.Result. Exactly one of fatalErr (exit non-zero) or
// notReadyReason (boot proceeds NOT READY until a background re-probe
// clears it, logged via logMsg) is set; both empty means the check passed
// outright.
type requirementsOutcome struct {
	fatalErr       error
	notReadyReason string
	logMsg         string
}

// decideRequirementsOutcome turns a preflight.Result into the boot
// decision. It performs no I/O of its own — every branch is a pure
// function of res, m, and database — which is what makes the ordering
// unit-testable without a real ClickHouse (see main_test.go).
//
// Order is load-bearing: res.Fatal, then res.Unreachable, then
// res.DatabaseAbsent each short-circuit BEFORE fatalAbsentMetricTablesErr
// runs, so a genuinely unreachable server or a not-yet-created database
// never gets misclassified as a fatal missing-table config error — see
// fatalAbsentMetricTablesErr's own doc for why that reachable-vs-absent
// boundary matters (#1905). The remaining (non-metrics) AbsentTables stay
// on preflight's original transient path.
func decideRequirementsOutcome(res preflight.Result, m schema.Metrics, database string) requirementsOutcome {
	if res.Fatal != nil {
		// Wrong-shape / too-old / unreadable — never self-heals. Exit even if
		// some tables are also absent: a too-old server won't fix itself by
		// waiting, and a wrong-shape table is a genuine misconfiguration.
		return requirementsOutcome{fatalErr: res.Fatal}
	}
	if res.Unreachable {
		// Transient: ClickHouse is not accepting connections yet (cerberus
		// booted ahead of the database). A dial / connection-refused error is
		// NOT a misconfiguration and self-heals once the server appears.
		return requirementsOutcome{
			notReadyReason: res.UnreachableReason(),
			logMsg:         "clickhouse not reachable at startup; serving unready until it appears (/readyz gates traffic)",
		}
	}
	if res.DatabaseAbsent {
		// Transient: ClickHouse is up but the configured database does not exist
		// yet (UNKNOWN_DATABASE / code 81). The connection carries the database
		// as its session default, so even SELECT version() fails until the
		// database is created — by the collector that owns schema creation, or by
		// the auto-create hook once it can reach the server. This is the same
		// class of cold-cluster race as an absent schema, NOT a misconfiguration.
		return requirementsOutcome{
			notReadyReason: res.DatabaseAbsentReason(database),
			logMsg:         "clickhouse database not yet provisioned; serving unready until it is created (/readyz gates traffic)",
		}
	}
	if err := fatalAbsentMetricTablesErr(m, res.AbsentTables); err != nil {
		// Fatal, unlike the general AbsentTables tolerance below (#1905): a
		// misconfigured metric table name never self-heals the way an
		// unprovisioned schema does, and a silently degraded metadata
		// surface (empty /api/v1/series responses, never an error) is harder
		// for an operator to notice than a boot failure that names the table
		// and the config key.
		return requirementsOutcome{fatalErr: err}
	}
	if !res.SchemaProvisioned() {
		// Transient: the schema has not been provisioned yet (cerberus booted
		// ahead of the collector that owns schema creation).
		return requirementsOutcome{
			notReadyReason: res.AbsentReason(),
			logMsg:         "schema not yet provisioned; serving unready until an external writer creates it (/readyz gates traffic)",
		}
	}
	return requirementsOutcome{}
}

// metricTableBinding pairs a resolved metric-table name with the
// schema.metrics.* config key that set it (see internal/config/nested.go's
// schema.metrics.* bindings, the source of truth these key strings mirror).
type metricTableBinding struct {
	table     string
	configKey string
}

// metricTableBindings lists the metric tables the /api/v1/series,
// /api/v1/labels, and /api/v1/label/<name>/values handlers union across
// (schema.Metrics.ConfiguredMetricTables' Gauge/Sum/Histogram set) paired
// with the config key each came from, for fatalAbsentMetricTablesErr's
// error text.
func metricTableBindings(m schema.Metrics) []metricTableBinding {
	return []metricTableBinding{
		{table: m.GaugeTable, configKey: "schema.metrics.gaugeTable"},
		{table: m.SumTable, configKey: "schema.metrics.sumTable"},
		{table: m.HistogramTable, configKey: "schema.metrics.histogramTable"},
	}
}

// fatalAbsentMetricTablesErr turns preflight's already-computed
// reachable-but-absent table list into a boot-fatal error scoped to the
// metrics surface (#1905): a cerberus.yaml table name that will never
// exist — a typo, or a table the operator forgot to provision — must not
// silently degrade every /api/v1/series, /api/v1/labels, and
// /api/v1/label/<name>/values request into a ClickHouse UNKNOWN_TABLE
// error. This is deliberately STRICTER than preflight's own general
// schema-shape gate, which treats an entirely-absent table as the
// transient cerberus-boots-before-the-collector race and waits rather than
// exiting (see internal/preflight's package doc, "Fatal vs transient: the
// absent-schema race"); the boundary is unchanged, though — the two
// callers below that already returned before this point cover "ClickHouse
// unreachable" (res.Unreachable) and "database not yet provisioned"
// (res.DatabaseAbsent), so neither reaches here, and an unconfigured
// (empty-string) table name never appears in absentTables in the first
// place, since preflight's checkSchema skips empty table names before
// ever probing them. What is left, by construction, is exactly "the
// server answered and this specific configured table is not there".
func fatalAbsentMetricTablesErr(m schema.Metrics, absentTables []string) error {
	absent := make(map[string]bool, len(absentTables))
	for _, t := range absentTables {
		absent[t] = true
	}
	var problems []string
	for _, b := range metricTableBindings(m) {
		if b.table != "" && absent[b.table] {
			problems = append(problems, fmt.Sprintf("table %q (set via %s) does not exist", b.table, b.configKey))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("metric table existence check failed: %s", strings.Join(problems, "; "))
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
	// A boot that started ahead of ClickHouse (or ahead of its database) never
	// got to read version(), so the re-probe is the FIRST place a server-level
	// warning can surface. Log each distinct warning once — the loop ticks every
	// few seconds and the findings are stable, so re-logging would be noise.
	warned := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res := preflight.Run(ctx, client, req)
		for _, w := range res.Warnings {
			if !warned[w] {
				warned[w] = true
				logger.Warn(w)
			}
		}
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
// A nil grpcServer (the tempo head disabled via CERBERUS_ENABLED_HEADS)
// disables the gRPC dispatch branch entirely: every request, including an
// HTTP/2 application/grpc one, flows to rootMux, which answers 404 for the
// unmounted Tempo routes. The concrete *grpc.Server type (rather than
// http.Handler) is taken on purpose so this nil check is a plain typed-nil
// compare, not the non-nil-interface-wrapping-nil trap.
func buildDualStackServer(addr string, httpCfg config.HTTPServerConfig, rootMux http.Handler, grpcServer *grpc.Server) *http.Server {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if grpcServer != nil && r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		// HTTP path only (the gRPC branch above has already returned, so gRPC
		// framing is never touched): cap the request body so an unauthenticated
		// ParseForm/FormValue read can't stream an unbounded body into memory.
		// 0 disables the cap.
		if httpCfg.MaxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, httpCfg.MaxBodyBytes)
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
