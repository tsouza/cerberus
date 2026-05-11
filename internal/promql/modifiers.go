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
// subtract from End. `@ start()` / `@ end()` are not yet supported and
// surface as an error.
type evalAnchor struct {
	End    time.Time
	Offset time.Duration
}

func anchorFromSelector(vs *parser.VectorSelector) (evalAnchor, error) {
	a := evalAnchor{Offset: vs.OriginalOffset}
	if vs.StartOrEnd != 0 {
		return evalAnchor{}, fmt.Errorf("promql: `@ start()` / `@ end()` modifiers are not yet supported (lands when the API layer threads the query range through lowering)")
	}
	if vs.Timestamp != nil {
		// Upstream stores @ as Unix milliseconds.
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
