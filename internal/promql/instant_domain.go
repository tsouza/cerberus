package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The instant domain is the span of absolute instants the lowering writes
// into ClickHouse SQL exactly. An instant reaches SQL as a DateTime64(9)
// value, either a [chplan.TimeLiteral] that ClickHouse parses or int64
// nanoseconds computed in Go. ClickHouse clamps a parsed DateTime64 earlier
// than 1900-01-01 to that day, and int64 nanoseconds end at
// 2262-04-11T23:47:16.854775807Z.
var (
	instantDomainMin = chplan.TimeLiteralMin
	instantDomainMax = chplan.StorageMaxTime

	// storageMin is the earliest instant a DateTime64(9) storage column
	// holds. [instantDomainMax] is also the latest.
	storageMin = chplan.StorageMinTime
)

// emptyVectorThreshold is the comparison threshold of
// [emptyInstantVector]: `vector(0) < -1` keeps no sample.
const emptyVectorThreshold = -1

// instantDomainError rejects a query that needs the instant t, which lies
// outside the instant domain.
func instantDomainError(t time.Time) error {
	return fmt.Errorf("promql: the query needs instant %s, outside the range cerberus supports (%s to %s): ClickHouse cannot represent it exactly",
		t.UTC().Format(time.RFC3339Nano), instantDomainMin.Format(time.RFC3339Nano), instantDomainMax.Format(time.RFC3339Nano))
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
}

// fitInstantDomain prepares expr for lowering at ctx so that no instant
// outside the instant domain reaches SQL.
//
// A selector whose sample window lies wholly outside what a storage column
// can hold reads no sample. An expression that is empty for that reason is
// replaced by [emptyInstantVector], which ClickHouse answers empty at
// in-domain instants. Every other expression that needs an instant outside
// the domain is rejected with [instantDomainError]. expr itself is never
// modified: a rewritten node is a copy.
func fitInstantDomain(expr parser.Expr, ctx lowerCtx) (parser.Expr, error) {
	env := domainEnv{start: ctx.start, end: ctx.end, step: ctx.step}
	if !ctx.end.IsZero() {
		env.known = true
		env.lo, env.hi = ctx.end, ctx.end
		if !ctx.start.IsZero() {
			env.lo = ctx.start
		}
		if err := checkInstantDomain(env.lo, env.hi, 0, 0); err != nil {
			return nil, err
		}
	}
	return fitArg(expr, env)
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
	return hi.Before(storageMin) || !lo.Before(instantDomainMax)
}

// pinnedEnv returns the evaluation instants of a node carrying the given
// `@` and `offset` modifiers inside env.
func pinnedEnv(ts *int64, startOrEnd parser.ItemType, offset time.Duration, env domainEnv) domainEnv {
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
	return env
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
}

// fitArg fits e in a position that accepts any expression of e's type and
// returns the expression to lower there.
func fitArg(e parser.Expr, env domainEnv) (parser.Expr, error) {
	out, res := fitReplacing(e, env)
	return out, res.err
}

// fitReplacing fits e and replaces it with [emptyInstantVector] when e is
// an empty instant vector that would otherwise be rejected.
func fitReplacing(e parser.Expr, env domainEnv) (parser.Expr, fitResult) {
	out, res := fitExpr(e, env)
	if res.err != nil && res.empty && e.Type() == parser.ValueTypeVector {
		return emptyInstantVector(), fitResult{empty: true}
	}
	return out, res
}

// fitExpr fits e without replacing e itself.
func fitExpr(e parser.Expr, env domainEnv) (parser.Expr, fitResult) {
	switch n := e.(type) {
	case *parser.VectorSelector:
		return n, fitWindow(pinnedEnv(n.Timestamp, n.StartOrEnd, n.OriginalOffset, env), instantLookback)
	case *parser.MatrixSelector:
		vs, ok := n.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return n, fitResult{}
		}
		return n, fitWindow(pinnedEnv(vs.Timestamp, vs.StartOrEnd, vs.OriginalOffset, env), n.Range)
	case *parser.SubqueryExpr:
		return fitSubquery(n, env)
	case *parser.Call:
		return fitCall(n, env)
	case *parser.AggregateExpr:
		return fitAggregate(n, env)
	case *parser.BinaryExpr:
		return fitBinary(n, env)
	case *parser.ParenExpr:
		inner, res := fitReplacing(n.Expr, env)
		if res.err != nil || inner == n.Expr {
			return n, res
		}
		cp := *n
		cp.Expr = inner
		return &cp, res
	case *parser.UnaryExpr:
		inner, res := fitReplacing(n.Expr, env)
		if res.err != nil || inner == n.Expr {
			return n, res
		}
		cp := *n
		cp.Expr = inner
		return &cp, res
	case *parser.StepInvariantExpr:
		inner, res := fitReplacing(n.Expr, env)
		if res.err != nil || inner == n.Expr {
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
// reaches back by reach from each evaluation instant.
func fitWindow(env domainEnv, reach time.Duration) fitResult {
	if !env.known {
		return fitResult{}
	}
	err := checkInstantDomain(env.lo, env.hi, reach, env.step)
	if err == nil {
		return fitResult{}
	}
	return fitResult{empty: outsideStorage(env.lo.Add(-reach), env.hi), err: err}
}

func fitSubquery(n *parser.SubqueryExpr, env domainEnv) (parser.Expr, fitResult) {
	step := n.Step
	if step <= 0 {
		step = defaultSubqueryStep
	}
	at := pinnedEnv(n.Timestamp, n.StartOrEnd, n.OriginalOffset, env)
	// The inner expression is evaluated at the grid instants of the
	// left-open window (at.lo - Range, at.hi].
	innerEnv := at
	innerEnv.lo = at.lo.Add(-n.Range)
	innerEnv.step = step
	inner, res := fitReplacing(n.Expr, innerEnv)
	if res.err != nil {
		return n, res
	}
	out := parser.Expr(n)
	if inner != n.Expr {
		cp := *n
		cp.Expr = inner
		out = &cp
	}
	if !at.known {
		return out, fitResult{empty: res.empty}
	}
	return out, fitResult{empty: res.empty, err: checkInstantDomain(at.lo, at.hi, n.Range, step+env.step)}
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

func fitCall(n *parser.Call, env domainEnv) (parser.Expr, fitResult) {
	var (
		args       parser.Expressions
		firstErr   error
		empty      bool
		seenSeries bool
	)
	for i, a := range n.Args {
		var (
			out parser.Expr
			res fitResult
		)
		if selectorArgPosition(n, i) {
			out, res = fitExpr(a, env)
		} else {
			out, res = fitReplacing(a, env)
		}
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		if t := a.Type(); !seenSeries && (t == parser.ValueTypeVector || t == parser.ValueTypeMatrix) {
			seenSeries = true
			empty = res.empty
		}
		if out != a && args == nil {
			args = make(parser.Expressions, len(n.Args))
			copy(args, n.Args)
		}
		if args != nil {
			args[i] = out
		}
	}
	empty = empty && emptyPreservingCall(n.Func)
	switch {
	case firstErr != nil && empty:
		return emptyInstantVector(), fitResult{empty: true}
	case firstErr != nil:
		return n, fitResult{err: firstErr}
	case args == nil:
		return n, fitResult{empty: empty}
	}
	cp := *n
	cp.Args = args
	return &cp, fitResult{empty: empty}
}

func fitAggregate(n *parser.AggregateExpr, env domainEnv) (parser.Expr, fitResult) {
	param := n.Param
	if param != nil {
		var res fitResult
		param, res = fitReplacing(n.Param, env)
		if res.err != nil {
			return n, res
		}
	}
	inner, res := fitReplacing(n.Expr, env)
	if res.err != nil || (inner == n.Expr && param == n.Param) {
		return n, res
	}
	cp := *n
	cp.Expr, cp.Param = inner, param
	return &cp, res
}

func fitBinary(n *parser.BinaryExpr, env domainEnv) (parser.Expr, fitResult) {
	lhs, l := fitReplacing(n.LHS, env)
	if l.err != nil {
		return n, l
	}
	rhs, r := fitReplacing(n.RHS, env)
	if r.err != nil {
		return n, r
	}
	// Set operators take two vectors. Any other operator answers no sample
	// when either operand answers none; a scalar operand is never empty.
	var empty bool
	switch n.Op {
	case parser.LOR:
		empty = l.empty && r.empty
	case parser.LUNLESS:
		empty = l.empty
	default:
		empty = l.empty || r.empty
	}
	if lhs == n.LHS && rhs == n.RHS {
		return n, fitResult{empty: empty}
	}
	cp := *n
	cp.LHS, cp.RHS = lhs, rhs
	return &cp, fitResult{empty: empty}
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
