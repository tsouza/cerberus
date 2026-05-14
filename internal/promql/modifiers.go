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
	return a, nil
}

// hasModifier reports whether vs has anything that affects the time
// anchor — `@`, `offset`, or `@ start()` / `@ end()`.
func hasModifier(vs *parser.VectorSelector) bool {
	return vs.Timestamp != nil || vs.OriginalOffset != 0 || vs.StartOrEnd != 0
}

// timeBoundExpr builds `<col> <= <anchor>` for an instant-vector
// VectorSelector with `@` or `offset`. The anchor is `now64(9)` (or a
// literal toDateTime64) optionally minus a nanosecond interval.
func timeBoundExpr(col string, a evalAnchor) chplan.Expr {
	base := anchorBaseExpr(a)
	bound := base
	if a.Offset > 0 {
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
		return &chplan.FuncCall{
			Name: "now64",
			Args: []chplan.Expr{&chplan.LitInt{V: 9}},
		}
	}
	return &chplan.FuncCall{
		Name: "toDateTime64",
		Args: []chplan.Expr{
			&chplan.LitString{V: a.End.Format("2006-01-02 15:04:05.000000000")},
			&chplan.LitInt{V: 9},
		},
	}
}
