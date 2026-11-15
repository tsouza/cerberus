package engine

import (
	"errors"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// ClassicHistogramMemoryLimitError is a ClickHouse memory-limit abort (code
// 241, *chclient.MemoryLimitError) of a statement that carries a
// classic-histogram window or bucket-merge stage
// (planShapeFacts.classicHistogramStage). The stage's memory is bounded per
// series and per merge, not as a whole, so the server's cap is what refuses
// a stage too large for it; this type lets a head answer that refusal as the
// stage's own rather than as a generic memory-limit failure.
//
// Unwrap returns the original error, so errors.Is(err,
// chclient.ErrMemoryLimitExceeded) and errors.As against
// *chclient.MemoryLimitError hold exactly as they do for the unwrapped
// error: the breaker, the route memo's outcome classification and the
// corpus outcome token read it unchanged.
type ClassicHistogramMemoryLimitError struct {
	// Limit is the per-query cap the aborting client was configured with
	// (chclient.MemoryLimitError.Limit); 0 means a server-side limit fired.
	Limit int64
	// Err is the original error chain carrying the *chclient.MemoryLimitError.
	Err error
}

func (e *ClassicHistogramMemoryLimitError) Error() string {
	return chplan.ClassicHistogramMemoryLimitMessage + ": " + e.Err.Error()
}

// Unwrap returns the original error chain.
func (e *ClassicHistogramMemoryLimitError) Unwrap() error { return e.Err }

// wrapClassicHistogramMemoryLimit returns err as a
// *ClassicHistogramMemoryLimitError when the statement carries a
// classic-histogram stage (classicHistogramStage) and err is a ClickHouse
// memory-limit abort; every other error, and every error of a statement
// without the stage, passes through unchanged.
func wrapClassicHistogramMemoryLimit(err error, classicHistogramStage bool) error {
	if !classicHistogramStage || err == nil {
		return err
	}
	var memLimit *chclient.MemoryLimitError
	if !errors.As(err, &memLimit) {
		return err
	}
	var wrapped *ClassicHistogramMemoryLimitError
	if errors.As(err, &wrapped) {
		return err
	}
	return &ClassicHistogramMemoryLimitError{Limit: memLimit.Limit, Err: err}
}

// planHasClassicHistogramStage reports the classicHistogramStage fact for a
// plan the route-B dispatch sites hold. Route A reads the fact off the
// inspection its settings composition already runs (routeAQuerySettings);
// route B's own inspection runs inside routeBExecCtx, which returns only a
// context, so these sites inspect the plan themselves, as
// planHasTSGridNative does for the ts-grid gate.
func planHasClassicHistogramStage(plan chplan.Node) bool {
	return inspectPlanShape(plan, "").classicHistogramStage
}

// classicHistogramCursor reports a memory-limit abort its wrapped cursor
// surfaces mid-drain as a *ClassicHistogramMemoryLimitError. Every other
// method delegates to the wrapped cursor, so the drain itself is unchanged.
type classicHistogramCursor struct {
	chclient.Cursor
}

// Err is the wrapped cursor's error, wrapped by
// wrapClassicHistogramMemoryLimit.
func (c *classicHistogramCursor) Err() error {
	return wrapClassicHistogramMemoryLimit(c.Cursor.Err(), true)
}

// classicHistogramComposedCursor is classicHistogramCursor over a
// chclient.ComposedCursor, keeping its TeardownBudget visible to
// chclient.CloseCursor.
type classicHistogramComposedCursor struct {
	classicHistogramCursor
	composed chclient.ComposedCursor
}

// TeardownBudget is the wrapped composed cursor's own budget.
func (c *classicHistogramComposedCursor) TeardownBudget() time.Duration {
	return c.composed.TeardownBudget()
}

// wrapClassicHistogramCursor returns cursor under classicHistogramCursor when
// its statement carries a classic-histogram stage, so a memory-limit abort
// the drain surfaces through cursor.Err() reaches the caller as a
// *ClassicHistogramMemoryLimitError. A statement without the stage, or a nil
// cursor, passes through unchanged.
func wrapClassicHistogramCursor(cursor chclient.Cursor, classicHistogramStage bool) chclient.Cursor {
	if !classicHistogramStage || cursor == nil {
		return cursor
	}
	wrapped := classicHistogramCursor{Cursor: cursor}
	if composed, ok := cursor.(chclient.ComposedCursor); ok {
		return &classicHistogramComposedCursor{classicHistogramCursor: wrapped, composed: composed}
	}
	return &wrapped
}
