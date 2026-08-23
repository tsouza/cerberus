//go:build chdb

// chDB-backed dual-emit parity gate for the anchor-injection window-slide
// classic-histogram ladder (chplan.RangeBucketWindowSlide, emitted by
// internal/chsql/range_bucket_window_slide.go).
//
// The test lowers the SAME range-mode
// `histogram_quantile(phi, <agg> by(le) (sum_over_time(<bucket>[5m])))`
// expression TWICE against the SAME seed — once with window-slide OFF (the
// RangeBucketFanout array-expression ladder fold) and once with it ON (the
// sliding-window anchor-injection aggregate) — runs BOTH on one ephemeral
// chDB session, and compares the answered quantile per (series, anchor) at
// full float64 precision.
//
// Why this is the correctness proof rather than a golden diff, and why the
// grid is 5m/30s specifically. The fan-out's expected_rows are already
// Prometheus-pinned by the spec corpus (histogram_quantile_window_slide_
// below_ratio_threshold.txtar carries `oracle: prometheus`), so
// window-slide == fan-out transitively proves window-slide == Prometheus. A
// golden alone would only pin what window-slide DOES, not that it agrees
// with the reference. The window/step ratio here (5m/30s = 10) is exactly
// windowSlideMinLookbackStepRatio (internal/promql/histogram_quantile_
// window_slide.go): below that ratio windowSlideEligible declines and every
// case would silently lower through the fan-out on BOTH sides, comparing
// the fan-out against itself and proving nothing about the new node — see
// histogram_quantile_window_slide_sum_over_time.txtar, which uses the
// identical grid for the same reason.
//
// windowFn is sum_over_time, never rate: windowSlideEligible declines any
// other windowFn (the mechanism has no counter-reset repair or rate
// extrapolation — see that function's own doc), so `rate` stays on
// [NativeClassicHistogramWindowLowerer]/the fan-out and is out of scope
// here; internal/chsql/histogram_quantile_classic_native_chdb_test.go
// covers that ladder.
//
// The seeds are chosen for the semantics most likely to break, mirroring
// the native ladder's own sibling suite for the shapes both mechanisms
// share (layout heterogeneity, temporality split, fold family), plus two
// this mechanism owns that the native ladder does not:
//
//   - malformed_bounds: duplicate and out-of-order ExplicitBounds in real
//     rows. hasGatedExtendedArrayFrag's own doc records that an EARLIER
//     version of this file's has-gated filter-sum used a positional
//     arrayCumSum instead, and answered a DIFFERENT (silently wrong)
//     number than the fan-out the moment a row's own bounds were duplicate
//     or unsorted — caught only by hand-derivation during review, not by
//     any existing test. This seed is the regression proof that gap stays
//     closed.
//   - boundary_exactness: the nanosecond/millisecond edge battery from
//     issue #2408's Task-1 spike (exact window edges, ±1ns/±1ms around
//     them, sub-millisecond clusters, a cluster straddling a millisecond
//     boundary, and an all-ms-aligned control) — proving the ceil-to-ms
//     encoding (windowSlideMsEncodeFrag) the spike claimed exact actually
//     IS exact against real chDB, not merely asserted in a doc comment.
//     The grid here (like every case in this file) carries 21 anchors
//     (10 minutes at a 30s step): a single-anchor grid would let even a
//     wrong ms-rounding variant look exact, since the pre-filter that
//     narrows a query down to its own scan range would mask a
//     frame-boundary bug entirely rather than surface it as a
//     cross-anchor divergence.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// wsCase is one seed + query pair, mirroring hqNativeCase exactly.
type wsCase struct {
	name  string
	query string
	rows  string
}

// wsWindow is the sum_over_time window every case uses. It is deliberately
// the SAME duration as hqNativeWindow's "5m" so the grid stays identical to
// the native ladder's own suite; only the windowFn differs (sum_over_time,
// never rate — see the package doc). Paired with hqNativeGridStep (30s),
// the ratio is exactly windowSlideMinLookbackStepRatio, so every case below
// is eligible for window-slide by construction.
const wsWindow = "5m"

func wsCases() []wsCase {
	q := func(agg string) string {
		return "histogram_quantile(0.5, " + agg + " by(le) (sum_over_time(http_server_request_duration_bucket[" + wsWindow + "])))"
	}
	return []wsCase{
		{
			// Baseline: one series, stable layout, monotone counter. Pins
			// that the ladder and the has-gated cumulative fold agree
			// before any of the harder rules are exercised.
			name:  "uniform_layout",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [3, 5, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [4, 9, 14], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:04:00', 9), [7, 11, 20], [0.1, 0.5, 1.0])`,
		},
		{
			// Two series whose bucket layouts share NO bound. The
			// per-series canonical-bound JOIN (windowSlideCanonBySeriesFrag)
			// must produce a ladder per series over ITS OWN layout; a rung
			// a series never reported must not be fabricated on its ladder.
			name:  "mixed_layouts_across_series",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [10, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), [14, 3, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [100.0, 200.0, 300.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), [0, 0, 10], [100.0, 200.0, 300.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:02:00', 9), [2, 0, 17], [100.0, 200.0, 300.0])`,
		},
		{
			// ONE series whose layout changes mid-window. A bound that
			// leaves the layout must still appear on the ladder for the
			// anchors whose window carries a row reporting it, and must
			// NOT appear (carrying a fabricated zero) for anchors whose
			// window does not — the has()-gate in hasGatedExtendedArrayFrag.
			name:  "layout_change_midwindow",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [2, 4, 6], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [4, 7, 11], [0.1, 0.5, 2.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [6, 9, 15], [0.1, 0.5, 2.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:06:00', 9), [9, 12, 21], [0.1, 0.5, 2.0])`,
		},
		{
			// A series whose counter restarts mid-window. sum_over_time has
			// no counter-reset repair of its own (unlike rate) — on EITHER
			// side of this comparison, a reset is just more raw samples
			// summed as-is — so this seed pins that both engines fold a
			// non-monotone series identically rather than one silently
			// applying a reset correction the other does not.
			name:  "counter_reset",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [10, 20, 30], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [12, 25, 38], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [1, 2, 4], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [3, 6, 10], [0.1, 0.5, 1.0])`,
		},
		{
			// Two series with different, irregular scrape cadences and
			// phases. sum_over_time applies no extrapolation (unlike rate),
			// so this seed pins that the ms-encoded RANGE frame
			// (windowSlideMsEncodeFrag) includes exactly the same set of
			// irregularly-timed rows the fan-out's own exact-nanosecond
			// comparison does.
			name:  "distinct_cadences",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:00:10', 9), [0, 1, 2], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:00:40', 9), [1, 3, 5], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:01:10', 9), [2, 6, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'fast'), toDateTime64('2026-01-01 00:01:40', 9), [4, 8, 14], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:02:00', 9), [3, 4, 7], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'slow'), toDateTime64('2026-01-01 00:04:00', 9), [5, 9, 15], [0.1, 0.5, 1.0])`,
		},
		{
			// DELTA aggregation temporality. windowSlideEligible's own
			// contract has no DELTA fold, so
			// WindowSlideClassicHistogramWindowLowerer's temporality split
			// must route the WHOLE series to its Fallback (the fan-out)
			// arm of the UnionAll rather than answering it via the sliding
			// window — this case proves the split routes it there.
			name:  "delta_temporality",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [1, 3, 4], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [2, 2, 5], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:03:00', 9), [1, 4, 6], [0.1, 0.5, 1.0], 1)`,
		},
		{
			// One CUMULATIVE series and one DELTA series in the same
			// query, so BOTH arms of the UnionAll (the sliding-window arm
			// and the fan-out Fallback arm) produce rows and the merge
			// folds them together.
			name:  "mixed_temporality",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:01:00', 9), [2, 4, 6], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'cum'), toDateTime64('2026-01-01 00:02:00', 9), [5, 8, 13], [0.1, 0.5, 1.0], 2),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:01:00', 9), [1, 3, 4], [0.1, 0.5, 1.0], 1),
    ('http_server_request_duration', map('host', 'dlt'), toDateTime64('2026-01-01 00:02:00', 9), [2, 2, 5], [0.1, 0.5, 1.0], 1)`,
		},
		{
			// A non-`sum` across-series fold. `max by(le)` is where a
			// uniform per-rung scale factor would still cancel but a
			// per-SERIES one would not, so it separates the two.
			name:  "max_by_le",
			query: q("max"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [1, 5, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), [2, 9, 17], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:30', 9), [7, 8, 9], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:03:00', 9), [11, 13, 15], [0.1, 0.5, 1.0])`,
		},
		{
			// `count by(le)` is the scale-INVARIANT fold: its ladder
			// counts contributing series, so a rung fabricated at any
			// anchor changes the answer outright rather than by a float
			// ULP.
			name:  "count_by_le",
			query: q("count"),
			rows: `
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), [4, 5, 6], [1.0, 2.0, 3.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), [0, 0, 0], [2.0, 4.0]),
    ('http_server_request_duration', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), [3, 8], [2.0, 4.0])`,
		},
		{
			// Duplicate and out-of-order ExplicitBounds across several
			// real rows in one series/window. hasGatedExtendedArrayFrag's
			// own doc: an earlier version of this emitter used a
			// POSITIONAL arrayCumSum keyed by index, which assumes
			// strictly ascending, distinct bounds and silently answers a
			// DIFFERENT number than the fan-out's own
			// classicBucketRowCumulativeExpr filter-sum the moment that
			// assumption breaks — a real, previously-shipped 2.2x-wrong-
			// answer bug caught only by hand-derivation, not by any test.
			// Row B repeats bound 1.0 twice (out of order); row C is
			// simply unsorted. Both are real shapes OTLP data can produce.
			// The counts are chosen with no shared factor so a
			// mis-attributed contribution changes the answered quantile
			// outright rather than by a rounding-sized amount.
			name:  "malformed_bounds",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:00:00', 9), [3, 7, 12], [0.5, 1.0, 2.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:01:00', 9), [5, 2, 9], [1.0, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'api'), toDateTime64('2026-01-01 00:02:00', 9), [4, 6, 8], [2.0, 0.5, 1.0])`,
		},
		// The six cases below are the nanosecond/millisecond edge battery
		// from issue #2408's Task-1 spike, proving windowSlideMsEncodeFrag's
		// ceil-to-ms formula against real chDB rather than a doc-comment
		// assertion. Each carries ONE small always-included baseline row
		// (mid-window, counts [1,1,1], present so windowSlideMinSamples
		// never drops the anchor to NaN) plus one or more high-magnitude
		// "probe" rows AT the edge under test, each probe's whole count
		// concentrated on the le=1.0 rung with a DISTINCT value far larger
		// than the baseline's. This is deliberately more sensitive than
		// spreading many comparably-sized rows across the window: a probe
		// contributing 1000+ against a baseline of 1 moves phi=0.5 across a
		// bucket boundary outright if either engine misclassifies it,
		// whereas comparably-sized rows can leave the interpolated
		// quantile unchanged even when one engine's row-membership is
		// wrong (confirmed empirically — an earlier, single, diluted
		// 24-row version of this case stayed bit-identical between a
		// correct and a deliberately mis-rounded emitter build, so it
		// caught nothing; this shape flips fanout=0.7499995 vs
		// native=0.3 for a single mis-rounded probe under that same
		// mutation).
		{
			// The exclusive left edge of anchor 00:05:00's window
			// (window = (00:00:00, 00:05:00]): exact-edge, -1ns and -1ms
			// probes must be EXCLUDED; +1ns and +1ms must be INCLUDED.
			// Each probe's own magnitude is distinct so no pair of
			// misclassifications could coincidentally cancel out.
			name:  "boundary_left_edge_open",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2026-01-01 00:02:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2026-01-01 00:00:00.000000000', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2025-12-31 23:59:59.999999999', 9), [0, 0, 2000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2026-01-01 00:00:00.000000001', 9), [0, 0, 3000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2025-12-31 23:59:59.999000000', 9), [0, 0, 4000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge1'), toDateTime64('2026-01-01 00:00:00.001000000', 9), [0, 0, 5000], [0.1, 0.5, 1.0])`,
		},
		{
			// The inclusive right edge of anchor 00:05:00's window: the
			// exact-edge probe must be INCLUDED; +1ns and +1ms must be
			// EXCLUDED from THIS anchor (they still land in a later
			// anchor's own window, which the cell-by-cell diff covers
			// too).
			name:  "boundary_right_edge_closed",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge2'), toDateTime64('2026-01-01 00:02:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge2'), toDateTime64('2026-01-01 00:05:00.000000000', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge2'), toDateTime64('2026-01-01 00:05:00.000000001', 9), [0, 0, 2000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge2'), toDateTime64('2026-01-01 00:05:00.001000000', 9), [0, 0, 3000], [0.1, 0.5, 1.0])`,
		},
		{
			// Four distinct sub-millisecond timestamps that all ceil-encode
			// to the SAME millisecond position, well inside the window (no
			// boundary nearby) — proving every row in a ClickHouse RANGE
			// frame peer group is summed, not just one survivor of the tie.
			name:  "boundary_submillisecond_peers",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge3'), toDateTime64('2026-01-01 00:01:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge3'), toDateTime64('2026-01-01 00:02:30.000000100', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge3'), toDateTime64('2026-01-01 00:02:30.000000300', 9), [0, 0, 2000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge3'), toDateTime64('2026-01-01 00:02:30.000000600', 9), [0, 0, 3000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge3'), toDateTime64('2026-01-01 00:02:30.000000900', 9), [0, 0, 4000], [0.1, 0.5, 1.0])`,
		},
		{
			// Five samples inside one millisecond at a different point in
			// the window — the same peer-group proof at higher density.
			name:  "boundary_dense_millisecond",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:01:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:03:30.000000050', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:03:30.000000200', 9), [0, 0, 2000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:03:30.000000450', 9), [0, 0, 3000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:03:30.000000700', 9), [0, 0, 4000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge4'), toDateTime64('2026-01-01 00:03:30.000000950', 9), [0, 0, 5000], [0.1, 0.5, 1.0])`,
		},
		{
			// A five-sample cluster straddling anchor 00:06:00's own left
			// edge (00:01:00) at sub-microsecond spacing — the ceil-to-ms
			// rounding must still classify each side correctly even when
			// the raw nanosecond deltas are far smaller than a
			// millisecond.
			name:  "boundary_straddle_millisecond",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:02:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:00:59.999998500', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:00:59.999999500', 9), [0, 0, 2000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:01:00.000000000', 9), [0, 0, 3000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:01:00.000000500', 9), [0, 0, 4000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge5'), toDateTime64('2026-01-01 00:01:00.000001500', 9), [0, 0, 5000], [0.1, 0.5, 1.0])`,
		},
		{
			// Two all-millisecond-aligned control probes — no sub-ms
			// remainder at all, so windowSlideMsCeilBias is a no-op on
			// either. A divergence here would indicate a bug in the
			// encoding unrelated to any rounding edge at all.
			name:  "boundary_ms_aligned_control",
			query: q("sum"),
			rows: `
    ('http_server_request_duration', map('service', 'edge6'), toDateTime64('2026-01-01 00:01:00.000000000', 9), [1, 1, 1], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge6'), toDateTime64('2026-01-01 00:04:00.123000000', 9), [0, 0, 1000], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('service', 'edge6'), toDateTime64('2026-01-01 00:04:30.500000000', 9), [0, 0, 2000], [0.1, 0.5, 1.0])`,
		},
	}
}

// wsMaxUlp is the divergence budget between the two evaluation orders — see
// maxHQNativeUlp's own doc for why this is a worst-case-measured arithmetic
// floor rather than a tolerance: an interpolation error moves a quantile by
// a bucket width, not by a last bit, so this budget still fails a real
// arithmetic divergence outright.
const wsMaxUlp = 2

// wsCell keys an answered quantile by (series-label JSON, anchor timestamp),
// mirroring hqCell exactly.
type wsCell struct {
	labels string
	anchor string
}

func TestWindowSlideClassicHistogramLadder_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	for _, tc := range wsCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			seed(t, db, hqNativeSeedDDL)
			cols := "(MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds"
			if strings.Contains(tc.rows, "], 1)") || strings.Contains(tc.rows, "], 2)") {
				cols += ", AggregationTemporality"
			}
			cols += ")"
			seed(t, db, "INSERT INTO otel_metrics_histogram "+cols+" VALUES "+tc.rows)

			fanout := runWSEmit(t, db, tc.query, false)
			windowSlide := runWSEmit(t, db, tc.query, true)

			if len(windowSlide) != len(fanout) {
				t.Fatalf("row-count divergence: windowSlide=%d fanout=%d cells\nwindowSlide=%v\nfanout=%v",
					len(windowSlide), len(fanout), windowSlide, fanout)
			}
			var diverged int
			var maxUlp uint64
			for cell, fv := range fanout {
				wv, ok := windowSlide[cell]
				if !ok {
					t.Errorf("cell %+v present in fan-out but absent in window-slide", cell)
					continue
				}
				if math.Float64bits(wv) == math.Float64bits(fv) {
					continue
				}
				if math.IsNaN(wv) && math.IsNaN(fv) {
					continue
				}
				u := ulpDistance(wv, fv)
				if u > wsMaxUlp {
					t.Errorf("cell %+v: windowSlide=%.20g fanout=%.20g differ by %d ULP (> %d — an "+
						"arithmetic divergence, not float-order noise)", cell, wv, fv, u, wsMaxUlp)
					continue
				}
				if u > maxUlp {
					maxUlp = u
				}
				diverged++
			}
			t.Logf("%s: %d/%d cells bit-identical, %d differ (worst %d ULP, budget %d)",
				tc.name, len(fanout)-diverged, len(fanout), diverged, maxUlp, wsMaxUlp)
		})
	}
}

// runWSEmit lowers + emits query at the shared range grid with window-slide
// on or off, runs it, and returns the answered quantile per cell. Mirrors
// runHQEmit exactly, toggling promql.RangeLowerers.ClassicHistogram between
// nil (resolves to FanoutClassicHistogramWindowLowerer at the lowering
// entry) and WindowSlideClassicHistogramWindowLowerer{Fallback: Fanout...}
// — the same seam native_strategy_coverage_test.go's ClassicHistogram row
// wires in production (Native{Fallback: WindowSlide{Fallback: Fanout{}}});
// this test isolates the WindowSlide-vs-Fanout link alone, since the native
// rate ladder never fires for a sum_over_time query in the first place
// (nativeClassicHistogramEligible requires windowFn == rate).
func runWSEmit(t *testing.T, db *sql.DB, query string, windowSlide bool) map[wsCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	if windowSlide {
		lowerers.ClassicHistogram = promql.WindowSlideClassicHistogramWindowLowerer{
			Fallback: promql.FanoutClassicHistogramWindowLowerer{},
		}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		hqNativeGridStart, hqNativeGridEnd, hqNativeGridStep,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (windowSlide=%v): %v", windowSlide, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (windowSlide=%v): %v", windowSlide, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS lbl, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (windowSlide=%v): %v\nSQL: %s", windowSlide, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[wsCell]float64)
	for rows.Next() {
		var lbl string
		var ts time.Time
		var v float64
		if err := rows.Scan(&lbl, &ts, &v); err != nil {
			t.Fatalf("scan (windowSlide=%v): %v", windowSlide, err)
		}
		out[wsCell{labels: lbl, anchor: ts.UTC().Format(time.RFC3339Nano)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (windowSlide=%v): %v", windowSlide, err)
	}
	if len(out) == 0 {
		t.Fatalf("windowSlide=%v produced zero rows — every case must yield a populated grid", windowSlide)
	}
	return out
}
