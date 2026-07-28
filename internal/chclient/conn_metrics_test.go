package chclient

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestConnMetrics builds a connection-lifecycle instrument set over a manual
// reader so a test can collect the exported series synchronously.
func newTestConnMetrics(t *testing.T) (*connMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return newConnMetrics(mp), reader
}

// counterSum returns the summed value of the named counter's data points whose
// attributes match every want entry, plus whether the counter was exported at
// all and how many matching data points were found. Attribute matching is a
// subset test so a caller can ask for one label without enumerating the rest.
func counterSum(t *testing.T, reader *sdkmetric.ManualReader, name string, want map[string]string) (sum int64, exported bool, points int) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			exported = true
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data: want Sum[int64], got %T", name, m.Data)
			}
			for _, dp := range s.DataPoints {
				matched := true
				for k, v := range want {
					got, ok := dp.Attributes.Value(attribute.Key(k))
					if !ok || got.AsString() != v {
						matched = false
						break
					}
				}
				if !matched {
					continue
				}
				sum += dp.Value
				points++
			}
		}
	}
	return sum, exported, points
}

// gaugeValue returns the named observable gauge's value for the given pool. A
// gauge is keyed by attribute set, so the pool label is part of the lookup: two
// concurrently-registered pools are two distinct series, and asking for one
// must never silently read the other's level.
func gaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name, pool string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s data: want Gauge[int64], got %T", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				got, has := dp.Attributes.Value(attrPoolName)
				if !has {
					t.Fatalf("%s: data point carries no %q attribute; two live pools would collide on one series",
						name, attrPoolName)
				}
				if got.AsString() == pool {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

// TestConnMetrics_DialCounter covers both halves of the dial counter's
// contract: the zero-valued series exists BEFORE any dial (so a replica that
// never churns a connection reads as a flat 0 rather than "No data"), and a
// successful dial through the production DialContext closure increments it.
func TestConnMetrics_DialCounter(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	sum, exported, points := counterSum(t, reader, "cerberus_ch_conn_dials_total", nil)
	if !exported {
		t.Fatal("cerberus_ch_conn_dials_total was not exported before any dial; " +
			"the zero-init seed is what keeps a healthy replica off \"No data\"")
	}
	if sum != 0 || points != 1 {
		t.Fatalf("pre-dial dials_total = %d over %d points; want 0 over 1", sum, points)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_ = conn.Close()
	}()

	dial := dialContext(time.Second, Config{KeepAliveEnabled: true}, m)
	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	<-accepted

	sum, _, _ = counterSum(t, reader, "cerberus_ch_conn_dials_total", nil)
	if sum != 1 {
		t.Fatalf("dials_total after one dial = %d; want 1", sum)
	}
}

// TestConnMetrics_DialCounter_FailedDialNotCounted pins the other arm: a dial
// that never produced a connection is a connectivity failure, not churn, and
// must not inflate the churn cost signal.
func TestConnMetrics_DialCounter_FailedDialNotCounted(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	// Bind then immediately release the port so the address is almost
	// certainly refusing connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close listener: %v", cerr)
	}

	dial := dialContext(time.Second, Config{}, m)
	conn, err := dial(context.Background(), addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("dial to a closed port unexpectedly succeeded")
	}

	sum, _, _ := counterSum(t, reader, "cerberus_ch_conn_dials_total", nil)
	if sum != 0 {
		t.Fatalf("dials_total after a failed dial = %d; want 0", sum)
	}
}

// stubStatsConn reports a fixed pool census.
type stubStatsConn struct{ open, idle int }

func (s stubStatsConn) Stats() driver.Stats {
	return driver.Stats{Open: s.open, Idle: s.idle, MaxOpenConns: 10, MaxIdleConns: 5}
}

// TestConnMetrics_PoolGauges confirms the pool census is reported from the
// driver's live Stats — and re-read on every collection, never snapshotted, so
// the level cannot linger at a value the pool has moved past.
func TestConnMetrics_PoolGauges(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	stats := stubStatsConn{open: 7, idle: 3}
	m.registerPoolGauges(DefaultPoolName, func() driver.Stats { return stats.Stats() })

	open, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName)
	if !ok {
		t.Fatal("cerberus_ch_conn_open was not exported")
	}
	if open != 7 {
		t.Errorf("conn_open = %d; want 7", open)
	}
	idle, ok := gaugeValue(t, reader, "cerberus_ch_conn_idle", DefaultPoolName)
	if !ok {
		t.Fatal("cerberus_ch_conn_idle was not exported")
	}
	if idle != 3 {
		t.Errorf("conn_idle = %d; want 3", idle)
	}

	stats.open, stats.idle = 2, 2
	open, _ = gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName)
	if open != 2 {
		t.Errorf("conn_open after the pool moved = %d; want 2 — the callback must re-read Stats, not snapshot it", open)
	}
}

// TestConnMetrics_PoolGauges_UnregisterStopsObserving pins the ownership half of
// the gauge contract. The instruments are process-wide but a pool is not:
// cmd/cerberus opens short-lived bootstrap clients alongside the serving one,
// and a callback outliving its conn would keep reporting a dead pool's census
// under the same (empty) attribute set as the live one. Close must therefore
// drop the callback, and dropping it twice — a ForHead view shares the func —
// must stay harmless.
func TestConnMetrics_PoolGauges_UnregisterStopsObserving(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	unregister := m.registerPoolGauges(DefaultPoolName, func() driver.Stats {
		return driver.Stats{Open: 4, Idle: 1}
	})
	if _, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName); !ok {
		t.Fatal("cerberus_ch_conn_open was not exported while the callback was registered")
	}

	unregister()
	unregister() // idempotent: a ForHead view shares the same func by pointer.

	if _, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName); ok {
		t.Error("cerberus_ch_conn_open is still observed after unregister; a closed client's dead pool would keep overwriting the live one's census")
	}
	if _, ok := gaugeValue(t, reader, "cerberus_ch_conn_idle", DefaultPoolName); ok {
		t.Error("cerberus_ch_conn_idle is still observed after unregister")
	}
}

// censusConn is a driver.Conn whose pool census is settable, so a Client built
// the production way can be observed reporting a level only its own conn knows.
type censusConn struct {
	chaosConn
	stats driver.Stats
}

func (c *censusConn) Stats() driver.Stats { return c.stats }

// TestClient_RegistersPoolCensus is the WIRING gate. Every other gauge test
// calls registerPoolGauges directly, which proves the callback works but proves
// nothing about the production assembly path — delete the census line from
// assembleClientFromConn and they all still pass while cerberus exports no pool
// census at all. This test goes through the same assembly New uses, so the
// series exists only if a real Client registered it, and stops only if
// Client.Close dropped it.
func TestClient_RegistersPoolCensus(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	const poolName = "serving-test"
	conn := &censusConn{stats: driver.Stats{Open: 6, Idle: 2}}
	c := assembleClientFromConn(Config{Addr: "127.0.0.1:9000", PoolName: poolName}, conn, m)

	open, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", poolName)
	if !ok {
		t.Fatalf("no conn_open series for pool %q; the assembled Client never registered its pool census", poolName)
	}
	if open != 6 {
		t.Errorf("conn_open{pool=%q} = %d; want 6 — the census must read this Client's own conn", poolName, open)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", poolName); ok {
		t.Errorf("conn_open{pool=%q} is still observed after Close; the callback outlived the pool it reads", poolName)
	}
}

// TestClient_PoolCensusDefaultsToServing pins the unnamed-pool label. A Config
// that names no pool is the serving one, and it must still carry an attribute —
// an empty label would put it back on the colliding series the label exists to
// separate.
func TestClient_PoolCensusDefaultsToServing(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	conn := &censusConn{stats: driver.Stats{Open: 3, Idle: 1}}
	c := assembleClientFromConn(Config{Addr: "127.0.0.1:9000"}, conn, m)
	t.Cleanup(func() { _ = c.Close() })

	open, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName)
	if !ok {
		t.Fatalf("no conn_open series for pool %q; an unnamed Config must label its census %[1]q", DefaultPoolName)
	}
	if open != 3 {
		t.Errorf("conn_open{pool=%q} = %d; want 3", DefaultPoolName, open)
	}
}

// TestConnMetrics_PoolGauges_ConcurrentPoolsDoNotCollide is the gate on the
// pool label. cmd/cerberus keeps bootstrap pools open ALONGSIDE the serving one
// — the schema-apply connection overlaps it outright — and the gauges are
// process-wide. Without a distinguishing attribute both callbacks write the
// same (empty) attribute set, the SDK keeps one data point, and whichever
// callback ran last silently wins: the operator reads one pool's census
// believing it describes the other, precisely during startup, when a
// pool-sizing problem first shows.
func TestConnMetrics_PoolGauges_ConcurrentPoolsDoNotCollide(t *testing.T) {
	m, reader := newTestConnMetrics(t)

	const bootstrapPool = "schema-apply"
	unregisterServing := m.registerPoolGauges(DefaultPoolName, func() driver.Stats {
		return driver.Stats{Open: 9, Idle: 4}
	})
	t.Cleanup(unregisterServing)
	unregisterBootstrap := m.registerPoolGauges(bootstrapPool, func() driver.Stats {
		return driver.Stats{Open: 1, Idle: 0}
	})
	t.Cleanup(unregisterBootstrap)

	serving, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", DefaultPoolName)
	if !ok {
		t.Fatalf("no conn_open series for pool %q while both pools are live", DefaultPoolName)
	}
	if serving != 9 {
		t.Errorf("conn_open{pool=%q} = %d; want 9", DefaultPoolName, serving)
	}
	bootstrap, ok := gaugeValue(t, reader, "cerberus_ch_conn_open", bootstrapPool)
	if !ok {
		t.Fatalf("no conn_open series for pool %q while both pools are live", bootstrapPool)
	}
	if bootstrap != 1 {
		t.Errorf("conn_open{pool=%q} = %d; want 1", bootstrapPool, bootstrap)
	}
}
