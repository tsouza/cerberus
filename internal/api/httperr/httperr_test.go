package httperr_test

import (
	"encoding/json"
	"errors"
	"fmt"
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
