package chsql

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emit_size_bound.go closes cerberus issue #2733.
//
// # The failure this converts
//
// A FOURTH bracket level of subquery composition over a mixed
// float/histogram inner — one more than issue #2728 answers — lowers
// correctly and emits correct SQL, but emits so MUCH of it that ClickHouse
// refuses to parse the statement at all:
//
//	Code: 62. DB::Exception: Max query size exceeded (can be increased with
//	the `max_query_size` setting): Syntax error: failed at position 262145
//
// Nothing is silently wrong and nothing hangs — the query fails immediately,
// truthfully, at the database. What it lacks is a cerberus-side name for the
// shape that cannot be served: before #2728 the same query was refused by
// lowerHistogramOrMixedSubqueryOuterFnInput's own gate with an error naming
// the unsupported composition, and #2728 opening that arm (correctly, for the
// one extra level that IS serveable) moved the deeper levels' failure from
// "cerberus says no" to "ClickHouse says no".
//
// # Why the SQL grows the way it does
//
// splitMixedRelByDiscriminator (internal/promql) partitions a Mixed relation
// into a histogram half and a float half and folds each, so a FOLD-family
// level names the relation beneath it twice. Stacking those levels compounds.
// Measured over `((m_exp_hist) or (m_gauge))`, emitted placeholder SQL, at an
// instant eval (the query_range fan-out runs ~1.5x larger, and the size is
// independent of the anchor count either way):
//
//	bracket levels   emitted SQL   plan nodes (DAG expanded)
//	             1        26,480                          16
//	             2       140,319                          67
//	             3       588,295                         147
//	             4     1,786,077                         306
//
// # Why the bound measures rendered bytes rather than predicting them
//
// A plan-side prediction was the first design tried, and the numbers above
// are what refuted it: the DAG-expanded plan-node count grows ~2.1x per
// level while the emitted SQL grows ~3-4x, because the emitter itself
// duplicates text the plan does not duplicate (rateWindowFanoutBoundedSourceFrag
// and lwrFanoutBoundedSourceFrag each render their fan-out source a second
// time for the truncation probe, and the histogram fold expressions render
// far more text per node than a float one). Bytes-per-plan-node therefore
// ranges from ~0.75KB (a plain `rate(<counter>[5m])`) to ~5.7KB (the level-4
// composition) across the shapes this package already serves — a spread no
// single node-count threshold can split into "fits" and "does not fit"
// without rejecting shapes ClickHouse would have parsed happily. Measuring
// the rendered statement is exact, and it costs one integer comparison.
//
// # Why the measure is the placeholder SQL, and why that is the safe direction
//
// The bytes ClickHouse's max_query_size counts are the statement AFTER
// clickhouse-go/v2 inlines every positional arg client-side — the native
// protocol has no bound-parameter channel (see internal/api/prom/metadata.go's
// boundQueryBytes and the #799 incident it records). So the wire statement is
// always at least as long as the `?`-carrying text this guard measures: every
// `?` is replaced by a literal of one byte or more. Measuring the placeholder
// text therefore UNDERCOUNTS, deliberately:
//
//   - every statement this guard rejects is one ClickHouse would also have
//     rejected, so the guard adds no restriction of its own — it only replaces
//     a driver-level code 62 with a cerberus error that names the shape;
//   - a statement it admits may still be refused by ClickHouse (the arg
//     literals push it over), which is exactly today's behaviour and no worse.
//
// The alternative — reusing metadata.go's deliberate OVER-approximation of the
// inlined size — would invert that: the issue #2728 level-2 composition renders
// 209,182 placeholder bytes with 1,442 args, which the over-approximation
// scores at 255,052 and a realistic inlining puts at 215,430. Add a handful of
// label matchers and the over-approximation crosses 262,144 while the real
// statement does not, and a shipping, chDB-proven shape would start being
// refused. Undercounting cannot make that mistake.
//
// # The two call sites
//
// Emit checks the finished statement, which is the whole point. renderNode
// checks each isolated sub-statement as it is produced, and that second site
// is what bounds the COST of rejecting: the final statement contains every
// rendered sub-statement verbatim, so a sub-statement already over the bound
// proves the whole is over it, and stopping there caps the emitter's peak
// allocation at roughly one bound's worth of text instead of building the
// whole thing first. Without it a query that simply stacks more brackets —
// a short string to type — walks the 4x-per-level curve: ~12MB of SQL at five
// levels, ~45MB at six, ~700MB at eight, all built and then thrown away.

// maxEmittedSQLBytes is the default ceiling on the bytes of SQL cerberus will
// emit for one statement. It is ClickHouse's own `max_query_size` default
// (256KiB), not a cerberus-chosen figure, and that identity is the whole
// argument for the guard being safe to apply to every plan of every head: a
// statement past this length is one the server will refuse to parse anyway
// (the measure undercounts the wire bytes — see this file's doc comment), so
// the guard changes the ERROR a user sees, never whether the query could have
// run.
//
// For scale: the largest statement in this repo's entire golden corpus (1,216
// `-- sql --` sections across all three heads) is 40,475 bytes, and the
// largest deliberately-shipping exotic shape — issue #2728's level-2
// composition under query_range — is 209,182. An operator whose server raises
// max_query_size raises this alongside it via
// CERBERUS_CH_MAX_EMITTED_SQL_BYTES (see WithMaxEmittedSQLBytes), rather than
// waiting for a release, exactly as issue #2667 made the three sample-fanout
// ceilings overridable.
const maxEmittedSQLBytes int64 = 262144

// ErrEmittedSQLTooLarge is the sentinel Emit / renderNode return for a
// statement past the emitted-SQL byte bound. It wraps ErrUnsupported so the
// existing emit-error handling (and the HTTP error mapping behind it) treats
// it as an ordinary unsupported-shape failure — which is what it is: the shape
// is correct, the emitter simply cannot express it in a statement ClickHouse
// will parse.
var ErrEmittedSQLTooLarge = fmt.Errorf("%w: emitted SQL exceeds the statement-size bound", ErrUnsupported)

// maxEmittedSQLBytesKey is the unexported context key carrying an
// operator-configured override for maxEmittedSQLBytes — see
// WithMaxEmittedSQLBytes / maxEmittedSQLBytesFromCtx below.
type maxEmittedSQLBytesKey struct{}

// WithMaxEmittedSQLBytes returns ctx carrying n as the operator override for
// the emitted-SQL byte bound (otherwise maxEmittedSQLBytes). The caller —
// internal/engine's emitForHead / routeBExecCtx — only threads this when
// CERBERUS_CH_MAX_EMITTED_SQL_BYTES is actually set: a statement-size bound of
// zero is never a legitimate operator intent (it would reject every query), so
// "never threaded" — not "threaded as zero" — is what selects the compiled-in
// default. Mirrors WithRateWindowFanoutMaxRows exactly.
func WithMaxEmittedSQLBytes(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, maxEmittedSQLBytesKey{}, n)
}

// maxEmittedSQLBytesFromCtx recovers the bound WithMaxEmittedSQLBytes set, or
// maxEmittedSQLBytes (ClickHouse's own default) when the caller never threaded
// one.
func maxEmittedSQLBytesFromCtx(ctx context.Context) int64 {
	if n, ok := ctx.Value(maxEmittedSQLBytesKey{}).(int64); ok {
		return n
	}
	return maxEmittedSQLBytes
}

// emittedSQLByteBound returns e's own resolved bound — e.emittedSQLMaxBytes,
// seeded once from ctx inside Emit — falling back to maxEmittedSQLBytes
// whenever the field reads as its Go zero value. The fallback is load-bearing
// for the same reason rateWindowFanoutRowBound's is: many round-trip tests in
// this package construct &emitter{} directly, bypassing Emit's ctx seeding
// entirely, and a bound of 0 read literally would reject every statement they
// render.
func (e *emitter) emittedSQLByteBound() int64 {
	if e.emittedSQLMaxBytes > 0 {
		return e.emittedSQLMaxBytes
	}
	return maxEmittedSQLBytes
}

// requireEmittedSQLBounded rejects sql — the statement rendered for n — when it
// is longer than e's resolved bound, naming the composition that produced it so
// the message says which query shape to change rather than only how many bytes
// it took.
//
// The shape named is the WHOLE plan (e.rootPlan, stamped once by Emit) even
// when the statement measured is one of its sub-statements, because that is the
// shape whose author can act on the message; "at least" in the wording is what
// keeps the byte figure honest in that case, since a sub-statement's length is
// a lower bound on the finished one's. An emitter built without going through
// Emit (EmitCompareRootLeg, and this package's own round-trip tests) carries no
// root, so it describes the node it was handed.
func (e *emitter) requireEmittedSQLBounded(n chplan.Node, sql string) error {
	maxBytes := e.emittedSQLByteBound()
	if int64(len(sql)) <= maxBytes {
		return nil
	}
	described := e.rootPlan
	if described == nil {
		described = n
	}
	return fmt.Errorf(
		"%w: %s renders at least %d bytes, past the %d-byte ceiling (ClickHouse's own "+
			"max_query_size); reduce the query's composition, or raise max_query_size and "+
			"CERBERUS_CH_MAX_EMITTED_SQL_BYTES together",
		ErrEmittedSQLTooLarge, planCompositionSummary(described), len(sql), maxBytes,
	)
}

// planCompositionSummary names n's shape in the two terms that actually drive
// the emitted size, so a rejection tells a user what to change:
//
//   - how many range-vector levels are stacked, counted as the longest chain of
//     [chplan.GridCarrier] nodes from the root to a leaf. GridCarrier is the
//     IR's own "this node owns an eval grid" declaration, closed by a
//     completeness ratchet (chplan/grid_carrier_completeness_test.go), so this
//     count cannot drift as node kinds are added the way an enumeration here
//     would;
//   - whether any level sits over a MIXED float/histogram relation, which is
//     what makes a level cost ~4x rather than ~2x (splitMixedRelByDiscriminator
//     names the relation beneath it once per half).
//
// The count is of PLAN levels, not of brackets in the query text: an
// innermost selector that is itself resolved as a range vector carries a grid
// of its own, so a three-bracket query reads here as four levels. The wording
// says "a plan stacking N range-vector levels" for exactly that reason.
func planCompositionSummary(n chplan.Node) string {
	levels, mixed := gridCarrierNesting(n)
	switch {
	case levels > 1 && mixed:
		return fmt.Sprintf("a plan stacking %d range-vector levels over a mixed float/histogram relation", levels)
	case levels > 1:
		return fmt.Sprintf("a plan stacking %d range-vector levels", levels)
	case mixed:
		return "a plan over a mixed float/histogram relation"
	default:
		return "this query's plan"
	}
}

// gridCarrierNesting returns the longest root-to-leaf chain of
// [chplan.GridCarrier] nodes in n, and whether n contains a Mixed
// [chplan.VectorSetOp] anywhere.
//
// The traversal follows Node.Children AND the plan subtrees embedded in Expr
// slots (the reach [chplan.WalkDeep] has and [chplan.Walk] does not), because
// the emitter renders both and a composition hidden inside a scalar subquery
// is no cheaper than one on the spine.
//
// It is memoised on node identity, which is not an optimisation but a
// requirement: a plan is a DAG, not a tree — splitMixedRelByDiscriminator hands
// the SAME relation to both of its halves — so an unmemoised descent is
// exponential in the nesting depth this function exists to describe.
func gridCarrierNesting(n chplan.Node) (levels int, mixed bool) {
	depth := map[chplan.Node]int{}
	var walk func(chplan.Node) int
	walk = func(n chplan.Node) int {
		if n == nil {
			return 0
		}
		if d, seen := depth[n]; seen {
			return d
		}
		if v, ok := n.(*chplan.VectorSetOp); ok && v.Mixed {
			mixed = true
		}
		deepest := 0
		descend := func(c chplan.Node) {
			if d := walk(c); d > deepest {
				deepest = d
			}
		}
		for _, c := range n.Children() {
			descend(c)
		}
		chplan.InspectNodeExprs(n, func(e chplan.Expr) {
			chplan.InspectExprNodes(e, func(chplan.Expr) bool { return true }, descend)
		})
		if _, ok := n.(chplan.GridCarrier); ok {
			deepest++
		}
		depth[n] = deepest
		return deepest
	}
	return walk(n), mixed
}
