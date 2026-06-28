package prom

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestRespondError_ResourceLimit pins that a bare resource-limit sentinel from
// a metadata drain (chclient.drainBudgetExceeded now bounds those drains) maps
// to the Prom 422, not the generic 500 — mirroring the matrix path.
func TestRespondError_ResourceLimit(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.respondError(rec, &chclient.TooManySamplesError{Limit: 5})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("TooManySamples: got %d, want 422", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.respondError(rec2, &chclient.MemoryLimitError{Limit: 1 << 30})
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("MemoryLimit: got %d, want 422", rec2.Code)
	}
	// A generic error is unchanged (500).
	rec3 := httptest.NewRecorder()
	h.respondError(rec3, errors.New("boom"))
	if rec3.Code != http.StatusInternalServerError {
		t.Fatalf("generic: got %d, want 500", rec3.Code)
	}
}
