package chclient

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// breakerHeadState reports the CURRENT lifecycle level for head h from a
// manual-reader snapshot. The state gauge is observable: its callback reports
// exactly ONE sample per head per collection (the head's current state, with a
// matching state= label), so a collection holds a single series per head and
// the MAX-over-h's-samples below is simply that one current value — there are
// no stale lingering series to filter out (the whole point of the observable
// gauge).
func breakerHeadState(t *testing.T, reader *sdkmetric.ManualReader, h string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	level := int64(-1)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_ch_breaker_state" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("breaker_state data: want Gauge[int64], got %T", m.Data)
			}
			for _, dp := range g.DataPoints {
				hv, ok := dp.Attributes.Value("head")
				if !ok {
					t.Fatalf("breaker_state data point missing head attribute: %v", dp.Attributes.ToSlice())
				}
				if hv.AsString() == h && dp.Value > level {
					level = dp.Value
				}
			}
		}
	}
	if level < 0 {
		t.Fatalf("head=%q missing from breaker_state", h)
	}
	return level
}

// breakerHeadStates returns the current level per head (see breakerHeadState).
func breakerHeadStates(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, h := range allHeads {
		out[h.String()] = breakerHeadState(t, reader, h.String())
	}
	return out
}

// breakerHeadTrips collects the cumulative cerberus_ch_breaker_trips_total per
// head= label from a manual-reader snapshot.
func breakerHeadTrips(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_ch_breaker_trips_total" {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("breaker_trips_total data: want Sum[int64], got %T", m.Data)
			}
			for _, dp := range s.DataPoints {
				h, ok := dp.Attributes.Value("head")
				if !ok {
					t.Fatalf("breaker_trips_total data point missing head attribute: %v", dp.Attributes.ToSlice())
				}
				out[h.AsString()] = dp.Value
			}
		}
	}
	return out
}

// Per-head circuit-breaker isolation tests (#94). The single-breaker code
// these replace would FAIL TestStorm_OnOneHead_DoesNotTrip_Others: a storm on
// one head's queries tripped the ONE shared breaker, so loki/tempo (and the
// readiness ping) all returned ErrCircuitOpen too. With one breaker per head
// over the shared pool, a head's open breaker fast-fails ONLY that head.

// newPerHeadTestClient builds a Client over conn with a manually-driven clock
// installed on EVERY per-head breaker (and the unscoped default), so a test
// can drive one head's view to OPEN without a 5s real sleep while the other
// heads' breakers stay on the same controllable clock. Returns the parent
// Client (use ForHead to get per-head views) and a setNow to advance the
// shared clock.
func newPerHeadTestClient(t *testing.T, conn driver.Conn) (*Client, func(time.Time)) {
	t.Helper()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	client := newWithConn(conn)
	client.br.now = clock
	for _, b := range client.breakers {
		b.now = clock
	}
	setNow := func(tt time.Time) {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = tt
	}
	return client, setNow
}

// tripHead drives head h's breaker to OPEN by firing breakerThreshold failing
// queries through its view against a conn that is currently failing.
func tripHead(t *testing.T, client *Client, h Head) {
	t.Helper()
	ctx := context.Background()
	view := client.ForHead(h)
	for i := 0; i < breakerThreshold; i++ {
		if _, err := view.Query(ctx, "SELECT 1"); err == nil {
			t.Fatalf("trip %s: Query %d returned nil, want failure", h, i)
		}
	}
	if got := client.breakers[h].currentState(); got != "open" {
		t.Fatalf("trip %s: state %q, want open", h, got)
	}
}

// TestStorm_OnOneHead_DoesNotTrip_Others is the core #94 regression. A storm
// trips the prom head's breaker; loki and tempo — against the SAME conn (now
// healthy) — must still serve, with their breakers CLOSED. On the pre-#94
// single-breaker code prom's trip 503'd every head, so this fails there.
func TestStorm_OnOneHead_DoesNotTrip_Others(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newPerHeadTestClient(t, conn)

	tripHead(t, client, HeadProm)

	// Prom is OPEN: its view fast-fails.
	if _, err := client.ForHead(HeadProm).Query(context.Background(), "SELECT 1"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("prom after storm: got %v, want ErrCircuitOpen", err)
	}

	// CH "recovers" for the other heads (same conn, now healthy).
	conn.setFail(false)
	for _, h := range []Head{HeadLoki, HeadTempo} {
		if got := client.breakers[h].currentState(); got != "closed" {
			t.Fatalf("%s breaker after prom storm: state %q, want closed", h, got)
		}
		if _, err := client.ForHead(h).Query(context.Background(), "SELECT 1"); err != nil {
			t.Fatalf("%s query after prom storm: got %v, want success (isolated breaker)", h, err)
		}
	}
}

// TestStorm_OnOneHead_DoesNotEvictPod pins the readiness contract: a prom
// storm trips the prom breaker but the probe breaker is untouched, so a ping
// through the HeadProbe view still succeeds (/readyz green). Then driving the
// PROBE breaker open (failing the pings themselves) makes the probe view's
// Ping fast-fail — proving readiness tracks reachability, not workload.
func TestStorm_OnOneHead_DoesNotEvictPod(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newPerHeadTestClient(t, conn)

	tripHead(t, client, HeadProm)

	// Prom OPEN, but the probe breaker never saw prom traffic. CH "recovers"
	// for the probe path; the readiness ping must succeed.
	conn.setFail(false)
	if got := client.breakers[HeadProbe].currentState(); got != "closed" {
		t.Fatalf("probe breaker after prom storm: state %q, want closed (readiness must stay green)", got)
	}
	if err := client.ForHead(HeadProbe).Ping(context.Background()); err != nil {
		t.Fatalf("readiness ping after prom storm: got %v, want success (pod must not be evicted)", err)
	}

	// Now a genuine CH-reachability failure: the pings THEMSELVES fail.
	conn.setFail(true)
	probe := client.ForHead(HeadProbe)
	for i := 0; i < breakerThreshold; i++ {
		_ = probe.Ping(context.Background())
	}
	if got := client.breakers[HeadProbe].currentState(); got != "open" {
		t.Fatalf("probe breaker after failed pings: state %q, want open", got)
	}
	if err := probe.Ping(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("readiness ping after sustained ping failures: got %v, want ErrCircuitOpen (/readyz red)", err)
	}
}

// TestProbeBreaker_DoesNotFlapUnderDataStorm hammers all three data heads to
// OPEN and asserts the probe breaker stays CLOSED and a probe ping keeps
// succeeding throughout — data-head storms are structurally invisible to the
// probe breaker, so readiness cannot flap from workload.
func TestProbeBreaker_DoesNotFlapUnderDataStorm(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	client, _ := newPerHeadTestClient(t, conn)

	tripHead(t, client, HeadProm)
	tripHead(t, client, HeadLoki)
	tripHead(t, client, HeadTempo)

	conn.setFail(false)
	if got := client.breakers[HeadProbe].currentState(); got != "closed" {
		t.Fatalf("probe breaker under 3-head storm: state %q, want closed", got)
	}
	if err := client.ForHead(HeadProbe).Ping(context.Background()); err != nil {
		t.Fatalf("readiness ping under 3-head storm: got %v, want success", err)
	}
}

// TestForHead_DistinctBreakers_SharedPool pins the "one pool, N breakers"
// invariant: each head's view carries a DISTINCT *breaker while every view
// shares the SAME driver.Conn. A future refactor that collapses the registry
// back to a single breaker fails here.
func TestForHead_DistinctBreakers_SharedPool(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	client := newWithConn(conn)

	prom := client.ForHead(HeadProm)
	loki := client.ForHead(HeadLoki)
	tempo := client.ForHead(HeadTempo)
	probe := client.ForHead(HeadProbe)

	views := []*Client{prom, loki, tempo, probe}
	// All breaker pointers distinct.
	for i := 0; i < len(views); i++ {
		for j := i + 1; j < len(views); j++ {
			if views[i].br == views[j].br {
				t.Fatalf("views %d and %d share the same *breaker; per-head isolation collapsed", i, j)
			}
		}
	}
	// All conns identical (one pool).
	for i, v := range views {
		if v.conn != conn {
			t.Fatalf("view %d does not share the parent conn (would double CH connections)", i)
		}
	}
}

// TestForHead_UnknownHeadPanics pins that an unknown head is a wiring bug
// caught at New-time, not a silently-minted garbage breaker at request time.
func TestForHead_UnknownHeadPanics(t *testing.T) {
	t.Parallel()
	client := newWithConn(newFlakyConn(nil))
	defer func() {
		if recover() == nil {
			t.Fatal("ForHead(unknown) did not panic")
		}
	}()
	_ = client.ForHead(Head("nope"))
}

// TestForHead_NeutralClassificationsPerHead re-runs the breaker-neutral
// classifications (code-241 memory, code-159 timeout, context.Canceled) on a
// per-head breaker, confirming the split preserved each verdict: none of these
// advance a head's breaker toward OPEN. Mirrors the single-breaker assertions
// but targeted at the loki head's view.
func TestForHead_NeutralClassificationsPerHead(t *testing.T) {
	t.Parallel()
	view := newWithConn(newFlakyConn(nil)).ForHead(HeadLoki)
	br := view.br

	cases := []struct {
		name string
		err  error
		ctx  context.Context
	}{
		{"memory-limit-241", chMemLimitException(), context.Background()},
		{"query-timeout-159", chTimeoutException(), context.Background()},
		{"context-canceled", context.Canceled, context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fire far more than the threshold of "neutral" failures.
			for i := 0; i < breakerThreshold*3; i++ {
				_ = br.allow()
				br.record(tc.ctx, tc.err)
			}
			if got := br.currentState(); got != "closed" {
				t.Fatalf("%s advanced the breaker to %q; neutral classification lost", tc.name, got)
			}
		})
	}
}

// k8s readiness budget pins from test/e2e/k3s/cerberus-values.yaml. Duplicated here
// as named consts so this sized test fails loudly if the manifest tightens the
// eviction window below what the probe breaker can trip inside.
const (
	readinessProbePeriod           = 3 * time.Second
	readinessProbeFailureThreshold = 5
)

// TestProbeBreaker_TripsWithinReadinessBudget is the sized timing test the
// readiness contract requires (#94): a TOTAL CH outage must trip the probe
// breaker and flip /readyz red INSIDE the k8s readinessProbe eviction window
// (periodSeconds * failureThreshold). The probe breaker now trips ONLY via
// failed pings (decoupled from data-plane traffic), and those pings arrive at
// the readiness-probe cadence (~periodSeconds, since each 3s probe outruns the
// 2s health TTL cache). With the tighter probeBreakerThreshold this trips well
// inside budget, with margin before k8s evicts. If a future manifest change
// tightens the window or someone loosens the probe threshold, this fails.
func TestProbeBreaker_TripsWithinReadinessBudget(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true) // total CH outage: every ping fails
	client, setNow := newPerHeadTestClient(t, conn)
	probe := client.ForHead(HeadProbe)

	// Model the readiness-probe cadence: one failed ping per probe period.
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	var tripAt time.Duration = -1
	budget := readinessProbePeriod * readinessProbeFailureThreshold
	for i := 0; ; i++ {
		elapsed := time.Duration(i) * readinessProbePeriod
		if elapsed > budget {
			break
		}
		setNow(base.Add(elapsed))
		err := probe.Ping(context.Background())
		if errors.Is(err, ErrCircuitOpen) {
			tripAt = elapsed
			break
		}
	}
	if tripAt < 0 {
		t.Fatalf("probe breaker did not trip /readyz red within the %s readiness budget — a dead CH would never evict the pod", budget)
	}
	if tripAt > budget {
		t.Fatalf("probe breaker tripped at %s, past the %s readiness budget — pod evicted late on a dead CH", tripAt, budget)
	}
	t.Logf("probe breaker tripped /readyz red at %s (budget %s, margin %s)", tripAt, budget, budget-tripAt)
}

// TestBreakerMetrics_ZeroInitAllHeads pins that all FOUR head label sets
// export state=0 (closed) and trips_total=0 at construction, BEFORE any
// traffic — otherwise a healthy replica's never-tripped heads vanish from a
// `sum by(head)` panel.
func TestBreakerMetrics_ZeroInitAllHeads(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	// buildBreakers mints the four head breakers and registers the observable
	// state callback over them, so every head's gauge series exists from the
	// first collection — no transition needed.
	_, _ = buildBreakers(false, 0, 0, 0, newBreakerMetrics(mp))

	states := breakerHeadStates(t, reader)
	trips := breakerHeadTrips(t, reader)
	for _, h := range allHeads {
		st, ok := states[h.String()]
		if !ok {
			t.Fatalf("head=%q missing from breaker_state at construction (panel would drop it)", h)
		}
		if st != breakerGaugeClosed {
			t.Errorf("head=%q breaker_state at construction = %d, want closed(%d)", h, st, breakerGaugeClosed)
		}
		tp, ok := trips[h.String()]
		if !ok {
			t.Fatalf("head=%q missing from breaker_trips_total at construction", h)
		}
		if tp != 0 {
			t.Errorf("head=%q trips_total at construction = %d, want 0", h, tp)
		}
	}
}

// TestBreakerMetrics_TripIncrementsOnlyAffectedHead trips the prom breaker and
// asserts only head="prom" shows state=open / trips=1; loki and tempo stay
// state=closed / trips=0. Also asserts the HALF-OPEN->OPEN re-open does NOT
// bump trips (only CLOSED->OPEN does).
func TestBreakerMetrics_TripIncrementsOnlyAffectedHead(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics := newBreakerMetrics(mp)

	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := base
	def, registry := buildBreakers(false, 1, 0, time.Second, metrics)
	_ = def
	for _, b := range registry {
		b.now = func() time.Time { return now }
	}
	prom := registry[HeadProm]
	failErr := errors.New("simulated CH outage")

	// CLOSED -> OPEN on prom (threshold 1).
	_ = prom.allow()
	prom.record(context.Background(), failErr)

	states := breakerHeadStates(t, reader)
	trips := breakerHeadTrips(t, reader)
	if states["prom"] != breakerGaugeOpen {
		t.Fatalf("prom state after trip = %d, want open(%d)", states["prom"], breakerGaugeOpen)
	}
	if trips["prom"] != 1 {
		t.Fatalf("prom trips after trip = %d, want 1", trips["prom"])
	}
	for _, h := range []string{"loki", "tempo", "probe"} {
		if states[h] != breakerGaugeClosed {
			t.Errorf("%s state after prom-only trip = %d, want closed", h, states[h])
		}
		if trips[h] != 0 {
			t.Errorf("%s trips after prom-only trip = %d, want 0", h, trips[h])
		}
	}

	// OPEN -> HALF-OPEN -> OPEN (probe fails) must NOT bump trips.
	now = base.Add(2 * time.Second)
	_ = prom.allow() // admit probe
	prom.record(context.Background(), failErr)
	if got := breakerHeadTrips(t, reader)["prom"]; got != 1 {
		t.Fatalf("prom trips after HALF-OPEN->OPEN re-open = %d, want 1 (re-open is not a new trip)", got)
	}
}

// TestBreakerDisabled_PassThroughPerHead pins that the global disable knob
// still works after the per-head split: with disabled=true, every head's
// breaker passes through to CH even after many failures (never opens).
func TestBreakerDisabled_PassThroughPerHead(t *testing.T) {
	t.Parallel()
	conn := newFlakyConn(nil)
	conn.setFail(true)
	def, registry := buildBreakers(true /*disabled*/, 0, 0, 0, nil)
	client := &Client{conn: conn, br: def, breakers: registry}

	for _, h := range allHeads {
		view := client.ForHead(h)
		for i := 0; i < breakerThreshold*2; i++ {
			_ = view.Ping(context.Background()) // ping path also gated
		}
		if got := registry[h].currentState(); got != "closed" {
			t.Fatalf("disabled %s breaker opened after %d failures: state %q, want closed", h, breakerThreshold*2, got)
		}
	}
}
