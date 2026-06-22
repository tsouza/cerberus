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
// the predicate. TraceLimit <= 0 is a no-op (the emitter renders Input
// unchanged); the lowering never constructs it that way.
type SearchTraceLimit struct {
	Input           Node
	TraceIDColumn   string
	TimestampColumn string
	TraceLimit      int64
}

func (*SearchTraceLimit) planNode() {}

func (n *SearchTraceLimit) Children() []Node { return []Node{n.Input} }

func (n *SearchTraceLimit) Equal(other Node) bool {
	o, ok := other.(*SearchTraceLimit)
	if !ok {
		return false
	}
	return n.TraceIDColumn == o.TraceIDColumn &&
		n.TimestampColumn == o.TimestampColumn &&
		n.TraceLimit == o.TraceLimit &&
		n.Input.Equal(o.Input)
}
