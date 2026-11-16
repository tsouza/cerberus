package engine

import (
	"errors"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// ClassicHistogramMemoryLimitError is a ClickHouse memory-limit abort (code
// 241, *chclient.MemoryLimitError) of a statement that carries a
// classic-histogram quantile (planShapeFacts.classicHistogramQuantile). The
// quantile's stages are bounded per series and per merge, not as a whole, so
// the server's cap is what refuses one too large for it; this type lets a
// head answer that refusal as the quantile's own rather than as a generic
// memory-limit failure.
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
// *ClassicHistogramMemoryLimitError when err is a ClickHouse memory-limit
// abort and plan carries a classic-histogram quantile
// (planShapeFacts.classicHistogramQuantile); every other error, and every
// error of a plan without one, passes through unchanged. The plan is
// inspected only once a memory-limit abort is in hand, so a dispatch that
// succeeds, or fails otherwise, pays nothing for the classification.
func wrapClassicHistogramMemoryLimit(err error, plan chplan.Node) error {
	if err == nil {
		return nil
	}
	var memLimit *chclient.MemoryLimitError
	if !errors.As(err, &memLimit) {
		return err
	}
	var wrapped *ClassicHistogramMemoryLimitError
	if errors.As(err, &wrapped) || !planHasClassicHistogramQuantile(plan) {
		return err
	}
	return &ClassicHistogramMemoryLimitError{Limit: memLimit.Limit, Err: err}
}

// planHasClassicHistogramQuantile reports the classicHistogramQuantile fact
// for plan.
func planHasClassicHistogramQuantile(plan chplan.Node) bool {
	return inspectPlanShape(plan, "").classicHistogramQuantile
}

// classicHistogramCursor reports a memory-limit abort its wrapped cursor
// surfaces mid-drain as a *ClassicHistogramMemoryLimitError when its plan
// carries a classic-histogram quantile. Every other method delegates to the
// wrapped cursor, so the drain itself is unchanged.
type classicHistogramCursor struct {
	chclient.Cursor
	plan chplan.Node
}

// Err is the wrapped cursor's error, wrapped by
// wrapClassicHistogramMemoryLimit.
func (c *classicHistogramCursor) Err() error {
	return wrapClassicHistogramMemoryLimit(c.Cursor.Err(), c.plan)
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

// wrapClassicHistogramCursor returns cursor under classicHistogramCursor, so
// a memory-limit abort the drain surfaces through cursor.Err() reaches the
// caller as a *ClassicHistogramMemoryLimitError when plan carries a
// classic-histogram quantile. A nil cursor passes through unchanged.
func wrapClassicHistogramCursor(cursor chclient.Cursor, plan chplan.Node) chclient.Cursor {
	if cursor == nil {
		return nil
	}
	wrapped := classicHistogramCursor{Cursor: cursor, plan: plan}
	if composed, ok := cursor.(chclient.ComposedCursor); ok {
		return &classicHistogramComposedCursor{classicHistogramCursor: wrapped, composed: composed}
	}
	return &wrapped
}
