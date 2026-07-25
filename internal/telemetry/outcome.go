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
//   - ReasonResourceExhausted — the server refused for capacity
//     reasons: rate limited, out of storage. Capacity, not
//     correctness.
//   - ReasonTimeout         — the request ran out of time, on either
//     side of the gateway.
//   - ReasonInternal        — a defect in cerberus itself: a recovered
//     panic or an unclassified 5xx. Always worth a page.
const (
	ReasonNone               = "none"
	ReasonBadRequest         = "bad_request"
	ReasonBackendUnavailable = "backend_unavailable"
	ReasonResourceExhausted  = "resource_exhausted"
	ReasonTimeout            = "timeout"
	ReasonInternal           = "internal"
)

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
