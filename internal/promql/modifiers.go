package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// evalAnchor describes the time-anchor that a VectorSelector's `@` and
// `offset` modifiers resolve to. End is the absolute anchor (zero means
// "use eval time, lower as `now64(9)` in SQL"); Offset is the shift to
// subtract from End.
//
// `@ start()` / `@ end()` resolve to the query-range start / end times
// when the API layer threads them through via [LowerAt]. Plain [Lower]
// (no range context) rejects start/end modifiers because the anchor
// times are not known at lowering time.
type evalAnchor struct {
	End    time.Time
	Offset time.Duration
}

// lowerCtx carries the query-time information needed by some modifier
// lowerings (`@ start()` / `@ end()`). Zero-value start/end means "no
// range threaded", and the start/end modifier path returns an error so
// callers see the misconfiguration rather than a silently wrong query.
//
// inRangeVector marks recursive descents into a range-vector context
// (under a matrix selector wrapped by rate / *_over_time / subquery).
// `lowerVectorSelector` checks the flag to decide whether to apply
// PromQL's instant-vector Latest-With-Respect-to-T (LWR) projection:
// at the top level (and under aggregations / arithmetic on instant
// vectors) we collapse to one row per series with the latest sample
// within the staleness window; under a range vector we leave every
// in-window sample for the RangeWindow node to consume.
type lowerCtx struct {
	start time.Time
	end   time.Time
	// step is the query_range step duration. When > 0 the lowering is
	// in "range mode" — synthetic-vector lowerings whose result would
	// otherwise have no driving input (`time()`, `vector(scalar)`,
	// zero-arg date fns, `absent(...)`) materialise a StepGrid source
	// so the emitted SQL produces one row per step in `[start, end]`,
	// matching Prom's `query_range` semantics ("emit one sample per
	// (start, end, step) regardless of input data"). step == 0 means
	// instant mode (start == end == eval ts) — the OneRow source is
	// kept for byte-stable SQL on the existing fixtures.
	step          time.Duration
	inRangeVector bool
	// metadataFullRange marks a lowering driven by a Prometheus metadata
	// endpoint (/series, /labels, /label/<name>/values). Metadata
	// enumerates which series/labels/values have ANY sample in the
	// requested [start,end] window — NOT the latest-per-series staleness
	// semantics of an instant query. When set, the bare-selector path
	// emits a closed [start,end] Timestamp filter (start/end carried in
	// ctx.start/ctx.end; a zero bound is omitted) and collapses to one
	// row per series WITHOUT the 5m LWR staleness window — so a series
	// whose only sample sits early in the window still surfaces. Set only
	// by [LowerMetadataRange]; every query-evaluation path leaves it
	// false and keeps the instant/range LWR semantics.
	metadataFullRange bool
	// outerByLabels carries the by-clause labels of an enclosing
	// vector aggregation, threaded down so the inner selector path
	// can inflate Attributes with the top-level OTel-CH columns
	// (currently `service_name` → `ServiceName`) the outer aggregate
	// needs. Empty (the default) means "no outer by-clause referencing
	// a top-level column" — the augmenting Project is suppressed and
	// the bare-selector / range-vector plan stays byte-identical with
	// pre-#232 fixtures.
	//
	// Only `by(...)` propagates; `without(...)` exclusion semantics
	// don't reference specific columns so the slot stays nil for the
	// without branch (see [lowerAggregate]).
	outerByLabels []string

	// experimentalTSGridRange opts the eligible `rate(<counter>[<range>])`
	// query_range shape into the ClickHouse-native `timeSeriesRateToGrid`
	// lowering (a RangeWindowNative node) instead of the default
	// arrayJoin fan-out (a RangeWindow node). Threaded from
	// Config.ExperimentalTSGridRange via [LowerAtRangeOpts]. Default
	// false — every other lowering path is byte-identical to today's, and
	// the only callers that set it true are the query_range handler
	// adapters. See [lowerRangeVectorCall] for the gating predicate.
	experimentalTSGridRange bool

	// attributesPreMerged signals that the selector input already carries
	// the resource-attribute merge in its `Attributes` column — i.e. each
	// arm projected `mapUpdate(sanitize(ResourceAttributes), Attributes)`
	// itself, so the raw `ResourceAttributes` column is NOT in scope above
	// the arm (e.g. the classic-histogram companion UnionAll, whose arms
	// collapse to the canonical Sample quadruple). When set,
	// [selectorAttributesExpr] uses the bare `Attributes` ColumnRef as the
	// outer-by overlay base instead of re-deriving the resource merge,
	// which would reference an out-of-scope `ResourceAttributes`.
	attributesPreMerged bool
}

// withAttributesPreMerged returns a copy of c with attributesPreMerged
// set, used by the companion-union path whose arms merge resource
// attributes per-arm before the union collapses the column set.
func (c lowerCtx) withAttributesPreMerged() lowerCtx {
	out := c
	out.attributesPreMerged = true
	return out
}

// withOuterByLabels returns a copy of c with outerByLabels set to
// the given list. nil/empty input clears the slot — the inner
// augmenting Project is suppressed and the lowering produces today's
// byte-stable plan tree. Mirrors the
// [internal/logql.lowerCtx.withOuterByLabels] convention from PR #666.
func (c lowerCtx) withOuterByLabels(labels []string) lowerCtx {
	out := c
	out.outerByLabels = labels
	return out
}

// instantLookback is the default Prometheus staleness window applied
// when an instant-vector selector picks the latest sample per series.
// Prom defaults to 5 minutes; cerberus matches the upstream constant
// rather than reading a per-deployment override so the LWR predicate
// behaves predictably across environments.
const instantLookback = 5 * time.Minute

func anchorFromSelector(vs *parser.VectorSelector, ctx lowerCtx) (evalAnchor, error) {
	a := evalAnchor{Offset: vs.OriginalOffset}
	switch vs.StartOrEnd {
	case parser.START:
		if ctx.start.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: `@ start()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.start.UTC()
	case parser.END:
		if ctx.end.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: `@ end()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.end.UTC()
	case 0:
		// no start/end modifier; fall through to literal @ handling
	default:
		return evalAnchor{}, fmt.Errorf("promql: unexpected StartOrEnd token %v", vs.StartOrEnd)
	}
	if vs.Timestamp != nil {
		// Upstream stores @ as Unix milliseconds. A literal @<ts>
		// modifier takes precedence over start()/end() (they can't
		// both be set — the parser enforces that).
		a.End = time.UnixMilli(*vs.Timestamp).UTC()
	}
	// No absolute @-pin: anchor the window to the query's eval instant
	// (ctx.end) so an instant /api/v1/query at time=T evaluates the
	// (T-range, T] window AT T — not at ClickHouse wall-clock now64(9).
	// EVERY range-vector lowering reaches the window anchor through this
	// one helper (matrix selector, sum_over_time / rate / increase / delta
	// / deriv, predict_linear, holt_winters, quantile_over_time,
	// histogram_quantile over-time, the bare-selector LWR staleness
	// window), so doing the back-fill HERE makes eval-instant anchoring
	// uniform and removes the per-caller duplication whose omission was the
	// rc.8 instant-window empty-hole bug: lowerRangeVectorCall + range_fns
	// left End zero, so emit rendered now64(9) and the window silently
	// became (serverNow-range, serverNow], ignoring time=T.
	//
	// Guarded on a.End.IsZero(): an @ / @start() / @end() / @<ts> pin (set
	// above) is preserved untouched. A genuinely context-free Lower() (no
	// eval range) leaves ctx.end zero, so End stays zero and emit falls
	// back to now64(9) — the correct neutral anchor when there is no
	// requested eval instant. In range mode the per-step / broadcast paths
	// re-pin End to the grid anchor as before; writing ctx.end here first
	// is idempotent with that.
	if a.End.IsZero() && !ctx.end.IsZero() {
		a.End = ctx.end.UTC()
	}
	return a, nil
}

// hasModifier reports whether vs has anything that affects the time
// anchor — `@`, `offset`, or `@ start()` / `@ end()`.
func hasModifier(vs *parser.VectorSelector) bool {
	return vs.Timestamp != nil || vs.OriginalOffset != 0 || vs.StartOrEnd != 0
}

// hasAbsoluteAt reports whether vs pins an ABSOLUTE evaluation anchor —
// `@<unix-ts>` (vs.Timestamp), `@ start()`, or `@ end()` (vs.StartOrEnd).
// A bare `offset <d>` (no `@`) does NOT count: `offset` is a relative
// shift applied against the surrounding query's eval time, so it remains
// per-step in range mode and the per-step LWR wrap stays correct.
//
// The signal drives the "fixed-anchor" OneRow + broadcast optimization in
// range mode: when this is true and ctx.step > 0, the same anchor value
// applies at every step grid point, so the SQL evaluates the LWR window
// ONCE and broadcasts the per-series result across the step grid via a
// CrossJoin instead of fanning the LWR window across N step anchors.
//
// Closes follow-up #2 from Pool-AK's per-step LWR rework (PR #347).
func hasAbsoluteAt(vs *parser.VectorSelector) bool {
	return vs.Timestamp != nil || vs.StartOrEnd != 0
}

// timeBoundExpr builds `<col> <= <anchor>` for an instant-vector
// VectorSelector with `@` or `offset`. The anchor is `now64(9)` (or a
// literal toDateTime64) optionally minus a nanosecond interval.
func timeBoundExpr(col string, a evalAnchor) chplan.Expr {
	base := anchorBaseExpr(a)
	bound := base
	// `a.Offset != 0` so a negative offset (Prom's forward-shift form,
	// `metric offset -5m` evaluates at `t - (-5m) = t + 5m`) still
	// emits the subtract — CH interval arithmetic flips the sign for us
	// and renders `anchor - toIntervalNanosecond(-N)` as `anchor + N`.
	if a.Offset != 0 {
		bound = &chplan.Binary{
			Op:   chplan.OpSub,
			Left: base,
			Right: &chplan.FuncCall{
				Name: "toIntervalNanosecond",
				Args: []chplan.Expr{&chplan.LitInt{V: a.Offset.Nanoseconds()}},
			},
		}
	}
	return &chplan.Binary{
		Op:    chplan.OpLe,
		Left:  &chplan.ColumnRef{Name: col},
		Right: bound,
	}
}

// anchorBaseExpr returns the SQL expression for an `evalAnchor`'s
// reference time. Zero anchor renders as `now64(9)`; an absolute
// anchor renders as `toDateTime64('YYYY-MM-DD HH:MM:SS.fffffffff',
// 9)`. The Offset field is NOT applied here — callers compose the
// subtract themselves (timeBoundExpr applies an offset; the LWR
// staleness predicate composes its own offset+lookback delta).
func anchorBaseExpr(a evalAnchor) chplan.Expr {
	if a.End.IsZero() {
		return chplan.NowNano()
	}
	return &chplan.FuncCall{
		Name: "toDateTime64",
		Args: []chplan.Expr{
			&chplan.LitString{V: a.End.Format("2006-01-02 15:04:05.000000000")},
			&chplan.LitInt{V: chplan.NanoScale},
		},
	}
}
