package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/chclient"
)

// Startup-resilience tests for the "ClickHouse down at boot" incident
// class (CI run 27272406583, job 80544908938): an HPA scale-up replica
// that booted while ClickHouse was saturated exited(1) on
// `dial tcp …:9000: connect: connection refused` and crash-looped.
// The contract pinned here is the k8s-idiomatic one:
//
//   - process construction succeeds with CH unreachable (chclient.New
//     is lazy — no startup dial is fatal),
//   - the HTTP listener serves /healthz 200 regardless,
//   - /readyz reports 503 until ClickHouse appears, then flips to 200,
//   - the auto-create-schema hook retries in the background instead of
//     exiting, flipping schemaReady on its first success.
//
// run() itself is not directly callable (see lifecycle_test.go for the
// rationale); these tests exercise the exact building blocks run()
// composes, using the same wiring shapes.

// unreachableAddr returns a 127.0.0.1 address that is guaranteed to
// refuse connections: it binds an ephemeral port, then closes the
// listener so the kernel RSTs any subsequent dial.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// TestStartup_CHDownAtBoot_ClientConstructionSucceeds pins the lazy
// chclient.New contract main.go depends on: with nothing listening on
// the CH address, construction must succeed (no exit(1) path) while
// Ping — the readiness signal — fails.
func TestStartup_CHDownAtBoot_ClientConstructionSucceeds(t *testing.T) {
	client, err := chclient.New(chclient.Config{
		Addr:        unreachableAddr(t),
		Database:    "otel",
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("chclient.New with unreachable CH must not fail-fast; got %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err == nil {
		t.Fatal("Ping against an unreachable ClickHouse must error (it is the readiness signal)")
	}
}

// TestStartup_CHDownAtBoot_HealthzUpReadyzUnready wires the same
// root-mux shape run() builds — health handler mounted next to the API
// mux — against a client whose CH is down, and pins the probe split:
// liveness (/healthz) 200, readiness (/readyz) 503. This is "started
// but unready": k8s keeps the pod alive and out of the Service
// endpoints instead of restart-looping it.
func TestStartup_CHDownAtBoot_HealthzUpReadyzUnready(t *testing.T) {
	client, err := chclient.New(chclient.Config{
		Addr:        unreachableAddr(t),
		Database:    "otel",
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var schemaReady atomic.Bool
	schemaReady.Store(true) // auto-create off — only the CH ping gates

	healthHandler := health.New(health.Options{
		Pinger:      client,
		SchemaReady: schemaReady.Load,
		CacheTTL:    -1, // no coalescing in tests
	})
	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)

	srv := httptest.NewServer(rootMux)
	t.Cleanup(srv.Close)

	gotHealthz := getStatus(t, srv.URL+"/healthz")
	if gotHealthz != http.StatusOK {
		t.Errorf("/healthz with CH down = %d; want 200 (liveness must not depend on CH)", gotHealthz)
	}
	gotReadyz := getStatus(t, srv.URL+"/readyz")
	if gotReadyz != http.StatusServiceUnavailable {
		t.Errorf("/readyz with CH down = %d; want 503", gotReadyz)
	}
}

// flipPinger fails until healthy is set — the minimal stand-in for
// "ClickHouse appears after the replica booted".
type flipPinger struct {
	healthy atomic.Bool
}

func (f *flipPinger) Ping(context.Context) error {
	if f.healthy.Load() {
		return nil
	}
	return errors.New("dial tcp 10.0.0.1:9000: connect: connection refused")
}

// TestStartup_ReadyzFlipsReadyOnceCHAppears pins the recovery half of
// the contract: a replica that booted unready must flip /readyz to 200
// as soon as the CH ping succeeds — no restart required.
func TestStartup_ReadyzFlipsReadyOnceCHAppears(t *testing.T) {
	pinger := &flipPinger{}
	healthHandler := health.New(health.Options{
		Pinger:   pinger,
		CacheTTL: -1, // each probe re-pings, so the flip is observed immediately
	})
	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)

	srv := httptest.NewServer(rootMux)
	t.Cleanup(srv.Close)

	if got := getStatus(t, srv.URL+"/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before CH appears = %d; want 503", got)
	}

	pinger.healthy.Store(true)

	if got := getStatus(t, srv.URL+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz after CH appears = %d; want 200", got)
	}
}

// TestRetrySchemaApply_FlipsReadyAfterTransientFailures pins the
// background auto-create-schema retry loop: apply failures (CH still
// booting) are retried on the interval, and the first success flips
// the ready flag /readyz consults — without ever returning an error
// that would tear the process down.
func TestRetrySchemaApply_FlipsReadyAfterTransientFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var ready atomic.Bool
	var attempts atomic.Int32
	apply := func(context.Context) error {
		if attempts.Add(1) < 3 {
			return errors.New("dial tcp 10.0.0.1:9000: connect: connection refused")
		}
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		retrySchemaApply(ctx, slog.New(slog.DiscardHandler), &ready, time.Millisecond, apply, func() {})
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retrySchemaApply did not return after a successful apply")
	}
	if !ready.Load() {
		t.Error("ready = false after successful apply; want true")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d; want 3 (two transient failures, then success)", got)
	}
}

// TestRetrySchemaApply_StopsOnContextCancel pins shutdown behavior: a
// SIGTERM (ctx cancel) must end the retry loop promptly without
// flipping ready.
func TestRetrySchemaApply_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var ready atomic.Bool
	apply := func(context.Context) error {
		return errors.New("dial tcp 10.0.0.1:9000: connect: connection refused")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		retrySchemaApply(ctx, slog.New(slog.DiscardHandler), &ready, time.Millisecond, apply, func() {})
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retrySchemaApply did not stop after context cancel")
	}
	if ready.Load() {
		t.Error("ready = true after cancellation; want false (apply never succeeded)")
	}
}

// TestStartup_AbsentSchema_HealthzUpReadyzUnready pins the absent-schema
// boot-but-wait contract: when the boot-time requirements check finds the
// configured tables not yet provisioned (the cerberus + collector startup
// race), the schemaPresentSignal carrier reports not-present, so the same
// root-mux run() builds serves /healthz 200 (liveness — the process is up)
// while /readyz is 503 with the precise absent reason. The process never
// exits — k8s keeps it alive and out of the Service endpoints, not
// crash-looping.
func TestStartup_AbsentSchema_HealthzUpReadyzUnready(t *testing.T) {
	const reason = "schema not yet provisioned: table otel_logs absent"
	signal := newSchemaPresentSignal(reason)

	healthHandler := health.New(health.Options{
		Pinger:        newHealthyPinger(),
		SchemaPresent: signal.Func(),
		CacheTTL:      -1,
	})
	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
	srv := httptest.NewServer(rootMux)
	t.Cleanup(srv.Close)

	if got := getStatus(t, srv.URL+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz with absent schema = %d; want 200 (liveness must not gate on schema)", got)
	}
	if got := getStatus(t, srv.URL+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz with absent schema = %d; want 503 (boot-but-wait)", got)
	}
}

// TestStartup_AbsentSchema_FlipsReadyOncePresent pins the recovery half:
// once an external writer provisions the schema, markPresent flips the
// carrier and /readyz returns 200 — no restart.
func TestStartup_AbsentSchema_FlipsReadyOncePresent(t *testing.T) {
	signal := newSchemaPresentSignal("schema not yet provisioned: table otel_logs absent")

	healthHandler := health.New(health.Options{
		Pinger:        newHealthyPinger(),
		SchemaPresent: signal.Func(),
		CacheTTL:      -1,
	})
	rootMux := http.NewServeMux()
	healthHandler.Mount(rootMux)
	srv := httptest.NewServer(rootMux)
	t.Cleanup(srv.Close)

	if got := getStatus(t, srv.URL+"/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before provisioning = %d; want 503", got)
	}

	// External writer (collector / auto-create) provisions the schema.
	signal.markPresent()

	if got := getStatus(t, srv.URL+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz after provisioning = %d; want 200 (flips ready, no restart)", got)
	}
}

// newHealthyPinger returns a flipPinger preset to report ClickHouse
// reachable, so absent-schema tests isolate the schema gate from the CH
// ping.
func newHealthyPinger() *flipPinger {
	p := &flipPinger{}
	p.healthy.Store(true)
	return p
}

// getStatus GETs url and returns the status code.
func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
