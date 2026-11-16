package chclient

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/ClickHouse/ch-go"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// transientCanaryErrors are canary errors that are not a refusal: each must
// classify as Unreachable under every canary's refusal set.
var transientCanaryErrors = []struct {
	name string
	err  error
}{
	{"timeout exceeded", &clickhouse.Exception{Code: chCodeTimeoutExceeded, Name: "TIMEOUT_EXCEEDED"}},
	{"memory limit exceeded", &clickhouse.Exception{Code: chCodeMemoryLimitExceeded, Name: "MEMORY_LIMIT_EXCEEDED"}},
	{"too many simultaneous queries", &clickhouse.Exception{Code: int32(chproto.ErrTooManySimultaneousQueries), Name: "TOO_MANY_SIMULTANEOUS_QUERIES"}},
	{"server-side socket timeout", &clickhouse.Exception{Code: int32(chproto.ErrSocketTimeout), Name: "SOCKET_TIMEOUT"}},
	{"server-side network error", &clickhouse.Exception{Code: int32(chproto.ErrNetworkError), Name: "NETWORK_ERROR"}},
	{"query cancelled", &clickhouse.Exception{Code: int32(chproto.ErrQueryWasCancelled), Name: "QUERY_WAS_CANCELLED"}},
	{"quota exceeded", &clickhouse.Exception{Code: int32(chproto.ErrQuotaExceeded), Name: "QUOTA_EXCEEDED"}},
	{"memory limit sentinel without a cause", &MemoryLimitError{}},
	{"query timeout sentinel without a cause", &QueryTimeoutError{}},
	{"wrapped memory limit", fmt.Errorf("chclient: query: %w", &clickhouse.Exception{Code: chCodeMemoryLimitExceeded})},
	{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
	{"circuit open", fmt.Errorf("chclient: query: %w", ErrCircuitOpen)},
}

// TestCanaryErrAnswer_TransientIsUnreachable pins that no
// canary reads a load or resource condition as a refusal.
func TestCanaryErrAnswer_TransientIsUnreachable(t *testing.T) {
	t.Parallel()
	refusals := map[string]func(error) bool{"settings": isSettingsRefusal, "query-log union": isQueryLogUnionCanaryRefusal}
	for set, refused := range refusals {
		for _, tc := range transientCanaryErrors {
			if got := canaryErrAnswer(tc.err, refused).verdict; got != chopt.CapabilityUnreachable {
				t.Errorf("%s canary: %s classified %v; want %v", set, tc.name, got, chopt.CapabilityUnreachable)
			}
		}
	}
}

// TestCanaryErrAnswer_UnknownServerErrorIsForbiddenForThePassOnly pins that a
// server exception that is neither a known refusal nor a known condition
// keeps the capability off for the pass that read it without being held,
// while a known refusal is held.
func TestCanaryErrAnswer_UnknownServerErrorIsForbiddenForThePassOnly(t *testing.T) {
	t.Parallel()
	refusals := map[string]func(error) bool{"settings": isSettingsRefusal, "query-log union": isQueryLogUnionCanaryRefusal}
	unknown := &clickhouse.Exception{Code: int32(chproto.ErrTooManyRowsOrBytes), Name: "TOO_MANY_ROWS_OR_BYTES"}
	refusal := &clickhouse.Exception{Code: chCodeReadonly, Name: "READONLY"}
	for set, refused := range refusals {
		if got := canaryErrAnswer(unknown, refused); got.verdict != chopt.CapabilityForbidden || !got.passOnly {
			t.Errorf("%s canary: unknown server error answered %v, passOnly=%v; want %v, passOnly", set, got.verdict, got.passOnly, chopt.CapabilityForbidden)
		}
		if got := canaryErrAnswer(refusal, refused); got.verdict != chopt.CapabilityForbidden || got.passOnly {
			t.Errorf("%s canary: refusal answered %v, passOnly=%v; want %v, held", set, got.verdict, got.passOnly, chopt.CapabilityForbidden)
		}
	}
}

// TestCanaryErrAnswer_AllStates pins the error mapping every settings canary
// relies on:
//   - nil                      -> Available
//   - the setting refused      -> Forbidden
//   - a condition of the moment -> Unreachable (no verdict; conservative)
//
// TestCanaryErrAnswer_UnknownServerErrorIsForbiddenForThePassOnly pins the
// remaining case, any other server exception.
func TestCanaryErrAnswer_AllStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want chopt.Capability
	}{
		{
			name: "nil error is available",
			err:  nil,
			want: chopt.CapabilityAvailable,
		},
		{
			name: "setting constraint violation (452) is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeSettingConstraintViolation,
				Name:    "SETTING_CONSTRAINT_VIOLATION",
				Message: "Setting allow_experimental_time_series_aggregate_functions should not be changed",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "readonly user (164) is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeReadonly,
				Name:    "READONLY",
				Message: "Cannot modify 'allow_experimental_time_series_aggregate_functions' setting in readonly mode",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "unknown setting (115) on a build without the setting is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeUnknownSetting,
				Name:    "UNKNOWN_SETTING",
				Message: "Setting allow_experimental_time_series_aggregate_functions is neither a builtin setting nor started with the prefix 'SQL_'",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "a server exception reporting a condition of the moment is unreachable",
			err:  &clickhouse.Exception{Code: chCodeMemoryLimitExceeded, Name: "MEMORY_LIMIT_EXCEEDED"},
			want: chopt.CapabilityUnreachable,
		},
		{
			name: "a ch-go exception carrying a refusal code is forbidden",
			err:  fmt.Errorf("chclient: query: %w", &ch.Exception{Code: chproto.ErrSettingConstraintViolation, Name: "SETTING_CONSTRAINT_VIOLATION"}),
			want: chopt.CapabilityForbidden,
		},
		{
			name: "wrapped typed exception is still forbidden (errors.As reaches it)",
			err:  fmt.Errorf("chclient: query: %w", &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Name: "SETTING_CONSTRAINT_VIOLATION"}),
			want: chopt.CapabilityForbidden,
		},
		{
			name: "plain transport error is unreachable",
			err:  errors.New("dial tcp 10.0.0.1:9000: connect: connection refused"),
			want: chopt.CapabilityUnreachable,
		},
		{
			name: "circuit-open transport error is unreachable",
			err:  fmt.Errorf("chclient: query: %w", ErrCircuitOpen),
			want: chopt.CapabilityUnreachable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := canaryErrAnswer(tc.err, isSettingsRefusal).verdict; got != tc.want {
				t.Errorf("canaryErrAnswer(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}
