//go:build startup_bench

// Package e2e — startup-speed benchmark.
//
// Measures wall-clock latency from `cerberus serve` process spawn to the
// first 200 OK from GET /healthz against a reachable ClickHouse. The
// target is < 2 seconds; the assertion uses a 2500 ms ceiling to absorb
// CI scheduler jitter while still catching real regressions (e.g. a new
// startup hook that blocks the listener bind).
//
// Build-tagged + env-gated so regular `go test ./...` doesn't pull in
// the build step. Run via `just startup-bench` or:
//
//	RUN_STARTUP_BENCH=1 go test -tags=startup_bench ./test/e2e/...
//
// Prerequisites:
//   - ClickHouse reachable at $CH_ADDR (default 127.0.0.1:9000), already
//     warm. The benchmark explicitly disables auto-create-schema so we
//     measure HTTP-listener bootstrap, not DDL apply time.
//   - A built `cerberus` binary; the test will `go build` one into a
//     temp dir when none is supplied via $CERBERUS_BIN.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	// startupTarget is the documented < 2 s goal from the 12-factor
	// disposability audit.
	startupTarget = 2 * time.Second
	// startupCeiling adds a 500 ms safety margin so CI scheduler jitter
	// doesn't flip the test red. Real regressions (extra synchronous
	// startup hooks, blocking DNS lookups, etc.) overshoot by seconds.
	startupCeiling = 2500 * time.Millisecond
	// pollTimeout caps the total spent waiting for /healthz.
	pollTimeout = 10 * time.Second
	// pollInterval is the gap between /healthz probes during the
	// warm-up race. 10 ms gives sub-percent measurement noise.
	pollInterval = 10 * time.Millisecond
)

func TestStartupSpeed_HealthzUnder2s(t *testing.T) {
	if os.Getenv("RUN_STARTUP_BENCH") != "1" {
		t.Skip("set RUN_STARTUP_BENCH=1 to run the startup benchmark")
	}

	binary := os.Getenv("CERBERUS_BIN")
	if binary == "" {
		binary = buildCerberus(t)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	chAddr := envOr("CH_ADDR", "127.0.0.1:9000")
	chDB := envOr("CH_DATABASE", "otel")
	chUser := envOr("CH_USERNAME", "default")
	chPass := envOr("CH_PASSWORD", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(),
		"CERBERUS_HTTP_ADDR="+addr,
		"CERBERUS_CH_ADDR="+chAddr,
		"CERBERUS_CH_DATABASE="+chDB,
		"CERBERUS_CH_USERNAME="+chUser,
		"CERBERUS_CH_PASSWORD="+chPass,
		// Measure HTTP-server bootstrap alone — auto-create DDL is a
		// separate cost path covered by the schema-ddl integration
		// test.
		"CERBERUS_AUTO_CREATE_SCHEMA=false",
		// No collector dependency.
		"CERBERUS_OTLP_ENDPOINT=",
		// Quiet logs so the test output stays readable.
		"CERBERUS_LOG_LEVEL=warn",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cerberus: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	url := "http://" + addr + "/healthz"
	if err := waitForHealthz(ctx, url, pollTimeout); err != nil {
		t.Fatalf("healthz never returned 200: %v (elapsed %s)", err, time.Since(start))
	}
	latency := time.Since(start)

	t.Logf("startup latency: process-start to /healthz 200 = %s (target < %s, ceiling %s)",
		latency, startupTarget, startupCeiling)

	if latency > startupCeiling {
		t.Fatalf("startup latency %s exceeds ceiling %s (target < %s)",
			latency, startupCeiling, startupTarget)
	}
	if latency > startupTarget {
		t.Logf("WARNING: startup latency %s exceeds target %s but is within ceiling %s",
			latency, startupTarget, startupCeiling)
	}
}

// waitForHealthz polls url every pollInterval until it returns 200 OK or
// the timeout elapses. The caller measures wall-clock latency from the
// pre-spawn marker so process fork + exec is included in the budget.
func waitForHealthz(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	var lastErr error
	for {
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timeout after %s: last error: %w", timeout, lastErr)
			}
			return fmt.Errorf("timeout after %s", timeout)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// buildCerberus compiles ./cmd/cerberus into a temp directory and returns
// the binary path. The temp dir is cleaned up by `t.TempDir`.
func buildCerberus(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cerberus")
	// Build from the repo root — walk up from the test file until we
	// find go.mod, in case the test is invoked from an arbitrary cwd.
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/cerberus")
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build cerberus: %v", err)
	}
	return bin
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("go.mod not found above %s", wd)
}

// freePort asks the kernel for an unused TCP port. Inherently racy
// (something else could grab it before cerberus binds) but acceptable
// for a benchmark that already builds in retry margin.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
