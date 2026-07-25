package routememo_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/routememo"
)

// benchPressureWindow mirrors the dispatch-deadline horizon a live caller
// passes to New (see routememo.New's doc comment) — it only needs to be a
// plausible duration, not a prod-shaped one.
const benchPressureWindow = time.Minute

// benchKeyCount is a small handful of distinct routing keys cycling by
// index. A live memo sees a bounded set of repeated plan shapes (the whole
// point of the structural fingerprint in Key), so a handful of synthetic
// keys exercises the hot path's map/lock/LRU behavior without pretending to
// model any real traffic distribution.
const benchKeyCount = 8

// benchKeys returns benchKeyCount distinct Keys, differing only by
// RootKind — enough for Memo to treat them as separate entries, same
// pattern memo_test.go uses for its own synthetic keys.
func benchKeys() []routememo.Key {
	keys := make([]routememo.Key, benchKeyCount)
	for i := range keys {
		keys[i] = routememo.Key{RootKind: fmt.Sprintf("bench-key-%d", i)}
	}
	return keys
}

// BenchmarkMemo_Lookup exercises the per-query Lookup call a live request
// makes before deciding whether to try route B. Half the keys are primed
// with a PreferB verdict (the memo-hit path); the other half are left
// Unknown (the memo-miss path) — a real memo serves both outcomes
// depending on whether route A has already failed enough times on that
// Key, so the benchmark mixes them rather than measuring only one branch.
func BenchmarkMemo_Lookup(b *testing.B) {
	m := routememo.New(benchPressureWindow)
	keys := benchKeys()
	for i, k := range keys {
		if i%2 == 0 {
			m.Observe(k, routememo.RouteB, routememo.OutcomeSuccess)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Lookup(keys[i%benchKeyCount])
	}
}

// BenchmarkMemo_Observe exercises the per-query Observe call a live
// dispatch makes with its terminal outcome. Real traffic alternates
// between clean completions and typed resource-exhaustion rejections, so
// the benchmark cycles both outcomes across the same small key set rather
// than measuring only the cheaper (Success) or costlier (ResourceFailure,
// which also feeds the cluster-wide pressure tracker) branch in isolation.
func BenchmarkMemo_Observe(b *testing.B) {
	m := routememo.New(benchPressureWindow)
	keys := benchKeys()
	outcomes := [...]routememo.Outcome{routememo.OutcomeSuccess, routememo.OutcomeResourceFailure}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Observe(keys[i%benchKeyCount], routememo.RouteA, outcomes[i%len(outcomes)])
	}
}

// BenchmarkMemo_AdmitDispatch exercises the single process-wide admission
// semaphore every non-baseline dispatch (probe, retry, memo-hit alike)
// acquires and releases exactly once per query — an immediate
// admit-then-release pair, mirroring the acquire-at-dispatch-start,
// release-at-dispatch-finish shape every real caller of AdmitDispatch
// follows.
func BenchmarkMemo_AdmitDispatch(b *testing.B) {
	m := routememo.New(benchPressureWindow)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release, ok := m.AdmitDispatch()
		if ok {
			release()
		}
	}
}
