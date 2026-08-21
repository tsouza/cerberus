// Deliberately NOT `//go:build chdb`. Record, LevelCount and SortByFanFactor
// are plain data plus a comparison — nothing here opens a chDB session or
// runs a query, unlike the rest of this package (profile.go, corpus.go). See
// the "Build-tag split" section of doc.go for why that matters: it lets a
// consumer that only merges/ranks profiles someone else already collected
// (cmd/perf-profile's `-merge` mode) import this package without pulling in
// libchdb.so.
package profile

import "sort"

// Record is the structured per-fixture profile emitted by [ProfileFixture].
// One JSON object per executable fixture; the nightly lane collects them
// into an array artifact and ranks by FanFactor.
type Record struct {
	// Fixture is the fixture identity as "<head>/<name>" (e.g.
	// "promql/lwr_range_rate"), derived from the path relative to
	// test/spec/.
	Fixture string `json:"fixture"`

	// ScanRows is the sum of the deepest (leaf) FROM-source subquery level
	// in every scan pipeline — the rows actually read off the seeded tables
	// before any fan-out stage. A UNION ALL contributes one leaf per arm.
	// The fan-out denominator.
	ScanRows int64 `json:"scan_rows"`

	// PeakIntermediate is the maximum count() over every non-aggregating
	// FROM-source subquery level, including the outer query. The fan-out
	// numerator: the widest the row set gets anywhere in the pipeline.
	PeakIntermediate int64 `json:"peak_intermediate"`

	// FanFactor is PeakIntermediate / ScanRows (1.0 when nothing fans
	// out; >1 when an intermediate stage widened the row set; 0 when
	// ScanRows is 0 — an empty-seed or all-filtered fixture, where every
	// level WAS measured and they all counted zero rows, a degenerate
	// ratio rather than an opaque one). The headline number the nightly
	// lane ranks on. nil (JSON null) — NEVER a fabricated 1.00 —
	// whenever [UncountableLevels] is nonzero: at least one pipeline
	// stage was opaque to the per-level count() decomposition (a CTE
	// reference, a pre-rendered subquery splice, or a RECURSIVE CTE
	// step), so PeakIntermediate does not actually reflect the widest
	// the row set gets anywhere in the pipeline. A 1.00 in that state
	// reads as "no fan-out" on exactly the shapes most likely to hide
	// one — see issue #1519, where emitSetOperation's `&&` CTE arms
	// measured fan_factor=1.00 while costing 4 leaf reads.
	FanFactor *float64 `json:"fan_factor"`

	// UncountableLevels is the number of pipeline stages the profiler
	// could not see through: a WITH-prefixed subquery (CTE reference /
	// pre-rendered subquery splice) the leftmost-descent decomposition
	// refused to enter because its body's names are only in scope at its
	// own level, a RECURSIVE CTE step (detected structurally via EXPLAIN
	// PLAN, wherever in the plan it sits), or a level whose isolated
	// count() outright failed. Nonzero forces [FanFactor] to nil — see
	// that field's doc. Zero means every level the decomposition visited
	// was measured, so FanFactor (when ScanRows > 0) is trustworthy.
	UncountableLevels int `json:"uncountable_levels"`

	// UncountableReasons is one human-readable line per event counted in
	// UncountableLevels, naming the depth (where applicable) and why it
	// was opaque. Kept for debugging an unmeasured fixture — parallel to
	// Levels but for the stages excluded from it.
	UncountableReasons []string `json:"uncountable_reasons,omitempty"`

	// ResultRows is count() of the full outer query — the rows the
	// fixture's SQL ultimately returns.
	ResultRows int64 `json:"result_rows"`

	// HasCrossJoin / HasArrayJoin / HasRecursiveCTE are structural flags
	// read off EXPLAIN PLAN actions=1. A CROSS JOIN or ARRAY JOIN over a
	// non-trivial scan is the classic fan-out shape; a recursive CTE is
	// the structural-recursion shape the cycle guards pin.
	HasCrossJoin    bool `json:"has_cross_join"`
	HasArrayJoin    bool `json:"has_array_join"`
	HasRecursiveCTE bool `json:"has_recursive_cte"`

	// MaxRecursionDepth is a lower-bound estimate of the recursive-CTE
	// expansion: when the plan carries a recursive CTE, it is the
	// result-row count of the recursive level (the closure size). 0 when
	// the query has no recursive CTE.
	MaxRecursionDepth int64 `json:"max_recursion_depth"`

	// PeakBytesRead is the chDB native bytes_read stat for the full
	// query — the peak-memory proxy. chDB's embedded engine exposes
	// neither system.query_log nor peak_memory_usage through any driver
	// surface, so bytes_read (the volume the engine pulled through the
	// pipeline) stands in for peak memory. Larger = more memory pressure.
	PeakBytesRead uint64 `json:"peak_bytes_read"`

	// Levels is the per-subquery-level count() decomposition, deepest
	// first. Kept for debugging an outlier: it shows exactly which
	// pipeline stage widened the row set.
	Levels []LevelCount `json:"levels,omitempty"`

	// Err, when non-empty, records why the fixture could not be fully
	// profiled (seed failure, unrunnable SQL). The fixture still emits a
	// Record so the nightly lane can surface profiling gaps rather than
	// silently dropping them.
	Err string `json:"err,omitempty"`
}

// LevelCount is the count() of one FROM-source subquery level. Depth 0 is
// the outermost query; higher depth is deeper nesting (closer to the
// leaf scan).
type LevelCount struct {
	Depth int   `json:"depth"`
	Count int64 `json:"count"`
}

// SortByFanFactor orders records descending by FanFactor (then by
// PeakIntermediate, then fixture name) so the nightly step-summary lists
// the worst fan-outs first. A nil FanFactor (unmeasured — see that
// field's doc) sorts before every measured value: an unknown fan-out is
// at least as much of a risk signal as a known-bad one, so it must not
// silently sink to the bottom of the list.
func SortByFanFactor(recs []Record) {
	sort.Slice(recs, func(i, j int) bool {
		fi, fj := recs[i].FanFactor, recs[j].FanFactor
		if (fi == nil) != (fj == nil) {
			return fi == nil
		}
		if fi != nil && fj != nil && *fi != *fj {
			return *fi > *fj
		}
		if recs[i].PeakIntermediate != recs[j].PeakIntermediate {
			return recs[i].PeakIntermediate > recs[j].PeakIntermediate
		}
		return recs[i].Fixture < recs[j].Fixture
	})
}
