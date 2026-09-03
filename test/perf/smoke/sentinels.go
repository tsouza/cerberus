package smoke

import (
	"net/url"
	"strconv"
	"time"
)

// Metric names the seed builders write and the sentinel queries read. Kept
// here (not in seed.go) so this file is the single place that names what a
// sentinel targets.
const (
	// NativeHistogramMetric is Sentinel 1's exponential-histogram metric.
	NativeHistogramMetric = "cerberus_smoke_latency_exp_hist"
	// WideCounterMetric is Sentinel 2's high-cardinality counter metric.
	WideCounterMetric = "cerberus_smoke_wide_requests_total"
)

// sentinelWindow / sentinelStep size every sentinel's query_range grid: 1h at
// 15s step (241 anchors), matching the exact scale
// applyNativeHistogramAnalyzerFix's doc comment cites as the validation
// methodology (500 series under a one-label GROUP BY at this grid measured
// 12.2-14.5s with the analyzer vs 2.3-2.6s without it).
const (
	sentinelWindow = time.Hour
	sentinelStep   = 15 * time.Second
)

// NativeHistogramSeriesCount is Sentinel 1's series cardinality. Fixed
// (not calibrated) at the exact count applyNativeHistogramAnalyzerFix's own
// doc comment names as the methodology that measured the #2355/#2358
// regression: "500 distinct series under a one-label GROUP BY".
const NativeHistogramSeriesCount = 500

// ServerFloor names the ClickHouse capability tier a sentinel needs. The
// corpus is not single-tier: chopt gates several SettingsRules mechanisms
// behind a server-version floor ABOVE the repo-wide pinned integration image,
// and a sentinel for such a mechanism cannot prove anything on a server where
// the feature does not resolve in at all. Rather than moving every existing
// sentinel's calibrated baseline onto a newer server (which changes the
// numbers this corpus exists to hold still), the harness boots one container
// PER floor and runs each sentinel on the lowest server that can actually
// exercise it — older-server behaviour stays measured on the older server.
type ServerFloor int

const (
	// FloorBase is the repo-wide pinned integration image
	// (realch_perfsmoke_integration_test.go's perfSmokeCHImage). Every
	// sentinel whose mechanism carries no chopt version floor lives here,
	// and its committed baseline is a measurement of THAT server.
	FloorBase ServerFloor = iota

	// FloorJoinSpill is chopt.FeatureJoinSpill's own 26.4 floor — the first
	// ClickHouse that has max_bytes_before_external_join at all. See
	// realch_perfsmoke_integration_test.go's perfSmokeJoinSpillCHImage for
	// the image that clears it and why it is a SECOND tier rather than a
	// bump of the first.
	FloorJoinSpill
)

// String renders the floor for test output and failure messages.
func (f ServerFloor) String() string {
	if f == FloorJoinSpill {
		return "join_spill (>= 26.4)"
	}
	return "base"
}

// settingMaxBytesBeforeExternalJoin is the ClickHouse setting
// internal/engine's applyJoinSpillSettings stamps on a join-bearing plan once
// chopt.FeatureJoinSpill has resolved in. Named here because the sentinel
// corpus is what asserts the stamp actually reaches the server.
const settingMaxBytesBeforeExternalJoin = "max_bytes_before_external_join"

// joinSpillCapDenominator mirrors internal/engine/spill.go's
// spillCapDenominator: applyJoinSpillSettings stamps spillThreshold(cap),
// which is the live per-query memory cap divided by this. Both engine
// constants are unexported, so the expected value is re-derived here rather
// than imported — deliberately, and as a cross-check rather than an accident:
// if the engine's own arithmetic moves, the join_spill sentinel's stamp
// assertion goes red and whoever moved it has to look at this corpus instead
// of silently changing what production stamps.
const joinSpillCapDenominator int64 = 2

// Sentinel describes one real-ClickHouse memory-bounding differential
// target: an HTTP request against the mounted production handlers, built to
// reach a specific plan shape and memory-bounding mechanism the #2364
// incident (and #2358/#2355/#2366) turned on, at a scale realistic enough
// that the mechanism is what keeps the query under the memory cap rather
// than the query being incidentally too small to need it.
type Sentinel struct {
	// Name identifies the sentinel in the baseline JSON and test output.
	Name string
	// Mechanism names the applyXxx settings rule
	// (internal/engine/query_settings_rules.go / spill.go) this sentinel
	// targets, for logging/doc purposes only.
	Mechanism string
	// Path is the HTTP path to issue the request against, on the combined
	// prom+tempo mux.
	Path string
	// Params returns the query-string params (sans start/end/step, which the
	// caller fills from Window/Step) for the request over [start, end).
	Params func(start, end time.Time) url.Values
	// Window / Step size the query_range grid this sentinel issues.
	Window time.Duration
	Step   time.Duration
	// Floor is the ClickHouse capability tier this sentinel must run on.
	// The zero value (FloorBase) is the repo-wide pinned image every
	// pre-existing sentinel is calibrated against.
	Floor ServerFloor
	// RequiredQuerySettings, when non-nil, returns the per-query ClickHouse
	// settings the harness must find in system.query_log's Settings map for
	// EVERY repeat of this sentinel, keyed by setting name and valued by the
	// exact string ClickHouse records. memoryCapBytes is the live
	// max_memory_usage the harness runs under, because a memory-bounding
	// stamp is sized from it.
	//
	// It exists because a settings-rule sentinel is otherwise unfalsifiable:
	// peak memory and HTTP status are both identical whether the rule fired
	// or was never wired at all (the stamps are RESULT-EQUIVALENT and
	// THRESHOLD-GATED by construction), so a sentinel that measured only
	// those two would pass just as green with the mechanism deleted. This
	// field is what makes the mechanism's ABSENCE fail.
	RequiredQuerySettings func(memoryCapBytes int64) map[string]string
}

// Sentinels is the single source of truth for the perf-smoke real-ClickHouse
// differential: every sentinel query the integration harness issues, and the
// specific #2364-class memory-bounding mechanism each one targets.
var Sentinels = []Sentinel{
	{
		// PRIMARY sentinel — this is what actually broke in #2364. Hits
		// chplan.HistogramQuantileNative / HistogramProjection ->
		// applyNativeHistogramAnalyzerFix directly.
		Name:      "native_histogram_quantile",
		Mechanism: "applyNativeHistogramAnalyzerFix (internal/engine/query_settings_rules.go)",
		Path:      "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {"histogram_quantile(0.9, sum by (pod) (rate(" + NativeHistogramMetric + "[5m])))"},
			}
		},
		Window: sentinelWindow,
		Step:   sentinelStep,
	},
	{
		// Spill, unconditional: applySpillSettings fires on EVERY data-plane
		// query, but only a high-cardinality GROUP BY actually crosses
		// spillThreshold(cap) and exercises the spill-to-disk path rather
		// than fitting comfortably in RAM regardless.
		Name:      "spill_high_cardinality_groupby",
		Mechanism: "applySpillSettings (internal/engine/spill.go)",
		Path:      "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {"sum by (session_id) (rate(" + WideCounterMetric + "[5m]))"},
			}
		},
		Window: sentinelWindow,
		Step:   sentinelStep,
	},
	{
		// compare(), gated on chplan.MetricsCompare via
		// applyCompareMemoryBound. Shape mirrors
		// test/spec/traceql/metrics_compare.txtar. Tempo's
		// /api/metrics/query_range defaults `step` (traceql.DefaultQueryRangeStep,
		// ~240 points) when omitted, matching Grafana Traces Drilldown's own
		// issue-detector query — Step is left zero here and the caller omits
		// the param.
		Name:      "compare_memory_bound",
		Mechanism: "applyCompareMemoryBound (internal/engine/query_settings_rules.go)",
		Path:      "/api/metrics/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{"q": {`{ } | compare({ status = error })`}}
		},
		Window: sentinelWindow,
		Step:   0,
	},
	{
		// Join spill, chopt-gated: applyJoinSpillSettings (spill.go) stamps
		// max_bytes_before_external_join = cap/joinSpillCapDenominator on a
		// plan chplan.HasJoin matches, but ONLY once SettingsRules.JoinSpill is
		// true — the boot-resolved chopt.FeatureJoinSpill verdict, false on
		// every server below 26.4. Hence Floor: FloorJoinSpill.
		//
		// The query is a vector-vector binary op, which lowers to
		// chplan.VectorJoin (test/spec/promql/binop_rate_div_rate_on.txtar
		// pins that shape) — the first arm of chplan.HasJoin's switch, and the
		// one carrying VectorJoin's own ManyToManyMatchMessage throwIf guard,
		// which exists precisely because this shape can build an unbounded
		// hash table. The two sides use DIFFERENT aggregation operators over
		// the same seeded metric so neither the optimizer nor ClickHouse can
		// fold them into one scan and quietly retire the join; `on
		// (session_id)` keeps the match one-to-one over
		// SeedHighCardinalityCounter's existing session_id fixture, so this
		// sentinel needs no new seed builder.
		//
		// RequiredQuerySettings is the load-bearing assertion here, not the
		// memory prongs: the stamp is threshold-gated and result-equivalent,
		// so a join whose build side fits in RAM measures identically whether
		// it was stamped or not. See the field's own doc.
		Name:      "join_spill_vector_join",
		Mechanism: "applyJoinSpillSettings (internal/engine/spill.go) via SettingsRules.JoinSpill / chopt.FeatureJoinSpill",
		Floor:     FloorJoinSpill,
		Path:      "/api/v1/query_range",
		Params: func(start, end time.Time) url.Values {
			return url.Values{
				"query": {
					`sum by (session_id) (rate(` + WideCounterMetric + `[5m]))` +
						` / on (session_id) ` +
						`max by (session_id) (rate(` + WideCounterMetric + `[5m]))`,
				},
			}
		},
		Window: sentinelWindow,
		Step:   sentinelStep,
		RequiredQuerySettings: func(memoryCapBytes int64) map[string]string {
			return map[string]string{
				settingMaxBytesBeforeExternalJoin: strconv.FormatInt(memoryCapBytes/joinSpillCapDenominator, 10),
			}
		},
	},
}

// RequiredSettings evaluates RequiredQuerySettings under memoryCapBytes,
// returning nil for a sentinel that declares none. Callers range over the
// result, so a sentinel without the field simply asserts nothing.
func (s Sentinel) RequiredSettings(memoryCapBytes int64) map[string]string {
	if s.RequiredQuerySettings == nil {
		return nil
	}
	return s.RequiredQuerySettings(memoryCapBytes)
}

// SentinelsForFloor returns the sentinels that run on floor, in Sentinels
// order. The harness boots one ClickHouse per floor and drives only that
// floor's slice against it.
func SentinelsForFloor(floor ServerFloor) []Sentinel {
	out := make([]Sentinel, 0, len(Sentinels))
	for _, s := range Sentinels {
		if s.Floor == floor {
			out = append(out, s)
		}
	}
	return out
}
