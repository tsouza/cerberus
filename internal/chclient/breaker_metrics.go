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

// attrBreakerHead labels every breaker sample with the logical head the
// breaker fronts — "prom" / "loki" / "tempo" / "probe". Adding this label
// (per-head isolation, #94) turns the single cerberus_ch_breaker_state /
// _trips_total stream into one series per head, so a `sum by(head)` panel
// shows exactly which head tripped. It is a non-breaking addition for
// bare-metric / `sum()`-style panels that don't pivot on it.
const attrBreakerHead = attribute.Key("head")

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
	// not blank) — for EVERY head (#94). OTel sync instruments export
	// nothing until their first record/Add, so without a per-head seed a
	// healthy replica whose loki breaker never trips would export NO
	// head="loki" series at all and a `sum by(head)` panel would silently
	// miss the healthy heads. Seeding all four at construction makes every
	// head's stream exist from process start. Both records happen before any
	// transition fires.
	for _, h := range allHeads {
		m.trips.Add(context.Background(), 0, metric.WithAttributes(
			attrBreakerHead.String(h.String()),
		))
		m.state.Record(context.Background(), breakerGaugeClosed, metric.WithAttributes(
			attrBreakerHead.String(h.String()),
			attrBreakerState.String("closed"),
		))
	}
	return m
}

// recordState fires the state gauge for the phase the breaker (fronting head)
// just entered. A nil receiver is the no-telemetry no-op for the zero-value
// breaker.
func (m *breakerMetrics) recordState(head Head, level int64, label string) {
	if m == nil {
		return
	}
	m.state.Record(context.Background(), level, metric.WithAttributes(
		attrBreakerHead.String(head.String()),
		attrBreakerState.String(label),
	))
}

// recordTrip increments the CLOSED->OPEN trip counter for head. A nil receiver
// is the no-telemetry no-op for the zero-value breaker. Increment is on the
// CLOSED->OPEN edge ONLY (not the HALF-OPEN->OPEN re-open), so rate() over the
// counter reads as "new outages/sec per head".
func (m *breakerMetrics) recordTrip(head Head) {
	if m == nil {
		return
	}
	m.trips.Add(context.Background(), 1, metric.WithAttributes(
		attrBreakerHead.String(head.String()),
	))
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
