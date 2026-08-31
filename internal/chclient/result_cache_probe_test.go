package chclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestClassifyCapabilityFromProbeErr_ResultCacheShapes pins the shared
// classifier against the specific rejection shapes a hardened deployment
// profile raises for use_query_cache — a SEPARATE setting name from the
// ts-grid experimental gate ts_grid_probe_test.go pins, confirming the
// shared classifier's mapping does not depend on the setting name at all
// (nil -> Available, typed exception -> Forbidden, transport error ->
// Unreachable).
func TestClassifyCapabilityFromProbeErr_ResultCacheShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want chopt.Capability
	}{
		{name: "nil error is available", err: nil, want: chopt.CapabilityAvailable},
		{
			name: "setting constraint violation on use_query_cache is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeSettingConstraintViolation,
				Name:    "SETTING_CONSTRAINT_VIOLATION",
				Message: "Setting use_query_cache should not be changed",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "readonly user on use_query_cache is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeReadonly,
				Name:    "READONLY",
				Message: "Cannot modify 'use_query_cache' setting in readonly mode",
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyCapabilityFromProbeErr(tc.err); got != tc.want {
				t.Errorf("classifyCapabilityFromProbeErr(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestResultCacheStamped confirms the settings-map detector queryContext
// uses to decide whether to wire the ProfileEvents observer: present and
// exactly 1 -> true; absent, or present with a different value, -> false.
func TestResultCacheStamped(t *testing.T) {
	cases := []struct {
		name string
		s    clickhouse.Settings
		want bool
	}{
		{name: "nil map", s: nil, want: false},
		{name: "absent", s: clickhouse.Settings{"max_threads": 4}, want: false},
		{name: "present and 1", s: clickhouse.Settings{SettingUseQueryCache: 1}, want: true},
		{name: "present but 0", s: clickhouse.Settings{SettingUseQueryCache: 0}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultCacheStamped(tc.s); got != tc.want {
				t.Errorf("resultCacheStamped(%v) = %v; want %v", tc.s, got, tc.want)
			}
		})
	}
}

// TestObserveResultCacheProfileEvents_SumsHitsAndMisses confirms the
// ProfileEvents handler sums BOTH counters independently across a batch
// (a real query may report either, both, or neither depending on server
// version) and ignores unrelated events.
func TestObserveResultCacheProfileEvents_SumsHitsAndMisses(t *testing.T) {
	before := resultCacheHits.Load()
	beforeMiss := resultCacheMisses.Load()
	observeResultCacheProfileEvents([]clickhouse.ProfileEvent{
		{Name: profileEventQueryCacheHits, Value: 3},
		{Name: "SelectedRows", Value: 100},
		{Name: profileEventQueryCacheMisses, Value: 2},
		{Name: profileEventQueryCacheHits, Value: 1},
	})
	if got := resultCacheHits.Load() - before; got != 4 {
		t.Errorf("resultCacheHits delta = %d; want 4", got)
	}
	if got := resultCacheMisses.Load() - beforeMiss; got != 2 {
		t.Errorf("resultCacheMisses delta = %d; want 2", got)
	}
}
