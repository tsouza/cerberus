// Package routememo implements the failure-driven route memo: a bounded,
// in-process cache that records the OUTCOME of dispatching a query on
// route A (single-statement) or route B (sharded pushdown), keyed by a
// literal-free structural fingerprint of the plan plus its cost-grid
// buckets. It replaces a predictive cost-threshold gate with an outcome
// signal: route A failing with resource exhaustion is ground truth that no
// static proxy threshold can be.
//
// The memo stores a routing DECISION only — never result rows, never
// query text, never label values. A stale or wrong entry can cost
// performance (an extra retry, or a request that stays on route A when B
// would have helped); it can never change an answer, because Route B's own
// output-equivalence proof (internal/chplan/sliceinvariant.go +
// internal/solver's structural gates) is unconditional on the memo — the
// memo only decides WHETHER to try B, never HOW B computes its answer.
//
// See docs/solver.md for the design rationale and the safety envelope.
package routememo

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Route identifies which side of the sharded-pushdown solver a dispatch ran
// on.
type Route int

const (
	// RouteA is the single-statement execution path.
	RouteA Route = iota
	// RouteB is the sharded-pushdown execution path.
	RouteB
)

// String renders the Route for logs/metrics labels.
func (r Route) String() string {
	switch r {
	case RouteA:
		return "A"
	case RouteB:
		return "B"
	default:
		return "unknown"
	}
}

// maxCostBucketExponent caps the log2 cost-bucket exponent. bucketLg already
// grows only as fast as log2 of the input, so this is a defensive ceiling
// against a pathological (overflow-adjacent) N/F/step value rather than a
// value any real request grid approaches.
const maxCostBucketExponent = 40

// bucketLg buckets v into a scale-free, bounded exponent: 0 for v<=1, else
// floor(log2(v)) clamped to maxCostBucketExponent. Two cost values in the
// same bucket are treated as routing-equivalent: the within-bucket cost
// spread is bounded by construction (a power-of-two band), and bucketing
// (rather than the raw value) is what lets genuinely repeated traffic — a
// dashboard panel refreshing on a rolling window — keep hitting the same Key
// across refreshes despite the anchor count drifting by one as time moves.
func bucketLg(v int64) int {
	if v <= 1 {
		return 0
	}
	lg := 0
	for v > 1 {
		v >>= 1
		lg++
	}
	if lg > maxCostBucketExponent {
		return maxCostBucketExponent
	}
	return lg
}

// Matcher operator-kind bits — the closed PromQL/LogQL matcher-operator
// vocabulary a Filter predicate can carry. These are operator KINDS only:
// never label names, never label values.
const (
	matcherOpEq uint8 = 1 << iota
	matcherOpNe
	matcherOpMatch
	matcherOpNotMatch
)

// Key is the routing-specific memo key: a literal-free structural
// fingerprint of a plan's shape plus its cost-grid buckets. Two requests
// sharing a Key are judged cost-equivalent enough to share a routing
// verdict.
//
// Key is comparable (usable as a Go map key): every field is a primitive,
// a bool, or a string built only from closed query-language vocabulary
// (function names, operator kinds) — never a metric name, label name,
// label value, or timestamp. This is the same category of information
// planShapeID (internal/engine/plan_shape_id.go) already exposes at a
// coarser grain, and no coarser than ClickHouse's own normalized
// query_log text.
type Key struct {
	// RootKind is the plan root node's Go type name (e.g. "*chplan.Aggregate").
	RootKind string

	// RangeFuncs is the per-level range-window function identity paired
	// with that window's own bucketed lookback exponent, encoded as
	// "func@bucket" pairs in tree pre-order, comma-joined. Closed PromQL
	// range-function vocabulary (rate / increase / *_over_time / lwr / ...).
	RangeFuncs string

	// AggFuncs is the per-level aggregate-function identity (Aggregate /
	// RangeBucketFanout AggFunc.Name), comma-joined in tree pre-order.
	// Closed ClickHouse aggregate vocabulary (sum / count / avg / quantile
	// / ...).
	AggFuncs string

	// MatcherCountLg is the bucketed exponent of the total Filter-predicate
	// leaf-comparison count anywhere in the plan.
	MatcherCountLg int

	// MatcherOpMask is the bitmask of matcherOp* kinds present anywhere in
	// the plan (=, !=, =~, !~ — operator kinds, never label names/values).
	MatcherOpMask uint8

	HasJoin     bool
	HasUnion    bool
	HasLimit    bool
	HasNative   bool
	HasResample bool

	// AnchorLg / FanoutLg / StepLg are the grid-geometry cost buckets: two
	// queries with identical function shapes but very different anchor
	// counts still have different cost, so these buckets keep genuinely
	// different-cost shapes from colliding.
	AnchorLg int
	FanoutLg int
	StepLg   int
}

// KeyFor builds the routing Key for plan at the given cost-grid readout
// (nAnchors, fanout, step — the same N/F/Step the solver's Planner already
// computes for its own cost gate).
func KeyFor(plan chplan.Node, nAnchors int, fanout int64, step time.Duration) Key {
	var w keyWalker
	w.walk(plan)

	return Key{
		RootKind:       nodeKind(plan),
		RangeFuncs:     strings.Join(w.rangeFuncs, ","),
		AggFuncs:       strings.Join(w.aggFuncs, ","),
		MatcherCountLg: bucketLg(int64(w.matcherCount)),
		MatcherOpMask:  w.matcherOpMask,
		HasJoin:        w.hasJoin,
		HasUnion:       w.hasUnion,
		HasLimit:       w.hasLimit,
		HasNative:      w.hasNative,
		HasResample:    w.hasResample,
		AnchorLg:       bucketLg(int64(nAnchors)),
		FanoutLg:       bucketLg(fanout),
		StepLg:         bucketLg(int64(step)),
	}
}

// nodeKind renders a plan node's dynamic Go type name — a fixed,
// closed-by-construction label set (the chplan node kinds cerberus itself
// defines), never any data the node carries.
func nodeKind(n chplan.Node) string {
	if n == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", n)
}

// keyWalker accumulates the routing-key signals over one plan-tree pass.
type keyWalker struct {
	rangeFuncs    []string
	aggFuncs      []string
	matcherCount  int
	matcherOpMask uint8

	hasJoin     bool
	hasUnion    bool
	hasLimit    bool
	hasNative   bool
	hasResample bool
}

func (w *keyWalker) walk(n chplan.Node) {
	chplan.Walk(n, func(node chplan.Node) bool {
		switch v := node.(type) {
		case *chplan.RangeWindow:
			w.rangeFuncs = append(w.rangeFuncs, v.Func+"@"+strconv.Itoa(bucketLg(int64(v.Range))))
		case *chplan.RangeLWR:
			w.rangeFuncs = append(w.rangeFuncs, "lwr@"+strconv.Itoa(bucketLg(int64(v.Lookback))))
		case *chplan.RangeBucketFanout:
			w.rangeFuncs = append(w.rangeFuncs, "bucketfanout@"+strconv.Itoa(bucketLg(int64(v.Lookback))))
			for _, fn := range v.AggFuncs {
				w.aggFuncs = append(w.aggFuncs, fn.Name)
			}
		case *chplan.RangeWindowNative:
			w.rangeFuncs = append(w.rangeFuncs, "native@"+strconv.Itoa(bucketLg(int64(v.Range))))
			w.hasNative = true
		case *chplan.RangeWindowResample:
			w.hasResample = true
		case *chplan.Aggregate:
			for _, fn := range v.AggFuncs {
				w.aggFuncs = append(w.aggFuncs, fn.Name)
			}
		case *chplan.VectorJoin:
			w.hasJoin = true
		case *chplan.UnionAll:
			w.hasUnion = true
		case *chplan.SetOperation:
			w.hasUnion = true
		case *chplan.NaryVectorSetOp:
			w.hasUnion = true
		case *chplan.Limit:
			w.hasLimit = true
		case *chplan.Filter:
			w.walkPredicate(v.Predicate)
		}
		return true
	})
}

// walkPredicate sweeps a Filter predicate expr tree for matcher-style
// binary comparisons, counting leaves and recording which operator KINDS
// (never operands) are present.
func (w *keyWalker) walkPredicate(e chplan.Expr) {
	chplan.InspectExpr(e, func(x chplan.Expr) bool {
		b, ok := x.(*chplan.Binary)
		if !ok {
			return true
		}
		switch b.Op {
		case chplan.OpEq:
			w.matcherCount++
			w.matcherOpMask |= matcherOpEq
		case chplan.OpNe:
			w.matcherCount++
			w.matcherOpMask |= matcherOpNe
		case chplan.OpMatch:
			w.matcherCount++
			w.matcherOpMask |= matcherOpMatch
		case chplan.OpNotMatch:
			w.matcherCount++
			w.matcherOpMask |= matcherOpNotMatch
		}
		return true
	})
}
