package chplan

import "time"

// RangeLWR is the single-pass, bounded last-with-respect-to (LWR) plan
// node for a BARE instant-vector selector evaluated over a PromQL
// `query_range` window. It replaces the O(rows × anchors) StepGrid
// CROSS JOIN + per-anchor argMax shape the lowering previously emitted
// (see internal/promql/lower.go:wrapRangeLatestPerSeries) with an
// O(rows × lookback/step) sample-side fan-out that does NOT scale with
// the anchor count N.
//
// Semantics (identical to the StepGrid path it supersedes):
//
//   - The eval grid is `[Start, End]` spaced by Step, end-inclusive
//     (N = (End-Start)/Step + 1 anchors).
//   - At each anchor `t` the value is the most-recent sample in the
//     half-open staleness window `(t - Offset - Lookback, t - Offset]`
//     (default Lookback = 5m). When no sample falls in that window the
//     anchor produces NO row — the PromQL staleness gap.
//   - Output schema is the canonical 4-column Sample contract:
//     `(MetricNameCol, AttributesCol, TimeUnix = anchor_ts, ValueCol)`
//     — one row per (series, anchor) that had a sample, with TimeUnix
//     set to the UNSHIFTED grid anchor (the Offset shifts only the
//     membership window, not the emitted timestamp).
//
// The emitter (internal/chsql.emitRangeLWR) renders this as a single
// pass over Input: each sample row fans out (via arrayJoin over a
// bounded `range(lo, hi)` index set) to ONLY the ≤ Lookback/Step + 1
// anchors whose staleness window contains it, then a
// `GROUP BY (series, anchor_ts)` with `argMax(Value, TimeUnix)` collapses
// each (series, anchor) bucket to its newest in-window sample. Because
// each sample touches a bounded, N-independent number of anchors the
// intermediate (sample, anchor) cardinality is `rows × (Lookback/Step)`,
// constant in the grid width — the linear-in-N blowup is gone.
//
// Input is the matchers-filtered scan (Scan, or Filter-over-Scan, or the
// gauge+sum merge() Scan / companion UnionAll). It must expose the
// MetricNameCol / AttributesCol / TimestampCol / ValueCol columns under
// those names; the dual-table gauge+sum merge for unsuffixed names is
// preserved transparently because it lives inside Input's Scan
// (UnionTables → CH `merge(...)`).
type RangeLWR struct {
	Input Node

	// Start / End define the eval grid; Step is the grid spacing.
	Start time.Time
	End   time.Time
	Step  time.Duration

	// Lookback is the staleness horizon (instantLookback, default 5m).
	// The per-anchor window is `(anchor - Offset - Lookback, anchor - Offset]`.
	Lookback time.Duration

	// Offset is the PromQL `offset` modifier folded onto the selector.
	// It shifts the membership window back by Offset (a negative Offset
	// shifts it forward) WITHOUT moving the emitted anchor timestamp.
	Offset time.Duration

	// Column names on Input (canonical OTel-CH: MetricName / Attributes /
	// TimeUnix / Value).
	MetricNameCol string
	AttributesCol string
	TimestampCol  string
	ValueCol      string
}

func (*RangeLWR) planNode() {}

func (r *RangeLWR) Children() []Node { return []Node{r.Input} }

func (r *RangeLWR) Equal(other Node) bool {
	o, ok := other.(*RangeLWR)
	if !ok {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.Step != o.Step || r.Lookback != o.Lookback || r.Offset != o.Offset {
		return false
	}
	if r.MetricNameCol != o.MetricNameCol || r.AttributesCol != o.AttributesCol {
		return false
	}
	if r.TimestampCol != o.TimestampCol || r.ValueCol != o.ValueCol {
		return false
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}
