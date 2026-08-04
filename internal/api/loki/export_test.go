package loki

// SetOnQueryRangeDrain installs the test-observable eager-drain hook on the
// handler. The hook fires once per /loki/api/v1/query_range request with
// res.Inspected — the number of rows h.Engine.Query pulled from ClickHouse
// before buildRangeData pivots them into the matrix/streams wire shape.
//
// This is the only entry point external (loki_test) tests have to read the
// eager-path drain count, because onQueryRangeDrain is unexported
// (production never installs a hook, keeping the hot path byte-unchanged).
// It mirrors api/prom's SetOnRangeDrain / SetOnInstantDrain and api/tempo's
// SearchMetrics.InspectedTraces: the boundsdrain harness reads it to assert
// a LogQL metric range query (e.g. count_over_time) stays O(output) =
// O(series × step) rather than O(raw log lines matched) as the seeded log
// density grows.
//
// Exposed via export_test.go so the field stays unexported in production
// code while remaining settable from the chdb-tagged regression tests,
// which live in package loki_test.
func (h *Handler) SetOnQueryRangeDrain(fn func(int64)) {
	h.onQueryRangeDrain = fn
}
