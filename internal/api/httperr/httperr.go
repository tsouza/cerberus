// Package httperr provides the shared HTTP-error primitives used by the
// prom / loki / tempo handlers.
//
// Each upstream API has its own JSON error envelope (Prom and Loki share
// `{status, errorType, error}`; Tempo uses `{traceID, spanID, error,
// message}`), so envelope shaping stays per-handler. What this package
// owns is the typed [Error] carrier that handlers raise through their
// internal call graph and the byte-identical [WriteJSON] response
// writer. The per-handler `respondError` shims unwrap [*Error] via
// [errors.As] and pass the status / kind / message into their own
// envelope-shaping helper.
//
// Design choice: this package intentionally does NOT own a generic
// `RespondError` that picks an envelope based on a flag, because the
// envelope is a wire-format invariant — Grafana parses each shape
// directly — and bundling all three shapes into one helper would force
// every handler to import a struct that only one of them uses.
package httperr

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// Error carries an HTTP status code plus a machine-parsable error-type
// string (Prom-vocabulary: "bad_data", "execution", "internal", ...)
// through a handler's internal error path. Handlers raise it from
// validation / execution helpers; the top-level respondError shim
// extracts it via [errors.As].
//
// The zero value is not useful; callers always construct a pointer
// literal: `&httperr.Error{Status: 400, Kind: ErrBadData, Err: err}`.
type Error struct {
	// Status is the HTTP status code to send (e.g. http.StatusBadRequest).
	Status int
	// Kind is the upstream-API error-type string. Prom and Loki use the
	// same vocabulary ("bad_data", "execution", "internal", "timeout",
	// "canceled"); Tempo doesn't surface a kind so callers pass "".
	Kind string
	// Err is the underlying error. Required.
	Err error
	// RetryAfterSeconds, when > 0, instructs the per-handler error
	// writer to stamp a `Retry-After: N` response header alongside
	// the JSON envelope. Used for the 503 fast-fail responses emitted
	// when the chclient circuit breaker is OPEN, so clients back off
	// for the breaker's recovery window instead of hammering the
	// gateway during an upstream CH outage.
	RetryAfterSeconds int
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying error so callers can use
// [errors.Is] / [errors.As] across the boundary.
func (e *Error) Unwrap() error { return e.Err }

// MarshalFailureKind is the error-type string [WriteJSON] reports when a
// response body cannot be marshalled. It is deliberately the Prom / Loki
// "internal" vocabulary word: a body that will not marshal is a defect in
// this gateway, never something the request asked for.
const MarshalFailureKind = "internal"

// marshalFailure is the envelope [WriteJSON] falls back to when the
// caller's body cannot be marshalled. Its field set is the union of the
// three heads' error shapes — `status`/`errorType`/`error` for Prom and
// Loki, `error`/`message` for Tempo — so whichever client is on the other
// end reads a populated error rather than a parse failure. Every field is
// a string, so this envelope cannot itself fail to marshal.
type marshalFailure struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

// WriteJSON writes `body` as JSON with the given HTTP status. The
// Content-Type is set to `application/json` before WriteHeader.
//
// The body is marshalled BEFORE the status line goes out. That ordering is
// the point of this function: [json.Encoder.Encode] buffers the whole value
// and only then reports a failure, so encoding straight to w commits the
// status first and discovers the problem second. A body carrying a NaN or
// ±Inf — the shape a numeric bug in any head produces — would then reach
// the client as a 200 with zero bytes, which is indistinguishable from a
// legitimately empty result. A wrong answer that presents as success is
// worse than an outage: nothing upstream can detect it. Marshalling first
// turns that class into a 500 carrying [marshalFailure] instead.
//
// Errors from the write itself stay discarded, unlike marshal errors: once
// the header is committed the status is on the wire, and a client that
// disconnected mid-body is not the server's to repair.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	// Encode, not Marshal: Encode is what the response bytes have always
	// been produced by, and it terminates the body with a newline that
	// handler-side snapshots depend on.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		writeMarshalFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeMarshalFailure reports an unmarshalable response body as a 500. It
// is reached only when [WriteJSON]'s caller handed over a value the JSON
// encoder rejects, so the status the caller asked for is discarded: that
// status described a response this gateway turned out to be unable to
// produce.
func writeMarshalFailure(w http.ResponseWriter, cause error) {
	msg := "response body could not be encoded: " + cause.Error()
	body, err := json.Marshal(marshalFailure{
		Status:    "error",
		ErrorType: MarshalFailureKind,
		Error:     msg,
		Message:   msg,
	})
	if err != nil {
		// Unreachable: marshalFailure is four strings. Fall back to a bare
		// status rather than pretending the response has a body.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(append(body, '\n'))
}
