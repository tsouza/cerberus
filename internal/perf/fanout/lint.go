// Package fanout is cerberus's static compute-fan-out linter — the
// cheap, always-on tripwire that catches a query stage exploding
// intermediate cardinality before the final aggregation collapses it.
//
// A compute fan-out touches a normal number of scanned rows but blows up
// the row count mid-plan: a step-grid CROSS JOIN that pairs every sample
// with every eval anchor (the range_lwr / histogram shapes), an
// arrayJoin explosion feeding a JOIN, or an unbounded WITH RECURSIVE
// structural closure (nested-set / descendant walks). None of these
// change the scanned-row count, so row-count- or EXPLAIN-based guards
// miss them.
//
// [Lint] inspects the lowered [chplan] IR and the emitted ClickHouse SQL
// statically — zero data, no execution — and returns a [Violation] for
// each unbounded fan-out shape. It is wired into the always-on
// (non-chDB) check gate by test/regression/fanout_lint_test.go, which
// runs it over every test/spec/** fixture so a PR that reintroduces an
// unbounded fan-out fails at review time.
//
// The API is deliberately representation-split: [Lint] takes both a plan
// tree and the SQL string; callers that only have one pass the other as
// its zero value (nil plan / empty SQL) and the rules keyed on the
// missing representation simply do not fire.
package fanout

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/spansscan"
)

// Rule identifies which fan-out shape a [Violation] flags.
type Rule string

const (
	// RuleUnboundedCrossJoin flags a chplan.CrossJoin where NEITHER side
	// is statically collapsed to a bounded row count — the range_lwr /
	// histogram step-grid class. A CROSS JOIN is safe only
	// when at least one side is provably ≤1 row (OneRow, a no-GROUP-BY
	// Aggregate, Limit 1, ...) or is already collapsed by an Aggregate
	// (the unavoidable per-(series, step) broadcast shape — the right
	// side has been reduced to series cardinality, not raw scan rows).
	RuleUnboundedCrossJoin Rule = "unbounded-cross-join"

	// RuleFanoutFeedingJoin flags a row-multiplying fan-out node
	// (RangeWindow / StepGrid / RangeLWR / RangeBucketFanout /
	// MetricsCompare) that feeds a JOIN side WITHOUT an intervening
	// Aggregate collapse — the arrayJoin-explosion-into-JOIN class.
	RuleFanoutFeedingJoin Rule = "fanout-feeding-join"

	// RuleUncappedRecursion flags a `WITH RECURSIVE` CTE emitted WITHOUT
	// a `_depth < <literal>` depth-cap bound — the nested-set /
	// structural-recursion class. Every recursive CTE must
	// carry an integer depth ceiling so a span-id cycle degrades to a
	// partial closure instead of an unbounded walk.
	RuleUncappedRecursion Rule = "uncapped-recursion"

	// RuleCorrelatedSubquery flags a chplan.ScalarSubquery whose embedded
	// plan is NOT statically pinned to one row by an aggregation guard —
	// a per-row correlated subquery, which CH evaluates once per outer
	// row. The legitimate scalar() lowering wraps its subquery in a
	// no-GROUP-BY Aggregate (one row by construction); anything else is a
	// fan-out.
	RuleCorrelatedSubquery Rule = "correlated-subquery"

	// RuleUnwindowedSpansScan flags a physical `otel_traces` scan that
	// sits in a context where ClickHouse CANNOT push the request window
	// down into the partition pruner — a recursive (`WITH RECURSIVE`) arm
	// or a `GROUP BY` root-lookup — yet carries no co-scope `Timestamp`
	// predicate. otel_traces is `PARTITION BY toDate(Timestamp)`, so ONLY
	// a Timestamp range sitting directly on the scan prunes partitions; a
	// windowed `TraceId IN (<seed>)` is inert for pruning. A recursive
	// STEP/ANCHOR arm or a pre-IN `GROUP BY` therefore reads the WHOLE
	// table unless the window is replicated onto the scan itself — the
	// traces-OOM class. The rule fires only when the statement otherwise
	// carries a request window (a rendered `fromUnixTimestamp64Nano(`
	// bound); a window-less query has nothing to push and defers to the
	// resource-bound gate.
	RuleUnwindowedSpansScan Rule = "unwindowed-spans-scan"
)

// Violation is a single static fan-out finding.
type Violation struct {
	// Rule is the fan-out class.
	Rule Rule
	// Detail is a human-readable description of where / why the shape
	// was flagged, suitable for a test-failure message.
	Detail string
}

func (v Violation) String() string {
	return fmt.Sprintf("[%s] %s", v.Rule, v.Detail)
}

// Lint statically inspects a lowered plan tree and its emitted SQL and
// returns every unbounded compute-fan-out shape it finds. A nil plan
// skips the IR-keyed rules; an empty sql skips the SQL-keyed rules.
//
// The returned slice is in deterministic order (IR rules in plan
// pre-order, then SQL rules) so callers can diff it stably.
func Lint(plan chplan.Node, sql string) []Violation {
	var out []Violation
	out = append(out, lintCrossJoins(plan)...)
	out = append(out, lintFanoutFeedingJoin(plan)...)
	out = append(out, lintCorrelatedSubquery(plan)...)
	out = append(out, lintRecursionCap(sql)...)
	out = append(out, lintUnwindowedSpansScan(sql)...)
	return out
}

// ---------------------------------------------------------------------
// Rule 1 — unbounded CROSS JOIN
// ---------------------------------------------------------------------

func lintCrossJoins(plan chplan.Node) []Violation {
	var out []Violation
	chplan.Walk(plan, func(n chplan.Node) bool {
		cj, ok := n.(*chplan.CrossJoin)
		if !ok {
			return true
		}
		// Safe iff at least one side is bounded-to-few-rows OR already
		// collapsed by an aggregation. The dangerous shape — the
		// step-grid blowup — is `CrossJoin{StepGrid, Filter(Scan)}`: an
		// N-anchor grid against RAW (un-collapsed) scan rows, yielding
		// rows×anchors intermediate cardinality.
		if sideIsBounded(cj.Left) || sideIsBounded(cj.Right) {
			return true
		}
		out = append(out, Violation{
			Rule: RuleUnboundedCrossJoin,
			Detail: fmt.Sprintf(
				"CrossJoin where neither side is bounded to a few rows: left=%s right=%s — pair every left row with every right row (the range_lwr/histogram step-grid blowup). Collapse one side with an Aggregate or pin it to one row.",
				nodeKind(cj.Left), nodeKind(cj.Right),
			),
		})
		return true
	})
	return out
}

// sideIsBounded reports whether a CROSS JOIN side is statically safe:
// either provably ≤1 row, or already reduced by an Aggregate (its row
// count is series cardinality, the unavoidable output shape — not raw
// scan rows). The check peels the alias/reshape wrappers (Project,
// Limit, OrderBy) the lowerings put above the collapsing node.
func sideIsBounded(n chplan.Node) bool {
	for {
		if collapsesFanout(n) {
			// An Aggregate (no-GROUP-BY → 1 row; with keys → series
			// cardinality) or a windowed-aggregate node — the side is no
			// longer a raw scan, so the product is bounded by the
			// already-required series count rather than rows×anchors.
			return true
		}
		switch v := n.(type) {
		case *chplan.OneRow:
			return true
		case *chplan.Limit:
			if v.Count == 1 {
				return true
			}
			n = v.Input
		case *chplan.Project:
			n = v.Input
		case *chplan.OrderBy:
			n = v.Input
		default:
			return false
		}
		if n == nil {
			return false
		}
	}
}

// ---------------------------------------------------------------------
// Rule 2 — fan-out feeding a JOIN
// ---------------------------------------------------------------------

// fanoutKind returns a non-empty label when n is an UN-COLLAPSED
// row-multiplying fan-out producer, "" otherwise.
//
// Only StepGrid qualifies: it emits N raw anchor rows with no GROUP BY,
// so feeding it into a join's side multiplies the other side by N. The
// windowed-aggregate nodes (RangeWindow / RangeLWR / RangeBucketFanout /
// MetricsCompare) DO internally arrayJoin, but each immediately collapses
// the explosion with a GROUP BY to one row per (series, anchor) — their
// OUTPUT is the bounded matrix shape, identical to an Aggregate's, so
// they are collapse boundaries (see fanoutBeforeCollapse), not raw
// multipliers.
func fanoutKind(n chplan.Node) string {
	switch n.(type) {
	case *chplan.StepGrid:
		return "StepGrid"
	}
	return ""
}

// collapsesFanout reports whether n collapses any fan-out below it to a
// bounded per-(series, anchor) row count — an Aggregate, or one of the
// windowed-aggregate nodes whose emit-time GROUP BY pins the output.
func collapsesFanout(n chplan.Node) bool {
	switch n.(type) {
	case *chplan.Aggregate,
		*chplan.RangeWindow,
		*chplan.RangeWindowNative,
		*chplan.RangeLWR,
		*chplan.RangeWindowResample,
		*chplan.RangeBucketFanout,
		*chplan.MetricsCompare,
		*chplan.MetricsAggregate,
		*chplan.MetricsHistogramOverTime,
		*chplan.AbsentOverTime:
		return true
	}
	return false
}

// joinSides returns the input subtrees of a join node, or nil if n is
// not a join.
func joinSides(n chplan.Node) []chplan.Node {
	switch v := n.(type) {
	case *chplan.VectorJoin:
		return []chplan.Node{v.Left, v.Right}
	case *chplan.StructuralJoin:
		return []chplan.Node{v.Left, v.Right}
	case *chplan.CrossJoin:
		// CROSS JOIN is its own rule; a fan-out directly under a CROSS
		// JOIN with the other side bounded is the legitimate broadcast,
		// so it is intentionally excluded here.
		return nil
	}
	return nil
}

func lintFanoutFeedingJoin(plan chplan.Node) []Violation {
	var out []Violation
	chplan.Walk(plan, func(n chplan.Node) bool {
		sides := joinSides(n)
		if sides == nil {
			return true
		}
		for _, side := range sides {
			if kind, ok := fanoutBeforeCollapse(side); ok {
				out = append(out, Violation{
					Rule: RuleFanoutFeedingJoin,
					Detail: fmt.Sprintf(
						"%s feeds a %s side without an intervening Aggregate collapse — the explosion enters the join un-reduced (rows×fanout × match). Collapse the fan-out before the join.",
						kind, nodeKind(n),
					),
				})
			}
		}
		return true
	})
	return out
}

// fanoutBeforeCollapse walks down a join side and reports the first
// fan-out node reached BEFORE any Aggregate (which would collapse it).
// It stops descending at an Aggregate (the fan-out below it is bounded)
// and at a nested join (that join's own sides are linted separately).
func fanoutBeforeCollapse(n chplan.Node) (string, bool) {
	var (
		found string
		ok    bool
	)
	var visit func(cur chplan.Node) bool
	visit = func(cur chplan.Node) bool {
		if cur == nil || ok {
			return false
		}
		if kind := fanoutKind(cur); kind != "" {
			found, ok = kind, true
			return false
		}
		if collapsesFanout(cur) {
			// Collapses everything below it — stop descending.
			return false
		}
		switch cur.(type) {
		case *chplan.VectorJoin, *chplan.StructuralJoin, *chplan.CrossJoin:
			// A nested join: its sides are visited by the outer Walk.
			return false
		}
		for _, c := range cur.Children() {
			visit(c)
		}
		return false
	}
	visit(n)
	return found, ok
}

// ---------------------------------------------------------------------
// Rule 4 — per-row correlated subquery
// ---------------------------------------------------------------------

func lintCorrelatedSubquery(plan chplan.Node) []Violation {
	var out []Violation
	chplan.Walk(plan, func(n chplan.Node) bool {
		for _, e := range nodeExprs(n) {
			chplan.InspectExpr(e, func(x chplan.Expr) bool {
				sub, ok := x.(*chplan.ScalarSubquery)
				if !ok {
					return true
				}
				if sub.Input != nil && !sideIsBounded(sub.Input) {
					out = append(out, Violation{
						Rule: RuleCorrelatedSubquery,
						Detail: fmt.Sprintf(
							"ScalarSubquery over a non-collapsed plan (%s) — evaluated per outer row. Pin it to one row with a no-GROUP-BY Aggregate.",
							nodeKind(sub.Input),
						),
					})
				}
				return true
			})
		}
		return true
	})
	return out
}

// nodeExprs returns the Expr slots a node carries that may nest a
// ScalarSubquery. It covers the nodes the lowerings actually thread a
// computed scalar argument through; nodes without Expr slots return nil.
// nodeExprsExhaustive (in the test) guards that no Expr-bearing node is
// silently dropped.
func nodeExprs(n chplan.Node) []chplan.Expr {
	var es []chplan.Expr
	switch v := n.(type) {
	case *chplan.Filter:
		es = append(es, v.Predicate)
	case *chplan.Project:
		for _, p := range v.Projections {
			es = append(es, p.Expr)
		}
	case *chplan.Aggregate:
		es = append(es, v.GroupBy...)
		for _, f := range v.AggFuncs {
			es = append(es, f.Params...)
			es = append(es, f.Args...)
		}
	case *chplan.RangeWindow:
		es = append(es, v.GroupBy...)
		es = append(es, v.ScalarExprs...)
	case *chplan.RangeBucketFanout:
		es = append(es, v.GroupBy...)
		for _, f := range v.AggFuncs {
			es = append(es, f.Params...)
			es = append(es, f.Args...)
		}
	case *chplan.OrderBy:
		for _, k := range v.Keys {
			es = append(es, k.Expr)
		}
	case *chplan.TopK:
		es = append(es, v.By...)
		es = append(es, v.SortExpr)
	case *chplan.MetricsAggregate:
		es = append(es, v.Attr)
		es = append(es, v.GroupBy...)
	case *chplan.MetricsCompare:
		es = append(es, v.Selection, v.Pairs)
	}
	return es
}

// ---------------------------------------------------------------------
// Rule 3 — uncapped WITH RECURSIVE
// ---------------------------------------------------------------------

var (
	reRecursive = regexp.MustCompile(`(?i)WITH\s+RECURSIVE`)
	// A depth cap is `<ident>._depth < <integer>` (or `_depth < N`) — the
	// shape the structural / nested-set emitter renders
	// (chsql.defaultStructuralRecursionDepth). The bound MUST be an
	// integer literal; a column / unbounded comparison does not count.
	reDepthCap = regexp.MustCompile(`(?i)_depth\s*<\s*\d+`)
)

func lintRecursionCap(sql string) []Violation {
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	recursive := len(reRecursive.FindAllString(sql, -1))
	if recursive == 0 {
		return nil
	}
	caps := len(reDepthCap.FindAllString(sql, -1))
	if caps >= recursive {
		return nil
	}
	return []Violation{{
		Rule: RuleUncappedRecursion,
		Detail: fmt.Sprintf(
			"%d WITH RECURSIVE CTE(s) but only %d `_depth < <literal>` depth-cap(s) — a recursive closure is emitted unbounded (a span-id cycle would loop / error CH 306). Every recursive CTE must carry an integer depth ceiling.",
			recursive, caps,
		),
	}}
}

// ---------------------------------------------------------------------
// Rule 5 — unwindowed physical spans scan (no partition pruning)
// ---------------------------------------------------------------------

const (
	// spansTable is the physical spans relation. It is
	// `PARTITION BY toDate(Timestamp)`, so only a Timestamp range
	// predicate sitting directly on a scan of it prunes partitions. The
	// corpus lint scopes the shared matcher to this default table; the emit
	// chokepoint scopes it to the context-threaded spans table.
	spansTable = "otel_traces"
	// requestWindowBound is the rendered request-window bound shape the
	// shared matcher keys on. It is re-exported here unchanged so the
	// package's own precondition tests can reference it by the local name.
	requestWindowBound = spansscan.RequestWindowBound
)

// lintUnwindowedSpansScan delegates to the shared spansscan matcher (the same
// matcher the emit chokepoint runs as the universal backstop) and maps each
// Finding onto a RuleUnwindowedSpansScan Violation. The detection logic lives
// in one place so the corpus tripwire and the construction-proof emit guard
// can never diverge.
func lintUnwindowedSpansScan(sql string) []Violation {
	findings := spansscan.UnwindowedSpansScans(sql, spansTable)
	if len(findings) == 0 {
		return nil
	}
	out := make([]Violation, 0, len(findings))
	for _, f := range findings {
		out = append(out, Violation{Rule: RuleUnwindowedSpansScan, Detail: f.Reason})
	}
	return out
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// nodeKind returns a short type label for a node, for violation messages.
func nodeKind(n chplan.Node) string {
	if n == nil {
		return "<nil>"
	}
	t := fmt.Sprintf("%T", n)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimPrefix(t, "*")
}
