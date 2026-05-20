package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Server-lifecycle tests live alongside otel.go in cmd/cerberus.
//
// run() in main.go is not directly callable here (it owns os.Stderr, a
// real config.FromEnv read, and a CH client built off env vars). The
// behaviors we *can* exercise are the building blocks the lifecycle is
// composed of: signal.NotifyContext fan-out, http.Server.Shutdown
// semantics under context expiry, the in-flight-request drain, and the
// goroutine accounting around them. Those building blocks ARE the
// lifecycle — if they hold, run() composes them and inherits the
// behavior.
//
// If you find yourself wanting to test run() directly here, the
// architectural fix is to split run() into a Process struct with
// injectable Pinger / Shutdowner deps. That's out-of-scope for this
// PR (Layer 8 is tests-only); see the PR body for the seam list.

// TestSignalNotifyContext_TerminatesOnSIGTERM verifies the OS-signal
// fan-out that main.go uses: signal.NotifyContext(SIGINT, SIGTERM)
// produces a Done context whose Err() is context.Canceled. The
// underlying behavior is the load-bearing primitive for graceful
// shutdown — main blocks on this context and starts srv.Shutdown when
// it fires.
func TestSignalNotifyContext_TerminatesOnSIGTERM(t *testing.T) {
	ctx, stop := signalNotifyContext()
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("ctx.Err() = %v; want context.Canceled", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not cancel context within deadline")
	}
}

// TestSignalNotifyContext_TerminatesOnSIGINT mirrors the SIGTERM path
// for the SIGINT signal (Ctrl+C in an interactive shell). Both should
// take the exact same shutdown path.
func TestSignalNotifyContext_TerminatesOnSIGINT(t *testing.T) {
	ctx, stop := signalNotifyContext()
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	select {
	case <-ctx.Done():
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not cancel context within deadline")
	}
}

// TestSignalNotifyContext_StopReleasesHandler confirms the stop()
// callback unhooks the signal handler so a second test in the same
// process doesn't see stray cancellations from the first test's
// signal.
func TestSignalNotifyContext_StopReleasesHandler(t *testing.T) {
	ctx, stop := signalNotifyContext()
	stop()
	// The context is cancelled after stop().
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Error("stop() did not cancel the context")
	}
}

// TestHTTPServer_GracefulShutdown_DrainsInflightRequest models the
// "SIGTERM during in-flight query" case. We start an httptest server
// whose handler blocks until the test releases it, fire a client
// request, then call srv.Shutdown. Shutdown must wait for the
// in-flight handler to return before completing.
func TestHTTPServer_GracefulShutdown_DrainsInflightRequest(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	var entered atomic.Bool
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		entered.Store(true)
		<-release // wait for the test to release us
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	// Fire the slow request.
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			clientDone <- err
			return
		}
		_ = resp.Body.Close()
		clientDone <- nil
	}()

	// Wait until the handler is actually executing — otherwise we'd
	// race the Shutdown.
	for !entered.Load() {
		time.Sleep(time.Millisecond)
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(shutdownCtx)
	}()

	// Shutdown must not complete while the handler is still blocked.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before handler completed: %v", err)
	case <-time.After(100 * time.Millisecond):
		// expected — still waiting
	}

	close(release) // let handler finish

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after handler released")
	}
	if err := <-clientDone; err != nil {
		t.Errorf("client: %v", err)
	}
}

// TestHTTPServer_GracefulShutdown_ContextDeadlineExceededForcesClose:
// when the shutdown context expires before the handler returns,
// Shutdown surfaces context.DeadlineExceeded. main.go propagates this
// as "graceful shutdown: ..." so the operator sees the deadline.
func TestHTTPServer_GracefulShutdown_ContextDeadlineExceededForcesClose(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-release:
			w.WriteHeader(http.StatusOK)
		}
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	// Fire one stuck request.
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// Now Shutdown with a very tight deadline — the handler is stuck,
	// so Shutdown must return DeadlineExceeded.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown err = %v; want context.DeadlineExceeded", err)
	}
}

// TestHTTPServer_HealthRoutesStillRespondPreShutdown documents the
// happy-path: /healthz returns 200 before any shutdown fires. Anchor
// for the "/healthz returns 503 during shutdown" downstream story —
// this is the baseline.
func TestHTTPServer_HealthRoutesStillRespondPreShutdown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

// TestHTTPServer_ListenAndServeErrServerClosed: after a normal shutdown,
// ListenAndServe must return http.ErrServerClosed. main.go relies on
// errors.Is(err, http.ErrServerClosed) to distinguish a planned
// shutdown from a real failure.
func TestHTTPServer_ListenAndServeErrServerClosed(t *testing.T) {
	srv := &http.Server{
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	// Give the goroutine a tick to enter Serve.
	time.Sleep(50 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case got := <-serveErr:
		if !errors.Is(got, http.ErrServerClosed) {
			t.Errorf("Serve err = %v; want http.ErrServerClosed", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

// TestHTTPServer_Shutdown_BlocksNewConnections: after Shutdown the
// underlying listener is closed — Accept must return net.ErrClosed.
// Pin the contract that distinguishes Shutdown from a keep-alive
// drain. (We intentionally do NOT dial the freed port: on busy CI
// runners the kernel can hand the same ephemeral port to an unrelated
// listener between Shutdown and the dial, and the test would flake on
// an OS-level race that has nothing to do with our shutdown contract.
// Accept() returning ErrClosed is the load-bearing signal.)
func TestHTTPServer_Shutdown_BlocksNewConnections(t *testing.T) {
	srv := &http.Server{
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	// Wrap the listener so we can observe Accept returns after Shutdown
	// closes it. http.Server calls ln.Close() inside Shutdown.
	acceptErr := make(chan error, 1)
	wrapped := &acceptRecorder{Listener: ln, errCh: acceptErr}
	go func() { _ = srv.Serve(wrapped) }()

	// Sanity-check the listener is up.
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial before shutdown: %v", err)
	}
	_ = conn.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After Shutdown the listener's Accept loop must exit with
	// net.ErrClosed — that's the kernel-level signal that no new
	// connection can be served by this server.
	select {
	case got := <-acceptErr:
		if !errors.Is(got, net.ErrClosed) {
			t.Errorf("Accept after Shutdown returned %v; want net.ErrClosed", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after Shutdown")
	}
}

// acceptRecorder wraps a net.Listener and forwards the first non-nil
// Accept error to errCh. http.Server.Shutdown calls ln.Close, which
// makes the in-flight Accept return net.ErrClosed.
type acceptRecorder struct {
	net.Listener
	errCh chan<- error
	once  sync.Once
}

func (a *acceptRecorder) Accept() (net.Conn, error) {
	c, err := a.Listener.Accept()
	if err != nil {
		a.once.Do(func() { a.errCh <- err })
	}
	return c, err
}

// TestHTTPServer_GoroutineDeltaWithinBound is a lightweight goroutine-leak
// guard for the listener path. We capture a baseline, start + shutdown a
// server with one in-flight handler, and confirm the goroutine count
// returns near the baseline. Not a substitute for goleak — but cheap
// and dependency-free.
func TestHTTPServer_GoroutineDeltaWithinBound(t *testing.T) {
	// Stabilise the runtime — let any GC sweeper goroutines settle.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		mux := http.NewServeMux()
		mux.HandleFunc("/echo", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: time.Second,
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		serveErr := make(chan error, 1)
		go func() { serveErr <- srv.Serve(ln) }()

		resp, err := http.Get("http://" + ln.Addr().String() + "/echo")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		cancel()
		<-serveErr
	}

	// After three cycles, allow for some scheduling slack. A small
	// constant delta is fine (the test runtime itself spawns watchers);
	// we just guard against linear growth proportional to the loop count.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - baseline; delta > 8 {
		t.Errorf("goroutine delta = %d; baseline=%d after=%d (suspicious leak)", delta, baseline, after)
	}
}

// TestHTTPServer_ConcurrentRequestsDuringShutdown ensures the drain
// path handles multiple in-flight requests simultaneously. main.go's
// 10s shutdown deadline must accommodate all of them.
func TestHTTPServer_ConcurrentRequestsDuringShutdown(t *testing.T) {
	release := make(chan struct{})
	var inflight atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	const N = 5
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	// Wait for every request to enter the handler before triggering
	// shutdown — otherwise we'd race the goroutine spawning.
	for inflight.Load() < int32(N) {
		time.Sleep(time.Millisecond)
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	// Shutdown must wait for every handler to finish.
	close(release)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked > 5s with all handlers released")
	}
	wg.Wait()
}

// TestHTTPServer_ShutdownNeverPanicsOnDoubleClose: a second srv.Close
// after a clean Shutdown must not panic. main.go's defer client.Close
// + Shutdown ordering produces this pattern under panic-recovery.
func TestHTTPServer_ShutdownNeverPanicsOnDoubleClose(t *testing.T) {
	srv := &http.Server{
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Calling Close on an already-shut-down server must be harmless.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Close panicked: %v", r)
		}
	}()
	_ = srv.Close()
}

// signalNotifyContext mirrors the exact construction main.go uses to
// produce its shutdown context. Sharing the helper across lifecycle
// tests keeps the construction in one place and lets the production
// code stay agnostic about test wiring.
//
// Hardcodes the (SIGINT, SIGTERM) pair main.go listens for. A runtime
// helper exported from main.go would violate the no-prod-changes rule.
func signalNotifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
