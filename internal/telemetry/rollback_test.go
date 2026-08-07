package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// recorder stands in for one provider constructor. It records whether the
// shutdown it handed back was called, which is how the tests below tell a
// real rollback from a silently skipped one.
type recorder struct {
	// shutdownErr is returned by the shutdown closure, modelling a
	// teardown that itself fails.
	shutdownErr error

	shutdownCalls int
}

func (r *recorder) shutdown(context.Context) error {
	r.shutdownCalls++
	return r.shutdownErr
}

// builders assembles a providerBuilders whose stages succeed until
// failAt, which fails with failErr. failAt is one of "tracer", "meter",
// "logger", or "" for an all-succeeding set.
func builders(tracer, meter, logger *recorder, failAt string, failErr error) providerBuilders {
	stage := func(name string) error {
		if name == failAt {
			return failErr
		}
		return nil
	}
	return providerBuilders{
		tracer: func(context.Context, Config, *resource.Resource) (trace.TracerProvider, shutdownFunc, error) {
			if err := stage("tracer"); err != nil {
				return nil, nil, err
			}
			return tracenoop.NewTracerProvider(), tracer.shutdown, nil
		},
		meter: func(context.Context, Config, *resource.Resource) (metric.MeterProvider, shutdownFunc, error) {
			if err := stage("meter"); err != nil {
				return nil, nil, err
			}
			return metricnoop.NewMeterProvider(), meter.shutdown, nil
		},
		logger: func(context.Context, Config, *resource.Resource) (otellog.LoggerProvider, shutdownFunc, error) {
			if err := stage("logger"); err != nil {
				return nil, nil, err
			}
			return lognoop.NewLoggerProvider(), logger.shutdown, nil
		},
	}
}

// TestNewProviders_RollsBackEarlierStages pins the rollback half of the
// construction contract: when one stage fails, every stage built before
// it is torn down exactly once, and no stage after it is built at all.
func TestNewProviders_RollsBackEarlierStages(t *testing.T) {
	t.Parallel()

	boom := errors.New("dial refused")

	cases := []struct {
		failAt string
		// prefix is the wrapper newProviders puts on the stage error.
		prefix       string
		wantTracer   int
		wantMeter    int
		wantLogger   int
		wantShutdown bool
	}{
		{failAt: "tracer", prefix: "trace exporter: "},
		{failAt: "meter", prefix: "metric exporter: ", wantTracer: 1},
		{failAt: "logger", prefix: "log exporter: ", wantTracer: 1, wantMeter: 1},
	}

	for _, tc := range cases {
		t.Run(tc.failAt, func(t *testing.T) {
			t.Parallel()

			tracer, meter, logger := &recorder{}, &recorder{}, &recorder{}
			p, err := newProviders(t.Context(), Config{Endpoint: "collector:4317"}, resource.Empty(),
				builders(tracer, meter, logger, tc.failAt, boom))
			if p != nil {
				t.Fatalf("providers returned alongside an error: %#v", p)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("error does not wrap the stage failure: %v", err)
			}
			if want := tc.prefix + boom.Error(); err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if tracer.shutdownCalls != tc.wantTracer {
				t.Errorf("tracer shutdown called %d times, want %d", tracer.shutdownCalls, tc.wantTracer)
			}
			if meter.shutdownCalls != tc.wantMeter {
				t.Errorf("meter shutdown called %d times, want %d", meter.shutdownCalls, tc.wantMeter)
			}
			// The logger stage is the last one, so its shutdown is never
			// a rollback target — a call here means a provider was built
			// after the failure.
			if logger.shutdownCalls != tc.wantLogger {
				t.Errorf("logger shutdown called %d times, want %d", logger.shutdownCalls, tc.wantLogger)
			}
		})
	}
}

// TestNewProviders_RollbackFailureSurfaces is the assertion the discarded
// `_ = traceShutdown(ctx)` form could not carry: when the rollback itself
// fails, the goroutine it was meant to stop is still running, so that
// failure has to reach the caller beside the error that triggered it.
//
// Every failing stage that has a stage before it gets its own case. The
// call-count assertions in TestNewProviders_RollsBackEarlierStages cannot
// stand in for these: the discarded form still calls each shutdown, so
// only a rollback that fails distinguishes the two.
func TestNewProviders_RollbackFailureSurfaces(t *testing.T) {
	t.Parallel()

	boom := errors.New("dial refused")
	traceStuck := errors.New("trace batcher still draining")
	metricStuck := errors.New("metric reader still draining")

	cases := []struct {
		failAt string
		// wantWrapped are the errors the returned error must carry: the
		// cause plus one per rolled-back stage whose teardown failed.
		wantWrapped []error
	}{
		{failAt: "meter", wantWrapped: []error{boom, traceStuck}},
		{failAt: "logger", wantWrapped: []error{boom, traceStuck, metricStuck}},
	}

	for _, tc := range cases {
		t.Run(tc.failAt, func(t *testing.T) {
			t.Parallel()

			tracer := &recorder{shutdownErr: traceStuck}
			meter := &recorder{shutdownErr: metricStuck}
			logger := &recorder{}

			_, err := newProviders(t.Context(), Config{Endpoint: "collector:4317"}, resource.Empty(),
				builders(tracer, meter, logger, tc.failAt, boom))
			if err == nil {
				t.Fatal("newProviders returned no error")
			}
			for _, want := range tc.wantWrapped {
				if !errors.Is(err, want) {
					t.Errorf("error %v does not wrap %v", err, want)
				}
			}
			// Each rollback failure is labelled as such, so an operator
			// reading the startup log can tell them from the cause.
			wantRollbacks := len(tc.wantWrapped) - 1
			if got := strings.Count(err.Error(), "rollback: "); got != wantRollbacks {
				t.Errorf("error %q labels %d rollback failures, want %d", err, got, wantRollbacks)
			}
		})
	}
}

// TestNewProviders_SuccessKeepsProvidersLive guards the other direction:
// a fully successful build must not tear anything down, and its Shutdown
// must reach all three providers.
func TestNewProviders_SuccessKeepsProvidersLive(t *testing.T) {
	t.Parallel()

	tracer, meter, logger := &recorder{}, &recorder{}, &recorder{}

	p, err := newProviders(t.Context(), Config{Endpoint: "collector:4317"}, resource.Empty(),
		builders(tracer, meter, logger, "", nil))
	if err != nil {
		t.Fatalf("newProviders: %v", err)
	}
	if tracer.shutdownCalls+meter.shutdownCalls+logger.shutdownCalls != 0 {
		t.Fatalf("a successful build tore providers down: tracer=%d meter=%d logger=%d",
			tracer.shutdownCalls, meter.shutdownCalls, logger.shutdownCalls)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if tracer.shutdownCalls != 1 || meter.shutdownCalls != 1 || logger.shutdownCalls != 1 {
		t.Errorf("Shutdown reached tracer=%d meter=%d logger=%d, want 1 each",
			tracer.shutdownCalls, meter.shutdownCalls, logger.shutdownCalls)
	}
}
