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
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying error so callers can use
// [errors.Is] / [errors.As] across the boundary.
func (e *Error) Unwrap() error { return e.Err }

// WriteJSON writes `body` as JSON with the given HTTP status. The
// Content-Type is set to `application/json` before WriteHeader, and the
// encoder's error is intentionally discarded — at that point the status
// is already on the wire and there's nothing the caller can do.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
