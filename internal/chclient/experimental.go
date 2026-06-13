package chclient

import "context"

// SettingExperimentalTSGridAggregate is the exact ClickHouse setting name
// that gates the experimental timeSeries*ToGrid aggregate family
// (timeSeriesRateToGrid and siblings, introduced in ClickHouse v25.6.0).
// A query that emits one of those aggregates must run with this setting
// at 1, or the server rejects it (UNKNOWN_AGGREGATE_FUNCTION on a build
// that has the function gated off; UNKNOWN_FUNCTION on a build < 25.6
// that lacks it entirely).
//
// It is a named constant so a single test can pin the exact spelling: if
// a future ClickHouse release renames the setting, that test fails
// loudly rather than the rename silently slipping past (chDB does not
// enforce the gate, so the chdb parity lane alone cannot catch a
// mis-spelled or omitted setting — see the package note in
// internal/chsql/range_window_native.go).
const SettingExperimentalTSGridAggregate = "allow_experimental_ts_to_grid_aggregate_function"

type tsGridSettingKeyType struct{}

var tsGridSettingKey = tsGridSettingKeyType{}

// WithTSGridSetting returns a ctx that signals the data-plane query
// methods to add SettingExperimentalTSGridAggregate=1 to the per-query
// ClickHouse settings map. The engine calls this ONLY when the emitted
// plan contains a chplan.RangeWindowNative node — so the experimental
// knob rides exactly the queries that use the native aggregate and never
// the unrelated ones (a plain unknown setting can itself error on an
// older ClickHouse, so it must not be sent globally).
//
// The signal is a context value rather than a Client field so it is
// per-request, not per-connection: two concurrent requests, one native
// and one not, must not cross-contaminate each other's settings.
func WithTSGridSetting(ctx context.Context) context.Context {
	return context.WithValue(ctx, tsGridSettingKey, true)
}

// wantTSGridSetting reports whether ctx was marked by WithTSGridSetting.
func wantTSGridSetting(ctx context.Context) bool {
	v, _ := ctx.Value(tsGridSettingKey).(bool)
	return v
}
