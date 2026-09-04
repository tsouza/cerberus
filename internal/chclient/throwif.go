package chclient

import (
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chCodeThrowIf is ClickHouse's FUNCTION_THROW_IF_VALUE_IS_NON_ZERO server
// error code (ErrorCodes.cpp: 395) — raised whenever any emitted
// `throwIf(...)` guard fires, regardless of which one. The code alone
// cannot say WHICH guard fired (every throwIf cerberus emits shares it);
// ThrowIfError.Message carries the exact text the guard's own
// `throwIf(cond, '<message>')` literal passed, which callers match against
// their own known message constants.
const chCodeThrowIf = 395

// chCodeTimeSeriesThrowDuplicateSeriesIf is ClickHouse's
// CANNOT_EXECUTE_PROMQL_QUERY server error code (ErrorCodes.cpp: 768) —
// raised by `timeSeriesThrowDuplicateSeriesIf` (chopt.
// FeatureTSThrowDuplicateSeriesIf, cerberus issue #3038), the duplicate-
// labelset guard's collector-backed variant. Verified directly against a
// real ClickHouse 26.2+ server: distinct from chCodeThrowIf (a DIFFERENT
// function, a DIFFERENT code — timeSeriesThrowDuplicateSeriesIf is not a
// throwIf call), and distinct from the code a bad
// timeSeriesGroupToTags/timeSeriesTagsToGroup argument raises (36,
// BAD_ARGUMENTS), so matching on this code alone cannot catch an unrelated
// tag-group-function fault. ThrowIfError.Message carries ClickHouse's own
// rendered text, `Multiple series have the same tags <tags>, duplicate
// series in the same result set are not allowed` (chplan.
// DuplicateSeriesTagsMessagePrefix names the stable prefix) — unlike every
// other ThrowIfError this package wraps, this message's `<tags>` portion is
// NOT a cerberus-supplied compile-time literal, so a caller that wants the
// real colliding tags must extract rather than merely match-and-restate it.
const chCodeTimeSeriesThrowDuplicateSeriesIf = 768

// ThrowIfError is the concrete error chclient surfaces when ClickHouse
// aborts a query via a deliberate, emitter-planted guard — either a
// `throwIf(...)` call (code 395) or `timeSeriesThrowDuplicateSeriesIf`
// (code 768, its collector-backed sibling for the duplicate-labelset
// guard) — cerberus's own resource-bound / shape-fault rejections
// (histogram merge budget, rate-window fanout budget, info() conflicting
// label, duplicate labelset, many-to-many match, …), never a genuine
// backend failure. It wraps the underlying *clickhouse.Exception (errors.As
// still reaches it) and carries the raw, driver-decoded Message so callers
// can match it against their own message constants — comparing typed,
// already-decoded text, never re-parsing err.Error()'s ambient formatting.
//
// That formatting genuinely differs by execution path: chdb-go (the
// embedded engine cerberus's own chdb-tagged tests run against) renders
// exceptions CLI-style, "Code: 395. DB::Exception: <message>: while
// executing …", while clickhouse-go/v2's native TCP driver (what chclient
// uses in production) decodes the bare wire-protocol exception and renders
// it as "code: %d, message: %s" — no "DB::Exception: " substring at all.
// Every guard classifier that string-matched err.Error() against a single
// assumed prefix was silently unmatchable on whichever path it wasn't
// written against (caught investigating #2429: the histogram-merge and
// info-conflict guards, both pre-existing, had only ever been exercised
// against chdb). Decoding via the typed *clickhouse.Exception once, here,
// removes the format entirely from the equation for every caller.
type ThrowIfError struct {
	// Message is the exact text ClickHouse decoded for the exception —
	// the guard's own message constant (e.g.
	// chplan.HistogramMergeBudgetMessage or
	// chsql.RateWindowFanoutBudgetMessage), or ClickHouse's own rendered
	// text for the code-768 variant — followed by ClickHouse's own
	// "while executing 'FUNCTION ...(...)" trailer, so callers match
	// with strings.HasPrefix against their own constant, not equality.
	Message string
	// Cause is the underlying ClickHouse error — the *clickhouse.Exception
	// carrying code 395 or code 768.
	Cause error
}

func (e *ThrowIfError) Error() string {
	return fmt.Sprintf("chclient: query aborted: %s", e.Message)
}

// Unwrap exposes the underlying ClickHouse exception (for errors.As
// against *clickhouse.Exception).
func (e *ThrowIfError) Unwrap() error { return e.Cause }

// isEmittedGuardCode reports whether code is one of the two ClickHouse
// error codes cerberus's own emitted guards raise — chCodeThrowIf (395) or
// chCodeTimeSeriesThrowDuplicateSeriesIf (768). Both wrap into the SAME
// *ThrowIfError shape: from a caller's side "a deliberate, emitter-planted
// guard aborted this query" is one class of event whichever function
// raised it, and every existing consumer of ThrowIfError / ThrowIfMessage
// (the resource-bound route-outcome classifier, the prom handler's guard
// classifier) already matches on the DECODED MESSAGE, not the code, so
// widening what wraps costs them nothing.
func isEmittedGuardCode(code int32) bool {
	return code == chCodeThrowIf || code == chCodeTimeSeriesThrowDuplicateSeriesIf
}

// wrapThrowIf converts a raw driver error into a *ThrowIfError when (and
// only when) the error chain carries a ClickHouse exception with a code
// [isEmittedGuardCode] recognises. Every other error passes through
// untouched.
//
// Detection is typed — errors.As against *clickhouse.Exception, never
// string matching — so a query whose result data happens to contain a
// guard's literal message text cannot be misclassified, mirroring
// wrapMemoryLimit's own reasoning for code 241.
func wrapThrowIf(err error) error {
	if err == nil {
		return nil
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && isEmittedGuardCode(ex.Code) {
		return &ThrowIfError{Message: ex.Message, Cause: err}
	}
	return err
}

// ThrowIfMessage returns the guard message ClickHouse decoded for an
// emitted guard rejection (throwIf's code 395 or
// timeSeriesThrowDuplicateSeriesIf's code 768), and whether err is one at
// all.
//
// It accepts BOTH shapes the same rejection wears depending on which dial
// produced it and how far up the stack the caller sits: the wrapped
// *ThrowIfError the row path's classifyDriverErr builds, and the bare
// *clickhouse.Exception the columnar (ch-go) path yields once normalised
// through translateCHGoErr. Callers outside this package classify guard
// rejections without having to know which dial ran the query — see
// internal/engine's route-outcome classifier, which must recognise a
// resource-bound rejection identically on both paths or the failure-driven
// route memo learns from one and not the other.
func ThrowIfMessage(err error) (string, bool) {
	var te *ThrowIfError
	if errors.As(err, &te) {
		return te.Message, true
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && isEmittedGuardCode(ex.Code) {
		return ex.Message, true
	}
	return "", false
}
