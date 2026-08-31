package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestDecideQueryWorkload_AllVerdicts pins the decision matrix
// resolveQueryWorkload's boot (fatalOnReject=true) and reprobe
// (fatalOnReject=false) callers both build on: Available always rides
// unchanged; Forbidden is fatal ONLY for boot under enforcing, and a
// WARN-and-skip ("") everywhere else; Unreachable (inconclusive) is never
// fatal in either caller, mirroring blockIsInconclusive's treatment of
// Unreachable elsewhere in the chopt resolver.
func TestDecideQueryWorkload_AllVerdicts(t *testing.T) {
	t.Parallel()

	const configured = "cerberus_queries"

	tests := []struct {
		name          string
		capability    chopt.Capability
		mode          chopt.Mode
		fatalOnReject bool
		wantResolved  string
		wantErr       bool
	}{
		{
			name:          "available always rides, regardless of mode or caller",
			capability:    chopt.CapabilityAvailable,
			mode:          chopt.Enforcing,
			fatalOnReject: true,
			wantResolved:  configured,
		},
		{
			name:          "available under permissive reprobe still rides",
			capability:    chopt.CapabilityAvailable,
			mode:          chopt.Permissive,
			fatalOnReject: false,
			wantResolved:  configured,
		},
		{
			name:          "forbidden + enforcing + boot (fatalOnReject) is a fatal error",
			capability:    chopt.CapabilityForbidden,
			mode:          chopt.Enforcing,
			fatalOnReject: true,
			wantErr:       true,
		},
		{
			name:          "forbidden + permissive + boot skips, no error",
			capability:    chopt.CapabilityForbidden,
			mode:          chopt.Permissive,
			fatalOnReject: true,
			wantResolved:  "",
		},
		{
			name:          "forbidden + enforcing + reprobe (never fatal) skips, no error",
			capability:    chopt.CapabilityForbidden,
			mode:          chopt.Enforcing,
			fatalOnReject: false,
			wantResolved:  "",
		},
		{
			name:          "unreachable + enforcing + boot never fatal (inconclusive)",
			capability:    chopt.CapabilityUnreachable,
			mode:          chopt.Enforcing,
			fatalOnReject: true,
			wantResolved:  "",
		},
		{
			name:          "unreachable + reprobe never fatal",
			capability:    chopt.CapabilityUnreachable,
			mode:          chopt.Enforcing,
			fatalOnReject: false,
			wantResolved:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decideQueryWorkload(configured, tc.capability, tc.mode, tc.fatalOnReject)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decideQueryWorkload(%v, %v, fatalOnReject=%v) = nil error; want an error",
						tc.capability, tc.mode, tc.fatalOnReject)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideQueryWorkload(%v, %v, fatalOnReject=%v) unexpected error: %v",
					tc.capability, tc.mode, tc.fatalOnReject, err)
			}
			if got != tc.wantResolved {
				t.Errorf("decideQueryWorkload(%v, %v, fatalOnReject=%v) = %q; want %q",
					tc.capability, tc.mode, tc.fatalOnReject, got, tc.wantResolved)
			}
		})
	}
}
