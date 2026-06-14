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

// breakerGaugeLevels collects every cerberus_ch_breaker_state data point from
// a manual-reader snapshot, keyed by its "state" attribute. The gauge is
// last-value, so the map holds the most recent level recorded per label.
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
				st, ok := dp.Attributes.Value("state")
				if !ok {
					t.Fatalf("breaker_state data point missing state attribute: %v", dp.Attributes.ToSlice())
				}
				levels[st.AsString()] = dp.Value
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
// constructed, BEFORE any transition. OTel sync instruments export nothing
// until their first record/Add, so without the zero-init in newBreakerMetrics
// the "ClickHouse circuit breaker" dashboard panel would render "No data" on a
// healthy replica whose breaker never trips. Mirrors admit.go's
// rejected_total zero-init contract.
func TestBreakerMetrics_ZeroInitAtConstruction(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	// Construct the metric set; never drive a transition.
	_ = newBreakerMetrics(mp)

	if got := breakerTripsTotal(t, reader); got != 0 {
		t.Errorf("trips_total before any trip: want 0, got %d", got)
	}
	levels := breakerGaugeLevels(t, reader)
	got, ok := levels["closed"]
	if !ok {
		t.Fatalf("breaker_state: missing zero-init closed stream (panel would show No data)")
	}
	if got != breakerGaugeClosed {
		t.Errorf("breaker_state closed: want %d at construction, got %d", breakerGaugeClosed, got)
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
	b := &breaker{
		threshold:    1,
		openInterval: time.Second,
		now:          func() time.Time { return now },
		metrics:      newBreakerMetrics(mp),
	}
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
	if lv := breakerGaugeLevels(t, reader); lv["open"] != breakerGaugeOpen {
		t.Fatalf("gauge open level after trip: want %d, got %d", breakerGaugeOpen, lv["open"])
	}

	// OPEN -> HALF-OPEN (probe admitted past the 1s backoff).
	now = base.Add(1500 * time.Millisecond)
	if !b.allow() {
		t.Fatal("allow() did not admit the half-open probe")
	}
	if lv := breakerGaugeLevels(t, reader); lv["half-open"] != breakerGaugeHalfOpen {
		t.Fatalf("gauge half-open level: want %d, got %d", breakerGaugeHalfOpen, lv["half-open"])
	}

	// HALF-OPEN -> CLOSED (probe succeeds).
	b.record(context.Background(), nil)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after probe success: state = %q, want closed", got)
	}
	if lv := breakerGaugeLevels(t, reader); lv["closed"] != breakerGaugeClosed {
		t.Fatalf("gauge closed level after recovery: want %d, got %d", breakerGaugeClosed, lv["closed"])
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
