package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recorder captures the shutdown sequence as it happens, so a test can assert
// both that a step ran and where in the chain it ran.
type recorder struct {
	steps        []string
	gracefulGRPC bool
}

func (r *recorder) steps3(httpErr, otlpErr error) shutdownSteps {
	return shutdownSteps{
		drainHTTP: func(context.Context) error {
			r.steps = append(r.steps, "http")
			return httpErr
		},
		stopGRPC: func(graceful bool) {
			r.steps = append(r.steps, "grpc")
			r.gracefulGRPC = graceful
		},
		flushOTLP: func(context.Context) error {
			r.steps = append(r.steps, "otlp")
			return otlpErr
		},
	}
}

// The regression this file exists for: run() used to `return` on the HTTP
// drain error, so a shutdown that blew its deadline exited with the gRPC
// listener still holding its sockets and every pending OTLP batch — including
// the telemetry describing that very failure — discarded unsent.
func TestShutdown_RunsTheTailWhenTheHTTPDrainFails(t *testing.T) {
	drainErr := errors.New("deadline exceeded draining connections")
	r := &recorder{}

	err := shutdown(context.Background(), r.steps3(drainErr, nil), discardLogger())

	if !errors.Is(err, drainErr) {
		t.Fatalf("shutdown err = %v, want it to wrap %v", err, drainErr)
	}
	if !strings.Contains(err.Error(), "graceful shutdown") {
		t.Errorf("shutdown err = %q, want it to name the failing stage", err)
	}
	want := []string{"http", "grpc", "otlp"}
	if got := strings.Join(r.steps, ","); got != strings.Join(want, ",") {
		t.Fatalf("steps run = [%s], want [%s] — the tail must run even when the drain fails", got, strings.Join(want, ","))
	}
}

// GracefulStop takes no context. Once the HTTP drain has spent the shutdown
// deadline there is nothing left to bound it, so the gRPC stop must be the
// forceful one instead of inheriting the hang.
func TestShutdown_ForcesTheGRPCStopAfterAFailedDrain(t *testing.T) {
	r := &recorder{}
	_ = shutdown(context.Background(), r.steps3(errors.New("boom"), nil), discardLogger())
	if r.gracefulGRPC {
		t.Error("gRPC stop was graceful after the HTTP drain failed; the shutdown deadline is already spent")
	}
}

func TestShutdown_HappyPathDrainsGracefullyInOrder(t *testing.T) {
	r := &recorder{}

	if err := shutdown(context.Background(), r.steps3(nil, nil), discardLogger()); err != nil {
		t.Fatalf("shutdown err = %v, want nil", err)
	}
	if got, want := strings.Join(r.steps, ","), "http,grpc,otlp"; got != want {
		t.Fatalf("steps run = [%s], want [%s]", got, want)
	}
	if !r.gracefulGRPC {
		t.Error("gRPC stop was forceful on a clean HTTP drain; in-flight streams must be allowed to finish")
	}
}

// A telemetry flush that errors is reported but is not the process's exit
// status: the serving path already stopped cleanly, and failing the exit on an
// unreachable collector would turn an observability outage into a crash.
func TestShutdown_TelemetryFlushErrorIsNotFatal(t *testing.T) {
	r := &recorder{}
	if err := shutdown(context.Background(), r.steps3(nil, errors.New("collector unreachable")), discardLogger()); err != nil {
		t.Fatalf("shutdown err = %v, want nil", err)
	}
}

// The tempo head can be disabled, in which case no gRPC server was ever built.
func TestShutdown_SkipsTheGRPCStopWhenNoServerWasBuilt(t *testing.T) {
	var flushed bool
	steps := shutdownSteps{
		drainHTTP: func(context.Context) error { return nil },
		flushOTLP: func(context.Context) error { flushed = true; return nil },
	}
	if err := shutdown(context.Background(), steps, discardLogger()); err != nil {
		t.Fatalf("shutdown err = %v, want nil", err)
	}
	if !flushed {
		t.Error("telemetry was not flushed when the tempo head is disabled")
	}
}
