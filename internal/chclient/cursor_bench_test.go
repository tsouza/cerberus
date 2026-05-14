package chclient

import (
	"testing"
	"time"
)

// chclient cursor micro-benchmarks (Layer 12). The streaming cursor
// is wired in front of the matrix-build path for /api/v1/query_range;
// the per-row decode cost dominates large-window requests, so a
// regression here scales with row count. The fake-rows shim lives in
// cursor_test.go and is shared.

func BenchmarkRowsCursor_DrainSmall(b *testing.B) {
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{
			MetricName: "up",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  ts.Add(time.Duration(i) * time.Second),
			Value:      float64(i),
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh fakeRows + cursor each iter — the cursor caches its
		// driver.Rows handle, and re-using it across iterations would
		// only measure the EOF check, not the Scan path.
		b.StopTimer()
		rows := &fakeRows{samples: samples}
		cursor := &rowsCursor{rows: rows}
		b.StartTimer()
		for cursor.Next() {
			_ = cursor.Sample()
		}
		_ = cursor.Close()
	}
}

func BenchmarkRowsCursor_DrainLarge(b *testing.B) {
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	samples := make([]Sample, 10_000)
	for i := range samples {
		samples[i] = Sample{
			MetricName: "up",
			Labels:     map[string]string{"job": "api", "instance": "host-0"},
			Timestamp:  ts.Add(time.Duration(i) * time.Second),
			Value:      float64(i),
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		rows := &fakeRows{samples: samples}
		cursor := &rowsCursor{rows: rows}
		b.StartTimer()
		count := 0
		for cursor.Next() {
			_ = cursor.Sample()
			count++
		}
		_ = cursor.Close()
		if count != len(samples) {
			b.Fatalf("drained %d samples; want %d", count, len(samples))
		}
	}
}

// TestAllocs_RowsCursor_Decode pins the per-row alloc count of the
// rowsCursor Scan + decode path. Each row decode currently allocates
// a fresh labels map (the upstream CH driver returns a new map per
// row); the ceiling here covers that plus a small overhead.
func TestAllocs_RowsCursor_Decode(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// Allocs-per-run is the average across runs; the fn must do the
	// same work every invocation. Drain a 1-row cursor each call so
	// the per-row cost is the dominant signal.
	got := testing.AllocsPerRun(50, func() {
		samples := []Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
		}
		rows := &fakeRows{samples: samples}
		cursor := &rowsCursor{rows: rows}
		for cursor.Next() {
			_ = cursor.Sample()
		}
		_ = cursor.Close()
	})
	// Baseline 7 allocs (fakeRows + sample + cursor + close path).
	// Ceiling = ~3×.
	const ceiling = 25.0
	if got > ceiling {
		t.Errorf("rowsCursor decode avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("rowsCursor decode avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}
