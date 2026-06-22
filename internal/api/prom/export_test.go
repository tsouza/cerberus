package prom

// SetOnRangeDrain installs the test-observable streaming-drain hook on the
// handler. The hook fires once per /api/v1/query_range request with the
// number of rows the handler pulled off the streaming cursor
// (cursor.Inspected() after matrixFromCursor drains it) — the buffer that
// OOMed the gateway (OOM #2: matrixFromCursor buffering O(series × scanned)
// before truncating).
//
// This is the only entry point external (prom_test) tests have to read the
// streaming-path drain count, because onRangeDrain is unexported (production
// never installs a hook, keeping the hot path byte-unchanged). It mirrors the
// eager path's Result.Inspected and Tempo's SearchMetrics.InspectedTraces:
// the boundsdrain harness reads it to assert the range matrix pivot stays
// O(output) = O(series × step) rather than O(rows scanned) as the dataset /
// window / cardinality axis grows.
//
// Exposed via export_test.go so the field stays unexported in production code
// while remaining settable from the chdb-tagged regression tests, which live
// in package prom_test.
func (h *Handler) SetOnRangeDrain(fn func(int64)) {
	h.onRangeDrain = fn
}
