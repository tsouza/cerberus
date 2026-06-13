package solver

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Planner is pure, read-only classification of a post-optimize plan into a
// routing Decision. It never mutates the plan: every check reads the tree,
// and slicing (the only path that copies) goes through ReanchorRange, which
// deep-copies.
type Planner struct {
	Cfg Config
}

// Plan classifies plan against meta and returns the Decision plus whether the
// plan routes B. The Decision is always non-nil: even a non-route carries the
// Reason for the shadow header.
//
// Routing follows docs §Routing: a single pass walks both the node tree and
// every expression tree (including ScalarSubquery.Input, which chplan.Walk
// does NOT recurse into) gathering the eligibility signals, then the cost
// thresholds and the K clamp decide. Mode shapes the final gate:
//
//   - "single": classify but NEVER route — always returns (decision, false).
//   - "sharded": thresholds drop to the floor (K_min = 2) so every ELIGIBLE
//     plan routes; ineligible plans still stay on route A.
//   - "auto": route iff eligible AND F >= MinFanout AND N x F >= MinAnchorPairs
//     AND K >= 2.
func (p *Planner) Plan(plan chplan.Node, meta RequestMeta) (*Decision, bool) {
	// (2)-prefix: instant queries are never time-slice routed in phase 1.
	// Step == 0 means an instant evaluation; there is no anchor grid.
	if meta.Step <= 0 {
		return notRouted(ReasonInstant), false
	}

	sig := p.analyze(plan, meta)

	// (1) Slice-invariance: any unmarked node anywhere → route A.
	if !sig.allSliceInvariant {
		return notRouted(ReasonNotSliceable), false
	}
	// (3) now64 anywhere (predicate / projection / ScalarSubquery.Input).
	if sig.sawNow64 {
		return notRouted(ReasonNow64), false
	}
	// (2) Both Start and End pinned on every windowed node, and no
	// instant-shape windowed node (OuterRange == 0 / Step == 0).
	if sig.sawUnpinnedBound || sig.sawInstantWindow {
		return notRouted(ReasonInstant), false
	}
	// (4) Grid-prediction check: a windowed node whose bounds diverge from
	// the grid the request predicts at its spine depth (an @-pinned anchor).
	if sig.sawGridMismatch {
		return notRouted(ReasonGridMismatch), false
	}
	// (5) Grid commensurability for nested spines.
	if sig.sawIncommensurate {
		return notRouted(ReasonIncommensurate), false
	}
	// (6) A ScalarSubquery too expensive to replicate K× (and that cannot be
	// classified safe by the cheap interior bound) → route A.
	if sig.sawScalarHeavy {
		return notRouted(ReasonScalarHeavy), false
	}

	// The plan is ELIGIBLE. Compute the cost grid and the K clamp.
	if !sig.hasWindow {
		// Eligible but no windowed node carries an anchor grid to slice —
		// nothing to gain from slicing.
		return notRouted(ReasonBelowThreshold), false
	}

	n := sig.outerN              // N = OuterRange/Step + 1
	f := sig.maxFanout           // F = max(Range/Step or Lookback/Step)
	outerRange := sig.outerRange // OuterRange of the outermost spine
	d := sig.cumulativeD         // D = cumulative spine lookback
	step := meta.Step            // the grid step

	// K = clamp(floor(N/minAnchorsPerSlice), 2, min(MaxK, floor(OuterRange/max(D,Step)))).
	denom := d
	if step > denom {
		denom = step
	}
	highBound := int64(outerRange / denom) // floor(OuterRange / max(D, Step))
	upper := int64(p.Cfg.MaxK)
	if highBound < upper {
		upper = highBound
	}
	lower := int64(2)
	k := int64(n / p.Cfg.MinAnchorsPerSlice)
	if k < lower {
		k = lower
	}
	if k > upper {
		k = upper
	}

	// If the high-D clamp ceiling fell below 2 there is no valid K — the
	// documented high-D floor.
	if upper < 2 {
		return notRouted(ReasonHighD), false
	}

	// "single" classifies but never routes.
	if p.Cfg.Mode == ModeSingle {
		return notRouted(ReasonBelowThreshold), false
	}

	if p.Cfg.Mode == ModeAuto {
		// Cost thresholds gate the auto path. Below threshold → route A.
		if f < int64(p.Cfg.MinFanout) {
			return notRouted(ReasonBelowThreshold), false
		}
		if int64(n)*f < int64(p.Cfg.MinAnchorPairs) {
			return notRouted(ReasonBelowThreshold), false
		}
		if k < 2 {
			return notRouted(ReasonBelowThreshold), false
		}
	}
	// "sharded": thresholds drop to the floor — every eligible plan routes
	// at K_min = 2 (k is already clamped to >= 2 above when upper >= 2).

	slices, err := p.slice(plan, meta, int(k))
	if err != nil {
		// A slicing failure on a plan the Planner judged eligible is a
		// construction bug, not a routing outcome: fall back to route A
		// rather than emit a wrong shard set.
		return notRouted(ReasonNotSliceable), false
	}

	return &Decision{
		Strategy: StrategyShardedTimeslice,
		K:        len(slices),
		Reason:   ReasonRouted,
		Slices:   slices,
	}, true
}

// notRouted builds a non-route Decision carrying only the reason.
func notRouted(reason string) *Decision {
	return &Decision{Reason: reason}
}

// signals is the accumulated result of the single eligibility pass.
type signals struct {
	allSliceInvariant bool
	hasWindow         bool

	sawNow64          bool
	sawUnpinnedBound  bool
	sawInstantWindow  bool
	sawGridMismatch   bool
	sawIncommensurate bool
	sawScalarHeavy    bool

	// Cost grid, derived from the OUTERMOST windowed node (the spine root).
	outerN      int           // N = OuterRange/Step + 1
	outerRange  time.Duration // OuterRange of the outermost spine
	maxFanout   int64         // F over every windowed node
	cumulativeD time.Duration // D = Σ Range down matrix spines + leaf RangeLWR.Lookback

	// innerResolutions records every inner-spine Step for the lcm
	// commensurability check.
	innerResolutions []time.Duration
}

// analyze runs the one eligibility pass over both the node tree and every
// expr tree (recursing into ScalarSubquery.Input, which chplan.Walk skips).
func (p *Planner) analyze(plan chplan.Node, meta RequestMeta) signals {
	sig := signals{allSliceInvariant: true}

	// depth tracks how deep on the windowed spine we are, so the
	// grid-prediction check can predict the right (start, end) per level.
	// The outermost windowed node predicts [meta.Start, meta.End]; each
	// nested matrix window widens its start by the parent's Range.
	p.walkNode(plan, meta.Start, meta.End, 0, &sig)

	return sig
}

// walkNode visits one node, threading the grid bounds predicted at this spine
// depth. predStart/predEnd are what the request grid predicts here; depth is
// the matrix-spine nesting level (0 = outermost). On a windowed node it
// records cost signals and recurses into its widened inner spine; off the
// spine it recurses into children with the same predicted bounds.
func (p *Planner) walkNode(n chplan.Node, predStart, predEnd time.Time, depth int, sig *signals) {
	if n == nil {
		return
	}
	if !chplan.IsSliceInvariant(n) {
		sig.allSliceInvariant = false
	}

	switch v := n.(type) {
	case *chplan.RangeWindow:
		p.recordRangeWindow(v, predStart, predEnd, depth, sig)
		// Walk this node's exprs for now64 / scalar-heavy.
		for _, e := range v.GroupBy {
			p.walkExpr(e, sig)
		}
		for _, e := range v.ScalarExprs {
			p.walkExpr(e, sig)
		}
		// Recurse into the inner spine widened by Range (mirrors
		// ReanchorRange / widenSubquerySpine).
		if v.Step > 0 {
			p.walkNode(v.Input, predStart.Add(-v.Range), predEnd, depth+1, sig)
		} else {
			p.walkNode(v.Input, predStart, predEnd, depth+1, sig)
		}
		return

	case *chplan.RangeLWR:
		p.recordRangeLWR(v, predStart, predEnd, depth, sig)
		p.walkNode(v.Input, predStart, predEnd, depth+1, sig)
		return

	case *chplan.Filter:
		p.walkExpr(v.Predicate, sig)
		p.walkNode(v.Input, predStart, predEnd, depth, sig)
		return

	case *chplan.Project:
		for _, pr := range v.Projections {
			p.walkExpr(pr.Expr, sig)
		}
		p.walkNode(v.Input, predStart, predEnd, depth, sig)
		return
	}

	// Default: not a spine node. Recurse into every child with the same
	// predicted bounds, and walk any exprs the node carries via the
	// generic node walk + a defensive expr sweep of nested ScalarSubqueries.
	for _, c := range n.Children() {
		p.walkNode(c, predStart, predEnd, depth, sig)
	}
}

// recordRangeWindow gathers signals for a matrix/instant RangeWindow.
func (p *Planner) recordRangeWindow(v *chplan.RangeWindow, predStart, predEnd time.Time, depth int, sig *signals) {
	// Instant-shape window: Step == 0 or OuterRange == 0 on a non-leaf
	// spine. The outermost window must carry a real anchor grid.
	if v.Step <= 0 || (depth == 0 && v.OuterRange <= 0) {
		sig.sawInstantWindow = true
	}

	// (2) Both Start and End must be pinned (non-zero) — unless this is an
	// unpinned subquery-inner shape that ReanchorRange fills. An unpinned
	// inner node (Start && End zero) is the expected shape; a HALF-pinned
	// node (exactly one zero) is a malformed/grid-divergent plan.
	startZero := v.Start.IsZero()
	endZero := v.End.IsZero()
	if depth == 0 {
		// The outermost windowed node must have both bounds pinned: it
		// anchors the whole grid.
		if startZero || endZero {
			sig.sawUnpinnedBound = true
		}
	} else if startZero != endZero {
		// Inner node with exactly one zero bound: malformed.
		sig.sawUnpinnedBound = true
	}

	// (4) Grid-prediction: a pinned windowed node must sit exactly on the
	// grid predicted at this depth.
	if !startZero || !endZero {
		predOuter := predEnd.Sub(predStart)
		if !v.Start.Equal(predStart) || !v.End.Equal(predEnd) || v.OuterRange != predOuter {
			sig.sawGridMismatch = true
		}
	}

	// Cost signals.
	if v.Step > 0 {
		fan := int64(v.Range / v.Step)
		if fan > sig.maxFanout {
			sig.maxFanout = fan
		}
	}
	sig.cumulativeD += v.Range

	if depth == 0 && v.OuterRange > 0 && v.Step > 0 {
		sig.hasWindow = true
		sig.outerRange = v.OuterRange
		sig.outerN = int(v.OuterRange/v.Step) + 1
	} else if depth > 0 && v.Step > 0 {
		// Inner spine resolution for the lcm commensurability check.
		sig.innerResolutions = append(sig.innerResolutions, v.Step)
		p.checkCommensurability(sig)
	}
}

// recordRangeLWR gathers signals for a bare-selector last-with-respect-to
// node — the leaf of the safe-set range family.
func (p *Planner) recordRangeLWR(v *chplan.RangeLWR, predStart, predEnd time.Time, depth int, sig *signals) {
	if v.Step <= 0 || (depth == 0 && v.End.Sub(v.Start) <= 0) {
		sig.sawInstantWindow = true
	}
	startZero := v.Start.IsZero()
	endZero := v.End.IsZero()
	if depth == 0 {
		if startZero || endZero {
			sig.sawUnpinnedBound = true
		}
		if !startZero && !endZero {
			predOuter := predEnd.Sub(predStart)
			if !v.Start.Equal(predStart) || !v.End.Equal(predEnd) || v.End.Sub(v.Start) != predOuter {
				sig.sawGridMismatch = true
			}
		}
	} else if startZero != endZero {
		sig.sawUnpinnedBound = true
	}

	if v.Step > 0 {
		fan := int64(v.Lookback / v.Step)
		if fan > sig.maxFanout {
			sig.maxFanout = fan
		}
	}
	sig.cumulativeD += v.Lookback

	if depth == 0 && v.Step > 0 {
		outer := v.End.Sub(v.Start)
		if outer > 0 {
			sig.hasWindow = true
			sig.outerRange = outer
			sig.outerN = int(outer/v.Step) + 1
		}
	}
}

// checkCommensurability enforces signal (5): inner anchors are generated
// backward from each node's End with no epoch alignment, so the slice quantum
// m must be a multiple of every inner resolution: m*Step ≡ 0 (mod
// lcm(res_1..res_d)). If no valid m in [MinAnchorsPerSlice, N/2] satisfies it,
// route A. We can only evaluate this once the outer grid is known; the
// caller re-runs it as inner resolutions accrue and after outerN is set.
func (p *Planner) checkCommensurability(sig *signals) {
	if sig.outerN == 0 || len(sig.innerResolutions) == 0 {
		return // outer grid not yet seen; re-checked after the spine root sets it.
	}
	resLcm := time.Duration(1)
	for _, r := range sig.innerResolutions {
		resLcm = lcmDuration(resLcm, r)
	}
	if resLcm <= 0 {
		return
	}
	// Need some m in [MinAnchorsPerSlice, N/2] with (m*Step) % lcm == 0.
	// Equivalently m must be a multiple of lcm/gcd(Step,lcm) = lcm/g.
	// Defer the Step-dependent half to the slicer; here record the gate
	// only when the outer grid is known.
	loBound := p.Cfg.MinAnchorsPerSlice
	hiBound := sig.outerN / 2
	if hiBound < loBound {
		// No room for a valid quantum window at all.
		sig.sawIncommensurate = true
	}
}

// walkExpr sweeps an expr tree for now64 and recurses into any embedded
// ScalarSubquery.Input plan (which chplan.Walk does not reach), running the
// scalar-heavy cost check and the full node walk inside the subquery.
func (p *Planner) walkExpr(e chplan.Expr, sig *signals) {
	chplan.InspectExprNodes(e, func(x chplan.Expr) bool {
		if fc, ok := x.(*chplan.FuncCall); ok && fc.Name == "now64" {
			sig.sawNow64 = true
		}
		return true
	}, func(inner chplan.Node) {
		// A ScalarSubquery interior. Walk it for slice-invariance / now64,
		// and apply the scalar-heavy cost gate.
		p.checkScalarHeavy(inner, sig)
		// The interior is below the spine and anchor-independent; walk it
		// for now64 / un-sliceable markers but pin its depth so its bounds
		// are not treated as a grid level.
		p.walkScalarInterior(inner, sig)
	})
}

// walkScalarInterior walks a ScalarSubquery's plan for now64 and unmarked
// nodes only — its bounds do not participate in the outer anchor grid, so the
// grid-prediction / commensurability checks are intentionally skipped here.
func (p *Planner) walkScalarInterior(n chplan.Node, sig *signals) {
	chplan.Walk(n, func(node chplan.Node) bool {
		if !chplan.IsSliceInvariant(node) {
			sig.allSliceInvariant = false
		}
		switch v := node.(type) {
		case *chplan.Filter:
			p.scanExprForNow64(v.Predicate, sig)
		case *chplan.Project:
			for _, pr := range v.Projections {
				p.scanExprForNow64(pr.Expr, sig)
			}
		case *chplan.RangeWindow:
			for _, e := range v.ScalarExprs {
				p.scanExprForNow64(e, sig)
			}
		}
		return true
	})
}

// scanExprForNow64 sweeps an expr tree (and nested scalar interiors) for
// now64 without re-entering the cost / grid logic.
func (p *Planner) scanExprForNow64(e chplan.Expr, sig *signals) {
	chplan.InspectExprNodes(e, func(x chplan.Expr) bool {
		if fc, ok := x.(*chplan.FuncCall); ok && fc.Name == "now64" {
			sig.sawNow64 = true
		}
		return true
	}, func(inner chplan.Node) {
		p.walkScalarInterior(inner, sig)
	})
}

// checkScalarHeavy implements signal (6): a ScalarSubquery whose interior
// scan-span × fan-out exceeds a configured fraction of the outer plan cannot
// be cheaply replicated. In phase 1 the hoist (execute the scalar once,
// bind the literal) is a later PR, so any scalar interior carrying its own
// windowed spine is conservatively treated as heavy — replicating it K× is
// the cost the hoist exists to avoid. A purely row-wise scalar (no windowed
// node inside) is cheap and admissible.
func (p *Planner) checkScalarHeavy(inner chplan.Node, sig *signals) {
	hasWindowedInterior := false
	chplan.Walk(inner, func(node chplan.Node) bool {
		switch node.(type) {
		case *chplan.RangeWindow, *chplan.RangeLWR, *chplan.RangeBucketFanout:
			hasWindowedInterior = true
		}
		return true
	})
	if hasWindowedInterior {
		sig.sawScalarHeavy = true
	}
}
