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

// breakerMetrics holds the OTel instruments the per-head breakers report
// through. A nil *breakerMetrics is the "no telemetry" sentinel: the
// zero-value breaker (and any breaker built without a MeterProvider) carries
// nil and the fire helpers are no-ops, so the un-instrumented hot path pays
// nothing. The production breaker, built in client.New, always carries a
// non-nil set wired to the global MeterProvider.
type breakerMetrics struct {
	// meter is retained so registerStateCallback can register the
	// observable-gauge callback AFTER buildBreakers has minted the live
	// per-head breakers the callback reads (the breakers don't exist yet at
	// newBreakerMetrics time).
	meter metric.Meter
	// state is a 0/1/2 observable gauge of each head breaker's CURRENT
	// lifecycle phase (closed/open/half-open). It is reported by a callback
	// on every collection interval, reading each live breaker's current
	// state, so the exported series always reflects the breaker's real state
	// and can NEVER go stale. Two distinct staleness traps are avoided:
	//
	//   1. A SYNCHRONOUS gauge recorded only on transitions lingers at the
	//      last-transitioned value (e.g. a transient HALF-OPEN) long after the
	//      breaker has closed. The OBSERVABLE callback re-reads live state
	//      every interval, so it can't.
	//   2. Keying the phase as a metric LABEL (state="open" / "closed") makes
	//      every transition mint a NEW series and ORPHAN the previous one. An
	//      OTel observable gauge only overwrites series it re-observes, so the
	//      orphaned series lingers at its last value over OTLP forever — a
	//      recovered breaker would still export a stale state="open"=1 next to
	//      the live state="closed"=0, and a max()-across-series read (the chaos
	//      lane's recovery assert) would read the breaker as permanently OPEN.
	//      So the phase rides ONLY the numeric level; the gauge is keyed on
	//      `head` alone — one series per head, overwritten in place.
	state metric.Int64ObservableGauge
	// trips is the cumulative count of CLOSED->OPEN trips — the
	// highest-blast-radius event (one trip 503s all three heads and
	// flips /readyz). Monotonic counter so `increase(...[5m])` resolves
	// to the number of outages in the window.
	trips metric.Int64Counter
}

// newBreakerMetrics builds the breaker instrument set off mp. The trips
// counter is zero-initialised per head so the dashboard renders a flat 0 — not
// "No data" — on a healthy replica whose breaker never trips. The state gauge
// is OBSERVABLE: it needs no zero-init because its callback (registered in
// registerStateCallback once the breakers exist) reports every head's current
// level on every collection interval, so the stream exists from the first
// collection regardless of whether a transition ever fired.
//
// The trips zero-init mirrors internal/api/admit/admit.go's rejected_total
// pre-registration: OTel synchronous counters export NOTHING until their first
// Add, so without seeding the trips counter at construction, a replica that
// stays CLOSED forever exports no trips stream at all and a rate() panel shows
// "No data" instead of a reassuring flat 0. Pre-registering at construction is
// the standard Prometheus practice for series whose label set is known in
// advance.
func newBreakerMetrics(mp metric.MeterProvider) *breakerMetrics {
	meter := mp.Meter(breakerMeterName)
	state, err := meter.Int64ObservableGauge(
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
	m := &breakerMetrics{meter: meter, state: state, trips: trips}
	// Zero-init the trips counter so increase() has a baseline — for EVERY
	// head (#94). OTel sync counters export nothing until their first Add, so
	// without a per-head seed a healthy replica whose loki breaker never trips
	// would export NO head="loki" trips series at all and a `sum by(head)`
	// panel would silently miss the healthy heads. The state gauge needs no
	// such seed: its observable callback reports every head every interval.
	for _, h := range allHeads {
		m.trips.Add(context.Background(), 0, metric.WithAttributes(
			attrBreakerHead.String(h.String()),
		))
	}
	return m
}

// registerStateCallback wires the observable-gauge callback that reports each
// breaker's CURRENT lifecycle phase on every collection interval. It is called
// by buildBreakers once the live per-head breakers exist (they post-date
// newBreakerMetrics). A nil receiver is the no-telemetry no-op for the
// zero-value breaker. The callback reads each breaker's state under the
// breaker's own mutex (observeLevel), so it is safe to invoke concurrently
// with breaker transitions — the gauge can never lag, reorder, or go stale
// against the state field it mirrors.
func (m *breakerMetrics) registerStateCallback(breakers ...*breaker) {
	if m == nil {
		return
	}
	_, err := m.meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			for _, b := range breakers {
				// The level (0/1/2) IS the phase — do NOT also stamp the
				// phase as a `state` label. A leveled gauge must keep ONE
				// series per head: if the phase rode a label, every
				// transition would mint a NEW series (state="open" →
				// state="closed") and ORPHAN the previous one. An OTel
				// observable gauge only overwrites series it re-observes each
				// interval, so an orphaned series lingers at its last value
				// forever — a recovered breaker would still export a stale
				// state="open"=1 alongside the live state="closed"=0, and any
				// max()-across-series read (the chaos lane's recovery assert)
				// would read the breaker as permanently OPEN. One series per
				// head, keyed only on head, overwrites in place every interval
				// and is the only encoding that can never go stale.
				observer.ObserveInt64(m.state, b.observeLevel(), metric.WithAttributes(
					attrBreakerHead.String(b.head.String()),
				))
			}
			return nil
		},
		m.state,
	)
	if err != nil {
		// Callback registration only fails on a misconfigured meter / a
		// nil instrument; surface loudly rather than silently dropping the
		// breaker's only state time-series.
		panic("chclient: register breaker state callback: " + err.Error())
	}
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
