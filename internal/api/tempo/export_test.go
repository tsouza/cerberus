package tempo

import "time"

// AlignMetricsWindowForTest re-exports alignMetricsWindow for the
// external tempo_test package — the grid-snap formula is pure and
// worth pinning directly, without driving a full handler round-trip.
var AlignMetricsWindowForTest = func(start, end time.Time, step time.Duration) (time.Time, time.Time) {
	return alignMetricsWindow(start, end, step)
}

// GroupBatchesForTest / GroupBatchesProtoForTest re-export the two
// trace-by-ID assemblers so the determinism tests can drive the pure
// assembly repeatedly (50 iterations, shuffled inputs) and compare
// serialized outputs byte-for-byte without a handler round-trip per
// iteration.
var (
	GroupBatchesForTest      = groupBatches
	GroupBatchesProtoForTest = groupBatchesProto
)
