package chclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// breakerGaugeLevels collects the cerberus_ch_breaker_state data points from a
// manual-reader snapshot, keyed by the breaker's HEAD. The gauge carries ONE
// series per head, keyed only on `head` — the numeric level (0/1/2) IS the
// phase, so there is deliberately NO `state` label. A leveled gauge keyed on
// the phase would orphan the prior-phase series on every transition (an OTel
// observable gauge only overwrites series it re-observes), leaving a stale
// state="open"=1 lingering after recovery — exactly the chaos-lane bug. So the
// helper additionally asserts NO data point carries a `state` attribute, and
// that no head appears more than once: the invariants that keep the gauge from
// going stale over OTLP.
func breakerGaugeLevels(t *testing.T, reader *metric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	levels := map[string]int64{}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_ch_breaker_state" {
				continue
			}
			found = true
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("breaker_state data: want Gauge[int64], got %T", m.Data)
			}
			for _, dp := range g.DataPoints {
				if _, ok := dp.Attributes.Value("state"); ok {
					t.Fatalf("breaker_state must NOT carry a `state` label (orphan-series bug): %v", dp.Attributes.ToSlice())
				}
				h, ok := dp.Attributes.Value("head")
				if !ok {
					t.Fatalf("breaker_state data point missing head attribute: %v", dp.Attributes.ToSlice())
				}
				if _, dup := levels[h.AsString()]; dup {
					t.Fatalf("breaker_state head %q appears twice in one collection (orphan-series bug)", h.AsString())
				}
				levels[h.AsString()] = dp.Value
			}
		}
	}
	if !found {
		t.Fatalf("cerberus_ch_breaker_state not exported")
	}
	return levels
}

// breakerTripsTotal returns the cumulative cerberus_ch_breaker_trips_total
// counter value from a manual-reader snapshot.
func breakerTripsTotal(t *testing.T, reader *metric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	var sum int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_ch_breaker_trips_total" {
				continue
			}
			found = true
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("breaker_trips_total data: want Sum[int64], got %T", m.Data)
			}
			if !s.IsMonotonic {
				t.Fatalf("breaker_trips_total: want monotonic sum (counter), got non-monotonic")
			}
			for _, dp := range s.DataPoints {
				sum += dp.Value
			}
		}
	}
	if !found {
		t.Fatalf("cerberus_ch_breaker_trips_total not exported")
	}
	return sum
}

// TestBreakerMetrics_ZeroInitAtConstruction pins the anti-"No data" invariant:
// both breaker streams exist at value 0 / closed the moment the breaker is
// constructed, BEFORE any transition. The trips counter is zero-init'd at
// construction (OTel sync counters export nothing until their first Add); the
// state gauge is OBSERVABLE, so its callback reports every breaker's current
// (closed) level on the first collection without needing a transition or a
// seed. Without either, the "ClickHouse circuit breaker" dashboard panel would
// render "No data" on a healthy replica whose breaker never trips.
func TestBreakerMetrics_ZeroInitAtConstruction(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	// Construct the metric set + a CLOSED breaker, register the observable
	// callback, but never drive a transition.
	m := newBreakerMetrics(mp)
	b := &breaker{head: HeadProm, metrics: m}
	m.registerStateCallback(b)

	if got := breakerTripsTotal(t, reader); got != 0 {
		t.Errorf("trips_total before any trip: want 0, got %d", got)
	}
	levels := breakerGaugeLevels(t, reader)
	got, ok := levels[HeadProm.String()]
	if !ok {
		t.Fatalf("breaker_state: missing prom-head stream (panel would show No data)")
	}
	if got != breakerGaugeClosed {
		t.Errorf("breaker_state prom: want %d (closed) at construction, got %d", breakerGaugeClosed, got)
	}
}

// TestBreakerMetrics_ObservableNeverStale is the regression pin for the chaos
// lane's last failure (dispatch 27508080750, ch-pod-kill): a breaker that
// closes and then stops transitioning must report its CURRENT level (0/closed)
// on a fresh collection, NOT linger at a transient half-open recorded earlier.
// The pre-fix synchronous gauge, recorded only on transitions, kept exporting
// the last-transitioned value (2/half-open) for minutes after the breaker had
// actually closed, so an instant query read a stale 2. The observable callback
// reads the live state every collection, so a CLOSED breaker reports 0 even
// with no recent transition.
func TestBreakerMetrics_ObservableNeverStale(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := base
	m := newBreakerMetrics(mp)
	b := &breaker{
		head:         HeadProm,
		threshold:    1,
		openInterval: time.Second,
		now:          func() time.Time { return now },
		metrics:      m,
	}
	m.registerStateCallback(b)
	failErr := errors.New("simulated CH outage")

	// Drive the full oscillation CLOSED -> OPEN -> HALF-OPEN -> CLOSED so the
	// most recent transition the pre-fix gauge would have recorded is the
	// transient half-open (the value the chaos harness saw stuck at 2).
	_ = b.allow()
	b.record(context.Background(), failErr) // CLOSED -> OPEN
	now = base.Add(1500 * time.Millisecond)
	_ = b.allow()                       // OPEN -> HALF-OPEN (the transient 2)
	b.record(context.Background(), nil) // HALF-OPEN -> CLOSED
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after recovery: state = %q, want closed", got)
	}

	// Many collection intervals later, with NO further transition, the
	// observable callback must still report the CURRENT level: 0/closed. The
	// single prom-head series is overwritten in place each collection, so it
	// reads 0 — and breakerGaugeLevels asserts there is exactly ONE series for
	// the head with NO `state` label, so no transient open/half-open level can
	// survive as an orphan series (the OTLP stale-gauge bug the chaos lane hit).
	now = base.Add(5 * time.Minute)
	levels := breakerGaugeLevels(t, reader)
	if got, ok := levels[HeadProm.String()]; !ok || got != breakerGaugeClosed {
		t.Fatalf("stale-gauge regression: prom level = %d (ok=%v), want %d (closed)", got, ok, breakerGaugeClosed)
	}
}

// TestBreakerMetrics_TransitionsRecorded drives the breaker through the full
// CLOSED -> OPEN -> HALF-OPEN -> CLOSED lifecycle and asserts the gauge tracks
// each phase and the trips counter increments on the CLOSED->OPEN edge.
func TestBreakerMetrics_TransitionsRecorded(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := base
	m := newBreakerMetrics(mp)
	b := &breaker{
		head:         HeadProm,
		threshold:    1,
		openInterval: time.Second,
		now:          func() time.Time { return now },
		metrics:      m,
	}
	m.registerStateCallback(b)
	failErr := errors.New("simulated CH outage")

	// CLOSED -> OPEN (threshold 1).
	_ = b.allow()
	b.record(context.Background(), failErr)
	if got := b.currentState(); got != "open" {
		t.Fatalf("after trip: state = %q, want open", got)
	}
	if got := breakerTripsTotal(t, reader); got != 1 {
		t.Fatalf("trips_total after one trip: want 1, got %d", got)
	}
	if lv := breakerGaugeLevels(t, reader); lv[HeadProm.String()] != breakerGaugeOpen {
		t.Fatalf("gauge level after trip: want %d (open), got %d", breakerGaugeOpen, lv[HeadProm.String()])
	}

	// OPEN -> HALF-OPEN (probe admitted past the 1s backoff).
	now = base.Add(1500 * time.Millisecond)
	if !b.allow() {
		t.Fatal("allow() did not admit the half-open probe")
	}
	if lv := breakerGaugeLevels(t, reader); lv[HeadProm.String()] != breakerGaugeHalfOpen {
		t.Fatalf("gauge level in half-open: want %d, got %d", breakerGaugeHalfOpen, lv[HeadProm.String()])
	}

	// HALF-OPEN -> CLOSED (probe succeeds).
	b.record(context.Background(), nil)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after probe success: state = %q, want closed", got)
	}
	if lv := breakerGaugeLevels(t, reader); lv[HeadProm.String()] != breakerGaugeClosed {
		t.Fatalf("gauge level after recovery: want %d (closed), got %d", breakerGaugeClosed, lv[HeadProm.String()])
	}
	// The trip counter is monotonic: recovery does NOT decrement it.
	if got := breakerTripsTotal(t, reader); got != 1 {
		t.Fatalf("trips_total after recovery: want 1 (monotonic), got %d", got)
	}

	// A second trip increments the counter again (not a stuck gauge).
	now = base.Add(2 * time.Second)
	_ = b.allow()
	b.record(context.Background(), failErr)
	if got := breakerTripsTotal(t, reader); got != 2 {
		t.Fatalf("trips_total after second trip: want 2, got %d", got)
	}
}

// TestBreakerMetrics_NilIsNoOp pins that the zero-value breaker (nil metrics)
// drives the full lifecycle without panicking — the un-instrumented hot path
// must stay allocation-free and crash-free.
func TestBreakerMetrics_NilIsNoOp(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := base
	b := &breaker{threshold: 1, openInterval: time.Second, now: func() time.Time { return now }}
	failErr := errors.New("ch down")

	_ = b.allow()
	b.record(context.Background(), failErr) // CLOSED -> OPEN, nil metrics
	now = base.Add(2 * time.Second)
	_ = b.allow()                       // OPEN -> HALF-OPEN
	b.record(context.Background(), nil) // HALF-OPEN -> CLOSED
	if got := b.currentState(); got != "closed" {
		t.Fatalf("nil-metrics breaker state = %q, want closed", got)
	}
}

// TestBreakerMetrics_WarnLogOnTrip pins that the CLOSED->OPEN edge emits a
// WARN slog line — the trip is the highest-blast-radius event (503s all three
// heads, flips /readyz) and must leave a transition log, not just a metric.
func TestBreakerMetrics_WarnLogOnTrip(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	orig := breakerLogger
	breakerLogger = func() *slog.Logger { return logger }
	t.Cleanup(func() { breakerLogger = orig })

	b := &breaker{threshold: 1}
	_ = b.allow()
	b.record(context.Background(), errors.New("ch down"))

	out := buf.String()
	if !strings.Contains(out, "tripped OPEN") {
		t.Fatalf("trip did not emit a WARN transition log; got: %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("trip transition log not at WARN level; got: %q", out)
	}
}
