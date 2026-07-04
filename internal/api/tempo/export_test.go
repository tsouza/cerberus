package tempo

import "time"

// AlignMetricsWindowForTest re-exports alignMetricsWindow for the
// external tempo_test package — the grid-snap formula is pure and
// worth pinning directly, without driving a full handler round-trip.
var AlignMetricsWindowForTest = func(start, end time.Time, step time.Duration) (time.Time, time.Time) {
	return alignMetricsWindow(start, end, step)
}

// PostProcessCompareForTest / CompareAnchorGridForTest re-export the
// compare() BaselineAggregator mirror so its top-N / totals / zero-fill
// semantics can be pinned directly on synthetic row streams without a
// handler round-trip.
var (
	PostProcessCompareForTest = postProcessCompare
	CompareAnchorGridForTest  = compareAnchorGrid
)

// GroupBatchesForTest / GroupBatchesProtoForTest re-export the two
// trace-by-ID assemblers so the determinism tests can drive the pure
// assembly repeatedly (50 iterations, shuffled inputs) and compare
// serialized outputs byte-for-byte without a handler round-trip per
// iteration.
var (
	GroupBatchesForTest      = groupBatches
	GroupBatchesProtoForTest = groupBatchesProto
)

// SetDrainBytesLimitForTest sets the wide-projection byte ceiling used by a
// search drain, so a test can trip the fail-closed gate on a modest fixture.
func (h *Handler) SetDrainBytesLimitForTest(n int64) { h.drainBytesLimit = n }
