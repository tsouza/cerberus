package chclient

import (
	"context"
	"sync"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// attrTeardownOutcome labels each cursor teardown with how the connection
// underneath it was released. It is the label that makes pool churn
// DECOMPOSABLE: dials_total counts every connection cerberus had to mint,
// teardown_total{outcome="abandoned"} counts the share cerberus itself
// cancelled, and the residual is the driver's own age-eviction.
const attrTeardownOutcome = attribute.Key("outcome")

// Cursor-teardown outcomes.
//
// clickhouse-go releases a connection back to the idle pool only when the
// query reaches EndOfStream with its context still live; a cancelled query
// has its socket torn down by connect.cancel(), unconditionally and by
// design. The two outcomes name exactly that fork:
//
//	drained   — Close() returned within cursorDrainBudget without cerberus
//	            forcing a cancel, so the driver reached its own terminal
//	            state and ran release(conn, nil).
//	abandoned — the drain budget expired and cerberus cancelled to unblock,
//	            so the driver destroyed the socket instead.
const (
	teardownDrained   = "drained"
	teardownAbandoned = "abandoned"
)

// connMetrics holds the OTel instruments describing the ClickHouse connection
// pool's lifecycle. A nil *connMetrics is the "no telemetry" sentinel and every
// method is a no-op on it, mirroring breakerMetrics.
//
// The four series answer one question that nothing else in cerberus can:
// WHICH of the three connection destroyers is paying for the pool churn.
// cerberus cannot observe a connection close directly — it happens inside the
// driver's release / conn_pool — only its cost, as the dial that replaces it.
type connMetrics struct {
	// meter is retained so registerPoolGauges can register the
	// observable-gauge callback once a live driver conn exists (it post-dates
	// newConnMetrics).
	meter metric.Meter
	// dials counts every TCP connection cerberus had to mint. A dial happens
	// exactly when the pool could not hand back a warm connection — because a
	// previous one was destroyed, or because the pool was cold — so it is the
	// attribution-free bottom-line COST of all three destroyers at once.
	dials metric.Int64Counter
	// teardowns splits cursor teardowns into the drained / abandoned arms.
	teardowns metric.Int64Counter
	// open / idle mirror the driver's own pool census (driver.Stats), reported
	// by an observable callback so the level can never go stale.
	open metric.Int64ObservableGauge
	idle metric.Int64ObservableGauge
}

// newConnMetrics builds the connection-lifecycle instrument set off mp. Both
// counters are zero-initialised so a healthy replica exports a flat 0 rather
// than "No data" — the same pre-registration contract newBreakerMetrics applies
// to the trips counter, and for the same reason: OTel synchronous counters
// export nothing at all until their first Add, so a rate() panel over a replica
// that never churned a connection would read "No data" instead of the
// reassuring zero that actually describes it.
func newConnMetrics(mp metric.MeterProvider) *connMetrics {
	meter := mp.Meter(breakerMeterName)
	dials, err := meter.Int64Counter(
		"cerberus_ch_conn_dials_total",
		metric.WithDescription(
			"Cumulative TCP connections opened to ClickHouse. A dial happens "+
				"only when the pool had no warm connection to hand back, so "+
				"this is the total cost of connection churn — cancelled "+
				"queries, abandoned cursor teardowns, and age eviction alike.",
		),
		metric.WithUnit("{dial}"),
	)
	if err != nil {
		// Instrument validation only fails on a misconfigured MeterProvider;
		// surface loudly rather than silently dropping the only series that
		// makes pool churn measurable.
		panic("chclient: build conn dials counter: " + err.Error())
	}
	teardowns, err := meter.Int64Counter(
		"cerberus_ch_cursor_teardown_total",
		metric.WithDescription(
			"Cumulative ClickHouse cursor teardowns by outcome. "+
				"outcome=drained: the cursor reached its terminal state within "+
				"the drain budget, so the driver returned the connection to "+
				"the idle pool. outcome=abandoned: the drain budget expired and "+
				"cerberus cancelled, so the driver destroyed the socket.",
		),
		metric.WithUnit("{teardown}"),
	)
	if err != nil {
		panic("chclient: build cursor teardown counter: " + err.Error())
	}
	open, err := meter.Int64ObservableGauge(
		"cerberus_ch_conn_open",
		metric.WithDescription("ClickHouse pooled connections currently open (busy + idle)."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		panic("chclient: build conn open gauge: " + err.Error())
	}
	idle, err := meter.Int64ObservableGauge(
		"cerberus_ch_conn_idle",
		metric.WithDescription("ClickHouse pooled connections currently idle and reusable."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		panic("chclient: build conn idle gauge: " + err.Error())
	}
	m := &connMetrics{meter: meter, dials: dials, teardowns: teardowns, open: open, idle: idle}
	m.dials.Add(context.Background(), 0)
	for _, outcome := range []string{teardownDrained, teardownAbandoned} {
		m.teardowns.Add(context.Background(), 0, metric.WithAttributes(
			attrTeardownOutcome.String(outcome),
		))
	}
	return m
}

// recordDial counts one freshly-minted ClickHouse connection.
func (m *connMetrics) recordDial() {
	if m == nil {
		return
	}
	m.dials.Add(context.Background(), 1)
}

// recordTeardown counts one cursor teardown under the given outcome.
func (m *connMetrics) recordTeardown(outcome string) {
	if m == nil {
		return
	}
	m.teardowns.Add(context.Background(), 1, metric.WithAttributes(
		attrTeardownOutcome.String(outcome),
	))
}

// registerPoolGauges wires the observable callback that reports the driver's
// own pool census on every collection interval. stats is read live inside the
// callback — never snapshotted — so the exported level always reflects the real
// pool and can never linger at a value the pool has moved past.
//
// It returns the callback's teardown, which the owning Client MUST run when it
// closes. The gauges are process-wide while pools are not: cmd/cerberus opens
// short-lived bootstrap clients alongside the serving one, and a callback that
// outlived its pool would keep reporting a dead conn's census under the same
// (empty) attribute set as the live one — one of the two would silently win.
// The returned func is safe to call repeatedly, so a ForHead view that shares
// it cannot double-unregister.
func (m *connMetrics) registerPoolGauges(stats func() driver.Stats) func() {
	if m == nil || stats == nil {
		return func() {}
	}
	reg, err := m.meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			s := stats()
			observer.ObserveInt64(m.open, int64(s.Open))
			observer.ObserveInt64(m.idle, int64(s.Idle))
			return nil
		},
		m.open, m.idle,
	)
	if err != nil {
		// Callback registration only fails on a misconfigured meter or a nil
		// instrument; surface loudly rather than silently dropping the pool
		// census.
		panic("chclient: register conn pool gauges: " + err.Error())
	}
	return sync.OnceFunc(func() { _ = reg.Unregister() })
}

// connTelemetry is the process-wide connection-lifecycle instrument set, built
// off the OTel global MeterProvider on first use. It is process-wide rather
// than per-Client because CloseCursor — the teardown contract every cursor
// consumer routes through — is a package-level function with no Client in hand,
// and because the pool it describes is the one pool cmd/cerberus opens.
// Construction is deferred to first use so cmd/cerberus's telemetry provider is
// installed by the time the instruments are minted.
var connTelemetry = sync.OnceValue(func() *connMetrics {
	return newConnMetrics(otel.GetMeterProvider())
})
