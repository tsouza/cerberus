package autotune

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope for the autotune gauges.
const meterName = "github.com/tsouza/cerberus/internal/autotune"

// RegisterMetrics registers observable gauges that publish the self-driving
// loop's decision state on every collection interval, read from the Reporter.
// Each series is stamped per-pod by the OTel resource's service.instance.id, so
// the fleet + historical view is a CH/PromQL aggregation over pods.
//
// Aggregation note for dashboards: the corpus-derived series (route_a_ooms,
// route_b_executions, route_b_ooms, oom_min_fanout, and the live thresholds) are
// computed from the SHARED corpus, so every pod reports ~the same value — take
// max/avg, never sum (summing N-inflates by pod count). The per-pod process
// series (applied_total, errors_total) are genuinely per-pod and may be summed.
//
// Autotune is prom-head-scoped (the solver is built only for the prom head), so
// wherever these gauges emit, that pod also serves PromQL — there is no pod that
// tunes but cannot be queried. Emission itself rides the OTLP pipeline, so it is
// independent of any head being enabled.
//
// A nil reporter is a no-op. It returns an error only on a misconfigured
// MeterProvider; the caller logs and continues (introspection is best-effort and
// never on the query path).
func RegisterMetrics(reporter *Reporter) error {
	if reporter == nil {
		return nil
	}
	meter := otel.GetMeterProvider().Meter(meterName)

	gauge := func(name, desc, unit string) (metric.Int64ObservableGauge, error) {
		return meter.Int64ObservableGauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
	}

	active, err := gauge("cerberus_solver_autotune_active",
		"1 when the self-driving loop is running on this pod (enabled, auto mode, corpus available), else 0.", "{state}")
	if err != nil {
		return err
	}
	minFanout, err := gauge("cerberus_solver_autotune_min_fanout",
		"Live MinFanout auto-gate the Planner is routing with right now.", "{anchor}")
	if err != nil {
		return err
	}
	minAnchorPairs, err := gauge("cerberus_solver_autotune_min_anchor_pairs",
		"Live MinAnchorPairs auto-gate the Planner is routing with right now.", "{pair}")
	if err != nil {
		return err
	}
	configuredMinFanout, err := gauge("cerberus_solver_autotune_configured_min_fanout",
		"Shipped (configured) MinFanout; live minus this is how far the loop has lowered the gate.", "{anchor}")
	if err != nil {
		return err
	}
	appliedTicks, err := gauge("cerberus_solver_autotune_applied_total",
		"Cumulative ticks that lowered the gate on this pod.", "{tick}")
	if err != nil {
		return err
	}
	errorTicks, err := gauge("cerberus_solver_autotune_errors_total",
		"Cumulative ticks whose corpus read failed on this pod.", "{tick}")
	if err != nil {
		return err
	}
	routeAOoms, err := gauge("cerberus_solver_autotune_route_a_ooms",
		"Rolling-window route-A below-threshold OOMs (corpus-derived); good = trending to 0.", "{query}")
	if err != nil {
		return err
	}
	routeBExecs, err := gauge("cerberus_solver_autotune_route_b_executions",
		"Rolling-window route-B executions — volume routed to the safe path (corpus-derived).", "{query}")
	if err != nil {
		return err
	}
	routeBOoms, err := gauge("cerberus_solver_autotune_route_b_ooms",
		"Rolling-window route-B OOMs — the mitigation itself failing; should stay 0 (corpus-derived).", "{query}")
	if err != nil {
		return err
	}
	oomFloorFanout, err := gauge("cerberus_solver_autotune_oom_min_fanout",
		"Observed route-A OOM floor fan-out the loop is tracking (corpus-derived).", "{anchor}")
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			s := reporter.Snapshot()
			o.ObserveInt64(active, boolToInt64(s.Active))
			o.ObserveInt64(minFanout, int64(s.Live.MinFanout))
			o.ObserveInt64(minAnchorPairs, int64(s.Live.MinAnchorPairs))
			o.ObserveInt64(configuredMinFanout, int64(s.Configured.MinFanout))
			o.ObserveInt64(appliedTicks, s.Stats.AppliedTicks)
			o.ObserveInt64(errorTicks, s.Stats.ErrorTicks)
			o.ObserveInt64(routeAOoms, s.Outcome.RouteAOomCount)
			o.ObserveInt64(routeBExecs, s.Outcome.RouteBExecutions)
			o.ObserveInt64(routeBOoms, s.Outcome.RouteBOomCount)
			o.ObserveInt64(oomFloorFanout, int64(s.Outcome.OOMMinFanout))
			return nil
		},
		active, minFanout, minAnchorPairs, configuredMinFanout,
		appliedTicks, errorTicks, routeAOoms, routeBExecs, routeBOoms, oomFloorFanout,
	)
	return err
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
