package promql

import (
	"fmt"
	"math"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

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
// outside the instant domain.
func instantDomainError(t time.Time) error {
	return &outsideDomainError{at: t.UTC()}
}

// outsideDomainError is the rejection [instantDomainError] returns.
type outsideDomainError struct {
	// at is the instant outside the domain the query needs.
	at time.Time
}

func (e *outsideDomainError) Error() string {
	return fmt.Sprintf("promql: the query needs instant %s, outside the range cerberus supports (%s to %s): ClickHouse cannot represent it exactly",
		e.at.Format(time.RFC3339Nano), instantDomainMin.Format(time.RFC3339Nano), instantDomainMax.Format(time.RFC3339Nano))
}

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
	// frame is how far the instants of env lie after the instants
	// Prometheus evaluates at ([fitInFrame]).
	frame time.Duration
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
// Every other expression that needs an instant outside the domain is
// rejected with [instantDomainError]. expr itself is never modified: a
// rewritten node is a copy.
func fitInstantDomain(expr parser.Expr, ctx lowerCtx) (parser.Expr, *domainFit, error) {
	return fitInFrame(expr, ctx)
}

// fitDomain fits expr at ctx, whose instants lie frameShift milliseconds
// after Prometheus's own ([fitInFrame]).
func fitDomain(expr parser.Expr, ctx lowerCtx, frameShift int64) (parser.Expr, *domainFit, error) {
	env := domainEnv{start: ctx.start, end: ctx.end, step: ctx.step, frame: time.Duration(frameShift) * time.Millisecond}
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
		env.hasEv = true
	}
	f := &domainFit{frameShift: frameShift}
	out, res := f.fitReplacing(expr, env)
	if res.rejected() && res.empty && out.Type() == parser.ValueTypeMatrix {
		out, res = emptyRangeVector(), fitResult{empty: true}
	}
	if res.err == nil {
		res.err = res.outside
	}
	if res.err == nil {
		f.recordRootGrid(out, env)
		res.err = f.checkRootGrid(out)
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

// checkRootGrid rejects a query whose root is a shifted subquery whose
// grid instants, which it answers as sample timestamps, lie outside the
// domain, or whose shift a nanosecond Interval cannot carry back
// ([unshiftRootGrid]).
func (f *domainFit) checkRootGrid(root parser.Expr) error {
	sub := rootSubquery(root)
	grid, ok := f.grids[sub]
	if sub == nil || !ok {
		return nil
	}
	frame := time.Duration(f.frameShift) * time.Millisecond
	if shift, _ := f.shiftOf(sub); !offsetFits(shift) {
		return instantDomainError(grid[1].Add(-frame))
	}
	return checkInstantDomain(grid[0].Add(-frame), grid[1].Add(-frame), 0, 0)
}

// recordRootGrid records the grid of root, the fitted query, when root is
// a subquery lowered in a shifted frame that no region shift recorded:
// such a root answers its grid instants, which [domainFit.checkRootGrid]
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
	return t.UnixNano() / int64(time.Millisecond/time.Nanosecond)
}

// checkInstantDomain checks the instants a node evaluated over [lo, hi]
// needs: a window reaching back by reach from each evaluation instant,
// widened by margin on both sides.
func checkInstantDomain(lo, hi time.Time, reach, margin time.Duration) error {
	switch {
	case lo.Before(instantDomainMin):
		return instantDomainError(lo)
	case lo.Add(-reach - margin).Before(instantDomainMin):
		return instantDomainError(lo.Add(-reach))
	case hi.Add(margin).After(instantDomainMax):
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
		at, _ := pinnedEnv(n.Timestamp, n.StartOrEnd, n.OriginalOffset, env)
		return n, fitWindow(at, instantLookback, pinned(n.Timestamp, n.StartOrEnd))
	case *parser.MatrixSelector:
		vs, ok := n.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return n, fitResult{}
		}
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
	return fitResult{empty: outsideStorage(env.lo.Add(-reach-env.frame), env.hi.Add(-env.frame)), err: err}
}

func (f *domainFit) fitSubquery(n *parser.SubqueryExpr, env domainEnv) (parser.Expr, fitResult) {
	step := n.Step
	if step <= 0 {
		step = defaultSubqueryStep
	}
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
	margin := step + env.step
	res.widen(at.lo.Add(-n.Range-margin), at.hi.Add(margin))
	res.stepMillis = lcmMillis(res.stepMillis, step.Milliseconds())
	if res.outside == nil {
		res.outside = checkInstantDomain(at.lo, at.hi, n.Range, margin)
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
		shift, ok := shiftIntoDomain(res)
		if !ok || !shiftableRegion(n.Expr) {
			return n, fitResult{err: res.outside}
		}
		out = repinSubquery(out, at.hi.UnixMilli()+shift)
		f.shiftRegion(out, shift, gridSpan(at, n.Range, step))
	case at.wrapped:
		if !shiftableRegion(n.Expr) {
			return n, fitResult{err: instantDomainError(at.pin)}
		}
		out = repinSubquery(out, at.hi.UnixMilli())
	}
	return out, fitResult{empty: res.empty}
}

// shiftUnpinned settles the region of the unpinned subquery n, evaluated
// at the instants of env and anchored at those of at, when those instants
// lie in the domain while the grid below them leaves it. A region that reads no
// storage, and whose subquery neither holds nor sits inside another, is
// moved into the domain by an offset, a multiple of its step as for a
// pinned region; any other region stays unsettled and res reaches the
// enclosing node.
func (f *domainFit) shiftUnpinned(n *parser.SubqueryExpr, at, env domainEnv, res fitResult) (parser.Expr, fitResult) {
	if res.outside == nil || res.reads || res.empty || env.inSubquery || !shiftableRegion(n.Expr) ||
		checkInstantDomain(env.lo, env.hi, 0, env.step) != nil {
		return n, res
	}
	shift, ok := shiftIntoDomain(res)
	if !ok || !offsetFits(shift) {
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

// shiftableRegion reports whether the region below a subquery, whose
// inner expression is e, may be lowered at a grid other than the one its
// pin or its enclosing evaluation names: a shifted grid, or the wrapped
// instant Prometheus evaluates a pin at. A region holding a nested
// subquery, or an `@ start()` or `@ end()` pin, may not: the lowering
// diverges from Prometheus on both whatever the instants (nested
// subqueries #3875, #3876, #3877, #3879; request pins #3880), so such a
// region keeps its rejection.
func shiftableRegion(e parser.Expr) bool {
	return !holdsSubquery(e) && !holdsRequestPin(e)
}

// holdsRequestPin reports whether e contains an `@ start()` or `@ end()`
// pin.
func holdsRequestPin(e parser.Expr) bool {
	found := false
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			found = found || n.StartOrEnd != 0
		case *parser.SubqueryExpr:
			found = found || n.StartOrEnd != 0
		}
		return nil
	})
	return found
}

// holdsSubquery reports whether e contains a subquery.
func holdsSubquery(e parser.Expr) bool {
	found := false
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if _, ok := node.(*parser.SubqueryExpr); ok {
			found = true
		}
		return nil
	})
	return found
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
// region r into the instant domain and is a multiple of every step of the
// region, so that every grid keeps its alignment. It reports false when no
// such shift exists.
func shiftIntoDomain(r fitResult) (int64, bool) {
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
	// Move the span halfway towards the centre of the domain, which keeps
	// it clear of both bounds, or all the way when halfway is not enough;
	// either shift is rounded to a whole number of units.
	want := (domLo + (domHi-domLo)/2) - (lo + (hi-lo)/2)
	for _, w := range []int64{want / 2, want} {
		shift := w / unit * unit
		if lo+shift >= domLo && hi+shift <= domHi {
			return shift, true
		}
	}
	return 0, false
}

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
		if _, r := f.fitExpr(arg, env); r.err != nil && r.empty {
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
