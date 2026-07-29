package httperr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/httperr"
)

// TestError_ErrorAndUnwrap — *Error.Error() returns the wrapped
// message, and Unwrap exposes the underlying error so errors.As works
// across a wrapping boundary.
func TestError_ErrorAndUnwrap(t *testing.T) {
	t.Parallel()

	inner := errors.New("boom")
	e := &httperr.Error{Status: http.StatusBadRequest, Kind: "bad_data", Err: inner}

	if got := e.Error(); got != "boom" {
		t.Errorf("Error(): got %q, want %q", got, "boom")
	}
	if got := errors.Unwrap(e); got != inner {
		t.Errorf("Unwrap(): got %v, want %v", got, inner)
	}

	// errors.As through a wrapping fmt.Errorf chain.
	wrapped := fmt.Errorf("context: %w", e)
	var asErr *httperr.Error
	if !errors.As(wrapped, &asErr) {
		t.Fatalf("errors.As: did not match *Error in wrapped chain")
	}
	if asErr.Status != http.StatusBadRequest || asErr.Kind != "bad_data" {
		t.Errorf("recovered Error fields: got status=%d kind=%q, want 400/bad_data",
			asErr.Status, asErr.Kind)
	}
}

// TestWriteJSON_ShapeAndHeader — WriteJSON sets Content-Type before the
// status line, writes the status, and emits the body using the standard
// JSON encoder (which terminates with a newline).
func TestWriteJSON_ShapeAndHeader(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httperr.WriteJSON(rec, http.StatusTeapot, map[string]any{
		"status": "error", "errorType": "bad_data", "error": "nope",
	})

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusTeapot {
		t.Fatalf("status: got %d, want %d", res.StatusCode, http.StatusTeapot)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "error" || body["errorType"] != "bad_data" || body["error"] != "nope" {
		t.Errorf("body: %+v", body)
	}
}

// TestWriteJSON_TrailingNewline — json.Encoder.Encode appends a "\n";
// the existing handler-side snapshots depend on that, so lock it in.
func TestWriteJSON_TrailingNewline(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httperr.WriteJSON(rec, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{OK: true})

	if !strings.HasSuffix(rec.Body.String(), "\n") {
		t.Errorf("body: missing trailing newline, got %q", rec.Body.String())
	}
}

// TestWriteJSON_UnmarshalableBodyIsNotASuccess pins the failure mode that
// makes marshal-before-header worth the buffer: a body carrying a NaN or ±Inf
// cannot be encoded, and encoding straight to the ResponseWriter commits the
// caller's status BEFORE that is discovered. The client then reads a 200 with
// zero bytes, which is exactly what a legitimately empty result looks like —
// so a numeric bug in any head ships as a successful query returning nothing.
func TestWriteJSON_UnmarshalableBodyIsNotASuccess(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			httperr.WriteJSON(rec, http.StatusOK, map[string]any{
				"series": []map[string]any{{"value": tc.value}},
			})

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode == http.StatusOK {
				t.Fatalf("a body that cannot be encoded was reported as success (200); "+
					"body was %q", rec.Body.String())
			}
			if res.StatusCode != http.StatusInternalServerError {
				t.Errorf("status: got %d, want %d", res.StatusCode, http.StatusInternalServerError)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}

			// The envelope has to carry the fields every head's client reads:
			// status/errorType/error for Prom and Loki, error/message for Tempo.
			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatalf("the failure envelope is not itself valid JSON: %v", err)
			}
			if body["status"] != "error" || body["errorType"] != httperr.MarshalFailureKind {
				t.Errorf("envelope: got %+v", body)
			}
			for _, field := range []string{"error", "message"} {
				msg, _ := body[field].(string)
				if !strings.Contains(msg, "could not be encoded") {
					t.Errorf("envelope %q does not say what went wrong: %q", field, msg)
				}
			}
		})
	}
}

// TestWriteJSON_MarshalFailureLeavesNoPartialBody guards the other half of the
// ordering: nothing of the rejected value may reach the wire. Encoding into the
// ResponseWriter would emit whatever prefix serialised before the bad value,
// producing a truncated document that a decoder reports as a parse error rather
// than as the server error it is.
func TestWriteJSON_MarshalFailureLeavesNoPartialBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httperr.WriteJSON(rec, http.StatusOK, map[string]any{
		"marker": "must-not-appear",
		"value":  math.NaN(),
	})

	if strings.Contains(rec.Body.String(), "must-not-appear") {
		t.Errorf("a prefix of the rejected body reached the wire: %q", rec.Body.String())
	}
}
