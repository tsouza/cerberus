package prom

import (
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
)

// Layer 11 — the range-lowering strategy table is LIVE state. Which native
// ts-grid lowerings are available is decided by the ClickHouse capability set,
// and that set is re-resolved while the process runs, so a cluster upgraded
// under a running cerberus must start emitting the native shape without a
// restart.

// nativeRateTable is the strategy table a capability set carrying the ts-grid
// range feature selects: native rate lowering with the fan-out lowering kept as
// the per-expression fallback.
func nativeRateTable() promql.RangeLowerers {
	return promql.RangeLowerers{Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}}
}

// TestLowerers_FallsBackToTheWiredTableUntilSwapped — a handler nobody has
// swapped lowers with exactly the table it was constructed with.
func TestLowerers_FallsBackToTheWiredTableUntilSwapped(t *testing.T) {
	t.Parallel()

	h := &Handler{Lowerers: nativeRateTable()}
	if got := countNativeNodes(planForRangeQuery(t, "sum by(job) (rate(requests_total[5m]))", h.lowerers())); got == 0 {
		t.Errorf("the wired native table produced no native node; lowerers() is not reading Lowerers")
	}
	if got := countNativeNodes(planForRangeQuery(t, "sum by(job) (rate(requests_total[5m]))", (&Handler{}).lowerers())); got != 0 {
		t.Errorf("a bare handler produced %d native nodes; want the all-fan-out table", got)
	}
}

// TestSetLowerers_ChangesHowAQueryLowers is the activation gate: swapping the
// table has to change the PLAN a range query builds, not merely the value of a
// field. A seam that stores the new table while the query path keeps reading
// the boot copy would leave every native lowering permanently off and still
// pass every structural check — the failure mode this pins.
func TestSetLowerers_ChangesHowAQueryLowers(t *testing.T) {
	t.Parallel()

	const q = "sum by(job) (rate(requests_total[5m]))"
	// Boot posture: a server below the ts-grid floor selects the all-fan-out
	// table, so nothing lowers natively.
	h := &Handler{}
	if got := countNativeNodes(planForRangeQuery(t, q, h.lowerers())); got != 0 {
		t.Fatalf("before the swap: %d native nodes; want 0", got)
	}

	// The re-probe finds an upgraded server and installs the table it now allows.
	h.SetLowerers(nativeRateTable())

	if got := countNativeNodes(planForRangeQuery(t, q, h.lowerers())); got == 0 {
		t.Errorf("after the swap: 0 native nodes; want the native lowering — the swapped table never " +
			"reached the plan builder, so a post-upgrade pod would keep emitting the fan-out shape forever")
	}
}

// TestSetLowerers_SwapIsReversible — a re-probe against a server that lost the
// capability (a rollback, or a probe that can only reach the supported floor)
// must be able to take the native lowering away again.
func TestSetLowerers_SwapIsReversible(t *testing.T) {
	t.Parallel()

	const q = "sum by(job) (rate(requests_total[5m]))"
	h := &Handler{Lowerers: nativeRateTable()}

	h.SetLowerers(promql.RangeLowerers{})
	if got := countNativeNodes(planForRangeQuery(t, q, h.lowerers())); got != 0 {
		t.Errorf("after the rollback: %d native nodes; want 0", got)
	}
}

// TestSetLowerers_ConcurrentWithReads — the re-probe goroutine swaps the table
// while request goroutines read it. Under -race this fails on any non-atomic
// seam.
func TestSetLowerers_ConcurrentWithReads(t *testing.T) {
	t.Parallel()

	h := &Handler{Lowerers: nativeRateTable()}
	const rounds = 500

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				h.SetLowerers(nativeRateTable())
			} else {
				h.SetLowerers(promql.RangeLowerers{})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = h.lowerers()
		}
	}()
	wg.Wait()
}
