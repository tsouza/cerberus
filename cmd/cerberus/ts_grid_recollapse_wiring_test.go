package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/promql"
)

// TestNativeRangeLowerers_RecollapseNarrowsTSGridRange pins the one dependency
// the chopt registry cannot express as a field: ts_grid_recollapse defers the
// label-shaping tower past the NATIVE rate grid, so it only means anything on
// top of ts_grid_range. nativeRangeLowerers reads it inside that branch, which
// makes "recollapse enabled but no native grid" unrepresentable — an operator
// who lists ts_grid_recollapse alone gets the plain fan-out lowerer, not a
// half-wired native one.
//
// It also pins the positive direction: with both resolved, the boot-wired
// strategy MUST carry Recollapse=true. An off-by-default perf feature that
// resolves green and then never activates is the failure mode this whole
// wiring exists to prevent.
func TestNativeRangeLowerers_RecollapseNarrowsTSGridRange(t *testing.T) {
	// 25.9 is the shared floor of the whole timeSeries*ToGrid family, and
	// CapabilityAvailable is the boot verdict that lets the experimental gate
	// resolve — together the only combination under which either feature is on.
	const nativeFloorMinor = 9
	server := chopt.Version{Major: 25, Minor: nativeFloorMinor}

	cases := []struct {
		name           string
		optimizations  string
		wantNative     bool
		wantRecollapse bool
	}{
		{name: "auto enables both", optimizations: "auto", wantNative: true, wantRecollapse: true},
		{name: "range alone keeps the two-level shape", optimizations: "ts_grid_range", wantNative: true, wantRecollapse: false},
		{name: "recollapse alone stays on the fan-out", optimizations: "ts_grid_recollapse", wantNative: false},
		{name: "off wires the fan-out", optimizations: "off", wantNative: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			optSet, _, err := chopt.Resolve(chopt.Config{
				Optimizations: tc.optimizations,
				Capability:    chopt.CapabilityAvailable,
			}, server)
			if err != nil {
				t.Fatalf("chopt.Resolve(%q): %v", tc.optimizations, err)
			}
			rate := nativeRangeLowerers(optSet).Rate
			if !tc.wantNative {
				if _, native := rate.(promql.NativeRateLowerer); native {
					t.Fatalf("Rate = %T; want promql.FanoutRateLowerer", rate)
				}
				return
			}
			native, ok := rate.(promql.NativeRateLowerer)
			if !ok {
				t.Fatalf("Rate = %T; want promql.NativeRateLowerer", rate)
			}
			if native.Recollapse != tc.wantRecollapse {
				t.Errorf("NativeRateLowerer.Recollapse = %v; want %v", native.Recollapse, tc.wantRecollapse)
			}
		})
	}
}
