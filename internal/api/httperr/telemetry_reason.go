package httperr

import (
	"context"
	"errors"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// TelemetryReason names the `cerberus_error_reason` label for the two
// failure classes an HTTP status alone cannot express, and "" for
// everything else (meaning: the status-derived classification in
// telemetry.ClassifyStatus is already right).
//
// It lives here, next to DistributedShardUnavailableMessage, for the same
// reason that message does: all three heads must render the same fact the
// same way, and a per-head copy of this sentinel list is exactly how they
// would drift.
//
// WHY A STATUS IS NOT ENOUGH. Upstream wire parity collides two pairs of
// distinct failures onto one status each, and cerberus cannot move either
// without breaking the differential harnesses:
//
//   - A query wall-clock timeout answers **503** on every head, because
//     upstream Prometheus and Loki answer their own `query.timeout` that
//     way. Status-derived, it reads as `backend_unavailable` —
//     indistinguishable from a real ClickHouse outage, on the one counter
//     an operator pages from. ClickHouse is healthy when it aborts an
//     over-long query; the chclient breaker treats code 159 as a success
//     for exactly that reason.
//   - A per-query budget refusal answers **422**. Status-derived, it reads
//     as `bad_request` — indistinguishable from a query cerberus cannot
//     parse or lower, which is also a 422 and is a genuinely different
//     thing to act on.
//
// The handler that already classified the error is therefore the one that
// says which it was; see internal/telemetry/reason_ctx.go for the
// plumbing.
func TelemetryReason(err error) string {
	if err == nil {
		return ""
	}
	// An Error whose constructor already classified the failure wins: it
	// saw the sentinel before restating the message in the upstream's
	// wording, which is exactly the step that loses the sentinel.
	var carrier *Error
	if errors.As(err, &carrier) && carrier.Reason != "" {
		return carrier.Reason
	}
	switch {
	// The ClickHouse server-side max_execution_time abort (TIMEOUT_EXCEEDED,
	// code 159) and the request's own deadline. A plain context.Canceled —
	// the client walking away — is deliberately NOT a timeout: nothing ran
	// out of time, the caller stopped waiting.
	case errors.Is(err, chclient.ErrQueryTimeout), errors.Is(err, context.DeadlineExceeded):
		return telemetry.ReasonTimeout
	// The three per-query budgets: the row-count sample budget, the
	// wide-projection drain-byte budget, and ClickHouse's own memory-limit
	// abort (code 241). Exactly the set the Tempo head already folds into
	// ErrClassResourceExhausted.
	case errors.Is(err, chclient.ErrTooManySamples),
		errors.Is(err, chclient.ErrDrainBytesExceeded),
		errors.Is(err, chclient.ErrMemoryLimitExceeded):
		return telemetry.ReasonResourceExhausted
	default:
		return ""
	}
}
