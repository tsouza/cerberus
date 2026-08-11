package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// unionDedupLimitPerIdentity is the `LIMIT <n> BY (TraceId, SpanId)`
// count for the span-identity dedup both set ops end in: keep exactly
// ONE row per distinct span identity. The value 1 *is* the dedup —
// `LIMIT 1 BY` returns the first row of each (TraceId, SpanId) group
// with no global row cap, which is exactly TraceQL's "a span appears at
// most once in the result" semantics. Named so the literal isn't a bare
// magic 1.
const unionDedupLimitPerIdentity = 1

// intersectSideCol is the marker column the `&&` fallback appends to
// each UNION-ALL leg, naming the arm that produced the row. It is what
// lets both trace cohorts be read off the union the shape is ALREADY
// scanning, instead of re-deriving each one from a second evaluation of
// its own arm — see intersectQuery.
//
// It never escapes the statement: the outer projection is
// `* EXCEPT (_setand_side)`, so a nested `&&` sees its inner `&&`'s
// column set unchanged and the two markers can never collide.
const intersectSideCol = "_setand_side"

// The intersectSideCol values, one per arm of the fallback's binary
// UNION ALL. Shape constants chosen by the emitter, never user data.
const (
	intersectLeftSide  = 0
	intersectRightSide = 1
)

// emitSetOperation renders a TraceQL spanset set-op (`A && B`, `A || B`).
//
// Both ops end in the SAME span-level union, deduped on SPAN IDENTITY —
// they differ only in which traces survive. That is not a cerberus
// simplification; it is what reference Tempo does. In
// `grafana/tempo pkg/traceql/ast_execute.go` `SpansetOperation.evaluate`
// runs both sides against one trace's spanset at a time and then:
//
//	case OpSpansetAnd:
//	    if len(lhs) > 0 && len(rhs) > 0 { output = addSpanset(input[i], uniqueSpans(lhs, rhs), output) }
//	case OpSpansetUnion:
//	    if len(lhs) > 0 || len(rhs) > 0 { output = addSpanset(input[i], uniqueSpans(lhs, rhs), output) }
//
// `uniqueSpans` is a span-level UNION with duplicate removal, used
// verbatim by both branches. So `&&` is a TRACE-level conjunction over a
// SPAN-level union — "traces where both sides matched something, and for
// those traces every span either side matched" — NOT a span-level
// intersection. `{ a } && { b }` where `a` and `b` match different spans
// of one trace returns both spans, where a span intersection returns
// nothing.
//
//   - SetIntersect (`&&`): one pass over the arms' union, each leg
//     tagged with the arm that produced its rows —
//     `SELECT * EXCEPT (_setand_side) FROM (
//     (SELECT *, 0 AS _setand_side FROM (<left>)) UNION ALL
//     (SELECT *, 1 AS _setand_side FROM (<right>)))
//     QUALIFY max(_setand_side = 0) OVER (PARTITION BY TraceId)
//     AND max(_setand_side = 1) OVER (PARTITION BY TraceId)
//     LIMIT 1 BY (TraceID, SpanID)` — the two window gates are the
//     `len(lhs) > 0 && len(rhs) > 0` trace gate, the UNION ALL +
//     `LIMIT 1 BY` is `uniqueSpans`. See intersectQuery for why the
//     cohorts are read off the union rather than from each arm.
//   - SetUnion (`||`): the same shape minus the trace gate, because
//     `len(lhs) > 0 || len(rhs) > 0` admits every trace either arm
//     already emitted a row for.
//
// Cerberus's row model has one spanset per trace (chplan.SetOperation's
// contract: spanset boundaries collapse to a flat row stream keyed on
// (TraceId, SpanId)), so "one input spanset" and "one trace" coincide
// and the trace-scoped gate is exactly Tempo's per-spanset gate.
//
// Dedup is on span identity, never the full row tuple. Both arms read
// the same spans table, so a span surfaced by both predicates is
// byte-identical and keeping one is result-identical to a full-row
// UNION DISTINCT — but it streams cheaply via CH's `LIMIT n BY` instead
// of building a hash set keyed on every (wide) column, including the
// nested Events.* / Links.* array columns the OTel-CH schema carries.
// The full-row UNION DISTINCT was the root cause of the
// showcase-traceql "Spanset ||" panel never loading at self-telemetry
// scale (~150k+ spans): the dedup hash over wide rows with array
// columns is O(rows × row-width) memory; identity dedup is
// O(rows × 2 ids). It also sits at the top of the Drilldown
// structure-tab `(... &>>) || (...)` query, so the same wide-row tax is
// lifted off that path's outer union. A CH Map compares POSITIONALLY
// over its (keys, values) arrays, so full-row dedup would additionally
// treat one span redelivered under a different OTLP attribute key order
// as two rows; identity dedup never looks at the Map columns.
//
// Both shapes flow through QueryBuilder so the typed slot lifecycle
// (FROM source, WHERE predicates, etc.) stays intact. UNION is a
// SELECT-level binary operator, not a clause keyword inside a single
// SELECT, so the UNION token sits between two pre-rendered QueryBuilder
// Frags rather than abusing a clause slot.
func (e *emitter) emitSetOperation(s *chplan.SetOperation) error {
	if s.TraceIDColumn == "" || s.SpanIDColumn == "" {
		return fmt.Errorf("%w: SetOperation column names unset", ErrUnsupported)
	}

	// Fast path first, BEFORE the arms are rendered: the fused shape
	// reads its predicates off the plan and never consumes an arm's
	// rendered SQL, so probing afterwards would render — and throw away —
	// both arms, and would advance the shared CTE counter that any
	// structural closure inside them draws from.
	if s.Op == chplan.SetIntersect {
		if scan, arms, ok := fusableIntersect(s); ok {
			return e.emitFusedIntersect(s, scan, arms)
		}
	}

	leftFrag, err := e.subqueryFrag(s.Left)
	if err != nil {
		return err
	}
	rightFrag, err := e.subqueryFrag(s.Right)
	if err != nil {
		return err
	}

	switch s.Op {
	case chplan.SetIntersect:
		return e.emitSelect(intersectQuery(s, leftFrag, rightFrag))
	case chplan.SetUnion:
		sb := NewQuery().
			Select(verbatim("*")).
			From(Paren(UnionAll(leftFrag, rightFrag))).
			Limit(unionDedupLimitPerIdentity).
			LimitBy(Col(s.TraceIDColumn), Col(s.SpanIDColumn))
		return e.emitSelect(sb)
	}
	return fmt.Errorf("%w: set op %q", ErrUnsupported, s.Op)
}

// intersectQuery assembles the `&&` fallback shape described in
// emitSetOperation's godoc: ONE pass over the arms' union, from which
// both trace cohorts are read.
//
// This is the shape for every `&&` fusableIntersect declines — a
// sub-pipeline arm (`({…} | count() > 0) && …`), a nested `||`, a
// BoundedTraceScope, arms over different Scans. Those arms are real
// subplans rather than row predicates, so the single-pass window gate
// cannot reproduce them; but that is no reason to READ each arm twice.
//
// WHY NOT A CTE PER ARM. The previous shape named each arm in a
// non-recursive `WITH` and then referenced that name TWICE: once as a
// UNION-ALL leg, and once as its own cohort gate
// `TraceId IN (SELECT TraceId FROM <arm>)`. A non-recursive relational
// `WITH` is a NAME, not a materialisation — ClickHouse INLINES it at
// every reference, so N references cost N evaluations (the same property
// vector_set_op.go and nested_set_annotate.go document). Naming the arm
// kept the emitted TEXT linear and left every re-read exactly where it
// was. Measured on chDB, counting leaf reads off `EXPLAIN PLAN` (#1525):
//
//	shape                        leaf reads   min-of-9 over 2M spans
//	CTE-named arms (before)           4             493 ms
//	arms written out in full          4             460 ms
//	union-tagged (after)              2             411 ms
//
// The first two rows are the whole point: naming an arm and splicing it in
// full cost exactly the same, because the name never materialised
// anything. The sub-pipeline shape, whose arms are two reads each, goes
// 8 -> 4 the same way. Count the reads on a Memory-engine table — a
// MergeTree `EXPLAIN` renders the cohort gates' `CreatingSets` step
// WITHOUT its children, so it reports the before and after shapes as
// identical and cannot see this at all.
//
// WHAT THIS SHAPE DOES INSTEAD. Each UNION-ALL leg tags its rows with
// intersectSideCol, so "did this arm match any span of this trace?"
// becomes `max(_setand_side = <arm>) OVER (PARTITION BY TraceId)` — read
// off rows the union is already carrying rather than from a second
// evaluation of the arm. Each arm is therefore referenced exactly ONCE,
// which is also what makes the arm CTE names redundant: a single
// reference has no text to deduplicate.
//
// The gates ride QUALIFY so no window value enters the projection. The
// marker cannot: it is real per-row data the window reads, so it must be
// projected, and the outer `* EXCEPT (_setand_side)` strips it back off.
// That keeps the statement's column set exactly the arms' own — the shape
// every downstream consumer, and the spec harness's star expansion,
// already understand.
//
// SEMANTICS ARE UNCHANGED, not approximated. The marker is a non-NULL
// shape constant, so each gate is a plain UInt8 0/1 with no three-valued
// corner, and a trace passes iff every arm contributed at least one row
// to it — precisely the membership `TraceId IN (SELECT TraceId FROM
// <arm>)` tested, which is Tempo's `len(side) > 0`. Row ORDER differs
// (a gate no longer sits between the union and its consumer), but both
// shapes feed the same `LIMIT 1 BY` span-identity dedup and TraceQL fixes
// the SET of spans a query returns, never their order.
func intersectQuery(s *chplan.SetOperation, leftFrag, rightFrag Frag) *QueryBuilder {
	// (SELECT *, <side> AS _setand_side FROM (<arm>)) — the arm's rows,
	// tagged with the arm that produced them.
	sideArm := func(armFrag Frag, side int) Frag {
		return NewQuery().
			Select(Star(), As(InlineLit(side), intersectSideCol)).
			From(armFrag).
			Frag()
	}
	// max(_setand_side = <side>) OVER (PARTITION BY TraceId) — "this arm
	// matched at least one span in this trace", i.e. Tempo's
	// `len(side) > 0`, computed over the union rather than over the arm.
	armTraceCohort := func(side int) Frag {
		return Window(
			Call("max", Eq(Col(intersectSideCol), InlineLit(side))),
			[]Frag{Col(s.TraceIDColumn)},
			nil,
		)
	}

	return NewQuery().
		Select(StarExcept(Star(), intersectSideCol)).
		From(Paren(UnionAll(
			sideArm(leftFrag, intersectLeftSide),
			sideArm(rightFrag, intersectRightSide),
		))).
		Qualify(armTraceCohort(intersectLeftSide), armTraceCohort(intersectRightSide)).
		Limit(unionDedupLimitPerIdentity).
		LimitBy(Col(s.TraceIDColumn), Col(s.SpanIDColumn))
}

// fusableIntersect decides whether an `&&` tree can be served by the
// single-pass window gate instead of the union-gated CTE shape, and if
// so returns the one Scan every arm reads plus the arms' predicates in
// left-to-right order.
//
// THE DECISION. The gate rewrites "traces where every arm matched at
// least one span, carrying every span any arm matched" from a union of
// separately-evaluated arms into ONE pass over the shared table:
//
//	SELECT * FROM <spans>
//	WHERE  (<p1>) OR (<p2>) …            -- the span-level union
//	QUALIFY max(<p1>) OVER (PARTITION BY <TraceId>)
//	   AND  max(<p2>) OVER (PARTITION BY <TraceId>) …   -- the trace gate
//	LIMIT 1 BY <TraceId>, <SpanId>
//
// That is only a faithful rewrite when each arm is a ROW PREDICATE over
// the same rows — every arm must reduce to "this span matches <p>", so
// that `max(<p>) OVER (PARTITION BY TraceId)` reproduces Tempo's
// `len(side) > 0` per-trace test, and so that restricting the scan to
// the disjunction cannot hide a row an arm needed (a row satisfying <p1>
// always satisfies `<p1> OR <p2>`).
//
// An arm qualifies iff it is a chain of Filters bottoming out in a Scan
// (conjoined into one predicate), the Scan is Equal to every other arm's,
// and the predicate is gate-safe (see gateSafePredicate).
//
// WHAT DOES NOT QUALIFY, and why the choice is load-bearing rather than
// cosmetic. `spanset_pipeline_intersect`'s arms —
// `({…} | count() > 0) && ({…} | count() > 0)` — lower to
// `Filter(TraceId IN {Aggregate pipeline}) → Filter(<selector>) → Scan`.
// Structurally that IS a Filter chain over the shared Scan, so a purely
// structural test would fuse it; the result would be WRONG, because the
// arm's membership is decided by a per-trace aggregate over the arm's own
// row set, which no per-row predicate — and therefore no `max(<p>) OVER`
// — can reproduce. gateSafePredicate's "no nested plan" rule is exactly
// the line between the two cases.
//
// Nested `&&` flattens: `A && B && C` arrives as
// SetOperation(SetOperation(A, B), C) and fuses to a single pass over all
// three arms, since "(A && B) matched this trace" is precisely "A matched
// and B matched" and its spans are A's ∪ B's. A chain that is only partly
// fusable falls back at the level that fails and still fuses below it,
// because the fallback renders its arms through the ordinary recursive
// emit.
func fusableIntersect(s *chplan.SetOperation) (*chplan.Scan, []chplan.Expr, bool) {
	var scan *chplan.Scan
	var arms []chplan.Expr
	if !collectIntersectArms(s, s, &scan, &arms) {
		return nil, nil, false
	}
	return scan, arms, true
}

// collectIntersectArms walks the `&&` chain rooted at root, appending one
// predicate per leaf arm and pinning the single Scan they must share.
// Reports false the moment any arm disqualifies.
func collectIntersectArms(n chplan.Node, root *chplan.SetOperation, scan **chplan.Scan, arms *[]chplan.Expr) bool {
	if inner, ok := n.(*chplan.SetOperation); ok {
		// Only an `&&` on the SAME identity key flattens. A nested `||`
		// is a different operator, and a nested `&&` keyed on a different
		// (TraceId, SpanId) pair is a spanset granularity change the gate
		// must not paper over.
		if inner.Op != chplan.SetIntersect ||
			inner.TraceIDColumn != root.TraceIDColumn ||
			inner.SpanIDColumn != root.SpanIDColumn {
			return false
		}
		return collectIntersectArms(inner.Left, root, scan, arms) &&
			collectIntersectArms(inner.Right, root, scan, arms)
	}
	pred, armScan, ok := filterChainOverScan(n)
	if !ok || !gateSafePredicate(pred) {
		return false
	}
	if *scan == nil {
		*scan = armScan
	} else if !(*scan).Equal(armScan) {
		return false
	}
	*arms = append(*arms, pred)
	return true
}

// filterChainOverScan peels a chain of Filters off a Scan and returns the
// conjunction of their predicates (nil for a bare, unfiltered Scan — an
// arm that matches every span). Reports false for any other node shape.
func filterChainOverScan(n chplan.Node) (chplan.Expr, *chplan.Scan, bool) {
	var preds []chplan.Expr
	for {
		switch v := n.(type) {
		case *chplan.Filter:
			// Collected outermost-first; reversed below so the rendered
			// conjunction reads innermost-first, the order the arm's own
			// Filter(Scan) emit would have produced. A predicate-less
			// Filter restricts nothing and contributes no conjunct — it
			// must not reach the AND chain, where a nil operand would
			// render as an empty token.
			if v.Predicate != nil {
				preds = append(preds, v.Predicate)
			}
			n = v.Input
		case *chplan.Scan:
			for i, j := 0, len(preds)-1; i < j; i, j = i+1, j-1 {
				preds[i], preds[j] = preds[j], preds[i]
			}
			return conjoinExprs(preds), v, true
		default:
			return nil, nil, false
		}
	}
}

// conjoinExprs folds preds into a single left-associated AND expression.
// Empty returns nil, the "no predicate at all" signal.
func conjoinExprs(preds []chplan.Expr) chplan.Expr {
	if len(preds) == 0 {
		return nil
	}
	out := preds[0]
	for _, p := range preds[1:] {
		out = &chplan.Binary{Op: chplan.OpAnd, Left: out, Right: p}
	}
	return out
}

// gateSafePredicate reports whether pred is a pure row predicate — one
// whose truth for a span depends on that span's own columns and nothing
// else — which is the precondition for `max(pred) OVER (PARTITION BY
// TraceId)` to mean "some span of this trace matched this arm".
//
// Two things disqualify, both because they smuggle a whole table read
// into what looks like a scalar test:
//
//   - A nested plan (chplan.InSubquery / chplan.ScalarSubquery). This is
//     the sub-pipeline arm of fusableIntersect's godoc: the arm's real
//     membership test lives in that subplan, not in the row.
//   - A BoundedTraceScope. It is an Expr LEAF, so the Node walk never
//     sees it, but the emitter re-derives a top-N trace subquery from it
//     at emit time — fusing one would keep a second read of the spans
//     table alive behind a shape that advertises a single pass.
//
// A nil predicate (unfiltered Scan arm) is trivially gate-safe.
func gateSafePredicate(pred chplan.Expr) bool {
	if pred == nil {
		return true
	}
	safe := true
	chplan.InspectExprNodes(pred, func(e chplan.Expr) bool {
		if _, ok := e.(*chplan.BoundedTraceScope); ok {
			safe = false
		}
		return safe
	}, func(chplan.Node) { safe = false })
	return safe
}

// emitFusedIntersect renders the single-pass window gate described in
// fusableIntersect's godoc.
//
// The arms' disjunction rides filterScanQuery, the same assembly an arm's
// own Filter(Scan) would have used, so the fused scan keeps PREWHERE
// granule pruning rather than trading four pruned reads for one unpruned
// one. Each arm predicate is therefore rendered twice — once in the
// WHERE/PREWHERE disjunction, once inside its `max(…) OVER (…)` gate —
// and binds its `?` args twice; that is the price of never materialising
// the predicate as a column.
//
// Not materialising it is the point. The obvious alternative, projecting
// `max(…) OVER (…) AS g` and filtering on `g`, forces the outer SELECT to
// strip the synthetic column back off with `* EXCEPT (g)`, which changes
// the statement's projection shape for every downstream consumer. QUALIFY
// filters on the window value while leaving the projection a bare
// `SELECT *` over the spans table — the same column set the arm CTEs
// produce today.
//
// The trace gate is `max(<pred>)`, not `countIf(<pred>) > 0`, so a
// Nullable predicate stays correct by CH's own three-valued rules: an arm
// whose predicate is NULL on every span of a trace yields a NULL gate,
// and `WHERE NULL` drops the row exactly as the arm's own
// `TraceId IN (<cohort>)` would have.
//
// Row ORDER differs from the fallback shape: the union emits all of the
// left arm then all of the right, while one pass emits table order. Both
// are unordered results feeding the same `LIMIT 1 BY` span-identity dedup
// — TraceQL's own contract fixes the SET of spans, never their order.
func (e *emitter) emitFusedIntersect(s *chplan.SetOperation, scan *chplan.Scan, arms []chplan.Expr) error {
	common, residuals := splitCommonConjuncts(arms)
	conds := append([]chplan.Expr(nil), common...)
	if disj := disjoinArms(residuals); disj != nil {
		conds = append(conds, disj)
	}

	var (
		sb  *QueryBuilder
		err error
	)
	if len(conds) > 0 {
		sb, err = filterScanQuery(&chplan.Filter{Input: scan, Predicate: conjoinExprs(conds)}, scan)
	} else {
		// Every arm matches every span, so there is nothing to restrict
		// the scan by.
		sb, err = scanQuery(scan)
	}
	if err != nil {
		return err
	}
	for _, pred := range residuals {
		if pred == nil {
			// The arm is fully implied by the shared conjuncts every
			// surviving row already satisfies, so its gate is constantly
			// true — including the unfiltered-arm case.
			continue
		}
		sb.Qualify(Window(
			Call("max", func(b *Builder) { _ = b.Expr(pred) }),
			[]Frag{Col(s.TraceIDColumn)},
			nil,
		))
	}
	sb.Limit(unionDedupLimitPerIdentity).
		LimitBy(Col(s.TraceIDColumn), Col(s.SpanIDColumn))
	return e.emitSelect(sb)
}

// splitCommonConjuncts factors the conjuncts every arm shares out of the
// arms' predicates, returning them plus each arm's residual (nil when an
// arm is wholly implied by the shared set).
//
// This is what keeps the fused scan as cheap as the arms it replaces. A
// windowed TraceQL query — the shape Grafana actually sends — puts the
// SAME request-window bounds on every arm, so the unfactored disjunction
// would be `(<p1> AND <window>) OR (<p2> AND <window>)`: ONE opaque
// conjunct, from which partitionPrewhere can promote nothing and CH can
// prune no `toDate(Timestamp)` partition. Factored, the restriction is
// `<window> AND (<p1> OR <p2>)` and the window conjuncts reach PREWHERE
// exactly as they do on an arm's own Filter(Scan). Without this the fast
// path would trade four partition-pruned reads for one full-retention
// scan and be a REGRESSION on the production path, not a win.
//
// Dropping the shared conjuncts from the gate terms is sound for the same
// reason it is safe to drop them from the disjunction: every row the
// window function sees has already passed them, so on that row set
// `<residual> AND <common>` and `<residual>` agree, and therefore so do
// their per-partition maxima.
func splitCommonConjuncts(arms []chplan.Expr) (common, residuals []chplan.Expr) {
	residuals = make([]chplan.Expr, len(arms))
	perArm := make([][]chplan.Expr, len(arms))
	for i, pred := range arms {
		if pred == nil {
			// An unfiltered arm shares nothing, so nothing is common to
			// ALL arms; every arm keeps its own predicate whole.
			return nil, arms
		}
		perArm[i] = flattenAnd(pred)
	}
	if len(perArm) == 0 {
		return nil, residuals
	}

	shared := make([]bool, len(perArm[0]))
	for i, c := range perArm[0] {
		shared[i] = true
		for _, other := range perArm[1:] {
			if !containsExpr(other, c) {
				shared[i] = false
				break
			}
		}
		if shared[i] {
			common = append(common, c)
		}
	}
	if len(common) == 0 {
		return nil, arms
	}
	for i, conjuncts := range perArm {
		var rest []chplan.Expr
		for _, c := range conjuncts {
			if !containsExpr(common, c) {
				rest = append(rest, c)
			}
		}
		residuals[i] = conjoinExprs(rest)
	}
	return common, residuals
}

// containsExpr reports whether list holds an expression equal to want.
func containsExpr(list []chplan.Expr, want chplan.Expr) bool {
	for _, e := range list {
		if e.Equal(want) {
			return true
		}
	}
	return false
}

// disjoinArms folds the arms' predicates into the span-level union's
// restriction. A nil arm predicate makes the whole disjunction a
// tautology, so the result is nil and the caller emits no restriction at
// all rather than a redundant `… OR true`.
func disjoinArms(arms []chplan.Expr) chplan.Expr {
	var out chplan.Expr
	for _, pred := range arms {
		if pred == nil {
			return nil
		}
		if out == nil {
			out = pred
			continue
		}
		out = &chplan.Binary{Op: chplan.OpOr, Left: out, Right: pred}
	}
	return out
}
