package chclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// chCodeUnknownSetting / chCodeResourceAccessDenied are the ClickHouse error
// codes a real 26.6 server answers with for, respectively, an unrecognized
// per-query setting NAME (a server too old to know `workload` at all) and an
// unresolvable WORKLOAD name under `throw_on_unknown_workload=true` (a
// hardened profile that requires every workload a query carries to name a
// real, provisioned WORKLOAD). Verified directly against a live
// clickhouse/clickhouse-server:26.6 instance rather than trusted from docs:
// `SELECT 1 SETTINGS bogus_setting=1` answers `Code: 115 (UNKNOWN_SETTING)`;
// with `throw_on_unknown_workload` reloaded to `true` server-side,
// `SELECT 1 SETTINGS workload='nonexistent'` answers
// `Code: 673 (RESOURCE_ACCESS_DENIED): Could not access resource 'cpu'.
// Please check throw_on_unknown_workload setting.`
const (
	chCodeUnknownSetting       = 115
	chCodeResourceAccessDenied = 673
)

// TestClassifyCapabilityFromProbeErr_WorkloadShapes pins the shared
// classifier against the rejection shapes a server that does not know the
// `workload` setting (too old) or a hardened profile that pins/forbids/
// requires-resolvable-workloads-for it raises — a THIRD setting name
// (workload) confirming, alongside ts_grid_probe_test.go and
// result_cache_probe_test.go, that the shared classifier's mapping does not
// depend on which setting the canary stamps.
func TestClassifyCapabilityFromProbeErr_WorkloadShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want chopt.Capability
	}{
		{name: "nil error is available", err: nil, want: chopt.CapabilityAvailable},
		{
			name: "unknown setting on a server too old for workload scheduling is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeUnknownSetting,
				Name:    "UNKNOWN_SETTING",
				Message: "Setting workload is neither a builtin setting nor started with the prefix 'SQL_' registered for user-defined settings",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "setting constraint violation on workload is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeSettingConstraintViolation,
				Name:    "SETTING_CONSTRAINT_VIOLATION",
				Message: "Setting workload should not be changed",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "unresolvable workload under throw_on_unknown_workload is forbidden",
			err: &clickhouse.Exception{
				Code:    chCodeResourceAccessDenied,
				Name:    "RESOURCE_ACCESS_DENIED",
				Message: "Could not access resource 'cpu'. Please check `throw_on_unknown_workload` setting.",
			},
			want: chopt.CapabilityForbidden,
		},
		{
			name: "wrapped typed exception is still forbidden (errors.As reaches it)",
			err:  fmt.Errorf("chclient: query: %w", &clickhouse.Exception{Code: chCodeUnknownSetting, Name: "UNKNOWN_SETTING"}),
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

// TestWithWorkloadSetting confirms WithWorkloadSetting stamps exactly
// SettingWorkload=name onto the per-request settings carrier, on top of any
// setting already attached to ctx (mirroring how the other WithXSetting
// helpers layer on the shared WithQuerySetting map).
func TestWithWorkloadSetting(t *testing.T) {
	t.Parallel()
	ctx := WithQuerySetting(context.Background(), "max_threads", 4)
	ctx = WithWorkloadSetting(ctx, "cerberus_queries")
	got := QuerySettingsFromContext(ctx)
	if got[SettingWorkload] != "cerberus_queries" {
		t.Fatalf("settings map = %v; want %s=cerberus_queries", got, SettingWorkload)
	}
	if got["max_threads"] != 4 {
		t.Fatalf("settings map = %v; want max_threads=4 preserved", got)
	}
}

// TestWorkloadProbeName confirms ProbeQueryWorkloadCapability's name
// selection: the configured name rides the canary unchanged when non-empty
// (so a hardened throw_on_unknown_workload=true server's rejection of an
// UNRESOLVABLE name is caught at boot), and an empty configured name (a
// caller that skipped cmd/cerberus's own "only probe when
// CERBERUS_CH_QUERY_WORKLOAD is non-empty" contract) falls back to the probe
// sentinel rather than sending an empty workload name.
func TestWorkloadProbeName(t *testing.T) {
	t.Parallel()
	if got := workloadProbeName("cerberus_queries"); got != "cerberus_queries" {
		t.Errorf("workloadProbeName(%q) = %q; want unchanged", "cerberus_queries", got)
	}
	if got := workloadProbeName(""); got != workloadCapabilityProbeName {
		t.Errorf("workloadProbeName(\"\") = %q; want %q", got, workloadCapabilityProbeName)
	}
}
