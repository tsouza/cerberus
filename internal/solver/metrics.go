package solver

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope for the solver's counters.
const meterName = "github.com/tsouza/cerberus/internal/solver"

// metrics holds the solver's lazily-built OTel instruments. They are
// constructed off the global MeterProvider on first use (mirroring
// internal/api/admit's pattern) so wiring order between telemetry init and
// solver construction never drops a counter. The build is once-guarded.
type solverMetrics struct {
	parallelismClamped metric.Int64Counter
}

var (
	metricsOnce sync.Once
	metricsInst *solverMetrics
)

// getMetrics returns the process-wide solver metric instruments, building
// them on first call. A build failure (only possible on a misconfigured
// provider) yields a metrics struct with nil instruments; the record
// helpers nil-check, so telemetry degrades to a no-op rather than panicking
// the data path.
func getMetrics() *solverMetrics {
	metricsOnce.Do(func() {
		meter := otel.GetMeterProvider().Meter(meterName)
		m := &solverMetrics{}
		if c, err := meter.Int64Counter(
			"cerberus_solver_parallelism_clamped_total",
			metric.WithDescription(
				"Routed requests whose effective shard parallelism was clamped "+
					"below the configured P because the two-stage admit top-up "+
					"could not grant all (P-1) extra units. The request still "+
					"succeeds — only latency differs (degrade, never reject).",
			),
			metric.WithUnit("{request}"),
		); err == nil {
			m.parallelismClamped = c
		}
		metricsInst = m
	})
	return metricsInst
}

// recordParallelismClamped bumps the clamp counter by one. Safe to call
// with a nil instrument (no-op) so a metrics-build failure never breaks the
// routed path.
func recordParallelismClamped(ctx context.Context) {
	m := getMetrics()
	if m == nil || m.parallelismClamped == nil {
		return
	}
	m.parallelismClamped.Add(ctx, 1)
}
