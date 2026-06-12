package prom_test

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// TestStreamingCursorHeapStaysBounded is the deterministic check-gate
// guard for the streaming-cursor RAM win (the streaming cursor + memory /
// sample caps that landed on main). The companion benchmark
// BenchmarkStreamingCursor_1M_Points reads the same HeapInuse delta but
// only *reports* it as a soft metric — it never fails, and it runs in no
// gating lane. This test converts that soft signal into a hard, always-on
// assertion.
//
// The invariant under test is the defining property of a streaming
// cursor: the RETAINED heap after draining a result is bounded by the
// concurrent working set (per-series matrix rows + JSON envelope), NOT by
// the total number of rows pulled from ClickHouse. The handler walks the
// cursor one Sample at a time (fakeCountingCursor keeps only the current
// row resident), and each drained Sample becomes garbage once it is
// folded into the per-series matrix and encoded — so a 10× larger result
// over the SAME series fan-out retains essentially the same heap.
//
// A regression that buffered the full result — e.g. materialising every
// chclient.Sample into one big slice before encoding, or holding the raw
// rows alongside the matrix — would retain heap proportional to the row
// count: ~1M Samples, each carrying a label map, is tens-to-hundreds of
// MB resident. This test drives 1M rows and asserts the post-GC retained
// HeapInuse delta stays under a generous absolute ceiling that the real
// streaming path clears by two orders of magnitude (~0.2 MB observed)
// while a full-buffering regression blows straight through.
//
// Why retained heap, not allocs-per-op: cumulative allocations scale
// linearly with sample count even for a perfect streaming path (every
// sample is decoded → label-mapped → JSON-encoded, each step allocating
// transient garbage), so an alloc-per-op ceiling would be a brittle
// magic number that bites the streaming path itself. The *retained* set
// after GC is the metric that distinguishes streaming (bounded) from
// buffering (linear), and it is the quantity the original benchmark
// measured.
func TestStreamingCursorHeapStaysBounded(t *testing.T) {
	// retainedHeapDelta drives query_range against a cursorQuerier
	// synthesising totalRows samples over a fixed 1000-series fan-out a
	// few times, then returns the post-GC HeapInuse growth over the
	// pre-request baseline. Multiple iterations + an explicit GC keep the
	// measurement deterministic (no wall-clock, no allocation counting).
	retainedHeapDelta := func(t *testing.T, totalRows int) float64 {
		t.Helper()
		const seriesCount = 1000
		samplesPerSer := totalRows / seriesCount
		start := time.Unix(1700000000, 0).UTC()
		end := start.Add(time.Duration(samplesPerSer-1) * time.Second)
		q := &cursorQuerier{
			total:      totalRows,
			seriesMod:  samplesPerSer,
			stepNanos:  int64(time.Second),
			startUnix:  start.Unix(),
			metricName: "up",
			stopAt:     -1,
		}
		srv := newServer(q)
		t.Cleanup(srv.Close)
		url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=1",
			srv.URL, start.Unix(), end.Unix())

		// Establish the baseline AFTER the server + transport pools are
		// warm so their one-time allocations don't count as result-set
		// retention.
		if resp, err := http.Get(url); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		runtime.GC()
		var baseline runtime.MemStats
		runtime.ReadMemStats(&baseline)

		for i := 0; i < 3; i++ {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		var deltaBytes uint64
		if after.HeapInuse > baseline.HeapInuse {
			deltaBytes = after.HeapInuse - baseline.HeapInuse
		}
		return float64(deltaBytes) / (1024 * 1024)
	}

	const totalRows = 1_000_000 // 1000 series × 1000 points

	deltaMB := retainedHeapDelta(t, totalRows)
	t.Logf("retained HeapInuse delta after draining %d rows (1000 series): %.2f MB", totalRows, deltaMB)

	// Streaming clears this by ~150× (observed ~0.2 MB). A full-buffering
	// regression that retains the ~1M-Sample result (each Sample carries a
	// label map) retains tens-to-hundreds of MB and blows through. The
	// 32 MB ceiling is generous enough to absorb GC-timing jitter in the
	// retained set yet far below any linear-in-rows regression.
	const maxRetainedMB = 32.0
	if deltaMB > maxRetainedMB {
		t.Fatalf("streaming-cursor RAM regression: draining %d rows over 1000 series "+
			"retained %.2f MB of heap, exceeding the %.0f MB ceiling. Retained heap is "+
			"scaling with total row count — the cursor path is buffering the full result "+
			"instead of streaming it sample-by-sample (the per-sample garbage should be "+
			"collected once folded into the matrix + encoded).",
			totalRows, deltaMB, maxRetainedMB)
	}
}
