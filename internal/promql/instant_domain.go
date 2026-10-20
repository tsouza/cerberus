package promql

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/parser/posrange"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The instant domain is the span of absolute instants the lowering writes
// into ClickHouse SQL exactly: the instants a DateTime64(9) value holds,
// int64 nanoseconds since the Unix epoch. It is also the span a storage
// column holds.
var (
	instantDomainMin = chplan.StorageMinTime
	instantDomainMax = chplan.StorageMaxTime
)

// maxDurationMillis is the longest offset, in milliseconds, a
// time.Duration holds.
const maxDurationMillis = int64(math.MaxInt64 / time.Millisecond)

// emptyVectorThreshold is the comparison threshold of
// [emptyInstantVector]: `vector(0) < -1` keeps no sample.
const emptyVectorThreshold = -1

// instantDomainError rejects a query that needs the instant t, which lies
// outside the instant domain. The message is built here, as a fmt.Errorf
// call, so the rejection-parity catalogue finds the site.
func instantDomainError(t time.Time) error {
	t = t.UTC()
	return &outsideDomainError{at: t, err: fmt.Errorf("promql: the query needs instant %s, outside the range cerberus supports (%s to %s): ClickHouse cannot represent it exactly",
		t.Format(time.RFC3339Nano), instantDomainMin.Format(time.RFC3339Nano), instantDomainMax.Format(time.RFC3339Nano))}
}

// outsideDomainError is the rejection [instantDomainError] returns.
type outsideDomainError struct {
	// at is the instant outside the domain the query needs.
	at time.Time
	// err carries the message.
	err error
}

func (e *outsideDomainError) Error() string { return e.err.Error() }

// domainEnv is the span of instants one expression is evaluated at.
type domainEnv struct {
	// known reports whether the lowering has an evaluation instant at all.
	// A bare [Lower] has none, and an unpinned selector then renders
	// now64(9).
	known  bool
	lo, hi time.Time
	// step is the spacing of the evaluation instants. The lowering snaps a
	// bound down to a step multiple or reaches one step past a window, so
	// the domain check widens every window by it.
	step time.Duration
	// start and end are the request bounds `@ start()` and `@ end()` name.
	start, end time.Time
	// ev is Prometheus's evaluator state at this node, meaningful when
	// hasEv is set: the request instants are known.
	ev    promEval
	hasEv bool
	// wrapped reports that the nearest `@` pin above evaluates at a wrapped
	// instant (see prom_at_offset.go), so every storage read below it is
	// empty. pin is that pin's own instant.
	wrapped bool
	pin     time.Time
	// frameMillis is how far, in milliseconds, the instants of env lie
	// after the instants Prometheus evaluates at ([fitInFrame]).
	frameMillis int64
	// inSubquery reports that the expression is evaluated on the grid of
	// an enclosing subquery.
	inSubquery bool
}

// domainFit is what fitting a query decides for the lowering beyond the
// rewritten expression.
type domainFit struct {
	// shifts maps every node of a shifted region to the region's shift in
	// milliseconds, and a selector pinned inside such a region to zero
	// ([domainFit.shiftRegion]).
	shifts map[parser.Node]int64
	// emptyReads holds the selectors that read no sample but stay in the
	// query because absent or absent_over_time reads their matchers.
	emptyReads map[*parser.VectorSelector]bool
	// grids maps each shifted subquery to the instants its grid spans
	// before the shift, the timestamps it answers as the query's root.
	grids map[*parser.SubqueryExpr][2]time.Time
	// gridShifts maps the inner expression of each shifted subquery,
	// parentheses removed, to the shift of its grid ([domainFit.gridShift]).
	gridShifts map[parser.Expr]int64
	// beyond holds, in the order the fit met them, the matrix selectors it
	// cut for a function that extrapolates to the window's edges.
	beyond []*beyondCut
	// keepSelectors is set while the fit walks an argument whose selector
	// a function reads by its matchers (absent, info's second argument),
	// which must stay a selector ([cutSelector]).
	keepSelectors bool
	// frameShift is the shift, in milliseconds, of the frame the whole
	// query is fitted and lowered in, zero when it is Prometheus's own
	// ([fitInFrame]).
	frameShift int64
	// unframedErr is the rejection the query raises in Prometheus's own
	// frame, which a lowering in the shifted frame that cannot answer the
	// query returns.
	unframedErr error
	// isolating is set while the fit isolates every outermost subquery.
	isolating bool
	// isolated maps the inner expression, parentheses removed, of each
	// isolated subquery to the state of the evaluator Prometheus runs that
	// inner expression with. An isolated subquery is a region that reads
	// no storage of its own: its inner expression is fitted and lowered on
	// its own, as a range query over the subquery's grid
	// ([lowerIsolatedSubquery]).
	isolated map[parser.Expr]promEval
	// sealed holds the plans of isolated subqueries, which are lowered in
	// frames of their own and which [frameStorage] therefore leaves as
	// they are.
	sealed map[chplan.Node]bool
	// readBounds maps each selector, by its position in the query text,
	// whose storage read Prometheus bounds with wrapped sums to a window
	// that holds only some of the samples its evaluation needs, to that
	// read in Unix milliseconds ([promSelectorHint]). Every scan the
	// selector lowers to reads only those rows ([domainFit.boundReads]).
	readBounds map[posrange.PositionRange]chplan.MillisSpan
	// boundScans holds the scans [domainFit.boundReads] has settled, and
	// selectorScans the scans it settled for each selector, by position.
	boundScans    map[*chplan.Scan]bool
	selectorScans map[posrange.PositionRange][]*chplan.Scan
}

// isolation returns the evaluator state of the inner expression of sub
// when sub is isolated.
func (f *domainFit) isolation(sub *parser.SubqueryExpr) (promEval, bool) {
	if f == nil {
		return promEval{}, false
	}
	ev, ok := f.isolated[unwrapParens(sub.Expr)]
	return ev, ok
}

// seal records plan as lowered in a frame of its own.
func (f *domainFit) seal(plan chplan.Node) {
	if f.sealed == nil {
		f.sealed = map[chplan.Node]bool{}
	}
	f.sealed[plan] = true
}

// shiftOf returns the time shift node is lowered with, the shift of its
// region on top of the query's frame, and whether either is recorded.
func (f *domainFit) shiftOf(node parser.Node) (int64, bool) {
	if f == nil {
		return 0, false
	}
	shift, ok := f.shifts[node]
	return shift + f.frameShift, ok || f.frameShift != 0
}

// cutsStorage reports whether the fit cuts windows back to the stored
// range. A shifted frame does not: its instants are not those a storage
// column holds.
func (f *domainFit) cutsStorage() bool { return f.frameShift == 0 }

// gridShift returns the time shift of the grid of subquery sub, zero
// when its grid is not shifted. It is looked up by sub's inner expression,
// because the lowering copies subquery nodes, unwrapping parentheses on
// the way, but never copies the expressions below them.
func (f *domainFit) gridShift(sub *parser.SubqueryExpr) int64 {
	if f == nil {
		return 0
	}
	return f.gridShifts[unwrapParens(sub.Expr)]
}

// readsNothing reports whether vs is a selector the fit found to read no
// sample.
func (f *domainFit) readsNothing(vs *parser.VectorSelector) bool {
	return f != nil && f.emptyReads[vs]
}

// fitInstantDomain prepares expr for lowering at ctx so that no instant
// outside the instant domain reaches SQL.
//
// Every `@` pin is resolved to the instant Prometheus evaluates it at,
// including the wrapped instant its overflowing arithmetic produces (see
// prom_at_offset.go), and a pinned subquery whose instant wrapped is
// re-pinned there.
//
// A selector whose sample window lies wholly outside what a storage column
// can hold, or that sits below a wrapped pin, reads no sample. An
// expression that is empty for that reason is replaced by
// [emptyInstantVector], which ClickHouse answers empty at in-domain
// instants; absent and absent_over_time of such a selector answer from
// its matchers alone.
//
// A pinned subquery that reads no storage but whose evaluation instants
// leave the domain is moved into the domain whole: its pin is shifted by
// a multiple of every step inside it, which keeps every grid aligned as
// Prometheus aligns it, and the lowering subtracts the shift wherever an
// evaluation instant becomes a value ([lowerCtx.evalMillis]).
//
// A query whose instants no single shift brings into the domain, because
// its subqueries reach further back than the domain is long, is fitted
// again with each outermost subquery isolated ([domainFit.isolated]).
//
// Every other expression that needs an instant outside the domain is
// rejected with [instantDomainError]. expr itself is never modified: a
// rewritten node is a copy.
func fitInstantDomain(expr parser.Expr, ctx lowerCtx) (parser.Expr, *domainFit, error) {
	out, f, err := fitInFrame(expr, ctx, false)
	// Every rejection of the fit names an instant outside the domain
	// ([outsideDomainError]), which only isolating a subquery can answer.
	if err == nil || !holdsSubquery(expr) {
		return out, f, err
	}
	if isolated, fi, ierr := fitInFrame(expr, ctx, true); ierr == nil {
		return isolated, fi, nil
	}
	return nil, nil, err
}

// holdsSubquery reports whether e holds a subquery.
func holdsSubquery(e parser.Expr) bool {
	found := false
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		_, ok := node.(*parser.SubqueryExpr)
		found = found || ok
		return nil
	})
	return found
}

// fitDomain fits expr at ctx, whose instants lie frameShift milliseconds
// after Prometheus's own ([fitInFrame]). isolating isolates every
// outermost subquery ([domainFit.isolated]).
func fitDomain(expr parser.Expr, ctx lowerCtx, frameShift int64, isolating bool) (parser.Expr, *domainFit, error) {
	env := domainEnv{start: ctx.start, end: ctx.end, step: ctx.step, frameMillis: frameShift}
	if !ctx.end.IsZero() {
		env.known = true
		env.lo, env.hi = ctx.end, ctx.end
		if !ctx.start.IsZero() {
			env.lo = ctx.start
		}
		if err := checkInstantDomain(env.lo, env.hi, 0, 0); err != nil {
			return nil, nil, err
		}
		start := promTimeMilliseconds(env.lo)
		env.ev = promEval{start: start, offEval: start, req: &[2]int64{start, promTimeMilliseconds(ctx.end)}}
		if ctx.isolatedEval != nil {
			env.ev = ctx.isolatedEval.moved(frameShift)
		}
		env.hasEv = true
	}
	f := &domainFit{frameShift: frameShift, isolating: isolating}
	out, res := f.fitReplacing(expr, env)
	if res.rejected() && res.empty && out.Type() == parser.ValueTypeMatrix {
		out, res = emptyRangeVector(), fitResult{empty: true}
	}
	if res.err == nil {
		res.err = res.outside
	}
	if res.err == nil {
		f.recordRootGrid(out, env)
	}
	if res.err != nil {
		return nil, nil, res.err
	}
	return out, f, nil
}

// gridSpan returns the first and last instants of the grid a subquery of
// the given range and step lays over the anchors of at: the multiples of
// step after the earliest anchor minus range, up to the latest anchor.
func gridSpan(at domainEnv, rng, step time.Duration) [2]time.Time {
	if step <= 0 {
		step = defaultSubqueryStep
	}
	unit := step.Milliseconds()
	first := promSubqueryStart(at.lo.UnixMilli(), 0, rng.Milliseconds(), unit)
	last := at.hi.UnixMilli()
	last -= ((last % unit) + unit) % unit
	return [2]time.Time{time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()}
}

// rootSubquery returns the subquery e is, below any parentheses, or nil.
func rootSubquery(e parser.Expr) *parser.SubqueryExpr {
	for {
		switch n := e.(type) {
		case *parser.ParenExpr:
			e = n.Expr
		case *parser.StepInvariantExpr:
			e = n.Expr
		case *parser.SubqueryExpr:
			return n
		default:
			return nil
		}
	}
}

// rootGridInDomain reports whether root, the fitted query, answers
// timestamps a DateTime64(9) holds: it is not a shifted subquery, or the
// instants of its grid, which it answers as sample timestamps, lie in the
// domain in Prometheus's frame.
func (f *domainFit) rootGridInDomain(root parser.Expr) bool {
	if f == nil {
		return true
	}
	sub := rootSubquery(root)
	grid, ok := f.grids[sub]
	if sub == nil || !ok {
		return true
	}
	return checkInstantDomain(moveMillis(grid[0], -f.frameShift), moveMillis(grid[1], -f.frameShift), 0, 0) == nil
}

// recordRootGrid records the grid of root, the fitted query, when root is
// a subquery lowered in a shifted frame that no region shift recorded:
// such a root answers its grid instants, which [domainFit.rootGridInDomain]
// judges in Prometheus's frame.
func (f *domainFit) recordRootGrid(root parser.Expr, env domainEnv) {
	sub := rootSubquery(root)
	if f.frameShift == 0 || sub == nil {
		return
	}
	if _, ok := f.grids[sub]; ok {
		return
	}
	at, _ := pinnedEnv(sub.Timestamp, sub.StartOrEnd, sub.OriginalOffset, env)
	if !at.known {
		return
	}
	if f.grids == nil {
		f.grids = map[*parser.SubqueryExpr][2]time.Time{}
	}
	f.grids[sub] = gridSpan(at, sub.Range, sub.Step)
}

// promTimeMilliseconds is Prometheus's timeMilliseconds.
func promTimeMilliseconds(t time.Time) int64 {
	return t.UnixNano() / int64(time.Millisecond)
}

// checkInstantDomain checks the instants a node evaluated over [lo, hi]
// needs: a window reaching back by reach from each evaluation instant,
// widened by margin on both sides.
func checkInstantDomain(lo, hi time.Time, reach, margin time.Duration) error {
	return checkInstantDomainAround(lo, hi, reach, margin, margin)
}

// checkInstantDomainAround is [checkInstantDomain] with a margin of before
// below the window and of after above it.
func checkInstantDomainAround(lo, hi time.Time, reach, before, after time.Duration) error {
	switch {
	case lo.Before(instantDomainMin):
		return instantDomainError(lo)
	case lo.Add(-reach).Add(-before).Before(instantDomainMin):
		return instantDomainError(lo.Add(-reach))
	case hi.Add(after).After(instantDomainMax):
		return instantDomainError(hi)
	}
	return nil
}

// outsideStorage reports whether the left-open sample window (lo, hi]
// holds no instant a storage column can carry.
func outsideStorage(lo, hi time.Time) bool {
	return hi.Before(instantDomainMin) || !lo.Before(instantDomainMax)
}

// pinnedEnv returns the evaluation instants of a node carrying the given
// `@` and `offset` modifiers inside env, and the offset Prometheus
// evaluates the node with.
//
// A pin whose instant does not wrap keeps the instant the lowering itself
// derives, the pin minus the offset, so a query that wraps nothing is
// judged exactly as it is lowered.
func pinnedEnv(ts *int64, startOrEnd parser.ItemType, offset time.Duration, env domainEnv) (domainEnv, time.Duration) {
	if env.hasEv {
		if pin, ok := promPinMillis(ts, startOrEnd, env.ev.req); ok {
			if instant, wrapped := promPinnedInstant(env.ev, pin, offset); wrapped {
				at := time.UnixMilli(instant).UTC()
				env.known, env.lo, env.hi = true, at, at
				env.wrapped, env.pin = true, time.UnixMilli(pin).UTC().Add(-offset)
				return env, promAtOffset(env.ev, pin, offset)
			}
		}
	}
	switch {
	case ts != nil:
		pin := time.UnixMilli(*ts).UTC()
		env.known, env.lo, env.hi = true, pin, pin
	case startOrEnd == parser.START:
		env.known = !env.start.IsZero()
		env.lo, env.hi = env.start, env.start
	case startOrEnd == parser.END:
		env.known = !env.end.IsZero()
		env.lo, env.hi = env.end, env.end
	}
	env.lo = env.lo.Add(-offset)
	env.hi = env.hi.Add(-offset)
	if pinned(ts, startOrEnd) {
		env.wrapped = false
		return env, promPinOffset(ts, startOrEnd, offset, env)
	}
	return env, offset
}

// promPinOffset is the offset Prometheus evaluates a pinned node with when
// its instant does not wrap.
func promPinOffset(ts *int64, startOrEnd parser.ItemType, offset time.Duration, env domainEnv) time.Duration {
	if !env.hasEv {
		return offset
	}
	pin, _ := promPinMillis(ts, startOrEnd, env.ev.req)
	return promAtOffset(env.ev, pin, offset)
}

// pinned reports whether a node carries an `@` pin.
func pinned(ts *int64, startOrEnd parser.ItemType) bool {
	return ts != nil || startOrEnd != 0
}

// fitResult is what fitting one expression yields.
type fitResult struct {
	// empty reports that the expression answers no sample at any instant
	// of its environment. Only a vector or matrix expression is ever empty.
	empty bool
	// err is the rejection the expression raises if it is lowered as it
	// stands. An enclosing node either replaces the expression, which
	// clears err, or returns err as its own.
	err error
	// outside is the rejection of an evaluation instant outside the domain
	// that no storage read depends on. A pinned subquery that reads no
	// storage resolves it by shifting ([domainFit.fitSubquery]); anywhere
	// else it is a rejection like err.
	outside error
	// reads reports that the expression reads storage at the evaluation
	// instants of its region: a read that is not empty and is not pinned
	// on its own.
	reads bool
	// lo and hi bound the instants the region's lowering writes, when
	// spanned is set.
	lo, hi  time.Time
	spanned bool
	// stepMillis is the least common multiple of the step, in
	// milliseconds, of every grid in the region; zero when there is none.
	stepMillis int64
}

// merge folds o into r: the first rejection wins, and the region facts
// accumulate.
func (r *fitResult) merge(o fitResult) {
	if r.err == nil {
		r.err = o.err
	}
	if r.outside == nil {
		r.outside = o.outside
	}
	r.reads = r.reads || o.reads
	if o.spanned {
		r.widen(o.lo, o.hi)
	}
	r.stepMillis = lcmMillis(r.stepMillis, o.stepMillis)
}

// widen widens r's span to cover [lo, hi].
func (r *fitResult) widen(lo, hi time.Time) {
	if !r.spanned {
		r.lo, r.hi, r.spanned = lo, hi, true
		return
	}
	if lo.Before(r.lo) {
		r.lo = lo
	}
	if hi.After(r.hi) {
		r.hi = hi
	}
}

// rejected reports whether lowering the expression as it stands raises a
// rejection.
func (r fitResult) rejected() bool {
	return r.err != nil || r.outside != nil
}

// fitReplacing fits e and replaces it with [emptyInstantVector] when e is
// an empty instant vector that would otherwise be rejected.
func (f *domainFit) fitReplacing(e parser.Expr, env domainEnv) (parser.Expr, fitResult) {
	out, res := f.fitExpr(e, env)
	if res.rejected() && res.empty && e.Type() == parser.ValueTypeVector {
		return emptyInstantVector(), fitResult{empty: true}
	}
	return out, res
}

// fitExpr fits e without replacing e itself.
func (f *domainFit) fitExpr(e parser.Expr, env domainEnv) (parser.Expr, fitResult) {
	switch n := e.(type) {
	case *parser.VectorSelector:
		if cut, ok := cutSelector(n, env); ok && !f.keepSelectors && f.cutsStorage() {
			return f.fitExpr(cut, env)
		}
		f.boundRead(n, 0, env)
		at, _ := pinnedEnv(n.Timestamp, n.StartOrEnd, n.OriginalOffset, env)
		return n, fitWindow(at, instantLookback, pinned(n.Timestamp, n.StartOrEnd))
	case *parser.MatrixSelector:
		vs, ok := n.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return n, fitResult{}
		}
		f.boundRead(vs, n.Range, env)
		at, _ := pinnedEnv(vs.Timestamp, vs.StartOrEnd, vs.OriginalOffset, env)
		return n, fitWindow(at, n.Range, pinned(vs.Timestamp, vs.StartOrEnd))
	case *parser.SubqueryExpr:
		return f.fitSubquery(n, env)
	case *parser.Call:
		return f.fitCall(n, env)
	case *parser.AggregateExpr:
		return f.fitAggregate(n, env)
	case *parser.BinaryExpr:
		return f.fitBinary(n, env)
	case *parser.ParenExpr:
		inner, res := f.fitReplacing(n.Expr, env)
		if res.rejected() || inner == n.Expr {
			return n, res
		}
		cp := *n
		cp.Expr = inner
		return &cp, res
	case *parser.UnaryExpr:
		inner, res := f.fitReplacing(n.Expr, env)
		if res.rejected() || inner == n.Expr {
			return n, res
		}
		cp := *n
		cp.Expr = inner
		return &cp, res
	case *parser.StepInvariantExpr:
		inner, res := f.fitReplacing(n.Expr, env)
		if res.rejected() || inner == n.Expr {
			return n, res
		}
		cp := *n
		cp.Expr = inner
		return &cp, res
	default:
		return e, fitResult{}
	}
}

// boundRead records, for selector vs read with window rng (zero for an
// instant selector), the storage read Prometheus bounds it with when its
// subqueries' offsets or ranges add up past what a time.Duration holds:
// in wrapped arithmetic, that read holds only some, or none, of the
// samples the evaluation needs ([promReadSums.wraps]), and every scan the
// selector lowers to reads only its rows ([domainFit.readBounds]).
func (f *domainFit) boundRead(vs *parser.VectorSelector, rng time.Duration, env domainEnv) {
	// An environment with no evaluator holds sums that never wrap.
	if pinned(vs.Timestamp, vs.StartOrEnd) || !env.ev.sums.wraps() {
		return
	}
	if f.readBounds == nil {
		f.readBounds = map[posrange.PositionRange]chplan.MillisSpan{}
	}
	// The read is Prometheus's, in its own frame, the frame of the stored
	// timestamps the scans bound.
	hint := promSelectorHint(env.ev, vs, rng)
	f.readBounds[vs.PosRange] = chplan.MillisSpan{From: hint[0] - env.frameMillis, To: hint[1] - env.frameMillis}
}

// boundReads bounds every scan of plan, the lowering of expr, that the
// lowering of no part of expr has settled, to the storage read of the
// selectors of expr whose scans plan does not already hold
// ([domainFit.readBounds]): a path that lowers a selector without
// lowering it as an expression of its own, or that drops such a lowering
// and lowers the selector again, leaves its scans to the smallest
// enclosing expression. Those selectors then all read the same rows, or
// the scans cannot tell them apart and the query is rejected.
func (f *domainFit) boundReads(expr parser.Expr, plan chplan.Node) error {
	if f == nil || len(f.readBounds) == 0 || plan == nil {
		return nil
	}
	if f.boundScans == nil {
		f.boundScans = map[*chplan.Scan]bool{}
		f.selectorScans = map[posrange.PositionRange][]*chplan.Scan{}
	}
	var open []*chplan.Scan
	held := map[*chplan.Scan]bool{}
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if f.sealed[n] {
			return false
		}
		if scan, ok := n.(*chplan.Scan); ok && !held[scan] {
			held[scan] = true
			if !f.boundScans[scan] {
				open = append(open, scan)
			}
		}
		return true
	})
	var unsettled []posrange.PositionRange
	var bounds, all []*chplan.MillisSpan
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		var b *chplan.MillisSpan
		if w, ok := f.readBounds[vs.PosRange]; ok {
			b = &w
		}
		all = append(all, b)
		if !slices.ContainsFunc(f.selectorScans[vs.PosRange], func(s *chplan.Scan) bool { return held[s] }) {
			unsettled = append(unsettled, vs.PosRange)
			bounds = append(bounds, b)
		}
		return nil
	})
	if len(open) == 0 {
		return nil
	}
	if len(bounds) == 0 {
		bounds = all
	}
	for _, b := range bounds[1:] {
		if !sameBound(bounds[0], b) {
			return errPromReadsMixed
		}
	}
	for _, scan := range open {
		f.boundScans[scan] = true
		if _, err := (chplan.Schema{Columns: scan.Roles}).UniqueNamedRole(chplan.RoleTimestamp); err == nil && bounds[0] != nil {
			w := *bounds[0]
			scan.ReadMillis = &w
		}
	}
	for _, pos := range unsettled {
		f.selectorScans[pos] = append(f.selectorScans[pos], open...)
	}
	return nil
}

// sameBound reports whether a and b bound a read alike.
func sameBound(a, b *chplan.MillisSpan) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// errPromReadsMixed rejects an expression whose lowering reads, in scans
// no part of it settles, for selectors whose storage reads Prometheus
// bounds differently ([domainFit.boundReads]).
var errPromReadsMixed = errors.New("promql: selectors whose subqueries' offsets and ranges add up to more than about 292 years read different spans of storage in one scan")

// fitWindow judges a selector evaluated over env whose sample window
// reaches back by reach from each evaluation instant. ownPin reports that
// the selector carries its own `@` pin, so its read belongs to no
// enclosing region.
func fitWindow(env domainEnv, reach time.Duration, ownPin bool) fitResult {
	if !env.known {
		return fitResult{}
	}
	if env.wrapped {
		// Prometheus reads the pinned window and evaluates the wrapped
		// one, which no sample of the first can fall in.
		return fitResult{empty: true, err: instantDomainError(env.pin)}
	}
	margin := env.step
	if ownPin {
		// A pinned window is read once, at its pin, whatever the step.
		margin = 0
	}
	err := checkInstantDomain(env.lo, env.hi, reach, margin)
	if err == nil {
		return fitResult{reads: !ownPin}
	}
	return fitResult{empty: outsideStorage(moveMillis(env.lo.Add(-reach), -env.frameMillis), moveMillis(env.hi, -env.frameMillis)), err: err}
}

func (f *domainFit) fitSubquery(n *parser.SubqueryExpr, env domainEnv) (parser.Expr, fitResult) {
	step := subqueryStep(n)
	at, offset := pinnedEnv(n.Timestamp, n.StartOrEnd, n.OriginalOffset, env)
	ownPin := pinned(n.Timestamp, n.StartOrEnd)
	// The inner expression is evaluated at the grid instants of the
	// left-open window (at.lo - Range, at.hi].
	innerEnv := at
	innerEnv.lo = at.lo.Add(-n.Range)
	innerEnv.step = step
	innerEnv.inSubquery = true
	if env.hasEv {
		innerEnv.ev = promSubqueryEval(env.ev, n, offset, step)
	}
	inner, res := f.fitReplacing(n.Expr, innerEnv)
	isolated := f.isolating && !env.inSubquery && at.known && env.hasEv
	if isolated {
		inner, res = f.isolate(n, inner, innerEnv), fitResult{}
	}
	if res.err != nil {
		return n, fitResult{empty: res.empty, err: res.err}
	}
	out := n
	if inner != n.Expr {
		cp := *n
		cp.Expr = inner
		out = &cp
	}
	if !at.known {
		return out, fitResult{empty: res.empty, outside: res.outside, reads: res.reads}
	}
	// The lowering reaches a step of the grid and one of the enclosing
	// evaluation past the window on either side. An isolated subquery's
	// grid is enumerated outside the SQL, run by run, so on either side it
	// reaches only the enclosing evaluation's step.
	margin, after := step+env.step, step+env.step
	if isolated {
		margin, after = env.step, env.step
	}
	res.widen(at.lo.Add(-n.Range).Add(-margin), at.hi.Add(after))
	res.stepMillis = lcmMillis(res.stepMillis, step.Milliseconds())
	if res.outside == nil {
		res.outside = checkInstantDomainAround(at.lo, at.hi, n.Range, margin, after)
	}
	if !ownPin {
		return f.shiftUnpinned(out, at, env, res)
	}
	// A pin starts a region of its own: what lies below it is settled
	// here and none of it reaches the enclosing region.
	switch {
	case res.outside != nil && (res.empty || res.reads):
		// An empty inner is replaced by the enclosing call. One that reads
		// storage at instants the domain does not hold cannot be lowered.
		return n, fitResult{empty: res.empty, err: res.outside}
	case res.outside != nil:
		shift, ok := shiftIntoDomain(res, anyShift)
		if !ok {
			return n, fitResult{err: res.outside}
		}
		out = repinSubquery(out, at.hi.UnixMilli()+shift)
		f.shiftRegion(out, shift, gridSpan(at, n.Range, step))
	case at.wrapped:
		out = repinSubquery(out, at.hi.UnixMilli())
	}
	return out, fitResult{empty: res.empty}
}

// isolate records the outermost subquery n, evaluated over innerEnv, as
// isolated, and returns the inner expression its isolated lowering
// evaluates: n's own, or fitted, the fitted one under a wrapped pin, below
// which Prometheus reads no sample and the fit has replaced every read
// with an empty vector.
func (f *domainFit) isolate(n *parser.SubqueryExpr, fitted parser.Expr, innerEnv domainEnv) parser.Expr {
	inner := n.Expr
	if innerEnv.wrapped {
		inner = fitted
	}
	if f.isolated == nil {
		f.isolated = map[parser.Expr]promEval{}
	}
	// The state is kept in Prometheus's own frame, in which the isolated
	// lowering starts.
	f.isolated[unwrapParens(inner)] = innerEnv.ev.moved(-f.frameShift)
	return inner
}

// shiftUnpinned settles the region of the unpinned subquery n, evaluated
// at the instants of env and anchored at those of at, when those instants
// lie in the domain while the grid below them leaves it. A region that reads no
// storage, and whose subquery sits inside no other, is moved into the domain by an offset, a multiple of its step as for a
// pinned region; any other region stays unsettled and res reaches the
// enclosing node.
func (f *domainFit) shiftUnpinned(n *parser.SubqueryExpr, at, env domainEnv, res fitResult) (parser.Expr, fitResult) {
	if res.outside == nil || res.reads || res.empty || env.inSubquery ||
		checkInstantDomain(env.lo, env.hi, 0, env.step) != nil {
		return n, res
	}
	shift, ok := shiftIntoDomain(res, offsetFits)
	if !ok {
		return n, res
	}
	cp := *n
	cp.OriginalOffset -= time.Duration(shift) * time.Millisecond
	cp.Offset = cp.OriginalOffset
	f.shiftRegion(&cp, shift, gridSpan(at, n.Range, n.Step))
	return &cp, fitResult{}
}

// offsetFits reports whether a shift of shift milliseconds fits a PromQL
// offset, a time.Duration.
func offsetFits(shift int64) bool {
	return shift >= -maxDurationMillis && shift <= maxDurationMillis
}

// repinSubquery returns a copy of n pinned at the instant ms with no
// offset, so that the lowering evaluates it exactly there.
func repinSubquery(n *parser.SubqueryExpr, ms int64) *parser.SubqueryExpr {
	cp := *n
	cp.Timestamp = &ms
	cp.StartOrEnd = 0
	cp.OriginalOffset, cp.Offset = 0, 0
	return &cp
}

// shiftIntoDomain returns a shift, in milliseconds, that moves the span of
// region r into the instant domain, is a multiple of every step of the
// region, so that every grid keeps its alignment, and that admits
// accepts. It reports false when no such shift exists.
func shiftIntoDomain(r fitResult, admits func(shift int64) bool) (int64, bool) {
	unit := r.stepMillis
	lo, hi := r.lo.UnixMilli(), r.hi.UnixMilli()
	// The domain bounds in whole milliseconds, rounded inward.
	domLo := instantDomainMin.UnixMilli()
	if time.UnixMilli(domLo).Before(instantDomainMin) {
		domLo++
	}
	domHi := instantDomainMax.UnixMilli()
	if unit <= 0 || hi-lo > domHi-domLo-unit {
		return 0, false
	}
	// Move the span halfway towards the centre of the domain, rounded to a
	// whole number of units, which keeps it clear of both bounds. Failing
	// that, move it by the fewest units that bring it inside: that shift
	// fits whenever any does, and admits accepts it whenever it accepts a
	// longer one. A span starting at domLo or ending at domHi needs no
	// move, so the bounds themselves fall to either case alike.
	want := (domLo + (domHi-domLo)/2) - (lo + (hi-lo)/2)
	least := int64(0)
	switch {
	case lo < domLo:
		least = ceilUnits(domLo-lo, unit)
	case hi > domHi:
		least = -ceilUnits(hi-domHi, unit)
	}
	for _, shift := range []int64{want / 2 / unit * unit, least} {
		if lo+shift >= domLo && hi+shift <= domHi && admits(shift) {
			return shift, true
		}
	}
	return 0, false
}

// ceilUnits returns d, a positive span of milliseconds, rounded up to a
// whole number of units.
func ceilUnits(d, unit int64) int64 {
	return (d + unit - 1) / unit * unit
}

// anyShift admits every shift: a pinned region is re-pinned at its shifted
// instant, which any shift into the domain leaves an int64 millisecond.
func anyShift(int64) bool { return true }

// lcmMillis is the least common multiple of two step lengths in
// milliseconds, either of which may be zero for "no step".
func lcmMillis(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	x, y := a, b
	for y != 0 {
		x, y = y, x%y
	}
	return a / x * b
}

// shiftRegion records that the region of the subquery n, which the fit
// has moved by shift milliseconds, is lowered with that shift, and the
// first and last instants of its grid before the shift. A selector pinned
// inside the region names its own instants, so its subtree is recorded
// with no shift.
func (f *domainFit) shiftRegion(n *parser.SubqueryExpr, shift int64, grid [2]time.Time) {
	if f.shifts == nil {
		f.shifts = map[parser.Node]int64{}
		f.grids = map[*parser.SubqueryExpr][2]time.Time{}
		f.gridShifts = map[parser.Expr]int64{}
	}
	f.grids[n] = grid
	f.shifts[n] = shift
	f.gridShifts[unwrapParens(n.Expr)] = shift
	f.markShift(n.Expr, shift)
}

// markShift records shift for node and its subtree. A pinned selector or
// subquery, `@ start()` and `@ end()` included, names instants of its own,
// which the region's shift does not move, so its subtree is recorded with
// no shift; a node a nested region already recorded keeps that region's
// shift. A subquery nested in the region lays its grid over the region's
// shifted instants, so its grid moves by the region's shift, a multiple
// of its step.
func (f *domainFit) markShift(node parser.Node, shift int64) {
	if _, done := f.shifts[node]; done {
		return
	}
	switch n := node.(type) {
	case *parser.VectorSelector:
		if pinned(n.Timestamp, n.StartOrEnd) {
			shift = 0
		}
	case *parser.MatrixSelector:
		if vs, ok := n.VectorSelector.(*parser.VectorSelector); ok && pinned(vs.Timestamp, vs.StartOrEnd) {
			shift = 0
		}
	case *parser.SubqueryExpr:
		if pinned(n.Timestamp, n.StartOrEnd) {
			shift = 0
		}
		f.gridShifts[unwrapParens(n.Expr)] = shift
	}
	f.shifts[node] = shift
	for _, c := range parser.Children(node) {
		f.markShift(c, shift)
	}
}

// firstArg returns n's first argument, nil when it has none.
func firstArg(n *parser.Call) parser.Expr {
	if len(n.Args) == 0 {
		return nil
	}
	return n.Args[0]
}

// emptyPreservingCall reports whether fn answers no sample whenever its
// first vector or matrix argument answers none. absent and
// absent_over_time answer exactly when that argument is empty.
func emptyPreservingCall(fn *parser.Function) bool {
	switch fn.Name {
	case "absent", "absent_over_time":
		return false
	}
	return fn.ReturnType == parser.ValueTypeVector
}

// selectorArgPosition reports whether argument i of n must stay the
// selector it is, so it cannot be replaced by another expression: absent
// reads its result labels from the selector's matchers, and info's second
// argument is a selector by syntax.
func selectorArgPosition(n *parser.Call, i int) bool {
	switch n.Func.Name {
	case "absent":
		return true
	case "info":
		return i == 1
	}
	return false
}

// absentSelector returns the selector absent or absent_over_time reads
// its labels from, when the argument is one, together with the argument
// itself without its parentheses.
func absentSelector(n *parser.Call) (*parser.VectorSelector, parser.Expr, bool) {
	if len(n.Args) != 1 {
		return nil, nil, false
	}
	arg := unwrapParens(n.Args[0])
	switch n.Func.Name {
	case "absent":
		vs, ok := arg.(*parser.VectorSelector)
		return vs, arg, ok
	case "absent_over_time":
		ms, ok := arg.(*parser.MatrixSelector)
		if !ok {
			return nil, nil, false
		}
		vs, ok := ms.VectorSelector.(*parser.VectorSelector)
		return vs, arg, ok
	}
	return nil, nil, false
}

func (f *domainFit) fitCall(n *parser.Call, env domainEnv) (parser.Expr, fitResult) {
	if vs, ok := unwrapParens(firstArg(n)).(*parser.VectorSelector); ok && n.Func.Name == "absent" && f.cutsStorage() {
		// absent(v) answers exactly when v's lookback holds no sample, as
		// absent_over_time(v[lookback]) does, whose window can be cut.
		if cut, _, ok := cutMatrix(&parser.MatrixSelector{VectorSelector: vs, Range: instantLookback}, env); ok {
			return f.fitCall(&parser.Call{Func: parser.Functions["absent_over_time"], Args: parser.Expressions{cut}, PosRange: n.PosRange}, env)
		}
	}
	if cut, ok := f.cutWindowArg(n, env); ok {
		return f.fitCall(cut, env)
	}
	if vs, arg, ok := absentSelector(n); ok {
		// A selector is empty only where its read is rejected.
		if _, r := f.fitExpr(arg, env); r.empty {
			// The selector reads nothing, so the call answers from its
			// matchers alone (lowerAbsent, lowerAbsentOverTime).
			if f.emptyReads == nil {
				f.emptyReads = map[*parser.VectorSelector]bool{}
			}
			f.emptyReads[vs] = true
			return n, fitResult{}
		}
	}
	var (
		args       parser.Expressions
		res        fitResult
		empty      bool
		seenSeries bool
	)
	for i, a := range n.Args {
		var (
			out parser.Expr
			r   fitResult
		)
		if selectorArgPosition(n, i) {
			keep := f.keepSelectors
			f.keepSelectors = true
			out, r = f.fitExpr(a, env)
			f.keepSelectors = keep
		} else {
			out, r = f.fitReplacing(a, env)
		}
		res.merge(r)
		if t := a.Type(); !seenSeries && (t == parser.ValueTypeVector || t == parser.ValueTypeMatrix) {
			seenSeries = true
			empty = r.empty
		}
		if out != a && args == nil {
			args = make(parser.Expressions, len(n.Args))
			copy(args, n.Args)
		}
		if args != nil {
			args[i] = out
		}
	}
	res.empty = empty && emptyPreservingCall(n.Func)
	switch {
	case res.rejected() && res.empty:
		return emptyInstantVector(), fitResult{empty: true}
	case res.rejected() || args == nil:
		return n, res
	}
	cp := *n
	cp.Args = args
	return &cp, res
}

func (f *domainFit) fitAggregate(n *parser.AggregateExpr, env domainEnv) (parser.Expr, fitResult) {
	var res fitResult
	param := n.Param
	if param != nil {
		param, res = f.fitReplacing(n.Param, env)
		if res.rejected() {
			return n, res
		}
	}
	inner, r := f.fitReplacing(n.Expr, env)
	res.merge(r)
	res.empty = r.empty
	if res.rejected() || (inner == n.Expr && param == n.Param) {
		return n, res
	}
	cp := *n
	cp.Expr, cp.Param = inner, param
	return &cp, res
}

func (f *domainFit) fitBinary(n *parser.BinaryExpr, env domainEnv) (parser.Expr, fitResult) {
	lhs, l := f.fitReplacing(n.LHS, env)
	if l.rejected() {
		return n, l
	}
	rhs, r := f.fitReplacing(n.RHS, env)
	if r.rejected() {
		return n, r
	}
	res := l
	res.merge(r)
	// Set operators take two vectors. Any other operator answers no sample
	// when either operand answers none; a scalar operand is never empty.
	switch n.Op {
	case parser.LOR:
		res.empty = l.empty && r.empty
	case parser.LUNLESS:
		res.empty = l.empty
	default:
		res.empty = l.empty || r.empty
	}
	if lhs == n.LHS && rhs == n.RHS {
		return n, res
	}
	cp := *n
	cp.LHS, cp.RHS = lhs, rhs
	return &cp, res
}

// emptyRangeVector builds `(vector(0) < -1)[1m:1m]`, a range vector that
// holds no sample at any evaluation instant.
func emptyRangeVector() parser.Expr {
	return &parser.SubqueryExpr{
		Expr:  &parser.ParenExpr{Expr: emptyInstantVector()},
		Range: defaultSubqueryStep,
		Step:  defaultSubqueryStep,
	}
}

// emptyInstantVector builds `vector(0) < -1`, an instant vector that keeps
// no sample at any evaluation instant.
func emptyInstantVector() parser.Expr {
	return &parser.BinaryExpr{
		Op: parser.LSS,
		LHS: &parser.Call{
			Func: parser.Functions["vector"],
			Args: parser.Expressions{&parser.NumberLiteral{Val: 0}},
		},
		RHS: &parser.NumberLiteral{Val: emptyVectorThreshold},
	}
}
