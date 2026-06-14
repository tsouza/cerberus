package chclient

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// breakerMeterName is the instrumentation-scope identifier stamped on the
// circuit-breaker telemetry. Distinct from the execute-span tracer scope so
// dashboards can pivot on the breaker-specific scope when drilling into
// trip events.
const breakerMeterName = "github.com/tsouza/cerberus/internal/chclient"

// attrBreakerState labels the gauge with the lifecycle phase the gauge sample
// corresponds to — "closed" / "open" / "half-open" — using the same stable
// vocabulary peek() / currentState() return.
const attrBreakerState = attribute.Key("state")

// breakerState gauge values. The gauge is a single 0/1/2 level so a dashboard
// can render the current phase as a state-timeline without needing three
// separate boolean series; the numeric mapping matches the breakerState iota
// order (closed=0, open=1, half-open=2) so the gauge value and the internal
// enum never drift.
const (
	breakerGaugeClosed   = 0
	breakerGaugeOpen     = 1
	breakerGaugeHalfOpen = 2
)

// breakerMetrics holds the OTel instruments a breaker fires on every state
// transition. A nil *breakerMetrics is the "no telemetry" sentinel: the
// zero-value breaker (and any breaker built without a MeterProvider) carries
// nil and the fire helpers are no-ops, so the un-instrumented hot path pays
// nothing. The production breaker, built in client.New, always carries a
// non-nil set wired to the global MeterProvider.
type breakerMetrics struct {
	// state is a 0/1/2 gauge of the current lifecycle phase
	// (closed/open/half-open). Recorded on every transition so a
	// dashboard state-timeline tracks the breaker live.
	state metric.Int64Gauge
	// trips is the cumulative count of CLOSED->OPEN trips — the
	// highest-blast-radius event (one trip 503s all three heads and
	// flips /readyz). Monotonic counter so `increase(...[5m])` resolves
	// to the number of outages in the window.
	trips metric.Int64Counter
}

// newBreakerMetrics builds the breaker instrument set off mp and
// zero-initialises both streams so the dashboard renders a flat 0 — not
// "No data" — on a healthy replica whose breaker never trips.
//
// The zero-init mirrors internal/api/admit/admit.go's rejected_total
// pre-registration: OTel synchronous instruments export NOTHING until their
// first record/Add, so without seeding the trips counter and the closed-state
// gauge at construction, a replica that stays CLOSED forever exports no
// breaker stream at all and the "ClickHouse circuit breaker" panel shows
// "No data" instead of a reassuring flat 0 / closed. Pre-registering at
// construction is the standard Prometheus practice for series whose label set
// is known in advance: the stream exists from process start, so rate() /
// the state-timeline resolve immediately.
func newBreakerMetrics(mp metric.MeterProvider) *breakerMetrics {
	meter := mp.Meter(breakerMeterName)
	state, err := meter.Int64Gauge(
		"cerberus_ch_breaker_state",
		metric.WithDescription(
			"ClickHouse circuit-breaker lifecycle phase: "+
				"0=closed, 1=open, 2=half-open. One trip OPEN fast-fails "+
				"(503) every promql/logql/traceql query and flips /readyz.",
		),
		metric.WithUnit("{state}"),
	)
	if err != nil {
		// Instrument validation only fails on a misconfigured
		// MeterProvider; surface loudly rather than silently dropping
		// the breaker's only time-series.
		panic("chclient: build breaker state gauge: " + err.Error())
	}
	trips, err := meter.Int64Counter(
		"cerberus_ch_breaker_trips_total",
		metric.WithDescription(
			"Cumulative CLOSED->OPEN trips of the ClickHouse circuit "+
				"breaker. Each trip fast-fails (503) every "+
				"promql/logql/traceql query and flips /readyz to unready "+
				"until the breaker recovers.",
		),
		metric.WithUnit("{trip}"),
	)
	if err != nil {
		panic("chclient: build breaker trips counter: " + err.Error())
	}
	m := &breakerMetrics{state: state, trips: trips}
	// Zero-init: seed the trips counter (so increase() has a baseline) and
	// the gauge at the closed level (so the state-timeline starts CLOSED,
	// not blank). Both records happen before any transition fires.
	m.trips.Add(context.Background(), 0)
	m.state.Record(context.Background(), breakerGaugeClosed, metric.WithAttributes(
		attrBreakerState.String("closed"),
	))
	return m
}

// recordState fires the state gauge for the phase the breaker just entered.
// A nil receiver is the no-telemetry no-op for the zero-value breaker.
func (m *breakerMetrics) recordState(level int64, label string) {
	if m == nil {
		return
	}
	m.state.Record(context.Background(), level, metric.WithAttributes(
		attrBreakerState.String(label),
	))
}

// recordTrip increments the CLOSED->OPEN trip counter. A nil receiver is the
// no-telemetry no-op for the zero-value breaker.
func (m *breakerMetrics) recordTrip() {
	if m == nil {
		return
	}
	m.trips.Add(context.Background(), 1)
}

// breakerLogger is the slog handle WARN transition logs flow through. Kept as
// a package var (defaulting to the global default logger) so tests can swap in
// a capturing handler without a logger plumbed through every breaker.
var breakerLogger = func() *slog.Logger { return slog.Default() }

// newGlobalBreakerMetrics builds the breaker instrument set off the OTel
// global MeterProvider. client.New calls this so the production breaker's
// telemetry flows to the configured OTLP exporter once cmd/cerberus installs
// the cerberus telemetry provider.
func newGlobalBreakerMetrics() *breakerMetrics {
	return newBreakerMetrics(otel.GetMeterProvider())
}
