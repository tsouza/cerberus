package httperr_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// TelemetryReason exists to survive the step that destroys a sentinel: a
// head restating the failure in its upstream's wording. Every case below
// is a shape that reaches a real respondError.
func TestTelemetryReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil is not a failure", nil, ""},
		{
			"a ClickHouse max_execution_time abort is a timeout",
			&chclient.QueryTimeoutError{Timeout: 0},
			telemetry.ReasonTimeout,
		},
		{
			"a wrapped deadline is a timeout",
			fmt.Errorf("engine: execute: %w", context.DeadlineExceeded),
			telemetry.ReasonTimeout,
		},
		{
			"a client hang-up is NOT a timeout — nothing ran out of time",
			fmt.Errorf("engine: execute: %w", context.Canceled),
			"",
		},
		{
			"the sample budget is a capacity refusal",
			&chclient.TooManySamplesError{Limit: 5},
			telemetry.ReasonResourceExhausted,
		},
		{
			"the drain-byte budget is a capacity refusal",
			&chclient.DrainByteBudgetError{Limit: 1 << 20},
			telemetry.ReasonResourceExhausted,
		},
		{
			"a ClickHouse memory-limit abort is a capacity refusal",
			&chclient.MemoryLimitError{Limit: 1 << 30},
			telemetry.ReasonResourceExhausted,
		},
		{
			"an open circuit breaker keeps the status-derived reason",
			chclient.ErrCircuitOpen,
			"",
		},
		{
			"an ordinary error keeps the status-derived reason",
			errors.New("boom"),
			"",
		},
		{
			// The shape that motivated the Reason field: a head's own
			// rejection constructor replaces the sentinel-bearing error
			// with upstream's wording, so nothing downstream could
			// recover the class from the error alone.
			"a restated rejection is classified from its recorded Reason",
			&httperr.Error{
				Status: http.StatusServiceUnavailable,
				Kind:   "timeout",
				Err:    errors.New("query timed out after 2m"),
				Reason: telemetry.ReasonTimeout,
			},
			telemetry.ReasonTimeout,
		},
		{
			"a restated rejection that recorded no Reason falls back to its cause",
			&httperr.Error{
				Status: http.StatusUnprocessableEntity,
				Kind:   "execution",
				Err:    &chclient.TooManySamplesError{Limit: 5},
			},
			telemetry.ReasonResourceExhausted,
		},
		{
			// Proves the fallback is real rather than incidental: an
			// Error whose cause carries no sentinel and no Reason must
			// leave the status-derived classification alone.
			"a restated rejection with neither is left to its status",
			&httperr.Error{
				Status: http.StatusUnprocessableEntity,
				Kind:   "execution",
				Err:    errors.New("vector cannot contain metrics with the same labelset"),
			},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httperr.TelemetryReason(tc.err); got != tc.want {
				t.Fatalf("TelemetryReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
