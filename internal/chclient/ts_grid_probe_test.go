package chclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// ClickHouse server error codes a hardened profile raises when cerberus stamps
// the constrained / change-forbidden experimental setting. Named so the test
// pins the exact mapping (these codes -> Forbidden).
const (
	chCodeSettingConstraintViolation = 452
	chCodeReadonly                   = 164
)

// TestClassifyTSGridCapability_AllStates pins the tri-state error mapping the
// boot capability canary relies on:
//   - nil                      -> Available
//   - typed server rejection   -> Forbidden (the setting was refused)
//   - transport failure        -> Unreachable (no server verdict; conservative)
func TestClassifyTSGridCapability_AllStates(t *testing.T) {
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
			name: "any other typed server exception is forbidden (server refused the probe)",
			err: &clickhouse.Exception{
				Code:    115,
				Name:    "UNKNOWN_SETTING",
				Message: "Unknown setting allow_experimental_time_series_aggregate_functions",
			},
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
			if got := classifyTSGridCapability(tc.err); got != tc.want {
				t.Errorf("classifyTSGridCapability(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}
