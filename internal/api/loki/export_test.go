package loki

import "github.com/tsouza/cerberus/internal/chsql"

// SetOnQueryRangeDrain installs the test-observable eager-drain hook on the
// handler. The hook fires once per /loki/api/v1/query_range request with
// res.Inspected — the number of rows h.Engine.Query pulled from ClickHouse
// before buildRangeData pivots them into the matrix/streams wire shape.
//
// This is the only entry point external (loki_test) tests have to read the
// eager-path drain count, because onQueryRangeDrain is unexported
// (production never installs a hook, keeping the hot path byte-unchanged).
// It mirrors api/prom's SetOnRangeDrain / SetOnInstantDrain and api/tempo's
// SearchMetrics.InspectedTraces: the boundsdrain harness reads it to assert
// a LogQL metric range query (e.g. count_over_time) stays O(output) =
// O(series × step) rather than O(raw log lines matched) as the seeded log
// density grows.
//
// Exposed via export_test.go so the field stays unexported in production
// code while remaining settable from the chdb-tagged regression tests,
// which live in package loki_test.
func (h *Handler) SetOnQueryRangeDrain(fn func(int64)) {
	h.onQueryRangeDrain = fn
}

// TailCapCloseReason exposes the /tail tail-budget-saturated close-frame
// reason text (issue #2048) to package loki_test, so a conformance test
// can assert the exact wording without hand-duplicating it — a drifted
// copy would pass even after the production string changed.
const TailCapCloseReason = tailCapCloseReason

// The served-label-set expression, exposed to the chdb-tagged
// differential test in package loki_test so it can run the SQL twin over
// the same corpus [format.NormalizeLabelMap] is run over. The frag has
// exactly one production caller (buildIndexVolumeSQL) and no reason to be
// part of the package's surface, so the export lives here.
//
// StoredLabelsAlias is the column the rendered expression reads — the
// alias /index/volume's stored-key pre-aggregation publishes, which the
// differential test supplies its corpus under and the emitted-SQL shape
// pins name.
const StoredLabelsAlias = volumeStoredLabelsAlias

// NormalizedLabelsSQL renders exactly what buildIndexVolumeSQL emits over
// [StoredLabelsAlias].
func NormalizedLabelsSQL() (string, []any) {
	return chsql.Render(normalizedLabelsFrag(chsql.Col(volumeStoredLabelsAlias)))
}
