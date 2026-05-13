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
	"syscall"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// Version is set at build time by goreleaser.
var Version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("cerberus exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger.Info("cerberus starting",
		"version", Version,
		"http_addr", cfg.HTTPAddr,
		"ch_addr", cfg.ClickHouse.Addr,
		"ch_db", cfg.ClickHouse.Database,
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	promHandler := prom.New(client, cfg.Schema, logger.With("api", "prom"))
	promHandler.Mount(mux)

	lokiHandler := loki.New(client, schema.DefaultOTelLogs(), logger.With("api", "loki"))
	lokiHandler.Mount(mux)

	tempoHandler := tempo.New(client, schema.DefaultOTelTraces(), Version, logger.With("api", "tempo"))
	tempoHandler.Mount(mux)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
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
	logger.Info("cerberus stopped")
	return nil
}
