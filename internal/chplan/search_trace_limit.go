package chplan

// SearchTraceLimit bounds a Tempo /api/search row source to the N newest
// traces, pushing the request's `limit` into SQL so the handler drains only
// those traces' spans instead of every matching row.
//
// It exists because /api/search groups spans into per-trace summaries in Go
// and only then truncates to `limit` (see internal/api/tempo/handler.go
// toTraceSummaries / TruncateSummaries): for a wide window the full match set
// is buffered first, OOMing the process before the limit ever bites. The
// emitter ranks traces by start time (min span Timestamp), descending, with a
// TraceId-ascending tie-break — exactly sortSummariesStartDesc's order — and
// keeps the top TraceLimit, so the SQL-selected set equals the set
// TruncateSummaries would have kept (no wire-shape change, just a bounded
// drain).
//
// The node wraps the lowered plain-search row source — a bare Scan (`{}`) or
// Filter(Scan) (`{ <matchers> }`), with the request time window folded into
// the predicate. Its trace-ID and timestamp input drivers are the uniquely
// named RoleTraceID and RoleTimestamp columns in Input.RowType; they are not
// properties of this pass-through node. The emitter rejects an open or
// ambiguous input schema, a missing or unnamed driver, and TraceLimit <= 0.
// Lowering only constructs the node with a closed schema and a positive limit.
//
// L1 (fat-trace): the node bounds the trace COUNT, not the spans-per-trace.
// A single trace with millions of matched spans still drains every matched
// span into the handler's per-trace SpanSet. This per-trace span fan-out is
// intentionally uncapped per the Matched parity contract: SpanSet.Matched is
// the uncapped total matched-span count, which the tempo compatibility differ
// (compatibility/tempo/driver/differ.go compareSpanSets) asserts byte-exact
// against reference Tempo — an SQL `LIMIT k BY TraceId` would cap the rows the
// handler sees and so cap Matched, breaking that parity. Process memory for a
// pathological fat trace is instead bounded by chclient.MaxQuerySamples (the
// query.maxSamples knob): crossing it aborts the row drain with a 422
// TooManySamplesError rather than growing the heap without limit. The fix is
// the same row-drain backstop a wide fat result set leans on — not a distinct
// unbounded path — so the trace-count bound deliberately leaves it alone.
type SearchTraceLimit struct {
	Input      Node
	TraceLimit int64
}

func (*SearchTraceLimit) planNode() {}

func (n *SearchTraceLimit) Children() []Node { return []Node{n.Input} }

func (n *SearchTraceLimit) Equal(other Node) bool {
	o, ok := other.(*SearchTraceLimit)
	if !ok {
		return false
	}
	return n.TraceLimit == o.TraceLimit &&
		n.Input.Equal(o.Input)
}
