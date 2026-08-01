package tempo

import (
	"net/http"
	"strconv"

	"github.com/tsouza/cerberus/internal/chclient"
)

// HeaderInspectedSpans reports how many span ROWS cerberus drained out of
// ClickHouse to answer a search. It is the byte-and-row volume signal —
// distinct from SearchMetrics.InspectedTraces, which counts traces — and it
// is what the trace-limit / two-phase resource-bound tests assert stays
// O(output) as the seeded input scales. Tempo's wire format has no field
// for it, so it rides as a cerberus response header (and, on the gRPC
// Search RPC, as the identically-named trailer).
const HeaderInspectedSpans = "X-Cerberus-Inspected-Spans"

// SearchMetricsFor is the ONE definition of the /api/search accounting.
// Both transports call it — the HTTP handler directly, the sibling gRPC
// package through this exported name — so InspectedTraces cannot mean
// different things depending on how the caller arrived.
//
// InspectedTraces is the PRE-TRUNCATION DISTINCT TRACE COUNT: the number of
// traces the drained rows grouped into, before the response `limit` cut the
// list down. Three properties make that the right definition:
//
//   - It is a trace count, matching both the field's name and upstream
//     Tempo's own semantics, so a client or differential harness comparing
//     cerberus against reference Tempo reads the same quantity.
//   - Pre-truncation is what "inspected" means. Post-truncation it would
//     just restate len(Traces) and carry no information at all.
//   - It is transport-neutral. It is derived from the grouped summaries,
//     which both heads build with the same toTraceSummaries call, so the
//     two cannot drift.
//
// The second return is the drained span-row count. It is real information —
// it is the load-bearing signal that the trace-limit pushdown and the
// structural two-phase split actually bound the drain — so it is relocated
// to HeaderInspectedSpans rather than dropped. Previously the HTTP head
// stamped it INTO InspectedTraces while the gRPC head stamped a trace
// count there, which is exactly the divergence this function removes.
//
// summaries must be the grouped-but-not-yet-truncated slice; drained must
// be the row set those summaries were built from.
func SearchMetricsFor(summaries []TraceSummary, drained []chclient.Sample) (SearchMetrics, int) {
	return SearchMetrics{InspectedTraces: len(summaries)}, len(drained)
}

// writeInspectedSpans stamps HeaderInspectedSpans on an HTTP search
// response. Must be called before the body is written.
func writeInspectedSpans(w http.ResponseWriter, spans int) {
	w.Header().Set(HeaderInspectedSpans, strconv.Itoa(spans))
}
