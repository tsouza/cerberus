package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/solver"
)

// TestStartRouteSelfTune_OffByDefault is the zero-behavior-change proof: with
// the default solver Config (SelfTune == false) startRouteSelfTune must return
// nil and start nothing — it returns before ever touching the CH client, so a
// nil client is safe here. This pins that an existing deployment (which never
// sets CERBERUS_ROUTE_SELFTUNE) gets no loop and no behavior change.
func TestStartRouteSelfTune_OffByDefault(t *testing.T) {
	cfg := solver.DefaultConfig()
	if cfg.SelfTune {
		t.Fatal("DefaultConfig.SelfTune must be false (off by default)")
	}
	s := solver.New(cfg, nil, solver.ExecDeps{})

	tuner := startRouteSelfTune(
		context.Background(),
		slog.Default(),
		nil, // never dereferenced on the off path
		config.Config{},
		s,
	)
	if tuner != nil {
		tuner.Stop()
		t.Fatal("self-tuning must not start when SelfTune is off")
	}
}

// TestStartRouteSelfTune_NilSolver: a disabled prom head passes a nil solver;
// startRouteSelfTune must tolerate it and return nil.
func TestStartRouteSelfTune_NilSolver(t *testing.T) {
	if tuner := startRouteSelfTune(context.Background(), slog.Default(), nil, config.Config{}, nil); tuner != nil {
		tuner.Stop()
		t.Fatal("nil solver must yield a nil tuner")
	}
}

// TestConfigFromEnv_SelfTuneOffByDefault pins that the env builder leaves
// SelfTune false unless the operator opts in.
func TestConfigFromEnv_SelfTuneOffByDefault(t *testing.T) {
	t.Setenv(solver.EnvRouteSelfTune, "")
	cfg, err := solver.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.SelfTune {
		t.Fatal("unset CERBERUS_ROUTE_SELFTUNE must leave SelfTune false")
	}

	t.Setenv(solver.EnvRouteSelfTune, "true")
	on, err := solver.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv (on): %v", err)
	}
	if !on.SelfTune {
		t.Fatal("CERBERUS_ROUTE_SELFTUNE=true must enable SelfTune")
	}
}
