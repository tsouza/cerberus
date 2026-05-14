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
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Version is set at build time by goreleaser.
var Version = "dev"

func main() {
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

	logger := config.NewLogger(os.Stderr, cfg.Log)
	slog.SetDefault(logger)

	logger.Info("cerberus starting",
		"version", Version,
		"http_addr", cfg.HTTPAddr,
		"ch_addr", cfg.ClickHouse.Addr,
		"ch_db", cfg.ClickHouse.Database,
		"log_format", cfg.Log.Format,
		"log_level", cfg.Log.Level.String(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := chclient.New(ctx, cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("connect to clickhouse: %w", err)
	}
	defer func() {
		_ = client.Close()
	}()

	// schemaReady tracks whether the auto-create-schema startup hook
	// has finished at least once. When auto-create is off the readiness
	// probe should not gate on it, so we seed it true.
	var schemaReady atomic.Bool
	if cfg.AutoCreateSchema {
		logger.Info("auto-creating OTel ClickHouse schema",
			"database", cfg.ClickHouse.Database,
			"signals", "metrics,logs,traces",
		)
		applyCfg := ddl.Config{Database: cfg.ClickHouse.Database}
		if err := ddl.ApplyWithConfig(ctx, client.Conn(), applyCfg, ddl.All); err != nil {
			return fmt.Errorf("auto-create schema: %w", err)
		}
		logger.Info("OTel ClickHouse schema ready")
		schemaReady.Store(true)
	} else {
		schemaReady.Store(true)
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
		ServiceName:    "cerberus",
		ServiceVersion: Version,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	installOTel(providers.TracerProvider)
	otel.SetMeterProvider(providers.MeterProvider)
	if cfg.OTLP.Endpoint != "" {
		logger.Info("OTLP exporters enabled",
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
		logger.Info("admission control enabled",
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

	// All three heads run on the shared engine.Engine pipeline; each
	// engine is constructed below from the shared Client + a seed
	// optimizer and assigned onto the per-head handler.
	promHandler := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	promHandler.Engine = &engine.Engine{Optimizer: promHandler.Optimizer, Client: client}
	promHandler.Limiter = promLimiter
	promHandler.Mount(traceMux)

	lokiHandler := loki.New(client, cfg.Logs, logger.With("api", "loki"))
	lokiHandler.Limiter = lokiLimiter
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
		SchemaReady: schemaReady.Load,
	})

	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
	rootMux.Handle("/", tracedAPI)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           rootMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

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
	// Flush any pending OTLP batches before the process exits. Noop
	// when telemetry was disabled (Endpoint == "").
	if err := providers.Shutdown(shutdownCtx); err != nil {
		logger.Warn("telemetry shutdown returned error", "err", err)
	}
	logger.Info("cerberus stopped")
	return nil
}
