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

	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/api/info"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chsql"
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

// admitLimiters carries the admission budgets this process fronts its
// routes with. Prom / loki / tempo are the per-head request budgets;
// lokiTail is the Loki head's SECOND budget, a distinct semaphore that
// bounds only the long-lived /tail WebSocket. A nil field means that
// budget is unadmitted (cap 0, or the master switch off) and the
// middleware degrades to a pass-through wrapper.
//
// Grouped in a struct rather than returned as a tuple because the set is
// no longer one-per-head: heads and budgets are different axes, and a
// positional 4-tuple of same-typed pointers is exactly the shape where a
// mis-ordered argument compiles and silently mounts /tail on the query
// budget — the bug this type exists to prevent.
type admitLimiters struct {
	prom     *admit.Limiter
	loki     *admit.Limiter
	tempo    *admit.Limiter
	lokiTail *admit.Limiter
}

// newAdmitLimiters builds the admission-control limiters. When
// CERBERUS_ADMIT_DISABLED=true every limiter is nil and the middleware
// short-circuits to a pass-through wrapper. Otherwise each cap
// CERBERUS_ADMIT_{PROM,LOKI,TEMPO,TAIL} sizes its limiter directly
// (resolved by config.admitFromEnv from an explicit integer or a
// true/false alias). admit.New / admit.NewTail return nil for a
// non-positive cap, so a disabled head and a zero cap collapse to the
// same pass-through path.
//
// CERBERUS_ADMIT_TAIL builds its own limiter through admit.NewTail — NOT
// a second reference to the Loki one. A /tail session occupies its slot
// until the client disconnects, so pointing both at one semaphore lets
// live-tail occupancy starve every ordinary Loki route on the replica
// (issue #1482).
func newAdmitLimiters(cfg config.Config, logger *slog.Logger) admitLimiters {
	if cfg.Admit.Disabled {
		logger.Info("admission control disabled (CERBERUS_ADMIT_DISABLED=true)")
		return admitLimiters{}
	}
	promCap := cfg.Admit.Prom
	lokiCap := cfg.Admit.Loki
	tempoCap := cfg.Admit.Tempo
	tailCap := cfg.Admit.Tail
	logger.Info(
		"admission control enabled",
		"prom", promCap,
		"loki", lokiCap,
		"tempo", tempoCap,
		"loki_tail", tailCap,
	)
	return admitLimiters{
		prom:     admit.New("prom", promCap),
		loki:     admit.New("loki", lokiCap),
		tempo:    admit.New("tempo", tempoCap),
		lokiTail: admit.NewTail("loki", tailCap),
	}
}

// apiHeads carries what run() needs back from mountAPIHeads: the Tempo gRPC
// server (nil when the tempo head is disabled, so the dual-stack dispatcher
// skips the gRPC branch and shutdown skips GracefulStop) and the capability-set
// consumers the periodic re-probe swaps a re-resolved set into.
type apiHeads struct {
	grpcServer *grpc.Server
	consumers  chOptConsumers
}

// resolveBoundOverrides resolves the six resource-bound safety-ceiling
// overrides run() threads into mountAPIHeads:
// the three chsql sample-fanout knobs (CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS
// / CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS / CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS)
// and the two PromQL-only histogram-merge cost-unit knobs
// (CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS /
// CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS, wired only onto the
// prom head's Handler.ResourceBounds — see newPromHandler's own doc for why
// LogQL/TraceQL never see it), all five from issue #2667, plus issue #2733's
// head-agnostic emitted-SQL statement-size bound
// (CERBERUS_CH_MAX_EMITTED_SQL_BYTES). Both resolutions share the same
// fail-fast contract as buildSolver's own solver.ConfigFromEnv() above: a
// typo'd or non-positive override aborts startup rather than silently falling
// back.
func resolveBoundOverrides() (engine.ResourceBoundOverrides, promql.ResourceBounds, error) {
	resourceBounds, err := engine.ResourceBoundsFromEnv()
	if err != nil {
		return engine.ResourceBoundOverrides{}, promql.ResourceBounds{}, err
	}
	promResourceBounds, err := promql.ResourceBoundsFromEnv()
	if err != nil {
		return engine.ResourceBoundOverrides{}, promql.ResourceBounds{}, err
	}
	return resourceBounds, promResourceBounds, nil
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
	limiters admitLimiters,
	logger *slog.Logger,
	resourceBounds engine.ResourceBoundOverrides,
	promResourceBounds promql.ResourceBounds,
	attrStrategies preflightAttrStrategies,
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
		evalSolver, err := buildSolver(logger, cfg.ClickHouse, cfg.ClusterTopology, promClient, limiters.prom)
		if err != nil {
			return apiHeads{}, fmt.Errorf("configure solver: %w", err)
		}
		// Issue #2789: same fail-fast contract as buildSolver's own
		// solver.ConfigFromEnv() above — a malformed CERBERUS_QUERY_ACTUALS_*
		// knob refuses to boot rather than silently running on an unintended
		// value. Prom-only, mirroring evalSolver/RouteMemo/PerRungAdmission's
		// own scope: the actuals hooks all key off the solver's own
		// plan-shape-id / K-clamp machinery, which is PromQL-only
		// (solver.RequestMeta.Lang's own doc).
		actualsTracker, err := buildActualsTracker(ctx, logger, promClient)
		if err != nil {
			return apiHeads{}, fmt.Errorf("configure query actuals: %w", err)
		}
		promHandler = newPromHandler(promClient, cfg, optSet, evalSolver, limiters.prom, logger, resourceBounds, promResourceBounds, actualsTracker)
		promHandler.Mount(traceMux)
		engines = append(engines, promHandler.Engine)
	}

	if cfg.HeadEnabled(config.HeadLoki) {
		lokiClient := client.ForHead(chclient.HeadLoki)
		lokiHandler := newLokiHandler(lokiClient, cfg, optSet, limiters, logger, resourceBounds, attrStrategies.Logs)
		lokiHandler.Mount(traceMux)
		engines = append(engines, lokiHandler.Engine)
	}

	var grpcServer *grpc.Server
	if cfg.HeadEnabled(config.HeadTempo) {
		tempoClient := client.ForHead(chclient.HeadTempo)
		tempoHandler := tempo.New(tempoClient, cfg.Traces, Version, logger.With("api", "tempo"))
		tempoHandler.Limiter = limiters.tempo
		tempoHandler.StructuralTwoPhase = cfg.TempoStructuralTwoPhase
		tempoHandler.ExternalTraceIDPush = buildExternalTraceIDPush(optSet, cfg.ClickHouse.Protocol)
		// Same knob + wiring shape as the prom and loki heads (see
		// newPromHandler / newLokiHandler above): the Go-side context-deadline
		// backstop that unblocks a hung handler and releases its admit slot +
		// pooled connection even if the server-side ClickHouse cap doesn't
		// fire. See issue #2302.
		tempoHandler.QueryTimeout = cfg.ClickHouse.QueryTimeout
		// TagCatalogEnabled (cerberus issue #2771) is the resolved chopt
		// tempo_tag_catalog_mv verdict — the SAME cfg.SchemaTempoTagCatalogMV
		// DDLConfig threads to gate provisioning the catalog table in the
		// first place, so the read path only ever attempts the catalog query
		// on a deployment where the DDL side actually created it. Mirrors
		// newLokiHandler's own LabelCatalogEnabled wiring.
		tempoHandler.TagCatalogEnabled = cfg.SchemaTempoTagCatalogMV
		// AttrStrategies (cerberus issue #2777 / #3062) is the preflight
		// boot probe's resolved verdict on the traces attribute-map
		// columns' physical shape — see runRequirementsCheck's doc for
		// the known cold-start-race limitation shared with the logs
		// wiring above. nil (the overwhelmingly common case: no
		// JSON-typed column detected, or the requirements check
		// disabled) renders byte-identical to before this field existed.
		tempoHandler.SetAttrStrategies(attrStrategies.Traces)
		tempoHandler.Engine.Settings = settingsRules(cfg, optSet)
		// The per-query sample budget the ENGINE-level bounds read. The cursor
		// enforces the same ceiling on rows it drains from ClickHouse, but the
		// engine's own gates (requireSubquerySampleBudget, and compare()'s
		// synthesised-grid budget) read it from here — left at zero they are
		// silently inert, which is how a compare() grid 108x over the configured
		// budget was still served.
		tempoHandler.Engine.MaxQuerySamples = tempoClient.MaxQuerySamples()
		// Issue #2733's emitted-SQL size bound, wired here for the same reason
		// it is wired onto the prom and loki heads: it bounds a property every
		// head has (the bytes of one emitted statement), unlike the row-fanout
		// ceilings, which gate node kinds only some heads lower.
		tempoHandler.Engine.MaxEmittedSQLBytes = resourceBounds.MaxEmittedSQLBytes
		tempoHandler.Mount(traceMux)
		engines = append(engines, tempoHandler.Engine)

		// Tempo gRPC StreamingQuerier — shares the Tempo HTTP handler's Engine
		// + schema + admit limiter so the streaming RPC bodies and the HTTP
		// handlers run the same parse + lower + emit pipeline. Built only when
		// tempo is enabled; nil otherwise.
		tempoGRPCService := tempogrpc.NewService(tempoHandler, limiters.tempo, logger.With("api", "tempo-grpc"))
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
	optSet, chOpts, optRes, err := startCHOptimizations(ctx, logger, client, &cfg)
	if err != nil {
		return err
	}

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
	schemaPresent, attrStrategies, err := runRequirementsCheck(ctx, logger, client, cfg)
	if err != nil {
		return err
	}

	// Build the admission-control limiters (see newAdmitLimiters).
	limiters := newAdmitLimiters(cfg, logger)

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
	resourceBounds, promResourceBounds, err := resolveBoundOverrides()
	if err != nil {
		return err
	}

	heads, err := mountAPIHeads(ctx, traceMux, client, cfg, optSet, limiters, logger, resourceBounds, promResourceBounds, attrStrategies)
	if err != nil {
		return err
	}
	grpcServer := heads.grpcServer

	// Periodic capability re-probe: re-resolves the optimization set against the
	// connected server and swaps a changed result into the heads mounted above,
	// so an upgraded ClickHouse is picked up without restarting cerberus. Bound
	// to the run ctx, so SIGTERM stops it.
	go reprobeCHOptimizations(ctx, logger, cfg, chOpts, heads.consumers, chOptReprobeInterval, optRes.RawQueryWorkload)

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
		Pinger:               client.ForHead(chclient.HeadProbe),
		SchemaReady:          schemaReady,
		SchemaPresent:        schemaPresent,
		HeadBreakers:         enabledHeadBreakers(client, cfg),
		CapabilitiesResolved: capabilitiesResolved(chOpts),
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
//
// resourceBounds carries the resolved CERBERUS_CH_*_MAX_ROWS overrides
// (issue #2667, engine.ResourceBoundsFromEnv) — RangeBucketFanoutMaxRows and
// RangeLWRFanoutMaxRows are wired here because RangeBucketFanout / RangeLWR
// are PromQL-only lowerings (see engine.Engine.RangeBucketFanoutMaxRows's
// doc); RateWindowFanoutMaxRows is wired here too — the prom head lowers
// chplan.RangeWindow for rate()/increase()/etc — and independently onto the
// Loki head below, since LogQL's own range aggregations lower to the same
// node kind.
func newPromHandler(
	client *chclient.Client, cfg config.Config, optSet chopt.EnabledSet, evalSolver *solver.Solver,
	limiter *admit.Limiter, logger *slog.Logger, resourceBounds engine.ResourceBoundOverrides,
	promResourceBounds promql.ResourceBounds, actualsTracker *actuals.Tracker,
) *prom.Handler {
	h := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	h.ResourceBounds = promResourceBounds
	// Constructed once and threaded into BOTH Engine fields below —
	// buildScanEstimateAdvisor's own doc explains why a near-empty advisory
	// estimate must seed THIS SAME instance rather than a second one.
	perRungAdmission := buildPerRungAdmission(evalSolver)
	h.Engine = &engine.Engine{
		Optimizer:               h.Optimizer,
		Client:                  client,
		Solver:                  evalSolver,
		Settings:                settingsRules(cfg, optSet),
		MaxQuerySamples:         client.MaxQuerySamples(),
		RouteMemo:               buildRouteMemo(evalSolver, logger),
		PerRungAdmission:        perRungAdmission,
		ScanEstimateAdvisor:     buildScanEstimateAdvisor(client, optSet, evalSolver, perRungAdmission),
		CardinalityProbeAdvisor: buildCardinalityProbeAdvisor(client, optSet, evalSolver, perRungAdmission),
		// The OPTIONAL predicted-vs-actual drift tracker (issue #2789),
		// gated on CERBERUS_QUERY_ACTUALS_ENABLED (mountAPIHeads's own
		// wiring) rather than a chopt feature — see actuals.Config.Enabled's
		// own doc for why. nil (the default) leaves every hook in
		// internal/engine/actuals_wiring.go inert.
		Actuals: actualsTracker,
		// PromQL-only: TraceQL / LogQL plans never carry a
		// chplan.RangeWindow.TemporalityColumn (the OTel Sum
		// AggregationTemporality concept), so this is inert for the other
		// heads and is not wired onto their engines — see
		// engine.Engine.DeltaPrefixLookback's doc.
		DeltaPrefixLookback: cfg.DeltaPrefixLookback,
		// Same PromQL-only inertness reasoning as DeltaPrefixLookback above
		// — see engine.Engine.DeltaPrefixReadEnabled's doc for why this is
		// a separate, later opt-in from schema.SchemaProvisioning.DeltaPrefixEnabled.
		DeltaPrefixReadEnabled: cfg.DeltaPrefixReadEnabled,
		// See this function's own doc above for why all three are wired here.
		RangeBucketFanoutMaxRows: resourceBounds.RangeBucketFanoutMaxRows,
		RangeLWRFanoutMaxRows:    resourceBounds.RangeLWRFanoutMaxRows,
		RateWindowFanoutMaxRows:  resourceBounds.RateWindowFanoutMaxRows,
		// Head-AGNOSTIC, unlike the three above: every head emits statements,
		// so issue #2733's emitted-SQL size bound is wired onto all three
		// engines — see engine.Engine.MaxEmittedSQLBytes' own doc.
		MaxEmittedSQLBytes: resourceBounds.MaxEmittedSQLBytes,
		// PromQL-only, same inertness reasoning: chplan.RangeBucketGridNative
		// only ever comes from PromQL's classic-histogram_quantile lowering —
		// see engine.Engine.RangeBucketGridNativeMaxRows's doc.
		RangeBucketGridNativeMaxRows:         cfg.RangeBucketGridNativeMaxRows,
		RangeBucketGridNativeMaxDensityUnits: cfg.RangeBucketGridNativeMaxDensityUnits,
	}
	h.Limiter = limiter
	h.Version = Version
	h.Lowerers = nativeRangeLowerers(optSet)
	// Unlike Lowerers (range-only), TagGroups (cerberus issue #2750) is
	// consulted on the instant path too — see Handler.TagGroups's own doc.
	h.TagGroups = optSet.Has(chopt.FeatureTSGridTagGroups)
	// ThrowDuplicateSeriesIf (cerberus issue #3038) is consulted on BOTH the
	// instant and the range-streaming paths — see
	// Handler.ThrowDuplicateSeriesIf's own doc.
	h.ThrowDuplicateSeriesIf = optSet.Has(chopt.FeatureTSThrowDuplicateSeriesIf)
	h.QueryTimeout = cfg.ClickHouse.QueryTimeout
	// CERBERUS_PROM_METADATA_LOOKBACK is the ONLY input to the windowless
	// metadata-discovery horizon; zero leaves the handler on its own
	// fallback. Nothing else in the config is evidence of what ClickHouse
	// physically retains — see config.Config.PromMetadataLookback.
	h.MetadataLookback = cfg.PromMetadataLookback
	return h
}

// buildRouteMemo wires the failure-driven route memo (internal/routememo,
// docs/solver.md §"Failure-driven route memo"). It is governed by
// CERBERUS_SOLVER_ADAPTIVE_ENABLED, which defaults to TRUE — see
// solver.Config.AdaptiveEnabled's doc for why. CERBERUS_SOLVER_ROUTE_MEMO_ENABLED
// is the soft-deprecated spelling and still applies. Returns nil (the engine's
// byte-unchanged, feature-off default) when an operator has opted out or when
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
	if evalSolver == nil || !evalSolver.Cfg.AdaptiveEnabled {
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

// buildPerRungAdmission wires the evidence-based per-rung admission
// refinement (internal/engine/per_rung_admission.go, issue #2709). Gated
// identically to buildRouteMemo — CERBERUS_SOLVER_ADAPTIVE_ENABLED, default
// TRUE — because it is the same family of mechanism (routing informed by
// what actually happened, not just by plan-time geometry), so the SAME knob
// that turns adaptive routing off turns this off too rather than adding a
// second, independently-configured flag for a narrower slice of the same
// idea. Returns nil (the engine's byte-unchanged, feature-off default) under
// the same conditions buildRouteMemo does.
func buildPerRungAdmission(evalSolver *solver.Solver) *engine.PerRungAdmissionLearner {
	if evalSolver == nil || !evalSolver.Cfg.AdaptiveEnabled {
		return nil
	}
	return engine.NewPerRungAdmissionLearner()
}

// buildScanEstimateAdvisor wires the advisory EXPLAIN ESTIMATE pre-flight
// (internal/engine/explain_estimate_wiring.go, issue #2787), gated on the
// chopt FeatureExplainEstimate feature — an operator-opt-in rollout / kill
// switch (that feature's own registry doc: AlwaysAvailable, AutoSelect=false,
// pending real-world calibration), not a real ClickHouse version floor.
// Returns nil (the engine's byte-unchanged, feature-off default) when the
// feature is not listed in CERBERUS_CH_OPTIMIZATIONS or evalSolver is nil —
// Engine.classify's own runtime check additionally gates every request on
// solver.ModeAuto, so a deployment that flips CERBERUS_EVAL_ROUTE away from
// "auto" after boot still runs the pre-#2787 path with no restart needed.
//
// perRungAdmission is threaded straight through so a near-empty advisory
// estimate can seed that learner's own priors (PerRungAdmissionLearner.
// SeedPriorFromEstimate) — the SAME instance buildPerRungAdmission
// constructed, never a second one, so the two mechanisms' state cannot
// diverge. Unlike newPromHandler's other Engine fields, this constructor
// needs no config surface of its own: Engine.classify builds the probe's
// emit closure from the SAME Engine fields (DeltaPrefixLookback,
// ResourceBoundOverrides, RangeBucketGridNativeMaxRows/…MaxDensityUnits)
// route A already emits with, at the call site, rather than this function
// threading a second copy of them in.
func buildScanEstimateAdvisor(
	client *chclient.Client,
	optSet chopt.EnabledSet,
	evalSolver *solver.Solver,
	perRungAdmission *engine.PerRungAdmissionLearner,
) *engine.ScanEstimateAdvisor {
	if evalSolver == nil || !optSet.Has(chopt.FeatureExplainEstimate) {
		return nil
	}
	return engine.NewScanEstimateAdvisor(client, perRungAdmission)
}

// buildCardinalityProbeAdvisor wires the advisory bounded cardinality
// pre-probe (internal/engine/cardinality_probe_wiring.go, issue #2788),
// gated on the chopt FeatureCardinalityProbe feature — an operator-opt-in
// rollout / kill switch, exactly mirroring buildScanEstimateAdvisor's own
// gating posture (that function's own doc explains the full reasoning:
// AlwaysAvailable + AutoSelect=false pending real-world calibration).
// Returns nil (the engine's byte-unchanged, feature-off default) when the
// feature is not listed in CERBERUS_CH_OPTIMIZATIONS or evalSolver is nil.
//
// perRungAdmission is threaded straight through so a near-empty advisory
// cardinality reading can seed that SAME learner's priors
// (PerRungAdmissionLearner.SeedPriorFromEstimate) — the SAME instance
// buildPerRungAdmission constructed and buildScanEstimateAdvisor also
// threads, never a third one, so all three mechanisms' state cannot
// diverge.
// buildExternalTraceIDPush resolves internal/api/tempo.Handler.ExternalTraceIDPush
// (cerberus issue #2783): whether the structural two-phase phase-B
// restriction (structural_two_phase.go's restrictStructural) may push a wide
// closure's TraceId set as a native-protocol external table instead of a
// spliced literal. Gated on TWO independent axes ANDed together —
//
//   - the chopt trace_id_external_table feature (an operator opt-in via
//     CERBERUS_CH_OPTIMIZATIONS: AlwaysAvailable + AutoSelect=false, pending
//     production calibration beyond this issue's own synthetic corpus — see
//     the registry entry's doc); and
//   - protocol == clickhouse.Native, because the mechanism itself
//     (clickhouse-go/v2's WithExternalTable) is the native-protocol wire
//     feature #2783 scoped this PR to — the chopt resolver has no visibility
//     into Config.Protocol, so this function is the single place both axes
//     combine.
//
// false (the default when the feature is unlisted, or when the deployment
// runs Protocol=http) keeps every phase-B restriction on the pre-#2783
// literal-splice path, byte-identical.
func buildExternalTraceIDPush(optSet chopt.EnabledSet, protocol clickhouse.Protocol) bool {
	return optSet.Has(chopt.FeatureTraceIDExternalTable) && protocol == clickhouse.Native
}

func buildCardinalityProbeAdvisor(
	client *chclient.Client,
	optSet chopt.EnabledSet,
	evalSolver *solver.Solver,
	perRungAdmission *engine.PerRungAdmissionLearner,
) *engine.CardinalityProbeAdvisor {
	if evalSolver == nil || !optSet.Has(chopt.FeatureCardinalityProbe) {
		return nil
	}
	return engine.NewCardinalityProbeAdvisor(client, perRungAdmission)
}

// buildActualsTracker wires issue #2789's predicted-vs-actual drift tracker
// (internal/actuals), gated on CERBERUS_QUERY_ACTUALS_ENABLED — a plain
// solver-policy config knob, NOT a chopt feature (actuals.Config.Enabled's
// own doc explains why: ProfileEvents on the native protocol and
// system.query_log are both ancient, always-available ClickHouse surfaces
// with no version floor to probe). Returns (nil, nil) — the engine's
// byte-unchanged, feature-off default — when the operator has not opted in;
// returns a non-nil error only on a malformed CERBERUS_QUERY_ACTUALS_* env
// var, the same fail-fast contract buildSolver's own solver.ConfigFromEnv()
// uses.
//
// When enabled, this ALSO starts the query_log fallback reconciler
// (query_log_actuals.go) on its own goroutine, bound to ctx — mirroring
// startOptCorpus's own goroutine-launch-and-log shape, independently
// implemented (see query_log_actuals.go's own doc for why this package
// cannot import internal/optcorpus).
func buildActualsTracker(ctx context.Context, logger *slog.Logger, promClient *chclient.Client) (*actuals.Tracker, error) {
	cfg, err := actuals.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	tracker := actuals.NewTracker(cfg)
	rec := engine.NewQueryLogActualsReconciler(queryLogQuerierAdapter{promClient}, tracker, cfg, logger.With("component", "query_actuals"))
	go rec.Run(ctx)
	logger.Info(
		"query actuals predicted-vs-actual drift tracker started",
		"query_log_poll_interval", cfg.QueryLogPollInterval.String(),
		"drift_band", fmt.Sprintf("[%g, %g]", cfg.DriftLowerRatio, cfg.DriftUpperRatio),
	)
	return tracker, nil
}

// queryLogQuerierAdapter adapts *chclient.Client to engine.QueryLogQuerier.
// The two QueryLogActualRow types (chclient's transport type,
// engine.QueryLogActualRow's package-local stand-in — see that type's own
// doc) are field-for-field identical; this is the one-line conversion that
// doc promises, kept here rather than in internal/engine so that package
// depends only on its own narrow interface, never chclient's concrete type
// (mirrors every other Estimator-style seam in this codebase).
type queryLogQuerierAdapter struct {
	client *chclient.Client
}

func (a queryLogQuerierAdapter) QueryLogActuals(ctx context.Context, since time.Time, shapeIDPrefix string, limit int) ([]engine.QueryLogActualRow, error) {
	rows, err := a.client.QueryLogActuals(ctx, since, shapeIDPrefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]engine.QueryLogActualRow, len(rows))
	for i, r := range rows {
		out[i] = engine.QueryLogActualRow{
			LogComment:  r.LogComment,
			ReadRows:    r.ReadRows,
			ReadBytes:   r.ReadBytes,
			MemoryUsage: r.MemoryUsage,
			EventTime:   r.EventTime,
		}
	}
	return out, nil
}

// nativeRangeLowerers builds the BOOT-WIRED polymorphic lowering dispatch table
// for the ClickHouse-native timeSeries*ToGrid family from the resolved
// optimization EnabledSet. The feature/version decision is made HERE, ONCE, at
// boot (optSet was produced by the single boot-time version probe in
// resolveCHOptimizations) and is the ONLY place the feature is read: each field
// is wired to a CONCRETE non-nil strategy —
//
//	rate      = enabled ? NativeRateLowerer{Fallback: rateFallback} : rateFallback
//	increase  = enabled ? NativeIncreaseLowerer{Fallback: increaseFallback} : increaseFallback
//	delta     = enabled ? NativeDeltaLowerer{Fallback: deltaFallback} : deltaFallback
//	  // where rateFallback/increaseFallback/deltaFallback are each
//	  // fixedAccumEnabled ? FixedAccumulator*Lowerer{Fallback: Fanout*Lowerer{}} : Fanout*Lowerer{}
//	staleness = enabled ? NativeStalenessLowerer{Fallback: FanoutStalenessLowerer{}} : FanoutStalenessLowerer{}
//	changes   = enabled ? NativeChangesLowerer{Fallback: changesFallback} : changesFallback
//	resets    = enabled ? NativeResetsLowerer{Fallback: resetsFallback} : resetsFallback
//	irate     = enabled ? NativeIrateLowerer{Fallback: irateFallback} : irateFallback
//	idelta    = enabled ? NativeIdeltaLowerer{Fallback: ideltaFallback} : ideltaFallback
//	  // where changesFallback/resetsFallback/irateFallback/ideltaFallback are each
//	  // laginframeEnabled ? LagAdjacency*Lowerer{Fallback: Fanout*Lowerer{}} : Fanout*Lowerer{}
//	deriv     = enabled ? NativeDerivLowerer{Fallback: FanoutDerivLowerer{}} : FanoutDerivLowerer{}
//	predict   = enabled ? NativePredictLinearLowerer{Fallback: FanoutPredictLinearLowerer{}} : FanoutPredictLinearLowerer{}
//	classicHq = enabled ? NativeClassicHistogramWindowLowerer{Fallback: Fanout…{}} : Fanout…{}
//	rankWalk  = enabled ? NativeQuantileRankWalkLowerer{} : FanoutQuantileRankWalkLowerer{}
//	lastOverTime = enabled ? NativeLastOverTimeLowerer{Fallback: FanoutLastOverTimeLowerer{}} : FanoutLastOverTimeLowerer{}
//	  // downsampleTierEnabled ? DownsampleTier{Irate,Idelta,LastOverTime}Lowerer{Fallback: <above>} : <above> unchanged
//	overTime  = sortedSlabEnabled ? SortedSlabOverTimeLowerer{Fallback: FanoutOverTimeLowerer{}} : FanoutOverTimeLowerer{}
//
// rankWalk (quantile_prom_histogram) is not a timeSeries*ToGrid member — it
// wraps ClickHouse's separate quantilePrometheusHistogram aggregate (floor
// 25.10) — and carries no embedded Fallback because it has no shape-based
// fallback to embed (see promql.QuantileRankWalkLowerer's own doc); it is
// resolved here, in this same table, only because RangeLowerers is the one
// seam every classic-histogram-quantile lowering already threads.
//
// overTime (sum_over_time/avg_over_time, issue #2761) is likewise not a
// timeSeries*ToGrid member and carries no native competitor, but unlike
// rankWalk it DOES embed a Fallback (the plain fan-out) — its sorted-slab
// decomposition is a shape-gated SQL rewrite with its own eligibility guard
// (sortedSlabOverTimeEligible), the same posture fixed_accumulator_extrapolated
// has for rate/increase/delta, not an always-applicable aggregate swap like
// rankWalk's.
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
//
// laginframe_adjacency (issue #2759) is a second non-independent knob, but in
// the opposite direction from recollapse: it narrows changes/resets/irate/
// idelta's FALLBACK (the arm a shape-ineligible or sub-25.9 server lands on),
// not their native arm. It was irate/idelta's ONLY non-fan-out strategy until
// issue #2746 gave them their own native timeSeriesInstantRateToGrid /
// timeSeriesInstantDeltaToGrid competitor, so all four of changes / resets /
// irate / idelta now share the identical three-tier Native{Fallback:
// LagAdjacency{Fallback: Fanout{}}} composition. Unlike every ts_grid_*
// feature it carries no version floor (chopt.AlwaysAvailable) and no
// experimental-setting gate, so it composes with the version-gated native
// features purely via which Fallback each Native*Lowerer embeds.
//
// fixed_accumulator_extrapolated (issue #2760) is laginframe_adjacency's own
// sibling for the extrapolated family: it narrows rate/increase/delta's
// FALLBACK the same way — delta gained its own native ts_grid_delta
// competitor (issue #2745), so as of that feature all three of rate /
// increase / delta share the identical three-tier
// Native{Fallback: FixedAccumulator{Fallback: Fanout{}}} composition. Same
// AlwaysAvailable floor, same no-experimental-setting posture, same "narrows
// the fallback, never competes with the native arm" shape.
//
// ts_grid_instant (issue #2748) narrows the NATIVE arm itself rather than the
// fallback — the opposite direction from recollapse/laginframe/fixed-
// accumulator above. Each of rate/changes/resets/deriv/predict_linear's
// Native*Lowerer gains an Instant field, set to tsGridInstant ONLY inside
// that same function's own "matrix feature enabled" branch, so the instant
// arm can never fire for a function whose matrix arm is off. increase() and
// delta() are out of scope (see chopt.FeatureTSGridInstant's own doc) and
// carry no Instant field.
//
// ts_grid_group_array (issue #2749) is a third non-swappable emission-detail
// bit, mirroring VectorAgg's own posture: it lives on RangeLowerers itself
// (promql.RangeLowerers.NativeGroupArray) rather than on any single
// Native*Lowerer, and is set unconditionally below because rate/increase/
// delta's fanout lowering reads it directly off the SAME chplan.RangeWindow
// node regardless of which of those three functions' own Native /
// FixedAccumulator / Fanout tiers produced it.
func nativeRangeLowerers(optSet chopt.EnabledSet) promql.RangeLowerers {
	var l promql.RangeLowerers
	// arg_and_max_fusion (issue #2764) is a plain emission-detail bit, not
	// a swappable strategy — see RangeLowerers.ArgAndMaxFusion's own doc.
	// It feeds BOTH the VectorJoin site (read directly off this table by
	// internal/promql/binary.go) and the RangeLWR site (its own copy on
	// FanoutStalenessLowerer below, threaded either directly or as the
	// native staleness lowerer's embedded Fallback).
	argAndMaxFusion := optSet.Has(chopt.FeatureArgAndMaxFusion)
	l.ArgAndMaxFusion = argAndMaxFusion
	// ts_grid_vector_agg (issue #2763) is a pure narrowing of ts_grid_range,
	// but — unlike ts_grid_recollapse — it lives on RangeLowerers itself
	// rather than on NativeRateLowerer (or any single Native*Lowerer),
	// because lowerAggregate consults it at a LATER lowering stage than any
	// range-function Lowerer: only after a range function has already
	// lowered its input, and only when that input turns out to be a
	// *chplan.RangeWindowGridNative. Setting it unconditionally here (never
	// nested inside `if optSet.Has(chopt.FeatureTSGridRange)`) is therefore
	// safe rather than a narrowing gap: the field is inert whenever no
	// native grid node exists for lowerAggregate to fold into.
	l.VectorAgg = optSet.Has(chopt.FeatureTSGridVectorAgg)
	// ts_grid_group_array (issue #2749) is, like VectorAgg immediately
	// above, a plain narrowing bit rather than a per-function Lowerer swap:
	// it never changes WHICH of rate/increase/delta's own Native /
	// FixedAccumulator / Fanout tiers fires, only how that tier's array-fold
	// fallback assembles its per-series sample array. Set unconditionally
	// here for the same reason VectorAgg is — it lives on RangeLowerers
	// itself, not on any single Native*Lowerer.
	l.NativeGroupArray = optSet.Has(chopt.FeatureTSGridGroupArray)
	// ts_grid_instant (issue #2748) is a pure narrowing of each of
	// rate/changes/resets/deriv/predict_linear's own matrix feature — it is
	// consulted ONLY inside each function's own "matrix feature enabled"
	// branch below, exactly like ts_grid_recollapse narrows ts_grid_range,
	// so it can never make a Native*Lowerer's instant arm reachable on its
	// own.
	tsGridInstant := optSet.Has(chopt.FeatureTSGridInstant)
	// fixed_accumulator_extrapolated (issue #2760) layers BENEATH
	// rate/increase/delta's own native ts_grid strategy, exactly like
	// laginframe_adjacency layers inside changes/resets below: it is the
	// improved fan-out a shape-ineligible, sub-25.9, or temporality-bearing
	// window falls back to, never a competitor to the native path.
	var rateFallback promql.RateLowerer = promql.FanoutRateLowerer{}
	var increaseFallback promql.IncreaseLowerer = promql.FanoutIncreaseLowerer{}
	var deltaFallback promql.DeltaLowerer = promql.FanoutDeltaLowerer{}
	if optSet.Has(chopt.FeatureFixedAccumulatorExtrapolated) {
		rateFallback = promql.FixedAccumulatorRateLowerer{Fallback: rateFallback}
		increaseFallback = promql.FixedAccumulatorIncreaseLowerer{Fallback: increaseFallback}
		deltaFallback = promql.FixedAccumulatorDeltaLowerer{Fallback: deltaFallback}
	}
	if optSet.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   rateFallback,
			Recollapse: optSet.Has(chopt.FeatureTSGridRecollapse),
			Instant:    tsGridInstant,
		}
	} else {
		l.Rate = rateFallback
	}
	if optSet.Has(chopt.FeatureTSGridIncrease) {
		l.Increase = promql.NativeIncreaseLowerer{Fallback: increaseFallback}
	} else {
		l.Increase = increaseFallback
	}
	if optSet.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}
	}
	// laginframe_adjacency (issue #2759) layers BENEATH changes/resets' own
	// native ts_grid strategy, exactly like ts_grid_recollapse layers inside
	// ts_grid_range above: it is the improved fan-out a shape-ineligible or
	// sub-25.9 server falls back to, never a competitor to the native path.
	var changesFallback promql.ChangesLowerer = promql.FanoutChangesLowerer{}
	var resetsFallback promql.ResetsLowerer = promql.FanoutResetsLowerer{}
	if optSet.Has(chopt.FeatureLagInFrameAdjacency) {
		changesFallback = promql.LagAdjacencyChangesLowerer{Fallback: changesFallback}
		resetsFallback = promql.LagAdjacencyResetsLowerer{Fallback: resetsFallback}
	}
	if optSet.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: changesFallback, Instant: tsGridInstant}
	} else {
		l.Changes = changesFallback
	}
	if optSet.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: resetsFallback, Instant: tsGridInstant}
	} else {
		l.Resets = resetsFallback
	}
	// irate/idelta gained their own native timeSeries*ToGrid member
	// (timeSeriesInstantRateToGrid / timeSeriesInstantDeltaToGrid, cerberus
	// issue #2746): laginframe_adjacency now layers BENEATH that native
	// strategy exactly like it does for changes/resets above, rather than
	// being their only non-fan-out strategy the way issue #2759 originally
	// left it.
	var irateFallback promql.IrateLowerer = promql.FanoutIrateLowerer{}
	var ideltaFallback promql.IdeltaLowerer = promql.FanoutIdeltaLowerer{}
	if optSet.Has(chopt.FeatureLagInFrameAdjacency) {
		irateFallback = promql.LagAdjacencyIrateLowerer{Fallback: irateFallback}
		ideltaFallback = promql.LagAdjacencyIdeltaLowerer{Fallback: ideltaFallback}
	}
	if optSet.Has(chopt.FeatureTSGridIrate) {
		l.Irate = promql.NativeIrateLowerer{Fallback: irateFallback}
	} else {
		l.Irate = irateFallback
	}
	if optSet.Has(chopt.FeatureTSGridIdelta) {
		l.Idelta = promql.NativeIdeltaLowerer{Fallback: ideltaFallback}
	} else {
		l.Idelta = ideltaFallback
	}
	if optSet.Has(chopt.FeatureTSGridDeriv) {
		l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}, Instant: tsGridInstant}
	} else {
		l.Deriv = promql.FanoutDerivLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridPredictLinear) {
		l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}, Instant: tsGridInstant}
	} else {
		l.PredictLinear = promql.FanoutPredictLinearLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridDelta) {
		l.Delta = promql.NativeDeltaLowerer{Fallback: deltaFallback}
	} else {
		l.Delta = deltaFallback
	}
	// The anchor-injection window-slide mechanism (#2408 follow-up, #2493)
	// was removed by #2511's root-cause investigation: its anchor-injection
	// UNION structurally requires the per-series canonical-bound subquery to
	// be inlined into BOTH UNION arms (real rows' JOIN and the sentinel
	// arm's FROM), and this codebase's chsql architecture only single-
	// evaluates a genuinely scalar CTE (QueryBuilder.WithScalar) — a
	// relational CTE re-inlines, and re-scans, at every reference
	// (QueryBuilder.With's own doc). Real EXPLAIN PLAN / EXPLAIN PIPELINE
	// evidence against chDB confirmed 3 independent ReadFromMergeTree scans
	// of the base table for one window-slide query where the fan-out needs
	// one, plus the shared LIMIT+throwIf resource-bound pattern doubling
	// whatever that count is — the real root cause of the ~5x-more-bytes
	// regression #2511 measured. No fix avoids this without either
	// reintroducing the O(rows^2)-per-series blow-up an earlier review
	// already rejected, or a per-row scalar-map lookup against thousands of
	// series (unvalidated and very likely slower still). Combined with the
	// mechanism's own real-world win being marginal at the modal dashboard
	// shape (1.12x at 5m/1m, per #2408's own Task-1 spike), fan-out is kept
	// as the sole classic-histogram range-window path below the native
	// rate ladder.
	if optSet.Has(chopt.FeatureTSGridHistogram) {
		l.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{
			Fallback: promql.FanoutClassicHistogramWindowLowerer{},
		}
	} else {
		l.ClassicHistogram = promql.FanoutClassicHistogramWindowLowerer{}
	}
	// quantile_prom_histogram has no shape-based fallback (see
	// promql.QuantileRankWalkLowerer's own doc), so the native strategy is
	// wired directly with no embedded Fallback field.
	if optSet.Has(chopt.FeatureQuantilePromHistogram) {
		l.QuantileRankWalk = promql.NativeQuantileRankWalkLowerer{}
	} else {
		l.QuantileRankWalk = promql.FanoutQuantileRankWalkLowerer{}
	}
	// ts_grid_last_over_time rides the SAME native strategy shape as every
	// other independent family member (Native{Fallback: Fanout{}}) — it has
	// no narrowing/narrowed sibling knob of its own (no recollapse-style
	// dependent, no laginframe/fixed-accumulator-style improved fallback).
	if optSet.Has(chopt.FeatureTSGridLastOverTime) {
		l.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
	} else {
		l.LastOverTime = promql.FanoutLastOverTimeLowerer{}
	}
	// downsample_tier (cerberus issue #2751) WRAPS whatever irate/idelta/
	// last_over_time strategy was just resolved above — it is a genuinely
	// different mechanism (an operator-provisioned, pre-populated table, not
	// a stateless read-time function swap over the raw table), so it sits
	// as an outer layer: an eligible shape routes to the tier, everything
	// else falls through to the family's own best-available raw-scan
	// strategy (native/laginframe/fanout) unchanged. See
	// chopt.FeatureDownsampleTier's own doc for why rate()/increase()/
	// delta() have no such wrapping at all.
	if optSet.Has(chopt.FeatureDownsampleTier) {
		l.Irate = promql.DownsampleTierIrateLowerer{Fallback: l.Irate}
		l.Idelta = promql.DownsampleTierIdeltaLowerer{Fallback: l.Idelta}
		l.LastOverTime = promql.DownsampleTierLastOverTimeLowerer{Fallback: l.LastOverTime}
	}
	// sorted_slab_over_time (issue #2761, widened to first_over_time /
	// stddev_over_time / stdvar_over_time / mad_over_time by issue #2804)
	// has no native timeSeries*ToGrid competitor: this whole function set's
	// only non-fan-out arm is the sorted-slab decomposition itself, so it is
	// wired directly with no embedded native layer above it (mirroring
	// quantile_prom_histogram's posture just above, though this strategy
	// DOES still embed its own Fallback — the plain fan-out — the way
	// fixed_accumulator_extrapolated does for rate/increase/delta). One
	// lowerer wiring covers the whole set: which PromQL function names
	// reach it at all is decided upstream, purely by AST dispatch, in
	// internal/promql/lower.go's `lowerRangeVectorCallFanout` switch — see
	// that switch's own doc for why last_over_time deliberately never
	// reaches this lowerer despite sharing its shape-eligibility check.
	if optSet.Has(chopt.FeatureSortedSlabOverTime) {
		l.OverTime = promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}}
	} else {
		l.OverTime = promql.FanoutOverTimeLowerer{}
	}
	// classic_bucket_merge_summap (issue #2756) has no version floor to
	// probe. #2817 closed its original correctness blocker; issue #2923's
	// real-ClickHouse re-measurement against the resulting (post-#2817)
	// construction then found its real cost within ~1% of the fold's, not
	// the estimated ~50x win — so AutoSelect stays false, a measured
	// negative result rather than an open question. See
	// promql.NativeClassicBucketMergeLowerer's own doc and
	// classic_bucket_merge_summap.go's header.
	if optSet.Has(chopt.FeatureClassicBucketMergeSumMap) {
		l.ClassicBucketMerge = promql.NativeClassicBucketMergeLowerer{
			Fallback: promql.FanoutClassicBucketMergeLowerer{},
		}
	} else {
		l.ClassicBucketMerge = promql.FanoutClassicBucketMergeLowerer{}
	}
	// exp_histogram_merge_summap (issue #2757) has no version floor to
	// probe, but ships AutoSelect: false: it now covers every shape —
	// instant AND range mode (cerberus issue #3027), any by()/without()
	// grouping (#2865), SUM or AVG fold (#2866) — each with its own
	// real-ClickHouse-calibrated budget guard rather than a reuse of the
	// classic fold's rows-dominated one — see
	// promql.NativeExpHistogramMergeLowerer's own doc.
	if optSet.Has(chopt.FeatureExpHistogramMergeSumMap) {
		l.ExpHistogramMerge = promql.NativeExpHistogramMergeLowerer{}
	} else {
		l.ExpHistogramMerge = promql.FanoutExpHistogramMergeLowerer{}
	}
	return l
}

// newLokiHandler builds the Loki head's handler with its limiters, version,
// timeouts, and the resolved per-query optimization SettingsRules wired in.
// Extracted (mirroring newPromHandler) so run's bootstrap stays within its
// maintainability budget as the optimization suite adds wiring.
//
// The head draws on TWO of the process's admission budgets: limiters.loki
// (CERBERUS_ADMIT_LOKI) fronts every short-lived route, and limiters.lokiTail
// (CERBERUS_ADMIT_TAIL) fronts only the long-lived /tail WebSocket. It takes
// the whole admitLimiters rather than the two pointers positionally for the
// reason that type exists: two same-typed *admit.Limiter parameters transpose
// silently at the callsite, and transposing THESE two mounts /tail on the
// request budget and every ordinary route on the tail budget — a subtler
// #1482. Selecting the fields by name here makes that untypeable.
func newLokiHandler(client *chclient.Client, cfg config.Config, optSet chopt.EnabledSet, limiters admitLimiters, logger *slog.Logger, resourceBounds engine.ResourceBoundOverrides, logsAttrStrategies chsql.AttrStrategies) *loki.Handler {
	h := loki.New(client, cfg.Logs, logger.With("api", "loki"))
	h.Limiter = limiters.loki
	h.TailLimiter = limiters.lokiTail
	h.Version = Version
	// text_index_line_filter (cerberus issue #2773) gates the LogQL
	// line-filter emitter's ANDed LIKE prefilter rewrite — read straight off
	// optSet, the SAME EnabledSet settingsRules below reads join_spill and
	// trace_id_bitmap_filter from, since this is a query-time rewrite, not
	// a DDL statement (see SchemaFullTextIndex's back-fill above for its
	// DDL sibling, full_text_index). h.Lang is the long-lived metadata-path
	// adapter; h.TextIndexLineFilter is copied onto the fresh *logql.Lang
	// langForRequest/langForRangeRequest build per /query and /query_range
	// request.
	h.TextIndexLineFilter = optSet.Has(chopt.FeatureTextIndexLineFilter)
	h.Lang.TextIndexLineFilter = h.TextIndexLineFilter
	// AttrStrategies (cerberus issue #2777) is the preflight boot probe's
	// resolved verdict on the logs attribute-map columns' physical shape —
	// see runRequirementsCheck's doc for the known cold-start-race
	// limitation. nil (the overwhelmingly common case: no JSON-typed
	// column detected, or the requirements check disabled) renders
	// byte-identical to before this field existed.
	h.AttrStrategies = logsAttrStrategies
	h.Lang.AttrStrategies = h.AttrStrategies
	h.QueryTimeout = cfg.ClickHouse.QueryTimeout
	h.TailWriteTimeout = cfg.LokiTailWriteTimeout
	// LabelCatalogEnabled (cerberus issue #2770) is the resolved chopt
	// loki_catalog_mv verdict — the SAME cfg.SchemaLokiCatalogMV DDLConfig
	// threads to gate provisioning the catalog table in the first place, so
	// the read path only ever attempts the catalog query on a deployment
	// where the DDL side actually created it.
	h.LabelCatalogEnabled = cfg.SchemaLokiCatalogMV
	// BodyTTL (cerberus issue #2769) is the SAME
	// cfg.SchemaProvisioning.LogsBodyTTL DDLConfig threads into
	// ddl.Config.ColumnTTL.LogsBody to gate the curated Body column TTL
	// ALTER in the first place, so /query and /query_range only ever warn
	// about an aged-Body window on a deployment where the DDL side
	// actually applied it. A plain config duration, not a chopt verdict —
	// see ddl.Config.ColumnTTL's doc comment for why this capability has
	// no version floor to gate on.
	h.BodyTTL = cfg.SchemaProvisioning.LogsBodyTTL
	h.Engine.Settings = settingsRules(cfg, optSet)
	// The prom head wires this (line ~713 above); the Loki head never did,
	// so requireSubquerySampleBudget's plan-time anchor-grid gate
	// (internal/engine/anchor_budget.go) fail-opened on every LogQL
	// subquery — issue #2055.
	h.Engine.MaxQuerySamples = client.MaxQuerySamples()
	// LogQL's own range aggregations lower to chplan.RangeWindow too (issue
	// #2667), so RateWindowFanoutMaxRows is wired here alongside the prom
	// head — see newPromHandler's own doc for why RangeBucketFanoutMaxRows /
	// RangeLWRFanoutMaxRows stay prom-only.
	h.Engine.RateWindowFanoutMaxRows = resourceBounds.RateWindowFanoutMaxRows
	// Issue #2733's emitted-SQL size bound is head-agnostic — every head emits
	// statements — so unlike the row-fanout ceilings it is wired onto this head
	// as well as prom and tempo. See engine.Engine.MaxEmittedSQLBytes' own doc.
	h.Engine.MaxEmittedSQLBytes = resourceBounds.MaxEmittedSQLBytes
	return h
}

// settingsRules builds the per-query ClickHouse settings rules from the
// resolved optimization EnabledSet plus the CERBERUS_* config. The
// aggregation-in-order, condition-cache, join-spill, trace-id-bitmap-filter,
// lazy-materialization and result-cache rules are all driven by the frozen
// EnabledSet (set.Has(...)), not raw env flags: under the default `auto` the
// stable 24.8-safe aggregation_in_order is on, condition_cache is on when the
// probed server is >= 25.3, join_spill is on when the probed server is >=
// 26.4, trace_id_bitmap_filter and lazy_materialization are on when the
// probed server is >= 25.11, and result_cache is on when the boot
// result-cache capability probe came back Available (its setting family
// predates cerberus's own 24.8 floor, so unlike the others it carries no
// version gate — see chopt.FeatureResultCache). log_comment shape stays its
// own dark flag (CERBERUS_LOG_COMMENT_SHAPE), wired alongside the corpus
// reconciler; the result-cache ingest-lag horizon and ttl are their own
// CERBERUS_RESULT_CACHE_* knobs (cfg.ResultCacheIngestLag / ResultCacheTTL),
// threaded straight through regardless of whether the feature itself
// resolved in. The schema instances are always supplied so the eligibility
// checks can map ANY scanned signal table to its sort-key prefix regardless
// of which head runs the query. Shared by all three heads' engines so the
// rules flip uniformly.
func settingsRules(cfg config.Config, set chopt.EnabledSet) engine.SettingsRules {
	return engine.SettingsRules{
		OptimizeAggregationInOrder: set.Has(chopt.FeatureAggregationInOrder),
		ConditionCache:             set.Has(chopt.FeatureConditionCache),
		JoinSpill:                  set.Has(chopt.FeatureJoinSpill),
		TraceIDBitmapFilter:        set.Has(chopt.FeatureTraceIDBitmapFilter),
		LogCommentShape:            cfg.LogCommentShape,
		ResultCache:                set.Has(chopt.FeatureResultCache),
		ResultCacheIngestLag:       cfg.ResultCacheIngestLag,
		ResultCacheTTL:             cfg.ResultCacheTTL,
		LazyMaterialization:        set.Has(chopt.FeatureLazyMaterialization),
		QueryWorkload:              cfg.CHQueryWorkload,
		Metrics:                    cfg.Schema,
		Traces:                     cfg.Traces,
		Logs:                       cfg.Logs,
	}
}

// viewRefreshStateInfo reads system.view_refreshes for one (database, view)
// over probe and renders it as an info.ViewRefreshState, degrading to the
// zero value (Configured=false) on either a query error or a not-found row
// — QueryViewRefreshState itself already degrades to Found=false on a
// deployment where the view was never provisioned (UNKNOWN_TABLE, or simply
// no matching row), the same honest "not configured" answer
// filesystemCacheInfoNow reports for an unconfigured cache; a query error
// beyond that degrades the same way rather than surfacing a transient error
// on a metadata endpoint that always returns 200. Extracted because
// infoOptions wired this exact body twice (LokiCatalogViewRefresh, cerberus
// issue #2770, and TempoTagCatalogViewRefresh, cerberus issue #2771) —
// mirroring internal/chclient's queryScanRows extraction, done for the same
// golangci-lint dupl reason.
func viewRefreshStateInfo(ctx context.Context, probe *chclient.Client, database, view string) info.ViewRefreshState {
	state, err := probe.QueryViewRefreshState(ctx, database, view)
	if err != nil || !state.Found {
		return info.ViewRefreshState{}
	}
	return info.ViewRefreshState{
		Configured:      true,
		Status:          state.Status,
		Exception:       state.Exception,
		LastSuccessTime: state.LastSuccessTime,
		LastRefreshTime: state.LastRefreshTime,
		Retry:           state.Retry,
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
		// ResultCacheStats is process-wide (cerberus issue #2781), not
		// per-client, so it is read directly off the chclient package rather
		// than through client — see internal/chclient/result_cache_metrics.go.
		ResultCache: func() info.ResultCacheState {
			hits, misses := chclient.ResultCacheStats()
			return info.ResultCacheState{Hits: hits, Misses: misses}
		},
		// FilesystemCache reports the server-side filesystem cache reading
		// (cerberus issue #2780), read over the SAME probe head as
		// Reachable/Ready. A query failure (server unreachable, or too old
		// to carry system.filesystem_cache_settings — the table has existed
		// well below cerberus's own 24.8 floor, but a query error is
		// possible on ANY live poll) degrades to the honest all-zero
		// Configured=false reading rather than surfacing a transient error
		// on a metadata endpoint that always returns 200.
		FilesystemCache: func(ctx context.Context) info.FilesystemCacheState {
			state, err := probe.QueryFilesystemCacheState(ctx)
			if err != nil {
				return info.FilesystemCacheState{}
			}
			return info.FilesystemCacheState{
				Configured:       state.Configured,
				Caches:           state.Caches,
				MaxSizeBytes:     state.MaxSizeBytes,
				CurrentSizeBytes: state.CurrentSizeBytes,
				CurrentElements:  state.CurrentElements,
			}
		},
		// LokiCatalogViewRefresh reports system.view_refreshes' status for
		// the loki label-catalog view (cerberus issue #2770), read over the
		// SAME probe head as FilesystemCache. Reported unconditionally —
		// not gated behind cfg.SchemaLokiCatalogMV — because
		// QueryViewRefreshState itself already degrades to Found=false on a
		// deployment where the view was never provisioned (UNKNOWN_TABLE,
		// or simply no matching row), the same honest "not configured"
		// answer FilesystemCache reports for an unconfigured cache; a query
		// error beyond that degrades the same way rather than surfacing a
		// transient error on a metadata endpoint that always returns 200.
		LokiCatalogViewRefresh: func(ctx context.Context) info.ViewRefreshState {
			return viewRefreshStateInfo(ctx, probe, cfg.ClickHouse.Database, schema.LabelCatalogTable+schema.LabelCatalogViewSuffix)
		},
		// TempoTagCatalogViewRefresh reports system.view_refreshes' status
		// for the Tempo tag-catalog view (cerberus issue #2771), the exact
		// same shape and posture LokiCatalogViewRefresh reports for its
		// sibling — see that field's own doc comment above for why this is
		// unconditional and how it degrades, and viewRefreshStateInfo's doc
		// for why the two share one body.
		TempoTagCatalogViewRefresh: func(ctx context.Context) info.ViewRefreshState {
			return viewRefreshStateInfo(ctx, probe, cfg.ClickHouse.Database, schema.TagCatalogTable+schema.TagCatalogViewSuffix)
		},
		StartTime:   startTime,
		Reachable:   func(ctx context.Context) bool { return probe.Ping(ctx) == nil },
		Breaker:     probe.PeekBreakerState,
		SchemaReady: schemaReadyNow,
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
	// QueryWorkload is the EFFECTIVE CERBERUS_CH_QUERY_WORKLOAD (cerberus
	// issue #2785): the configured name when the capability probe found the
	// server accepts it, else "" (unconfigured, or a rejected/unreachable
	// probe). Unlike Set it is not a chopt registry feature — see
	// resolveQueryWorkload's own doc — but it rides this same struct so
	// reprobeCHOptimizations can swap it live alongside Set on a capability
	// change, exactly like every other field here.
	QueryWorkload string
	// RawQueryWorkload is the ORIGINAL, un-resolved CERBERUS_CH_QUERY_
	// WORKLOAD (config.Config.CHQueryWorkload, read BEFORE
	// resolveCHOptimizations mutates its own *config.Config copy's field
	// down to "" on a rejected/unreachable boot probe). run() threads this
	// into reprobeCHOptimizations so a later re-probe re-evaluates the
	// ORIGINAL request against a possibly-upgraded server rather than
	// being permanently pinned to whatever boot's own probe found — see
	// reprobeCHOptimizations's own doc.
	RawQueryWorkload string
}

// supportedFloorVersion is the oldest ClickHouse cerberus supports, and the
// version an unreachable server is assumed to be running: resolving against it
// keeps every floor-safe optimization on while holding back anything newer, so
// a failed probe degrades the feature set rather than the process.
var supportedFloorVersion = chopt.Version{Major: 24, Minor: 8}

// startCHOptimizations wraps resolveCHOptimizations's boot call for run():
// it runs the ONE-TIME resolution and derives the two forms every later
// caller in run() needs from it (the plain EnabledSet, and the resolution
// wrapped in a live holder for reprobeCHOptimizations / the /info handler),
// collapsing what would otherwise be five separate statements in run() into
// one call + the mandatory error check.
func startCHOptimizations(ctx context.Context, logger *slog.Logger, client *chclient.Client, cfg *config.Config) (chopt.EnabledSet, *chOptLive, chOptResolution, error) {
	optRes, err := resolveCHOptimizations(ctx, logger, client, cfg)
	if err != nil {
		return chopt.EnabledSet{}, nil, chOptResolution{}, err
	}
	return optRes.Set, newCHOptLive(optRes), optRes, nil
}

// resolveCHOptimizations probes the connected ClickHouse server version and
// resolves the CERBERUS_CH_OPTIMIZATIONS auto-picker against it at boot,
// returning the EnabledSet the process starts on. It back-fills
// cfg.ExperimentalTSGridRange from the resolved set so the legacy ts-grid
// consumers (the PromQL lowering, the engine native gate, the preflight version
// floor) read a single source of truth, and logs the resolved set + the server
// version + any warnings (permissive skips and the legacy-alias deprecation).
// It ALSO probes and back-fills cfg.CHQueryWorkload (issue #2785): a
// configured-but-rejected workload name is cleared (permissive/auto
// fallback) or fatal (enforcing) — see the probe call's own comment below.
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
	rawQueryWorkload := cfg.CHQueryWorkload
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

	// A SECOND, independent capability canary for the result_cache feature
	// (cerberus issue #2781): the query-result-cache setting family predates
	// cerberus's own 24.8 floor, so there is no version gate to check here —
	// what this catches is a hardened/constrained profile that pins or
	// forbids use_query_cache, or a server whose query cache is disabled
	// entirely. Probed unconditionally alongside the ts-grid canary, over the
	// same bootstrap/default-DB connection, never fatal here.
	resultCacheCapability := probeResultCacheCapabilityOverBootstrap(ctx, cfg.ClickHouse)

	set, warnings, err := chopt.Resolve(chopt.Config{
		Optimizations:         cfg.CHOptimizations,
		Mode:                  cfg.CHOptimizationsMode,
		LegacyTSGrid:          cfg.LegacyTSGridFlag,
		Capability:            capability,
		ResultCacheCapability: resultCacheCapability,
	}, resolvedVersion)
	if err != nil {
		return chOptResolution{}, fmt.Errorf("resolve clickhouse optimizations: %w", err)
	}
	for _, w := range warnings {
		logger.Warn("ch_opt: " + w)
	}

	// A THIRD, independent capability canary — for the operator-configured
	// CERBERUS_CH_QUERY_WORKLOAD (cerberus issue #2785). See
	// resolveQueryWorkload's own doc for the full fatal-vs-skip contract;
	// boot is the ONE caller allowed to fail startup (fatalOnReject=true).
	resolvedQueryWorkload, queryWorkloadCapability, err := resolveQueryWorkload(
		ctx, logger, cfg.ClickHouse, cfg.CHQueryWorkload, cfg.CHOptimizationsMode, true,
	)
	if err != nil {
		return chOptResolution{}, err
	}
	cfg.CHQueryWorkload = resolvedQueryWorkload

	// Single source of truth: the legacy ts-grid bool is now derived from the
	// resolved set, not the raw env.
	cfg.ExperimentalTSGridRange = set.Has(chopt.FeatureTSGridRange)

	// Same back-fill for map_bucketed_serialization (cerberus issue #2774):
	// schemaboot.DDLConfig reads cfg.SchemaMapBucketedSerialization directly
	// rather than taking an EnabledSet parameter, so this is the ONE place the
	// version-gated, capability-independent verdict is threaded into it. This
	// runs before schemaboot.DDLConfig is called below (schemaReady/applyCfg),
	// so the auto-create DDL reflects the SAME resolved server this boot log
	// line reports.
	cfg.SchemaMapBucketedSerialization = set.Has(chopt.FeatureMapBucketedSerialization)

	// Same back-fill for column_statistics (cerberus issue #2766) — see the
	// map_bucketed_serialization comment immediately above for why this runs
	// here rather than being threaded as an EnabledSet parameter.
	cfg.SchemaColumnStatistics = set.Has(chopt.FeatureColumnStatistics)

	// Same back-fill for trace_id_projection (cerberus issue #2767) — see
	// the map_bucketed_serialization comment above for why this runs here
	// rather than being threaded as an EnabledSet parameter.
	cfg.SchemaTraceIDProjection = set.Has(chopt.FeatureTraceIDProjection)

	// Same back-fill for full_text_index (cerberus issue #2773) — see
	// the map_bucketed_serialization comment above for why this runs here
	// rather than being threaded as an EnabledSet parameter. Its sibling
	// query-time feature, text_index_line_filter, gates a loki-handler
	// rewrite rather than any DDL statement, so — like join_spill and
	// trace_id_bitmap_filter — it is read straight off the EnabledSet at
	// newLokiHandler's construction site instead of being back-filled here.
	cfg.SchemaFullTextIndex = set.Has(chopt.FeatureFullTextIndex)

	// Same back-fill for loki_catalog_mv (cerberus issue #2770) — see the
	// map_bucketed_serialization comment above for why this runs here
	// rather than being threaded as an EnabledSet parameter.
	cfg.SchemaLokiCatalogMV = set.Has(chopt.FeatureLokiCatalogMV)

	// Same back-fill for tempo_tag_catalog_mv (cerberus issue #2771) — see
	// the map_bucketed_serialization comment above for why this runs here
	// rather than being threaded as an EnabledSet parameter.
	cfg.SchemaTempoTagCatalogMV = set.Has(chopt.FeatureTempoTagCatalogMV)

	// Same back-fill for downsample_tier (cerberus issue #2751) — see the
	// map_bucketed_serialization comment above for why this runs here
	// rather than being threaded as an EnabledSet parameter. Unlike the
	// others above, this SAME verdict also drives query routing (see
	// nativeRangeLowerers below) — schema.DownsampleTierTable's own doc
	// explains why one flag safely covers both here.
	cfg.SchemaDownsampleTier = set.Has(chopt.FeatureDownsampleTier)

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
		"server_result_cache_capability", resultCacheCapability.String(),
		"query_workload", cfg.CHQueryWorkload,
		"server_query_workload_capability", queryWorkloadCapability.String(),
		"enabled", strings.Join(set.IDs(), ","),
	)
	return chOptResolution{
		Set:              set,
		ResolvedVersion:  resolvedVersion,
		VersionFallback:  versionFallback,
		QueryWorkload:    resolvedQueryWorkload,
		RawQueryWorkload: rawQueryWorkload,
	}, nil
}

// resolveQueryWorkload decides the EFFECTIVE ClickHouse `workload` name to
// stamp on cerberus's own queries (cerberus issue #2785): "" when configured
// is empty (the default — no probe runs, no extra boot/reprobe round trip),
// else the boot-time capability canary's verdict on configured.
//
// fatalOnReject distinguishes boot (true) from a live reprobe (false):
// boot is allowed to fail startup on a definitive Forbidden verdict under
// mode==Enforcing (an operator config error — the same treatment an
// explicitly-requested-but-unsupported chopt registry feature gets), while
// reprobeCHOptimizations never repeats boot's fatal-on-config-fault
// behaviour (see its own doc) — a Forbidden verdict there is logged and the
// workload is dropped from the live rules, exactly like a Set feature that
// regresses across a live server change. Unreachable (inconclusive, a
// transient connectivity failure rather than a verdict from the server) is
// NEVER fatal in either caller, mirroring blockIsInconclusive's treatment
// of Unreachable elsewhere in the chopt resolver.
func resolveQueryWorkload(
	ctx context.Context,
	logger *slog.Logger,
	chCfg chclient.Config,
	configured string,
	mode chopt.Mode,
	fatalOnReject bool,
) (resolved string, capability chopt.Capability, err error) {
	if configured == "" {
		return "", chopt.CapabilityUnknown, nil
	}
	capability = probeQueryWorkloadCapabilityOverBootstrap(ctx, chCfg, configured)
	resolved, err = decideQueryWorkload(configured, capability, mode, fatalOnReject)
	if err != nil {
		return "", capability, err
	}
	if resolved == "" {
		if capability == chopt.CapabilityForbidden {
			logger.Warn("ch_opt: CERBERUS_CH_QUERY_WORKLOAD rejected by server, skipping", "workload", configured, "mode", mode.String())
		} else {
			logger.Warn("ch_opt: CERBERUS_CH_QUERY_WORKLOAD capability probe unreachable, skipping", "workload", configured)
		}
	}
	return resolved, capability, nil
}

// decideQueryWorkload is resolveQueryWorkload's pure decision core, split out
// so the fatal-vs-skip branching is unit-testable without a live ClickHouse
// connection (the probe itself, in resolveQueryWorkload, cannot be). Given
// the ALREADY-PROBED capability verdict, it returns the effective workload
// name ("" meaning: don't stamp it) or a fatal error — see
// resolveQueryWorkload's own doc for the full fatalOnReject contract.
// configured is assumed non-empty; resolveQueryWorkload never calls this
// with an empty one (it short-circuits before probing).
func decideQueryWorkload(configured string, capability chopt.Capability, mode chopt.Mode, fatalOnReject bool) (string, error) {
	switch capability {
	case chopt.CapabilityAvailable:
		return configured, nil
	case chopt.CapabilityForbidden:
		if fatalOnReject && mode == chopt.Enforcing {
			return "", fmt.Errorf(
				"clickhouse server rejected CERBERUS_CH_QUERY_WORKLOAD=%q (the `workload` setting, or the named workload itself under throw_on_unknown_workload, was refused) under CERBERUS_CH_OPTIMIZATIONS_MODE=enforcing",
				configured,
			)
		}
		return "", nil
	default: // chopt.CapabilityUnreachable
		return "", nil
	}
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

// probeResultCacheCapabilityOverBootstrap runs the query-result-cache
// capability canary over a short-lived client bound to ClickHouse's
// always-present `default` database, exactly like
// probeTSGridCapabilityOverBootstrap — same reasoning for binding to
// `default` (the canary must not depend on the configured otel database
// existing yet). A failure to even open the client is itself an unreachable
// verdict (conservative: result_cache stays off), never fatal.
func probeResultCacheCapabilityOverBootstrap(ctx context.Context, chCfg chclient.Config) chopt.Capability {
	bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg, resultCacheProbePool))
	if err != nil {
		return chopt.CapabilityUnreachable
	}
	defer func() {
		_ = bootClient.Close()
	}()
	return bootClient.ProbeResultCacheCapability(ctx)
}

// probeQueryWorkloadCapabilityOverBootstrap runs the `workload` setting
// capability canary over a short-lived client bound to ClickHouse's
// always-present `default` database, exactly like
// probeResultCacheCapabilityOverBootstrap — same reasoning for binding to
// `default`. Unlike the other two canaries it stamps the OPERATOR'S OWN
// configured workload name (workloadName), not a fixed sentinel — see
// ProbeQueryWorkloadCapability's own doc for why. A failure to even open the
// client is itself an unreachable verdict (conservative: the knob is
// dropped, never fatal from this function alone — resolveCHOptimizations
// decides fatal-vs-skip from the verdict plus CHOptimizationsMode).
func probeQueryWorkloadCapabilityOverBootstrap(ctx context.Context, chCfg chclient.Config, workloadName string) chopt.Capability {
	bootClient, err := chclient.New(bootstrapClickHouseConfig(chCfg, queryWorkloadProbePool))
	if err != nil {
		return chopt.CapabilityUnreachable
	}
	defer func() {
		_ = bootClient.Close()
	}()
	return bootClient.ProbeQueryWorkloadCapability(ctx, workloadName)
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
//
// topology carries chopt.ClusterTopology.DataShardCount (CERBERUS_CH_DATA_SHARDS,
// cerberus issue #3081) — this is the ONE place it is copied into
// solver.Config.DataShardCount, keeping internal/solver's own import
// surface free of chopt (see solver.Config.DataShardCount's own doc). It
// also sizes the SECOND, independent DataShardFanoutGate semaphore
// alongside the pre-existing Gate: nil (never allocated) whenever
// DataShardCount <= 1 — see solver.NewDataShardFanoutGate's own doc.
func buildSolver(
	logger *slog.Logger,
	chCfg chclient.Config,
	topology chopt.ClusterTopology,
	client *chclient.Client,
	promLimiter *admit.Limiter,
) (*solver.Solver, error) {
	cfg, err := solver.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	cfg.DataShardCount = topology.DataShardCount
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// A soft-deprecated env name still applies, so it changes nothing about
	// the resolved Config — but a rename nobody is told about is a rename that
	// rots. Announce it here, at the one place the solver environment is read,
	// mirroring resolveCHOptimizations' legacy-alias notice.
	for _, warn := range solver.DeprecatedEnvWarnings() {
		logger.Warn(warn)
	}

	// GLOBAL shard gate: MaxOpenConns − reserve, floored at 2 so the
	// Executor's gate/2 cap never collapses to zero. The pool size is the
	// validated, already-positive value config.FromEnv resolved.
	gateCap := int64(chCfg.MaxOpenConns - solverGateReserve)
	if gateCap < 2 {
		gateCap = 2
	}
	gate := semaphore.NewWeighted(gateCap)

	// SECOND, independent global semaphore bounding aggregate ClickHouse-side
	// data-shard fan-out (cerberus issue #3081) — nil/dataShardFanoutCap==gateCap
	// whenever cfg.DataShardCount <= 1, an EXACT no-op for every deployment
	// that predates this field. See solver.NewDataShardFanoutGate's own doc.
	dataShardFanoutGate, dataShardFanoutCap := solver.NewDataShardFanoutGate(cfg, gateCap)

	// The admit top-up is only meaningful when admission control is enabled.
	// A nil *admit.Limiter (CERBERUS_ADMIT_DISABLED=true) leaves ExecDeps.Admit
	// nil, which the Executor treats as "no cap" (it runs at full P). Passing
	// the typed-nil *admit.Limiter directly would defeat the Executor's
	// nil-interface guard, so gate the assignment on a non-nil limiter.
	deps := solver.ExecDeps{
		Client:              client,
		Gate:                gate,
		GateCap:             gateCap,
		DataShardFanoutGate: dataShardFanoutGate,
		DataShardFanoutCap:  dataShardFanoutCap,
		Breaker:             client,
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
		"data_shard_count", cfg.DataShardCount,
		"data_shard_fanout_cap", dataShardFanoutCap,
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
		// The downsample tier's CREATE TABLE / MV (cerberus issue #2751) use
		// AggregateFunction(timeSeriesLastTwoSamples, ...) — like every
		// other consumer of that experimental function family, ClickHouse
		// rejects it unless allow_experimental_time_series_aggregate_functions
		// is set on the DDL-issuing session (chclient.WithTSGridSetting is
		// the SAME carrier the query path stamps for the timeSeries*ToGrid
		// family — see planHasTSGridNative). Stamped only when the tier is
		// actually enabled, so a deployment that never opts in issues
		// byte-identical DDL sessions to today.
		if applyCfg.DownsampleTierEnabled {
			ctx = chclient.WithTSGridSetting(ctx)
		}
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
	versionProbePool       = "version-probe"
	tsGridProbePool        = "tsgrid-probe"
	resultCacheProbePool   = "result-cache-probe"
	queryWorkloadProbePool = "query-workload-probe"
	schemaApplyPool        = "schema-apply"
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
// preflightRequirementsFromConfig translates the resolved config into
// preflight.Requirements, including the enabled-head -> Signal mapping
// (#1949): a Head maps 1:1 onto a Signal (prom -> Metrics, loki -> Logs,
// tempo -> Traces), so a deployment that narrows CERBERUS_ENABLED_HEADS
// (e.g. a Loki-only split-mode pod) never gates readiness — or spends
// boot-time introspection — on a signal's tables it will never ingest. It
// also carries the storage-tiering config (CERBERUS_SCHEMA_STORAGE_POLICY /
// CERBERUS_SCHEMA_TIER_VOLUME) gate 3 checks for an accepted-but-inert
// multi-volume policy. Pure function of cfg so it is unit-testable without a
// real ClickHouse client; runRequirementsCheck is the only caller.
func preflightRequirementsFromConfig(cfg config.Config) preflight.Requirements {
	return preflight.Requirements{
		Database:          cfg.ClickHouse.Database,
		NativeRateEnabled: cfg.ExperimentalTSGridRange,
		Metrics:           cfg.Schema,
		Logs:              cfg.Logs,
		Traces:            cfg.Traces,
		StoragePolicy:     cfg.SchemaProvisioning.StoragePolicy,
		TierVolume:        cfg.SchemaProvisioning.TierVolume,
		Signals: preflight.Signals{
			Metrics: cfg.HeadEnabled(config.HeadProm),
			Logs:    cfg.HeadEnabled(config.HeadLoki),
			Traces:  cfg.HeadEnabled(config.HeadTempo),
		},
	}
}

// preflightAttrStrategies carries runRequirementsCheck's per-signal
// AttrStrategies findings back to the caller. Named — rather than two
// positional chsql.AttrStrategies return values — for the identical
// anti-transposition reason admitLimiters exists (see its own doc): two
// same-typed values transpose silently at a callsite, and swapping these
// would silently apply the traces schema's strategy to the logs head (and
// vice versa) with no compiler diagnostic.
type preflightAttrStrategies struct {
	Logs   chsql.AttrStrategies
	Traces chsql.AttrStrategies
}

// runRequirementsCheck also returns the resolved per-signal AttrStrategies
// (cerberus issue #2777, extended to traces by #3062) so the caller can
// wire them onto the Loki / Tempo heads' Handlers — see newLokiHandler and
// mountAPIHeads' tempoHandler.SetAttrStrategies call. Known limitation
// (documented rather than silently accepted): on the TRANSIENT not-ready
// path (a table doesn't exist yet at boot), both fields are necessarily
// the zero value — the shape can't be introspected before the table
// exists — and they stay zero for the rest of THIS process's life even
// after the background re-probe (reprobeSchema) flips /readyz once the
// schema appears; a JSON-typed schema created during that race window is
// picked up correctly only on the NEXT boot. This mirrors
// decideRequirementsOutcome's own boot-resolved-once posture for every
// other preflight finding — nothing here re-derives shape facts after the
// readiness re-probe passes, and doing so would require making the
// Handler's resolved strategy mutable under concurrent requests, which
// cerberus issue #2777 explicitly avoids (chopt-style pure, boot-resolved
// strategy, zero per-query branching).
func runRequirementsCheck(
	ctx context.Context,
	logger *slog.Logger,
	client *chclient.Client,
	cfg config.Config,
) (health.SchemaPresentFunc, preflightAttrStrategies, error) {
	if !cfg.RequirementsCheck {
		logger.Info("requirements check disabled (CERBERUS_REQUIREMENTS_CHECK=false)")
		return nil, preflightAttrStrategies{}, nil
	}
	req := preflightRequirementsFromConfig(cfg)
	res := preflight.RunIfEnabled(ctx, cfg.RequirementsCheck, client, req)
	// Logged BEFORE the fatal check: a warning describes the server cerberus is
	// pointed at, and an operator debugging a failed boot needs it just as much
	// as one whose boot succeeds.
	for _, w := range res.Warnings {
		logger.Warn(w)
	}

	outcome := decideRequirementsOutcome(res, cfg.Schema, cfg.ClickHouse.Database)
	if outcome.fatalErr != nil {
		return nil, preflightAttrStrategies{}, outcome.fatalErr
	}
	if outcome.notReadyReason != "" {
		// Transient: boot but stay NOT READY; the background re-probe (reusing
		// the auto-create retry cadence) flips readiness once the condition
		// decideRequirementsOutcome named clears. No restart.
		logger.Warn(outcome.logMsg, "reason", outcome.notReadyReason)
		present := newSchemaPresentSignal(outcome.notReadyReason)
		go reprobeSchema(ctx, logger, client, req, present, schemaRetryInterval)
		return present.Func(), preflightAttrStrategies{}, nil
	}

	logger.Info(
		"requirements check passed",
		"database", cfg.ClickHouse.Database,
		"native_rate", cfg.ExperimentalTSGridRange,
	)
	return nil, preflightAttrStrategies{Logs: res.LogsAttrStrategies, Traces: res.TracesAttrStrategies}, nil
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
// boundary matters (#1905). Every other AbsentTables entry — including a
// metric table whose name cerberus defaulted rather than the operator
// setting it — stays on preflight's original transient path.
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
		// Fatal, unlike the general AbsentTables tolerance below (#1905): an
		// explicitly configured metric table name that is absent never
		// self-heals the way an unprovisioned schema does, and a silently
		// degraded metadata surface (empty /api/v1/series responses, never
		// an error) is harder for an operator to notice than a boot failure
		// that names the table and the config key.
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
// schema.metrics.* config key that would set it (see
// internal/config/nested.go's schema.metrics.* bindings, the source of truth
// these key strings mirror) and with whether that key actually did.
type metricTableBinding struct {
	table     string
	configKey string
	// explicit reports whether configuration set this table name, as
	// opposed to it defaulting. Only an explicit name is an operator
	// assertion that the table exists, and only an assertion can be
	// falsified — see fatalAbsentMetricTablesErr.
	explicit bool
}

// metricTableBindings lists the metric tables the /api/v1/series,
// /api/v1/labels, and /api/v1/label/<name>/values handlers union across
// (schema.Metrics.ConfiguredMetricTables' Gauge/Sum/Histogram set) paired
// with the config key each came from, for fatalAbsentMetricTablesErr's
// decision and error text.
func metricTableBindings(m schema.Metrics) []metricTableBinding {
	return []metricTableBinding{
		{table: m.GaugeTable, configKey: "schema.metrics.gaugeTable", explicit: m.TableOverrides.Gauge},
		{table: m.SumTable, configKey: "schema.metrics.sumTable", explicit: m.TableOverrides.Sum},
		{table: m.HistogramTable, configKey: "schema.metrics.histogramTable", explicit: m.TableOverrides.Histogram},
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
// absent-schema race"); the two callers before this point keep that
// boundary intact by covering "ClickHouse unreachable" (res.Unreachable)
// and "database not yet provisioned" (res.DatabaseAbsent), so neither
// reaches here.
//
// The gate is scoped to EXPLICITLY CONFIGURED names, which is what makes
// the extra strictness sound. Only a name configuration set is an operator
// asserting that a table exists under exactly that spelling; a defaulted
// name is nothing more than cerberus's built-in guess at where a stock
// OTel-CH deployment writes metrics. A deployment that ingests only logs
// and traces never provisions otel_metrics_gauge / _sum / _histogram and is
// not misconfigured for it, and one whose collector has not written its
// first metric yet has not provisioned them YET — treating either as fatal
// turns the ordinary cerberus-before-the-collector race into a crash loop
// for every metrics-less deployment, which is the exact failure this gate
// exists to keep OUT of the metadata surface. Defaulted-and-absent
// therefore falls through to the transient not-ready path below, where an
// absent table has always belonged. schema.MetricTableOverrides carries the
// provenance here because the two cases are identical strings by the time
// they reach this function.
//
// An empty table name is never explicit (the resolvers treat unset and
// whitespace-only alike) and is never probed either, since preflight's
// checkSchema skips empty names before ever reaching system.columns.
func fatalAbsentMetricTablesErr(m schema.Metrics, absentTables []string) error {
	absent := make(map[string]bool, len(absentTables))
	for _, t := range absentTables {
		absent[t] = true
	}
	var problems []string
	for _, b := range metricTableBindings(m) {
		if b.explicit && absent[b.table] {
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
