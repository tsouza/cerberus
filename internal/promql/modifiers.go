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
type lowerCtx struct {
	start time.Time
	end   time.Time
}

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
	var base chplan.Expr
	if a.End.IsZero() {
		base = &chplan.FuncCall{
			Name: "now64",
			Args: []chplan.Expr{&chplan.LitInt{V: 9}},
		}
	} else {
		base = &chplan.FuncCall{
			Name: "toDateTime64",
			Args: []chplan.Expr{
				&chplan.LitString{V: a.End.Format("2006-01-02 15:04:05.000000000")},
				&chplan.LitInt{V: 9},
			},
		}
	}
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
