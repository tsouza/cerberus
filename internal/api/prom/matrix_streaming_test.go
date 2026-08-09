package prom

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMatrixFromCursor_MultiShardBurstsCoalesceIntoOneSeries feeds
// matrixFromCursor a cursor shaped like a solver-routed (route B) drain:
// two "shards" concatenated back to back, each internally grouped by
// series and ascending by timestamp (RangeSeriesOrder's contract for route
// A's own SQL, and the natural per-shard order a route-B shard's SQL
// carries), but the two shards themselves are NOT merged —
// solver.shardCursor drains shard 0 to exhaustion, then shard 1 — so a
// series present in both shards arrives as two separate, non-adjacent
// bursts rather than one contiguous run.
//
// #1442's rewrite must still produce exactly ONE MatrixSample per series,
// carrying every point from both bursts in ascending order — not a
// duplicate entry per burst, and not a dropped burst. This is the
// correctness property that lets matrixFromCursor append directly into a
// series' final slot instead of buffering raw rows: see matrixFromCursor's
// doc comment for the full argument.
func TestMatrixFromCursor_MultiShardBurstsCoalesceIntoOneSeries(t *testing.T) {
	t.Parallel()
	base := time.Unix(1778457600, 0).UTC()

	mkSample := func(seriesLabel string, offsetSeconds int, value float64) chclient.Sample {
		return chclient.Sample{
			MetricName: "m",
			Labels:     map[string]string{"series": seriesLabel},
			Timestamp:  base.Add(time.Duration(offsetSeconds) * time.Second),
			Value:      value,
		}
	}

	samples := []chclient.Sample{
		// shard 0 (oldest anchors): series a, then series b, each ascending.
		mkSample("a", 0, 1),
		mkSample("a", 10, 2),
		mkSample("b", 0, 10),
		mkSample("b", 10, 20),
		// shard 1 (strictly newer anchors, per the solver's disjoint
		// oldest-first slicing): series a, then series b, again.
		mkSample("a", 20, 3),
		mkSample("a", 30, 4),
		mkSample("b", 20, 30),
		mkSample("b", 30, 40),
	}

	out, err := matrixFromCursor(&orderTestCursor{samples: samples, idx: -1},
		base.Add(-time.Hour), base.Add(time.Hour), 10*time.Second)
	if err != nil {
		t.Fatalf("matrixFromCursor: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 series (one per label, coalesced across both shard bursts), got %d: %+v", len(out), out)
	}

	for _, ms := range out {
		if len(ms.Values) != 4 {
			t.Fatalf("series %v: expected 4 points (2 per shard burst), got %d: %v", ms.Metric, len(ms.Values), ms.Values)
		}
		for i := 1; i < len(ms.Values); i++ {
			prev := ms.Values[i-1][0].(float64)
			cur := ms.Values[i][0].(float64)
			if prev >= cur {
				t.Fatalf("series %v: values not strictly ascending at index %d: %v", ms.Metric, i, ms.Values)
			}
		}
	}
}

// TestMatrixFromCursor_OutOfOrderRowsWithinSeriesAreSorted directly
// exercises the defensive re-sort (sortMatrixSamplePoints) matrixFromCursor
// falls back to when a row's timestamp regresses relative to what was
// already appended for its series — the case neither RangeSeriesOrder's
// SQL ordering (route A) nor the solver's disjoint-oldest-first shard
// composition (route B) is assumed to guarantee unconditionally. Output
// order must be correct regardless.
func TestMatrixFromCursor_OutOfOrderRowsWithinSeriesAreSorted(t *testing.T) {
	t.Parallel()
	base := time.Unix(1778457600, 0).UTC()
	samples := []chclient.Sample{
		{MetricName: "m", Labels: map[string]string{"s": "x"}, Timestamp: base.Add(30 * time.Second), Value: 3},
		{MetricName: "m", Labels: map[string]string{"s": "x"}, Timestamp: base.Add(10 * time.Second), Value: 1},
		{MetricName: "m", Labels: map[string]string{"s": "x"}, Timestamp: base.Add(20 * time.Second), Value: 2},
	}

	out, err := matrixFromCursor(&orderTestCursor{samples: samples, idx: -1},
		base.Add(-time.Hour), base.Add(time.Hour), 10*time.Second)
	if err != nil {
		t.Fatalf("matrixFromCursor: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(out), out)
	}
	if len(out[0].Values) != 3 {
		t.Fatalf("expected 3 points, got %d: %v", len(out[0].Values), out[0].Values)
	}

	wantValues := []float64{1, 2, 3}
	var prevTS float64
	for i, want := range wantValues {
		ts, ok := out[0].Values[i][0].(float64)
		if !ok {
			t.Fatalf("point %d: timestamp slot is %T, want float64", i, out[0].Values[i][0])
		}
		if i > 0 && ts <= prevTS {
			t.Fatalf("point %d: timestamp %v did not increase from %v — output not sorted", i, ts, prevTS)
		}
		prevTS = ts
		got, err := strconv.ParseFloat(out[0].Values[i][1].(string), 64)
		if err != nil {
			t.Fatalf("point %d: value not parseable: %v", i, err)
		}
		if got != want {
			t.Fatalf("point %d: value = %v, want %v (output order should track ts, not arrival order)", i, got, want)
		}
	}
}

// --- peak-allocation regression guard -----------------------------------

// matrixFromCursorWholeBuffer is a FROZEN copy of matrixFromCursor as it
// existed before #1442: every row is buffered into a per-series
// []chclient.Sample map (bySeries) for the ENTIRE drain, and only once the
// cursor is exhausted does it convert that whole buffer into the returned
// []MatrixSample — so for the span of one call, the raw-row buffer and the
// final matrix are BOTH fully resident at once. It is kept here, in the
// test file only, as the allocation baseline
// TestMatrixFromCursor_AllocatesLessThanWholeBufferBaseline measures
// against; it is never called from production code.
func matrixFromCursorWholeBuffer(
	cursor chclient.Cursor,
	start, end time.Time,
) ([]MatrixSample, error) {
	type seriesState struct {
		labels map[string]string
		rows   []chclient.Sample
	}

	bySeries := map[string]*seriesState{}
	order := make([]string, 0)
	memo := newLabelMemo(0)
	for cursor.Next() {
		s := cursor.Sample()
		labels := memo.normalize(s)
		key := format.CanonicalKey(labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: labels}
			bySeries[key] = st
			order = append(order, key)
		}
		st.rows = append(st.rows, s)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)

	out := make([]MatrixSample, 0, len(bySeries))
	for _, key := range order {
		st := bySeries[key]
		for i := 1; i < len(st.rows); i++ {
			for j := i; j > 0 && st.rows[j-1].Timestamp.After(st.rows[j].Timestamp); j-- {
				st.rows[j-1], st.rows[j] = st.rows[j], st.rows[j-1]
			}
		}
		ms := MatrixSample{Metric: st.labels}
		for _, r := range st.rows {
			if r.Timestamp.Before(start) || r.Timestamp.After(end) {
				continue
			}
			appendMatrixPoint(&ms, r)
		}
		if len(ms.Values) > 0 || len(ms.Histograms) > 0 {
			out = append(out, ms)
		}
	}
	return out, nil
}

// peakAllocMatrixSamples builds a synthetic cursor's worth of rows over
// `series` distinct series, `rowsPerSeries` rows each, one shared Labels
// map per series (the interned-cursor shape both implementations are
// optimised for), grouped by series and ascending by timestamp — the shape
// RangeSeriesOrder's SQL guarantees for route A, the case this test wants
// each implementation exercised under.
func peakAllocMatrixSamples(series, rowsPerSeries int) []chclient.Sample {
	base := time.Unix(1778457600, 0).UTC()
	out := make([]chclient.Sample, 0, series*rowsPerSeries)
	for s := 0; s < series; s++ {
		lset := map[string]string{
			"route":       fmt.Sprintf("/api/%d", s),
			"method":      "GET",
			"status_code": "200",
			"instance":    fmt.Sprintf("host-%d", s),
		}
		for r := 0; r < rowsPerSeries; r++ {
			out = append(out, chclient.Sample{
				MetricName: "http_requests_total",
				Labels:     lset,
				SeriesID:   uint32(s + 1),
				Timestamp:  base.Add(time.Duration(r) * time.Second),
				Value:      float64(r),
			})
		}
	}
	return out
}

// totalAllocBytes runs f and returns the bytes runtime.MemStats.TotalAlloc
// grew by. TotalAlloc is a monotonically increasing cumulative counter
// (unaffected by GC), so this is deterministic — no wall-clock, no GC
// timing, unlike a live/retained-heap measurement.
func totalAllocBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestMatrixFromCursor_AllocatesLessThanWholeBufferBaseline is the
// deterministic, always-on proof that #1442's rewrite actually shrinks
// peak memory rather than just moving the buffering point: it compares
// TOTAL BYTES ALLOCATED (runtime.MemStats.TotalAlloc, monotonic — immune
// to GC-timing flakiness) between the production matrixFromCursor and
// matrixFromCursorWholeBuffer, a frozen copy of the pre-#1442 algorithm,
// over the identical row stream.
//
// Total allocated bytes is the right proxy here (rather than post-GC
// retained heap, which internal/api/prom/handler_streaming_alloc_test.go
// uses for a different question — see its own doc comment): both
// implementations' extra state is live for the WHOLE call and freed only
// on return, so nothing is transient garbage a mid-call GC could reclaim
// early — what one implementation allocates beyond the other, it holds for
// the entire drain. The whole-buffer baseline allocates the final matrix
// AND a full separate copy of every raw chclient.Sample row (in bySeries)
// simultaneously; the new implementation allocates only the final matrix.
func TestMatrixFromCursor_AllocatesLessThanWholeBufferBaseline(t *testing.T) {
	const seriesCount = 50
	const rowsPerSeries = 2000 // 100,000 total rows
	samples := peakAllocMatrixSamples(seriesCount, rowsPerSeries)
	start := time.Unix(1778457600, 0).UTC().Add(-time.Hour)
	end := start.Add(24 * time.Hour)

	runtime.GC()
	newBytes := totalAllocBytes(func() {
		cur := &benchSliceCursor{samples: samples, idx: -1}
		if _, err := matrixFromCursor(cur, start, end, time.Second); err != nil {
			t.Fatalf("matrixFromCursor: %v", err)
		}
	})

	runtime.GC()
	oldBytes := totalAllocBytes(func() {
		cur := &benchSliceCursor{samples: samples, idx: -1}
		if _, err := matrixFromCursorWholeBuffer(cur, start, end); err != nil {
			t.Fatalf("matrixFromCursorWholeBuffer: %v", err)
		}
	})

	t.Logf("%d series x %d rows: new=%d bytes, whole-buffer baseline=%d bytes (%.1f%% of baseline)",
		seriesCount, rowsPerSeries, newBytes, oldBytes, 100*float64(newBytes)/float64(oldBytes))

	// Generous margin (new must be under 80% of the baseline): the raw-row
	// buffer the baseline holds alongside the final matrix is eliminated
	// entirely, not merely shrunk, so the real gap is much larger than
	// this floor — the margin exists to absorb unrelated allocator noise,
	// not to approximate the expected ratio.
	const maxRatio = 0.80
	if float64(newBytes) > maxRatio*float64(oldBytes) {
		t.Fatalf("matrixFromCursor allocated %d bytes, want less than %.0f%% of the "+
			"whole-buffer baseline's %d bytes (got %.1f%%) — the rewrite should no longer "+
			"hold a full duplicate raw-row buffer alongside the final matrix",
			newBytes, maxRatio*100, oldBytes, 100*float64(newBytes)/float64(oldBytes))
	}
}
