package prom

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestClassifyEngineError_EmittedSQLTooLargeIs422 pins the wire status of
// cerberus issue #2733's rejection: a query whose plan composes to more SQL
// than ClickHouse will parse is 422 errorType=execution — the "valid query,
// cannot be served" class the sample budget, the memory-limit abort and every
// throwIf resource guard already land in — not the 500 the emit stage's
// catch-all would otherwise give it.
//
// 500 would be wrong twice over: it reports a user's exotic query as a
// cerberus fault (paging an operator, and counting against the error-rate
// SLO), and it contradicts the status this same query returned before issue
// #2728 opened the composition arm it now rides, when the identical shape was
// refused at lowering — a 422.
func TestClassifyEngineError_EmittedSQLTooLargeIs422(t *testing.T) {
	t.Parallel()

	// The engine tags each stage's failure with an "engine: <stage>:" prefix;
	// this is exactly what a chsql.Emit rejection arrives as.
	err := classifyEngineError(fmt.Errorf("engine: emit: %w", chsql.ErrEmittedSQLTooLarge))

	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("classifyEngineError returned %T (%v), want *apiError", err, err)
	}
	if ae.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (422 execution — the query is valid but unserveable)",
			ae.Status, http.StatusUnprocessableEntity)
	}
	if ae.Kind != ErrExecution {
		t.Errorf("errorType = %q, want %q", ae.Kind, ErrExecution)
	}
	if !errors.Is(ae.Err, chsql.ErrEmittedSQLTooLarge) {
		t.Errorf("the wire error dropped the sentinel, so the message no longer names the "+
			"composition that was refused: %v", ae.Err)
	}
}

// TestClassifyEngineError_OtherEmitFailuresStay500 is the other half of the
// pin: only the size bound moves. An emit failure that is a genuine cerberus
// defect — an unsupported node the emitter cannot render — must keep reporting
// itself as one, or the new 422 arm would quietly reclassify every emitter bug
// as a user error.
func TestClassifyEngineError_OtherEmitFailuresStay500(t *testing.T) {
	t.Parallel()

	err := classifyEngineError(fmt.Errorf("engine: emit: %w: some node", chsql.ErrUnsupported))

	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("classifyEngineError returned %T (%v), want *apiError", err, err)
	}
	if ae.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d — an unsupported-node emit failure is a cerberus defect",
			ae.Status, http.StatusInternalServerError)
	}
}
