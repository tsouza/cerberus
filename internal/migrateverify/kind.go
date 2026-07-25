package migrateverify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// Result kinds name the SHAPE a corpus entry returns, and therefore which
// comparator judges it. They are the SAME tokens that appear in
// OutOfScopeEntry.Kind: an entry either has a comparator for its shape or is
// reported out of scope by that shape's name, and one vocabulary covers both.
//
// They name what a query RETURNS, and are distinct from
// migrate.CorpusQuery.Kind, which names its PROVENANCE (record / alert / panel).
const (
	// KindMetricMatrix: a labelled sample matrix — PromQL, LogQL metric queries
	// and TraceQL metrics queries all land here, and one comparator judges them.
	KindMetricMatrix = "metric-matrix"
	// KindLogStream: a LogQL query that returns log lines, not a metric matrix.
	KindLogStream = "log-stream"
	// KindTraceSearch: a TraceQL query that returns trace summaries, not a metric matrix.
	KindTraceSearch = "trace-search"
	// KindTraceByID: a single-trace fetch, DERIVED from a trace-search result
	// (never a top-level corpus entry — no query language names a trace ID). It
	// returns the whole trace as a span set.
	KindTraceByID = "trace-by-id"
	// KindTagDiscovery: a tag/label-name or tag/label-value enumeration probe,
	// DERIVED from the corpus's own matchers and attribute references (never a
	// top-level corpus entry). It returns a set.
	KindTagDiscovery = "tag-discovery"
	// KindMetricsCompare: a TraceQL compare() query, whose baseline-vs-selection
	// attribute split is not a comparable metric matrix.
	KindMetricsCompare = "metrics-compare"
	// KindUnparseable: the head's own parser rejected the expression, so its
	// shape cannot be classified at all.
	KindUnparseable = "unparseable"
	// KindUnknownLang: the corpus entry names a query language this build has no
	// parity lane for.
	KindUnknownLang = "unknown-lang"
)

// Comparison units name what a family's evidence counter COUNTS, so a report row
// can never read "412 series compared" about a lane that diffed log lines. They
// are part of the report's wire vocabulary — a consumer reads FamilySummary.Unit
// to word its own message — so they are exported alongside the kind tokens.
const (
	UnitSeries     = "series"
	UnitLogEntries = "log entries"
	UnitTraces     = "traces"
	UnitSpans      = "spans"
	UnitTagValues  = "tag values"
	// UnitComparisons is the shape-agnostic noun, used when a report attributed no
	// shape to a head's replays and nothing says what its units were.
	UnitComparisons = "comparison units"
)

// KindBackend fetches ONE non-matrix comparison unit. It sits ALONGSIDE Backend
// rather than replacing it: the matrix path keeps its one-method contract
// byte-for-byte, so every existing mock, helper and test is untouched, and a
// mock built for one kind is never forced to answer for a kind it does not
// serve.
type KindBackend interface {
	Fetch(ctx context.Context, q Query, p Params) (FetchResult, error)
}

// FetchResult is one non-matrix fetch's transport outcome. Status carries the
// HTTP code so the unsupported-vs-error classification is the SAME rule the
// matrix path applies; Payload is the kind's decoded body, opaque to the driver
// and type-asserted only inside that kind's own comparator.
type FetchResult struct {
	Status  int
	Payload any
}

// KindDialect is one (head, result-kind) wire contract: where the endpoint
// lives, how the probe is encoded, and how a 200 body decodes into that kind's
// payload. It is the non-matrix sibling of Dialect and, like Dialect, carries
// DATA only — auth, the response-size cap, credential redaction and the non-200
// rule stay written exactly once in HTTPBackend.
type KindDialect interface {
	// Kind is the result kind this dialect fetches.
	Kind() string
	// Head is the head token this dialect speaks for.
	Head() string
	// Path is the endpoint appended to the backend base URL for q.
	Path(q Query) string
	// Encode builds the query-string parameters for q over p.
	Encode(q Query, p Params) url.Values
	// Header is the per-kind request header set, or nil when the kind needs none.
	Header() http.Header
	// Decode parses a 200 body into this kind's payload. A decode error means
	// the body was not a usable response for this kind at all.
	Decode(body []byte) (any, error)
}

// Limitation is a dimension a comparison could NOT judge, stated in the
// operator's words with an exact count.
//
// It is the OPPOSITE of an allow-list. An allow-list hides a failure by
// declaring it acceptable; a Limitation declares that NO verdict was reached on
// a named dimension and says how much of the result that covers. It never
// suppresses a divergence: every dimension a comparator CAN judge is still
// judged and still blocks.
type Limitation struct {
	Code   string `json:"code"`
	Count  int    `json:"count"`
	Detail string `json:"detail"`
}

// Limitation codes. Each names one dimension a comparator declines to judge and
// says so with a count, so an operator reads how much of the result the
// statement covers rather than a bare "some things were not compared".
const (
	// LimitLogEntryOrder: entry order within a stream and stream order across the
	// response are not a wire contract on both sides.
	LimitLogEntryOrder = "log-entry-order"
	// LimitLogStructuredMeta: the two backends' categorized encodings are
	// structurally different objects, so per-entry structured metadata is not
	// requested and not compared.
	LimitLogStructuredMeta = "log-structured-metadata"
	// LimitLogTruncationBand: entries at or below the truncation boundary, where
	// the two results cover different depths.
	LimitLogTruncationBand = "log-truncation-band"
	// LimitSearchTruncated: a trace search cut at the replay limit on at least one
	// side, so neither result is the complete answer to the query and set
	// membership is not a judgeable dimension.
	LimitSearchTruncated = "trace-search-truncated"
	// LimitSpanSetCapped: a matched spanset cut at the spans-per-set cap, where
	// upstream leaves unspecified WHICH matched spans survive.
	LimitSpanSetCapped = "trace-search-spanset-capped"
	// LimitSearchRefPartial: a search the REFERENCE itself reported as partial, so
	// the traces it did not return are not evidence of absence.
	LimitSearchRefPartial = "trace-search-reference-partial"
	// LimitSpanAttrValueType: a span attribute whose reference type is one the
	// OTel-CH string-map carrier cannot round-trip (double / array / kvlist /
	// bytes), so its VALUE is not compared — only that both sides carry the key.
	LimitSpanAttrValueType = "span-attribute-value-type"
	// LimitTagDiscoveryRefPartial: a tag/tag-value enumeration the REFERENCE
	// itself reported as partial, so the values it did not return are not
	// evidence of absence.
	LimitTagDiscoveryRefPartial = "tag-discovery-reference-partial"
)

// limitationDetails is the canonical, invariant explanation for each limitation
// code. A per-query Limitation may append the specifics of that one comparison
// (the truncation boundary it hit); the roll-up merges by code and uses the
// canonical text, so a summary line never quotes one query's boundary as if it
// held for every query behind the count.
var limitationDetails = map[string]string{
	LimitLogEntryOrder: "entry order within a stream and stream order across the response were not compared: " +
		"cerberus renders every stream ascending regardless of the requested direction and builds the stream list " +
		"from a Go map, while reference Loki renders in direction order and sorts streams by label string. " +
		"Order is not a wire contract on both sides, so the comparison is defined on the entry multiset instead.",
	LimitLogStructuredMeta: "per-entry structured metadata was not compared: the two backends' categorized encodings " +
		"are structurally different objects (reference Loki omits an empty structuredMetadata and can carry a parsed " +
		"sub-object; cerberus always emits the key and never emits parsed), so the replay does not request " +
		"categorization and every entry is judged on (labels, timestamp, line).",
	LimitLogTruncationBand: "entries at or below the truncation boundary were not judged: both results were cut at " +
		"the replay limit, the two backends break a timestamp tie differently (reference Loki on streamHash then " +
		"labels, cerberus on ClickHouse row order), and below the boundary the two results cover different depths.",
	LimitSearchTruncated: "trace-set membership was not judged: at least one side returned exactly the replay limit " +
		"of summaries, so each result is a PREFIX of a ranking neither backend's wire contract fixes (cerberus ranks " +
		"newest-first by trace start time; reference Tempo returns what its block search reached before the limit), " +
		"and a trace returned by only one of them can be a legitimate ranking difference rather than a defect. The " +
		"count is the number of trace IDs exactly one backend returned; every trace BOTH returned was still " +
		"field-diffed, and a field difference there still blocks.",
	LimitSpanSetCapped: "which matched spans survive the spans-per-set cap is unspecified upstream, so a capped " +
		"spanset was judged on its uncapped matched total and its kept-span count only, never on which spans it " +
		"kept. The count is the number of traces whose spanset was capped on at least one side.",
	LimitSearchRefPartial: "the reference reported its OWN search as partial (it completed fewer jobs than it " +
		"started), so the traces it did not return are not evidence of absence and set membership was not judged " +
		"against cerberus's complete answer. The count is the number of searches the reference answered partially; " +
		"the traces both backends returned were still field-diffed.",
	LimitSpanAttrValueType: "an attribute's VALUE was not compared: the OTel-CH string-map carrier erases a " +
		"non-string attribute's original type, so a reference double / array / kvlist / bytes value cannot be " +
		"round-tripped against cerberus's always-string rendering. The attribute's KEY is still compared, so a " +
		"missing attribute is still a divergence; only its value is declined. The count is the number of " +
		"attributes this covers.",
	LimitTagDiscoveryRefPartial: "the reference reported its OWN tag/tag-value enumeration as partial (it " +
		"completed fewer jobs than it started), so the values it did not return are not evidence of absence and " +
		"set membership was not judged against cerberus's complete answer. The count is the number of probes the " +
		"reference answered partially.",
}

// limitation builds one limitation from its code and count, carrying the
// canonical explanation for that code.
func limitation(code string, count int) Limitation {
	return Limitation{Code: code, Count: count, Detail: limitationDetails[code]}
}

// mergeLimitations folds add into dst by code, summing counts and keeping the
// canonical per-code detail, and returns the result in code-sorted order. The
// sort is what keeps a report byte-deterministic across runs: limitations arrive
// in comparator order, which varies with the corpus, and the report's
// determinism is a pinned contract.
func mergeLimitations(dst, add []Limitation) []Limitation {
	if len(dst) == 0 && len(add) == 0 {
		// Nil, not an empty slice: the field is omitempty, so an empty slice would
		// marshal away and come back nil, and a report would stop round-tripping to
		// itself.
		return nil
	}
	byCode := make(map[string]int, len(dst)+len(add))
	for _, l := range dst {
		byCode[l.Code] += l.Count
	}
	for _, l := range add {
		byCode[l.Code] += l.Count
	}
	codes := make([]string, 0, len(byCode))
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := make([]Limitation, 0, len(codes))
	for _, c := range codes {
		out = append(out, limitation(c, byCode[c]))
	}
	return out
}

// Outcome is what EVERY comparator returns, so the single verdict switch
// consumes all kinds identically.
type Outcome struct {
	Verdict   Verdict
	FirstDiff *FirstDiff
	// Detail explains a verdict the counts alone cannot: a payload the
	// comparator could not read. It is empty on a plain match or divergence,
	// whose story the FirstDiff already tells.
	Detail string
	// Compared is the evidence count in this kind's unit — the number of
	// comparison units the comparator actually put side by side. A verdict with
	// Compared == 0 proves nothing, whatever it says.
	Compared int
	// Limitations names the dimensions this comparison could NOT judge.
	Limitations []Limitation
	// Derived are follow-on comparison units this comparison produced. Replayed
	// in a second phase through the same accounting. Never serialized — the
	// report carries the resulting QueryResults, not the plan that produced them.
	Derived []Query
}

// kindHandler is the closed set of behaviours one result kind needs. It is the
// ONLY extension point a new shape touches; Verify's loop and the verdict switch
// never grow a branch.
type kindHandler struct {
	kind    string
	unit    string
	compare func(q Query, ref, cerberus any, p Params) Outcome
}

// kindHandlers maps a result kind onto the comparator that judges it. A query
// routed to a kind with no entry here is an ERROR, never a silent pass: the gate
// cannot claim parity for a shape it has no definition of equality for.
var kindHandlers = map[string]kindHandler{
	KindLogStream:    {kind: KindLogStream, unit: UnitLogEntries, compare: compareLogStreams},
	KindTraceSearch:  {kind: KindTraceSearch, unit: UnitTraces, compare: compareTraceSearch},
	KindTraceByID:    {kind: KindTraceByID, unit: UnitSpans, compare: compareTraceByID},
	KindTagDiscovery: {kind: KindTagDiscovery, unit: UnitTagValues, compare: compareTagDiscovery},
}

// kindUnit returns the comparison unit a result kind counts in. The matrix kind
// is not in kindHandlers — it runs on the Backend contract, not KindBackend — so
// its unit is named here alongside the rest rather than duplicated at each call
// site.
func kindUnit(kind string) string {
	if h, ok := kindHandlers[kind]; ok {
		return h.unit
	}
	return UnitSeries
}

// verifyKind runs the parity check for one non-matrix comparison unit. The
// transport classification is deliberately the SAME rule the matrix path
// applies — a cerberus 4xx is an honest "I can't serve this" (unsupported,
// non-blocking), any other non-200 from either side is a broken backend
// (error) — so a lane's verdict never depends on which comparator judged it.
func verifyKind(ctx context.Context, q Query, lane Lane, p Params) (QueryResult, []Query) {
	out := QueryResult{Head: q.Head, Kind: q.Kind, Source: q.Source, Expr: q.Expr}
	h, ok := kindHandlers[q.Kind]
	if !ok {
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("no comparator is registered for result kind %q, so this query was never judged", q.Kind)
		return out, nil
	}

	refRes, refErr := lane.RefKind.Fetch(ctx, q, p)
	cerRes, cerErr := lane.CerberusKind.Fetch(ctx, q, p)

	switch {
	case refErr != nil:
		out.Verdict, out.Detail = VerdictError, fmt.Sprintf("reference request failed: %v", refErr)
	case cerErr != nil:
		out.Verdict, out.Detail = VerdictError, fmt.Sprintf("cerberus request failed: %v", cerErr)
	case cerRes.Status != http.StatusOK && !isClientReject(cerRes.Status):
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("cerberus returned status=%d (backend error, not a query rejection)", cerRes.Status)
	case cerRes.Status != http.StatusOK:
		out.Verdict = VerdictUnsupported
		out.Detail = fmt.Sprintf("cerberus returned status=%d for a %s query", cerRes.Status, q.Kind)
	case refRes.Status != http.StatusOK:
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("reference returned status=%d (no baseline to compare)", refRes.Status)
	default:
		oc := h.compare(q, refRes.Payload, cerRes.Payload, p)
		out.Verdict = oc.Verdict
		out.FirstDiff = oc.FirstDiff
		out.ComparedUnits = oc.Compared
		out.Limitations = oc.Limitations
		if oc.Detail != "" {
			out.Detail = oc.Detail
		}
		return out, oc.Derived
	}
	return out, nil
}
