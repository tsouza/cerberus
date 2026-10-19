package promql

import (
	"errors"
	"math"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// A query whose instants all lie within a span shorter than the instant
// domain, but not within the domain itself, is answered in a shifted
// frame: every instant the query names, its request range and each `@`
// pin, is moved by one shift, every stored sample is read moved by the
// same shift ([chplan.Scan.TimeShiftMillis]), and the lowering subtracts
// the shift wherever an instant becomes a value or a reported timestamp.
//
// Prometheus's evaluation is invariant under such a move. Every window,
// lookback and offset is relative to an evaluation instant, and its `@`
// arithmetic is int64 arithmetic, which a common shift leaves unchanged
// while nothing overflows. A subquery aligns its grid to absolute multiples
// of its step, so the shift is a multiple of every subquery step. A stored
// sample whose moved timestamp leaves the domain is not read, and no window
// of the shifted query reaches it.

// framePinBound bounds the `@` pins, in milliseconds, of a query fitted in
// a shifted frame. Prometheus's `@` arithmetic over pins within it never
// overflows int64, before or after the shift, so the shift moves every
// instant the arithmetic yields by exactly the shift.
const framePinBound = int64(1) << 62

// fitInFrame fits expr at ctx ([fitDomain]). When Prometheus's own frame
// needs an instant outside the domain, it fits expr again in a frame
// shifted towards the domain's centre, first by half the distance from
// the rejected instant to the centre, which keeps a short query clear of
// both bounds, then by all of it; each shift is rounded to a multiple of
// every subquery step. It returns the first frame that fits, recorded as
// [domainFit.frameShift], and the rejection in Prometheus's frame when
// none does.
func fitInFrame(expr parser.Expr, ctx lowerCtx) (parser.Expr, *domainFit, error) {
	out, f, err := fitDomain(expr, ctx, 0)
	var outside *outsideDomainError
	if err == nil || ctx.end.IsZero() || !errors.As(err, &outside) || !framePinsFit(expr) || !frameableSubqueries(expr) {
		return out, f, err
	}
	unit := frameUnit(expr)
	centre := instantDomainMin.UnixMilli() + (instantDomainMax.UnixMilli()-instantDomainMin.UnixMilli())/2
	toCentre := centre - outside.at.UnixMilli()
	for _, want := range []int64{toCentre / 2, toCentre} {
		shift := want / unit * unit
		if shift == 0 || !offsetFits(shift) {
			continue
		}
		framed, ff, ferr := fitDomain(shiftPins(expr, shift), ctx.inFrame(shift), shift)
		if ferr == nil {
			ff.unframedErr = err
			return framed, ff, nil
		}
	}
	return nil, nil, err
}

// framePinsFit reports whether every `@` pin of e lies within
// [framePinBound].
func framePinsFit(e parser.Expr) bool {
	fits := true
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		var ts *int64
		switch n := node.(type) {
		case *parser.VectorSelector:
			ts = n.Timestamp
		case *parser.SubqueryExpr:
			ts = n.Timestamp
		}
		if ts != nil && (*ts > framePinBound || *ts < -framePinBound) {
			fits = false
		}
		return nil
	})
	return fits
}

// frameableSubqueries reports whether every subquery of e may be lowered
// in a shifted frame: one whose region [shiftableRegion] admits. A region
// it does not admit diverges from Prometheus whatever its instants, so a
// query holding one keeps its rejection.
func frameableSubqueries(e parser.Expr) bool {
	ok := true
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if n, isSub := node.(*parser.SubqueryExpr); isSub && !shiftableRegion(n.Expr) {
			ok = false
		}
		return nil
	})
	return ok
}

// frameUnit returns the least common multiple, in milliseconds, of the
// step of every subquery in e, and one millisecond when e holds none.
func frameUnit(e parser.Expr) int64 {
	unit := int64(1)
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if n, ok := node.(*parser.SubqueryExpr); ok {
			step := n.Step
			if step <= 0 {
				step = defaultSubqueryStep
			}
			unit = lcmMillis(unit, step.Milliseconds())
		}
		return nil
	})
	return unit
}

// shiftPins returns e with every `@` pin moved by shift milliseconds
// ([mapPins]).
func shiftPins(e parser.Expr, shift int64) parser.Expr {
	return mapPins(e, func(p pin) pin {
		if p.ts == nil {
			return p
		}
		ts := *p.ts + shift
		return pin{ts: &ts}
	})
}

// resolveRequestPins returns e with every `@ start()` and `@ end()` pin
// replaced by the instant it names, start or end milliseconds, as
// Prometheus resolves it before evaluating ([mapPins]).
func resolveRequestPins(e parser.Expr, start, end int64) parser.Expr {
	return mapPins(e, func(p pin) pin {
		switch p.startOrEnd {
		case parser.START:
			return pin{ts: &start}
		case parser.END:
			return pin{ts: &end}
		}
		return p
	})
}

// pin is the `@` modifier of a selector or subquery: an instant in
// milliseconds, or `start()` or `end()`.
type pin struct {
	ts         *int64
	startOrEnd parser.ItemType
}

// mapPins returns e with the `@` pin of every selector and subquery
// replaced by what fn maps it to; fn returns a pin unchanged to keep it.
// A node whose subtree keeps every pin is returned as it is, and e itself
// is never modified.
func mapPins(e parser.Expr, fn func(pin) pin) parser.Expr {
	switch n := e.(type) {
	case *parser.VectorSelector:
		p := fn(pin{ts: n.Timestamp, startOrEnd: n.StartOrEnd})
		if p.ts == n.Timestamp && p.startOrEnd == n.StartOrEnd {
			return n
		}
		cp := *n
		cp.Timestamp, cp.StartOrEnd = p.ts, p.startOrEnd
		return &cp
	case *parser.MatrixSelector:
		vs := mapPins(n.VectorSelector, fn)
		if vs == n.VectorSelector {
			return n
		}
		cp := *n
		cp.VectorSelector = vs
		return &cp
	case *parser.SubqueryExpr:
		inner := mapPins(n.Expr, fn)
		p := fn(pin{ts: n.Timestamp, startOrEnd: n.StartOrEnd})
		if inner == n.Expr && p.ts == n.Timestamp && p.startOrEnd == n.StartOrEnd {
			return n
		}
		cp := *n
		cp.Expr, cp.Timestamp, cp.StartOrEnd = inner, p.ts, p.startOrEnd
		return &cp
	case *parser.Call:
		args := mapPinsAll(n.Args, fn)
		if args == nil {
			return n
		}
		cp := *n
		cp.Args = args
		return &cp
	case *parser.AggregateExpr:
		inner, param := mapPins(n.Expr, fn), n.Param
		if n.Param != nil {
			param = mapPins(n.Param, fn)
		}
		if inner == n.Expr && param == n.Param {
			return n
		}
		cp := *n
		cp.Expr, cp.Param = inner, param
		return &cp
	case *parser.BinaryExpr:
		lhs, rhs := mapPins(n.LHS, fn), mapPins(n.RHS, fn)
		if lhs == n.LHS && rhs == n.RHS {
			return n
		}
		cp := *n
		cp.LHS, cp.RHS = lhs, rhs
		return &cp
	case *parser.ParenExpr:
		inner := mapPins(n.Expr, fn)
		if inner == n.Expr {
			return n
		}
		cp := *n
		cp.Expr = inner
		return &cp
	case *parser.UnaryExpr:
		inner := mapPins(n.Expr, fn)
		if inner == n.Expr {
			return n
		}
		cp := *n
		cp.Expr = inner
		return &cp
	case *parser.StepInvariantExpr:
		inner := mapPins(n.Expr, fn)
		if inner == n.Expr {
			return n
		}
		cp := *n
		cp.Expr = inner
		return &cp
	default:
		return e
	}
}

// mapPinsAll returns args with [mapPins] applied to each, or nil when no
// argument changes.
func mapPinsAll(args parser.Expressions, fn func(pin) pin) parser.Expressions {
	var out parser.Expressions
	for i, a := range args {
		moved := mapPins(a, fn)
		if moved == a {
			continue
		}
		if out == nil {
			out = append(parser.Expressions(nil), args...)
		}
		out[i] = moved
	}
	return out
}

// inFrame returns c with its request range moved by shift milliseconds,
// the frame [fitInFrame] fits a query in.
func (c lowerCtx) inFrame(shift int64) lowerCtx {
	by := time.Duration(shift) * time.Millisecond
	if !c.start.IsZero() {
		c.start = c.start.Add(by)
	}
	if !c.end.IsZero() {
		c.end = c.end.Add(by)
	}
	return c
}

// withDomain returns c lowering the fitted query f describes: in its
// frame, when f shifted one, with that frame's shift as the shift of
// every instant the lowering writes.
func (c lowerCtx) withDomain(f *domainFit) lowerCtx {
	c.domain = f
	if f != nil && f.frameShift != 0 {
		c = c.inFrame(f.frameShift)
		c.shiftMillis = f.frameShift
	}
	return c
}

// framed reports whether the query is lowered in a shifted frame.
func (c lowerCtx) framed() bool { return c.domain != nil && c.domain.frameShift != 0 }

// sampleMillis renders the Unix milliseconds of the stored sample read
// through ts as Prometheus stores them: the sample's own milliseconds, with
// a shifted frame's shift taken back in Int64.
func (c lowerCtx) sampleMillis(ts chplan.Expr) chplan.Expr {
	millis := &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{ts}}
	if !c.framed() {
		return millis
	}
	return &chplan.Binary{Op: chplan.OpSub, Left: millis, Right: &chplan.LitInt{V: c.domain.frameShift}}
}

// sampleSecondsValue renders the value `timestamp(v)` reports for the
// stored sample read through ts: Prometheus's `float64(t) / 1000` over
// [lowerCtx.sampleMillis].
func (c lowerCtx) sampleSecondsValue(ts chplan.Expr) chplan.Expr {
	return millisSecondsValue(c.sampleMillis(ts))
}

// unframeInstant renders the DateTime64(9) instant ts moved back by shift
// milliseconds, in Int64 nanoseconds.
func unframeInstant(ts chplan.Expr, shift int64) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnFromUnixNanos, Args: []chplan.Expr{&chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{ts}},
		Right: &chplan.LitInt{V: shift * int64(time.Millisecond)},
	}}}
}

// unframeMillis renders the Int64 Unix milliseconds of the instant ts
// moved back by shift milliseconds.
func unframeMillis(ts chplan.Expr, shift int64) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnToInt64, Args: []chplan.Expr{&chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{ts}},
		Right: &chplan.LitInt{V: shift},
	}}}
}

// errFramedStorage is why a plan lowered in a shifted frame is not
// answered: it reads a table whose rows carry no sample timestamp the
// scan could move.
var errFramedStorage = errors.New("promql: a table without a sample timestamp is read in a shifted frame")

// frameStorage moves every read of plans into the frame of f: each scan
// of a table with a sample timestamp reads it moved by the frame's shift,
// and each range window reports its timestamps moved back by it. It
// mutates the freshly lowered plans in place, visiting a node they share
// once.
func frameStorage(f *domainFit, plans ...chplan.Node) error {
	if f == nil || f.frameShift == 0 {
		return nil
	}
	seen := map[chplan.Node]bool{}
	var err error
	for _, plan := range plans {
		chplan.WalkDeep(plan, func(n chplan.Node) bool {
			if seen[n] {
				return false
			}
			seen[n] = true
			switch v := n.(type) {
			case *chplan.Scan:
				if _, rerr := (chplan.Schema{Columns: v.Roles}).UniqueNamedRole(chplan.RoleTimestamp); rerr == nil {
					v.TimeShiftMillis = f.frameShift
				} else if v.Database == "" {
					err = errFramedStorage
				}
			case *chplan.RangeWindow:
				v.TimeShiftMillis += f.frameShift
			}
			return true
		})
	}
	return err
}

// unshiftRoot returns plan, the lowering of the query root, with the
// timestamps it reports moved back by the shift they were lowered with:
// the frame's, plus a shifted subquery root's own. Every root but a
// subquery answers evaluation instants of the request or stored
// timestamps, which lie in the domain, and reports them as a
// DateTime64(9). A subquery root answers its grid instants themselves;
// when they leave the domain ([domainFit.rootGridInDomain]) it reports
// them as Int64 Unix milliseconds, the precision of a PromQL timestamp,
// which hold every instant Prometheus evaluates at; so it does when its
// shift has no Int64 nanosecond form.
func unshiftRoot(plan chplan.Node, root parser.Expr, s schema.Metrics, ctx lowerCtx) chplan.Node {
	shift := int64(0)
	if ctx.domain != nil {
		shift = ctx.domain.frameShift
		if sub := rootSubquery(root); sub != nil {
			shift, _ = ctx.domain.shiftOf(sub)
		}
	}
	if shift == 0 {
		return plan
	}
	rows := plan.RowType()
	ts, err := rows.UniqueNamedRole(chplan.RoleTimestamp)
	if err != nil {
		named, ok := rows.ByName(s.TimestampColumn)
		if !ok && !rows.Open {
			// A root that reports no timestamp has none to move back.
			return plan
		}
		ts = chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp, HistogramField: named.HistogramField}
	}
	moved := chplan.Projection{Expr: unframeInstant(&chplan.ColumnRef{Name: ts.Name}, shift), Alias: ts.Name}
	if !ctx.domain.rootGridInDomain(root) || !offsetFits(shift) {
		moved.Expr = unframeMillis(&chplan.ColumnRef{Name: ts.Name}, shift)
	}
	if rows.Open {
		return &chplan.Project{Roles: []chplan.Column{ts}, Input: plan, Replacements: []chplan.Projection{moved}}
	}
	// A closed row type names every column, so the projection lists them
	// in order rather than replacing one under a star.
	projections := make([]chplan.Projection, 0, len(rows.Columns))
	for _, c := range rows.Columns {
		if c.Name == ts.Name {
			projections = append(projections, moved)
			continue
		}
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: c.Name}, Alias: c.Name})
	}
	return &chplan.Project{Roles: rows.Columns, Input: plan, Projections: projections}
}

// guardPlans returns the plans of the scalar guards the lowering recorded.
func (c lowerCtx) guardPlans() []chplan.Node {
	if c.guards == nil {
		return nil
	}
	plans := make([]chplan.Node, 0, len(*c.guards))
	for _, g := range *c.guards {
		plans = append(plans, g.Plan)
	}
	return plans
}

// maxRangeSpan is the longest span of request steps one lowering covers:
// half of what a time.Duration holds, so that the Int64 nanoseconds
// between any two instants of the span, widened by its windows, fit.
const maxRangeSpan = time.Duration(math.MaxInt64 / 2)

// errSplitOpenRows is why a range query split into runs of its steps is
// not answered: its result names no closed list of columns the runs'
// union could align.
var errSplitOpenRows = errors.New("promql: a range query split into runs of its steps reports no closed list of result columns")

// lowerRangeInParts lowers a range query whose steps span more than
// [maxRangeSpan] as the union of consecutive runs of its steps, each
// spanning at most that and fitted in a frame of its own
// ([fitInFrame]). Prometheus evaluates every step of a range query on its
// own, so the union answers what the whole range does. A pin naming the
// request range, `@ start()` or `@ end()`, is resolved first to the
// instant it names, as Prometheus resolves it, since a run's own range is
// not the request's. It reports false for a range that needs no split.
func lowerRangeInParts(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool, error) {
	// The span may exceed what a time.Duration holds, so it is measured in
	// milliseconds.
	stepMillis := ctx.step.Milliseconds()
	if stepMillis <= 0 || ctx.end.UnixMilli()-ctx.start.UnixMilli() <= maxRangeSpan.Milliseconds() {
		return nil, false, nil
	}
	expr = resolveRequestPins(expr, ctx.start.UnixMilli(), ctx.end.UnixMilli())
	runSteps := time.Duration(maxRangeSpan.Milliseconds() / stepMillis)
	var arms []chplan.Node
	for from := ctx.start; !from.After(ctx.end); {
		part := ctx
		part.start, part.end = from, from.Add((runSteps-1)*ctx.step)
		if part.end.After(ctx.end) {
			part.end = ctx.end
		}
		plan, err := lowerFitted(expr, s, part)
		if err != nil {
			return nil, true, err
		}
		arms = append(arms, plan)
		from = part.end.Add(ctx.step)
	}
	// UNION ALL matches columns by position, so every run lists the first
	// run's columns in its order.
	rows := arms[0].RowType()
	if rows.Open {
		return nil, true, errSplitOpenRows
	}
	for i, arm := range arms {
		projections := make([]chplan.Projection, 0, len(rows.Columns))
		for _, c := range rows.Columns {
			projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: c.Name}, Alias: c.Name})
		}
		arms[i] = &chplan.Project{Roles: rows.Columns, Input: arm, Projections: projections}
	}
	return &chplan.UnionAll{Inputs: arms}, true, nil
}

// bucketRowsAnswer reports whether the DELTA-prefix aggregate and the
// downsample tier may answer rw. They answer from bucket rows aligned to
// the window's own edges, which a cut window does not carry, and carry no
// sample timestamp a shifted frame could move; the raw samples answer
// such a window alone.
func (c lowerCtx) bucketRowsAnswer(rw *chplan.RangeWindow) bool {
	return rw.StartBeyond == 0 && rw.EndBeyond == 0 && !c.framed()
}
