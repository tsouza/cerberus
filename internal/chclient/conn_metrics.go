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

// DefaultPoolName labels the census of a Client whose Config named no pool —
// the serving pool cmd/cerberus opens for the data plane, and the pool every
// test-built Client describes.
const DefaultPoolName = "serving"

// Cursor-teardown outcomes.
//
// clickhouse-go releases a connection back to the idle pool only when the
// query reaches EndOfStream with its context still live; a cancelled query
// has its socket torn down by connect.cancel(), unconditionally and by
// design. The three outcomes name that fork plus WHO drove it:
//
//	drained   — Close() returned within the drain budget on a live context,
//	            so the driver reached its own terminal state and ran
//	            release(conn, nil). The only outcome that pools a connection.
//	abandoned — the drain budget expired and cerberus cancelled to unblock,
//	            so the driver destroyed the socket instead.
//	cancelled — the query context was already dead when teardown began (a
//	            client disconnect, a wall-clock timeout, a sibling shard's
//	            error). The socket was destroyed by the same driver branch as
//	            abandoned, but nothing cerberus can tune would have changed
//	            it — folding the two together would make an unremediable
//	            churn source look like a drain-budget problem.
const (
	teardownDrained   = "drained"
	teardownAbandoned = "abandoned"
	teardownCancelled = "cancelled"
)

// attrPoolName distinguishes the connection pool a census sample describes.
// The gauges are process-wide while pools are not — cmd/cerberus keeps
// short-lived bootstrap pools open alongside the serving one — so without it
// two concurrently-live pools would write the same (empty) attribute set and
// one would silently win.
const attrPoolName = attribute.Key("pool")

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
//
// The seed is only as good as mp: OTel's global MeterProvider DELEGATES, and a
// synchronous Add recorded before delegation is dropped on the floor with no
// buffering (observable-gauge callbacks re-register and do survive, which is
// why the pool census would mask the loss). cmd/cerberus therefore installs the
// real MeterProvider before it builds anything that mints instruments — see the
// telemetry block at the top of run(), pinned by
// test/regression/telemetry_provider_ordering_test.go.
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
				"cerberus cancelled, so the driver destroyed the socket. "+
				"outcome=cancelled: the query context was already dead when "+
				"teardown began, so the socket was destroyed by that "+
				"cancellation rather than by cerberus's budget.",
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
	for _, outcome := range []string{teardownDrained, teardownAbandoned, teardownCancelled} {
		m.teardowns.Add(context.Background(), 0, metric.WithAttributes(
			attrTeardownOutcome.String(outcome),
		))
	}
	return m
}

// resolvePoolName is the pool-census label for cfg: the operator-facing name
// when one was set, DefaultPoolName otherwise.
func resolvePoolName(cfg Config) string {
	if cfg.PoolName != "" {
		return cfg.PoolName
	}
	return DefaultPoolName
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
// Every sample carries the pool's name, because the gauges are process-wide
// while pools are not: cmd/cerberus opens bootstrap clients that are live
// CONCURRENTLY with the serving one (the schema-apply connection overlaps it
// outright), and same-named samples would land on one attribute set with one of
// the two silently winning — precisely during startup, when a pool-sizing
// problem first shows.
//
// It returns the callback's teardown, which the owning Client MUST run when it
// closes, so a callback cannot outlive the pool it reads. The returned func is
// safe to call repeatedly, so a ForHead view that shares it cannot
// double-unregister.
func (m *connMetrics) registerPoolGauges(pool string, stats func() driver.Stats) func() {
	if m == nil || stats == nil {
		return func() {}
	}
	attrs := metric.WithAttributes(attrPoolName.String(pool))
	reg, err := m.meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			s := stats()
			observer.ObserveInt64(m.open, int64(s.Open), attrs)
			observer.ObserveInt64(m.idle, int64(s.Idle), attrs)
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
//
// Construction is deferred to first use, but that alone does NOT make the
// instruments land on the right provider: the first use is Client construction,
// which happens at startup. cmd/cerberus installs its MeterProvider before it
// builds any Client — see newConnMetrics for why a synchronous counter minted
// on the un-delegated global loses its zero seed.
var connTelemetry = sync.OnceValue(func() *connMetrics {
	return newConnMetrics(otel.GetMeterProvider())
})
