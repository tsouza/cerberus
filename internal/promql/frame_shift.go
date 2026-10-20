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
// both bounds, then by all of it, then by just enough to bring the
// rejected instant inside, which fits a query spanning nearly the whole
// domain; each shift is a multiple of every subquery step. It returns the first frame that fits, recorded as
// [domainFit.frameShift], and the rejection in Prometheus's frame when
// none does.
func fitInFrame(expr parser.Expr, ctx lowerCtx, isolating bool) (parser.Expr, *domainFit, error) {
	out, f, err := fitDomain(expr, ctx, 0, isolating)
	var outside *outsideDomainError
	if err == nil || ctx.end.IsZero() || !errors.As(err, &outside) || !framePinsFit(expr) {
		return out, f, err
	}
	unit := frameUnit(expr)
	at := outside.at.UnixMilli()
	centre := instantDomainMin.UnixMilli() + (instantDomainMax.UnixMilli()-instantDomainMin.UnixMilli())/2
	toCentre := centre - at
	// The least shift moves the rejected instant just inside the domain,
	// clear of it by the margins the fit widens grids by and a unit. A
	// rejected instant lies outside the domain, never at its centre.
	cushion := unit + frameMargin(expr, ctx.step)
	least := int64(0)
	if at < centre {
		least = ceilUnits(instantDomainMin.UnixMilli()-at+cushion, unit)
	} else {
		least = -ceilUnits(at-instantDomainMax.UnixMilli()+cushion, unit)
	}
	for _, shift := range []int64{toCentre / 2 / unit * unit, toCentre / unit * unit, least} {
		if shift == 0 {
			continue
		}
		framed, ff, ferr := fitDomain(shiftPins(expr, shift), ctx.inFrame(shift), shift, isolating)
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

// frameUnit returns the least common multiple, in milliseconds, of the
// step of every subquery in e, and one millisecond when e holds none.
func frameUnit(e parser.Expr) int64 {
	unit := int64(1)
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if n, ok := node.(*parser.SubqueryExpr); ok {
			unit = lcmMillis(unit, subqueryStep(n).Milliseconds())
		}
		return nil
	})
	return unit
}

// frameMargin returns, in milliseconds, the most the fit widens the
// instants of e evaluated at steps of step: each subquery's grid by its
// own step and the step of the evaluation enclosing it
// ([domainFit.fitSubquery]), and a window by the request step.
func frameMargin(e parser.Expr, step time.Duration) int64 {
	margin := step.Milliseconds()
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if n, ok := node.(*parser.SubqueryExpr); ok {
			margin += 2 * subqueryStep(n).Milliseconds()
		}
		return nil
	})
	return margin
}

// subqueryStep returns the step of n's grid: its own, or the default
// subquery step when it names none.
func subqueryStep(n *parser.SubqueryExpr) time.Duration {
	if n.Step <= 0 {
		return defaultSubqueryStep
	}
	return n.Step
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
// is never modified ([mapExpr]).
func mapPins(e parser.Expr, fn func(pin) pin) parser.Expr {
	return mapExpr(e, func(node parser.Expr) parser.Expr {
		switch n := node.(type) {
		case *parser.VectorSelector:
			p := fn(pin{ts: n.Timestamp, startOrEnd: n.StartOrEnd})
			if p.ts == n.Timestamp && p.startOrEnd == n.StartOrEnd {
				return n
			}
			cp := *n
			cp.Timestamp, cp.StartOrEnd = p.ts, p.startOrEnd
			return &cp
		case *parser.SubqueryExpr:
			p := fn(pin{ts: n.Timestamp, startOrEnd: n.StartOrEnd})
			if p.ts == n.Timestamp && p.startOrEnd == n.StartOrEnd {
				return n
			}
			cp := *n
			cp.Timestamp, cp.StartOrEnd = p.ts, p.startOrEnd
			return &cp
		}
		return node
	})
}

// inFrame returns c with its request range moved by shift milliseconds,
// the frame [fitInFrame] fits a query in.
func (c lowerCtx) inFrame(shift int64) lowerCtx {
	if !c.start.IsZero() {
		c.start = moveMillis(c.start, shift)
	}
	if !c.end.IsZero() {
		c.end = moveMillis(c.end, shift)
	}
	return c
}

// moveMillis returns t moved by shift milliseconds, exactly for a shift
// further than a time.Duration holds: a frame may move a query by up to the
// length of the domain, about 584 years.
func moveMillis(t time.Time, shift int64) time.Time {
	return chplan.GridInstant(t, time.Millisecond, shift)
}

// withDomain returns c lowering the fitted query f describes: in its
// frame, when f shifted one, with that frame's shift as the shift of
// every instant the lowering writes.
func (c lowerCtx) withDomain(f *domainFit) lowerCtx {
	c.domain = f
	if f.frameShift != 0 {
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
// milliseconds, in Int64 nanoseconds. A shift whose nanoseconds Int64 does
// not hold, which a frame spanning most of the domain needs, is taken back
// in parts that each do: ts and the instant it moves to lie in the domain,
// and so does every instant a part lands on between them.
func unframeInstant(ts chplan.Expr, shift int64) chplan.Expr {
	part := min(max(shift, -maxDurationMillis), maxDurationMillis)
	moved := &chplan.FuncCall{Fn: chplan.FnFromUnixNanos, Args: []chplan.Expr{&chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{ts}},
		Right: &chplan.LitInt{V: part * int64(time.Millisecond)},
	}}}
	if part == shift {
		return moved
	}
	return unframeInstant(moved, shift-part)
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
			if seen[n] || f.sealed[n] {
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
// which hold every instant Prometheus evaluates at. Every other root
// reports a DateTime64(9) whatever its shift ([unframeInstant]), so every
// run of a split range reports its timestamps in one type.
func unshiftRoot(plan chplan.Node, root parser.Expr, s schema.Metrics, ctx lowerCtx) chplan.Node {
	shift := int64(0)
	if ctx.domain != nil {
		shift = ctx.domain.frameShift
		if sub := rootSubquery(root); sub != nil {
			shift, _ = ctx.domain.shiftOf(sub)
		}
	}
	shift -= ctx.rootFrame
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
	if ctx.rootFrame == 0 && !ctx.domain.rootGridInDomain(root) {
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

// oneInstantStep is the step a range of one instant is lowered at when
// its own step is too long to fit ([lowerCtx.oneInstant]): one second, a
// whole number of seconds as the grid-native shapes need, and a margin
// every frame holds.
const oneInstantStep = time.Second

// oneInstant returns c with its step replaced by [oneInstantStep] when c is
// a range of one instant with a longer step, and reports whether it is.
// Prometheus evaluates such a range at its one instant alone, whatever the
// step, while the lowering reaches a step past every window it reads and
// the fit widens them by the step to match ([domainEnv.step]): a step of
// centuries can leave no frame those margins fit in.
func (c lowerCtx) oneInstant() (lowerCtx, bool) {
	if !c.rangeMode() || !c.start.Equal(c.end) || c.step <= oneInstantStep {
		return c, false
	}
	c.step = oneInstantStep
	return c, true
}

// maxRangeSpan is the longest span of request steps one lowering covers:
// half of what a time.Duration holds, so that the Int64 nanoseconds
// between any two instants of the span, widened by its windows, fit.
const maxRangeSpan = time.Duration(math.MaxInt64 / 2)

// forEachRun calls run with the first and last instant of each consecutive
// run of the instants first, first+step, ... up to last, each run spanning
// at most [maxRangeSpan], and stops at the first error. A step longer than
// a run evaluates one instant per run. step is positive.
func forEachRun(first, last time.Time, step time.Duration, run func(from, to time.Time) error) error {
	runSteps := time.Duration(max(maxRangeSpan.Milliseconds()/step.Milliseconds(), 1))
	for from := first; !from.After(last); {
		to := from.Add((runSteps - 1) * step)
		if to.After(last) {
			to = last
		}
		if err := run(from, to); err != nil {
			return err
		}
		from = to.Add(step)
	}
	return nil
}

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
	var arms []chplan.Node
	err := forEachRun(ctx.start, ctx.end, ctx.step, func(from, to time.Time) error {
		part := ctx
		part.run = true
		part.start, part.end = from, to
		plan, err := lowerFitted(expr, s, part)
		if err != nil {
			return err
		}
		arms = append(arms, plan)
		return nil
	})
	if err != nil {
		return nil, true, err
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

// stampRunInstant returns plan, the lowering of a run of one instant,
// with that instant as its timestamp and grid anchor. A range of one
// instant lowers to the shape of an instant query, which reports no
// timestamp, while the other runs of the range, and the subquery a run
// answers, list their rows' instants ([subqueryAnchorShape]). The instant
// is the run's, in the frame it is lowered in, which [unshiftRoot] then
// moves back as it moves every root's timestamps. Any other plan is
// returned as it is.
func stampRunInstant(plan chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	if !ctx.run || !ctx.start.Equal(ctx.end) {
		return plan
	}
	rows := plan.RowType()
	if rows.Open {
		return plan
	}
	if _, ok := rows.ByName(s.TimestampColumn); ok {
		return plan
	}
	instant := chplan.TimeLiteral(ctx.end)
	var (
		columns     []chplan.Column
		projections []chplan.Projection
	)
	for _, c := range rows.Columns {
		columns = append(columns, c)
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: c.Name}, Alias: c.Name})
	}
	if _, ok := rows.ByName(chplan.RangeWindowAnchorColumn); !ok {
		columns = append(columns, chplan.Column{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor})
		projections = append(projections, chplan.Projection{Expr: instant, Alias: chplan.RangeWindowAnchorColumn})
	}
	columns = append(columns, chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp})
	projections = append(projections, chplan.Projection{Expr: instant, Alias: s.TimestampColumn})
	return &chplan.Project{Roles: columns, Input: plan, Projections: projections}
}
