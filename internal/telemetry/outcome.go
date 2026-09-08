package telemetry

import "net/http"

// Result label values for AttrResult.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Failure reasons for AttrErrorReason — a CLOSED enum. The set answers
// the one question a `result="error"` ratio alone cannot: whether the
// caller or the gateway is at fault, and if the gateway, which
// subsystem. A raw error string or a bare status code must never be
// folded in here; both are unbounded and would blow up the label set.
//
//   - ReasonNone            — the query succeeded; carried so ok and
//     error series share one label set.
//   - ReasonBadRequest      — the request is not answerable as written
//     (unparseable query, unsupported expression, missing parameter, a
//     query over a per-query budget). Retrying it unchanged cannot
//     help; the caller must change it.
//   - ReasonBackendUnavailable — the gateway could not reach a working
//     ClickHouse (dial failure, circuit breaker open, upstream 5xx).
//     The query is fine; the dependency is not.
//   - ReasonResourceExhausted — a per-query budget refused the work: the
//     sample budget, the wide-projection byte budget, or ClickHouse's own
//     memory-limit abort. Capacity, not correctness.
//   - ReasonTimeout         — the request ran out of time: the ClickHouse
//     max_execution_time cap, or the request's own deadline.
//   - ReasonCanceled       — the CALLER went away before the answer was
//     ready. Nothing ran out of time and nothing failed; the client simply
//     stopped waiting. It is the one error reason never worth acting on —
//     Grafana cancels every in-flight request on a panel re-render, a query
//     edit or a tab switch — and it exists so those do not inflate the
//     reasons that ARE.
//   - ReasonInternal        — a defect in cerberus itself: a recovered
//     panic or an unclassified 5xx. Always worth a page.
const (
	ReasonNone               = "none"
	ReasonBadRequest         = "bad_request"
	ReasonBackendUnavailable = "backend_unavailable"
	ReasonResourceExhausted  = "resource_exhausted"
	ReasonTimeout            = "timeout"
	ReasonCanceled           = "canceled"
	ReasonInternal           = "internal"
)

// ErrorReasons returns every member of the cerberus_error_reason closed enum,
// in a stable order.
//
// This is the ONE authoritative membership list. Before it existed the set was
// re-typed by hand in four places — the vocabulary test, the public-contract
// test, the metrics status table, and (on the request-scoped-override path) an
// accept-list that SILENTLY DROPPED anything missing from it — so adding a
// member compiled, passed, and simply went unrecorded in whichever copies were
// missed. Every consumer now derives from this function, leaving exactly one
// place where membership is stated in code and one deliberate literal pin (the
// public-contract test) that a contract change is supposed to touch.
//
// It is NOT the list of reasons ClassifyStatus can produce: ReasonCanceled,
// ReasonTimeout and ReasonResourceExhausted are reached only through
// SetReason, because no status the heads actually write identifies them.
func ErrorReasons() []string {
	return []string{
		ReasonNone,
		ReasonBadRequest,
		ReasonBackendUnavailable,
		ReasonResourceExhausted,
		ReasonTimeout,
		ReasonCanceled,
		ReasonInternal,
	}
}

// Status families for AttrStatusClass. Bounded by construction — the
// status code is collapsed to its family before it ever reaches a label.
const (
	StatusClass1xx     = "1xx"
	StatusClass2xx     = "2xx"
	StatusClass3xx     = "3xx"
	StatusClass4xx     = "4xx"
	StatusClass5xx     = "5xx"
	StatusClassUnknown = "unknown"
)

// statusCodeCeiling is one past the last status code that belongs to a
// defined family; anything at or above it is StatusClassUnknown.
const statusCodeCeiling = 600

// Outcome is the bounded classification of a finished query: the
// ok/error bucket, the failure reason, and the HTTP status family. Every
// field is drawn from a closed vocabulary, so the three of them together
// add a fixed number of label combinations to cerberus_queries_total.
type Outcome struct {
	Result      string
	Reason      string
	StatusClass string
}

// OutcomeOK is the classification of a query that finished
// successfully. Convenience for call sites that record an outcome
// without an HTTP status to inspect.
func OutcomeOK() Outcome { return ClassifyStatus(http.StatusOK) }

// ClassifyStatus maps a final HTTP status code onto the bounded Outcome
// triple.
//
// Bucketing rationale: any status >= 400 is an error because
// cerberus_queries_total{result} is a QUERY-outcome metric, not an
// HTTP-SLO metric — a 400 parse rejection IS a failed query from the
// caller's point of view. The reason dimension is what keeps that
// honest without erasing the distinction an operator has to act on: a
// 4xx says the caller sent something unanswerable, a 5xx says cerberus
// or ClickHouse failed to answer something valid. Alerting on the error
// ratio alone conflates the two.
//
// A zero status means the handler returned without ever calling
// WriteHeader, which net/http turns into an implicit 200 on the wire —
// so it classifies as one.
func ClassifyStatus(status int) Outcome {
	if status == 0 {
		status = http.StatusOK
	}
	out := Outcome{
		Result:      ResultOK,
		Reason:      ReasonNone,
		StatusClass: statusClass(status),
	}
	if status < http.StatusBadRequest {
		return out
	}
	out.Result = ResultError
	out.Reason = reasonForStatus(status)
	return out
}

// statusClass collapses a status code to its family label.
func statusClass(status int) string {
	switch {
	case status >= http.StatusContinue && status < http.StatusOK:
		return StatusClass1xx
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return StatusClass2xx
	case status >= http.StatusMultipleChoices && status < http.StatusBadRequest:
		return StatusClass3xx
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return StatusClass4xx
	case status >= http.StatusInternalServerError && status < statusCodeCeiling:
		return StatusClass5xx
	default:
		return StatusClassUnknown
	}
}

// reasonForStatus maps an error status onto the closed reason enum.
// Codes that carry a specific meaning are matched exactly; everything
// else falls back to its family — a 4xx the caller must fix, a 5xx
// cerberus must fix.
//
// This is the DEFAULT, not the whole story. Upstream wire parity collides
// distinct failures onto one status — every head answers a query
// wall-clock timeout with 503 and a per-query budget refusal with 422 —
// so a handler that knows better overrides the result through
// SetReason (reason_ctx.go). Without that override a timeout would read
// as backend_unavailable and a capacity refusal as bad_request.
func reasonForStatus(status int) string {
	switch status {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ReasonTimeout
	case http.StatusTooManyRequests, http.StatusInsufficientStorage:
		return ReasonResourceExhausted
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return ReasonBackendUnavailable
	}
	if statusClass(status) == StatusClass4xx {
		return ReasonBadRequest
	}
	return ReasonInternal
}
