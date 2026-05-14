package format_test

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
)

// format helper micro-benchmarks (Layer 12). CanonicalKey is on the
// per-row hot path inside matrixFromCursor — every sample that lands
// in the streaming cursor's per-series bucket map runs through it. A
// few extra allocs here multiply by row count, so the ceiling on the
// allocs test is tight.

// makeLabels returns a synthetic label set of the requested size.
func makeLabels(n int) map[string]string {
	m := make(map[string]string, n)
	m["__name__"] = "http_requests_total"
	for i := 0; i < n-1; i++ {
		m[fmt.Sprintf("k%02d", i)] = fmt.Sprintf("v%02d", i)
	}
	return m
}

func BenchmarkCanonicalKey_Small(b *testing.B) {
	labels := makeLabels(3)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = format.CanonicalKey(labels)
	}
}

func BenchmarkCanonicalKey_Large(b *testing.B) {
	labels := makeLabels(20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = format.CanonicalKey(labels)
	}
}

// TestAllocs_CanonicalKey pins the alloc count of CanonicalKey at the
// per-row hot-path scale. The function builds a sorted []string of
// keys + a []byte buffer for the output — two allocs of expected
// shape. Anything materially larger means somebody slipped a slice
// growth or a sort.Slice swap into the hot loop.
func TestAllocs_CanonicalKey(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	labels := makeLabels(5)
	got := testing.AllocsPerRun(100, func() {
		_ = format.CanonicalKey(labels)
	})
	// Current baseline is 6 allocs (sort.Strings backing array + the
	// []byte → string conversion). Ceiling is set with ~30% slack so
	// regressions trip without false-positiving on micro-fluctuations.
	const ceiling = 8.0
	if got > ceiling {
		t.Errorf("CanonicalKey avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("CanonicalKey avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}
