package solver

import (
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Planner is pure, read-only classification of a post-optimize plan into a
// routing Decision. It never mutates the plan: every check reads the tree,
// and slicing (the only path that copies) goes through ReanchorRange, which
// deep-copies.
//
// The two auto-mode cost thresholds (MinFanout, MinAnchorPairs) may be
// hot-swapped at runtime by the self-driving autotune loop via SetThresholds:
// the read in Plan goes through an atomic pointer, so a concurrent reload is
// seen atomically (never a torn pair) and the classification stays pure — no
// per-request mutable state, no RNG. A nil overlay (the default) reads the
// configured Cfg values, so a build with Autotune off is byte-identical.
type Planner struct {
	Cfg Config

	// tuned is the hot-reloadable (MinFanout, MinAnchorPairs) overlay set by
	// the autotune loop. nil means "use Cfg".
	tuned atomic.Pointer[tunedThresholds]
}

// tunedThresholds is the atomically-swapped auto-gate threshold pair.
type tunedThresholds struct {
	MinFanout      int
	MinAnchorPairs int
}

// thresholds returns the active auto-gate thresholds: the hot-reloaded overlay
// when present, else the configured Cfg values.
func (p *Planner) thresholds() (minFanout, minAnchorPairs int) {
	if t := p.tuned.Load(); t != nil {
		return t.MinFanout, t.MinAnchorPairs
	}
	return p.Cfg.MinFanout, p.Cfg.MinAnchorPairs
}

// SetThresholds atomically hot-swaps the auto-gate thresholds. Called only by
// the autotune loop after it certifies a candidate; safe to call concurrently
// with Plan.
func (p *Planner) SetThresholds(minFanout, minAnchorPairs int) {
	p.tuned.Store(&tunedThresholds{MinFanout: minFanout, MinAnchorPairs: minAnchorPairs})
}

// CurrentThresholds reports the active auto-gate thresholds for the autotune
// loop's baseline and for the shadow header / metric.
func (p *Planner) CurrentThresholds() (minFanout, minAnchorPairs int) {
	return p.thresholds()
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
	// (2)-prefix: instant queries are never time-slice routed.
	// Step == 0 means an instant evaluation; there is no anchor grid. The
	// analyze pass below derives the cost grid (N/F/D/OuterRange); it has not
	// run yet here, so the only meaningful scalar is Step (zero on an instant
	// query). costGrid threads whatever the empty signals carry plus meta.Step.
	if meta.Step <= 0 {
		return notRouted(ReasonInstant).withGrid(signals{}, meta), false
	}

	sig := p.analyze(plan, meta)

	// (1) Slice-invariance: any unmarked node anywhere → route A.
	if !sig.allSliceInvariant {
		return notRouted(ReasonNotSliceable).withGrid(sig, meta), false
	}
	// (1b) Routable-spine restriction: the routable spine families are the
	// *RangeWindow matrix family and the *RangeLWR bare-selector
	// last-with-respect-to family — ReanchorRange re-grids both. A
	// RangeBucketFanout / StepGrid spine bound-carrier is still left at
	// zero/stale bounds (ReanchorRange CloneNode's their grid verbatim), so
	// those fail closed to route A. Widening to those families extends
	// ReanchorRange + adds coverage.
	if sig.sawNonRangeWindowSpine {
		return notRouted(ReasonNotSliceable).withGrid(sig, meta), false
	}
	// (3) now64 anywhere (predicate / projection / ScalarSubquery.Input).
	if sig.sawNow64 {
		return notRouted(ReasonNow64).withGrid(sig, meta), false
	}
	// (2) Both Start and End pinned on every windowed node, and no
	// instant-shape windowed node (OuterRange == 0 / Step == 0).
	if sig.sawUnpinnedBound || sig.sawInstantWindow {
		return notRouted(ReasonInstant).withGrid(sig, meta), false
	}
	// (4) Grid-prediction check: a windowed node whose bounds diverge from
	// the grid the request predicts at its spine depth (an @-pinned anchor).
	if sig.sawGridMismatch {
		return notRouted(ReasonGridMismatch).withGrid(sig, meta), false
	}
	// (5) Grid commensurability for nested spines.
	if sig.sawIncommensurate {
		return notRouted(ReasonIncommensurate).withGrid(sig, meta), false
	}
	// (6) A ScalarSubquery too expensive to replicate K× (and that cannot be
	// classified safe by the cheap interior bound) → route A.
	if sig.sawScalarHeavy {
		return notRouted(ReasonScalarHeavy).withGrid(sig, meta), false
	}

	// The plan is ELIGIBLE. Compute the cost grid and the K clamp.
	if !sig.hasWindow {
		// Eligible but no windowed node carries an anchor grid to slice —
		// nothing to gain from slicing.
		return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
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
		return notRouted(ReasonHighD).withGrid(sig, meta), false
	}

	// "single" classifies but never routes.
	if p.Cfg.Mode == ModeSingle {
		return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
	}

	if p.Cfg.Mode == ModeAuto {
		// Cost thresholds gate the auto path. Below threshold → route A. Read
		// through the atomic overlay so a concurrent autotune reload is seen as
		// one consistent (MinFanout, MinAnchorPairs) pair.
		minFanout, minAnchorPairs := p.thresholds()
		if f < int64(minFanout) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if int64(n)*f < int64(minAnchorPairs) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if k < 2 {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
	}
	// "sharded": thresholds drop to the floor — every eligible plan routes
	// at K_min = 2 (k is already clamped to >= 2 above when upper >= 2).

	slices, err := p.slice(plan, meta, int(k))
	if err != nil {
		// A slicing failure on a plan the Planner judged eligible is a
		// construction bug, not a routing outcome: fall back to route A
		// rather than emit a wrong shard set.
		return notRouted(ReasonNotSliceable).withGrid(sig, meta), false
	}

	// The doc's invariant is "route iff K >= 2". The clamp above keeps the
	// requested K >= 2, but the singleton-tail merge inside slice() can
	// still collapse a tiny-N grid to a SINGLE produced slice — one shard
	// is route A with extra machinery, not a sharded route. Report the
	// ACTUAL produced slice count and only route when it is >= 2.
	if len(slices) < 2 {
		return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
	}

	return (&Decision{
		Strategy: StrategyShardedTimeslice,
		K:        len(slices),
		Reason:   ReasonRouted,
		Slices:   slices,
	}).withGrid(sig, meta), true
}

// notRouted builds a non-route Decision carrying only the reason. Callers
// chain .withGrid(sig, meta) to attach the cost-grid readout once analyze
// has run; the pre-analyze instant guard passes empty signals.
func notRouted(reason string) *Decision {
	return &Decision{Reason: reason}
}

// withGrid stamps the RAW classifier cost scalars (N/F/D/OuterRange/Step)
// onto the Decision from the eligibility-pass signals plus the request grid.
// It is a pure readout of values analyze already computed — it changes no
// routing behavior — and is applied to BOTH routed and not-routed decisions
// so the calibration corpus can compare route-A and route-B cost
// distributions at equal (N, F, D). Returns the receiver for chaining.
func (d *Decision) withGrid(sig signals, meta RequestMeta) *Decision {
	d.NAnchors = sig.outerN
	d.Fanout = sig.maxFanout
	d.CumulativeD = sig.cumulativeD
	d.OuterRange = sig.outerRange
	d.Step = meta.Step
	return d
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

	// sawNonRangeWindowSpine records a routed-spine grid bound-carrier whose
	// grid ReanchorRange does NOT re-anchor — a RangeBucketFanout or StepGrid,
	// which ReanchorRange CloneNode's verbatim (every shard would emit
	// stale bounds). The routable set is the *RangeWindow matrix family
	// plus the *RangeLWR bare-selector family: both are
	// re-gridded by ReanchorRange and zeroed/re-filled by unpinSpine, so
	// neither sets this flag. RangeBucketFanout / StepGrid fail closed to
	// route A until ReanchorRange learns their grids.
	sawNonRangeWindowSpine bool

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

	case *chplan.Aggregate:
		// Aggregate is slice-invariant AND the OUTERMOST node of the
		// dominant routed shape sum(rate(m[5m])). Its key/value exprs are
		// off the windowed spine, so a now64 hidden in a group key or an
		// aggregate argument would otherwise never reach walkExpr and the
		// plan would route despite two shards seeing different now64
		// wall-clocks. Sweep them explicitly before recursing.
		for _, e := range v.GroupBy {
			p.walkExpr(e, sig)
		}
		for _, fn := range v.AggFuncs {
			for _, e := range fn.Params {
				p.walkExpr(e, sig)
			}
			for _, e := range fn.Args {
				p.walkExpr(e, sig)
			}
		}
		p.walkNode(v.Input, predStart, predEnd, depth, sig)
		return

	case *chplan.RangeBucketFanout:
		// RangeBucketFanout is the array-aggregate sibling of RangeLWR and
		// is slice-invariant; like Aggregate it carries group keys + agg
		// args that the spine recursion never sweeps. Cover the same now64
		// gap. It also carries its own eval grid (Start/End/Step) that
		// ReanchorRange clones VERBATIM (never re-anchors), so every shard
		// would emit stale bounds — fail closed to route A.
		if v.Step > 0 {
			sig.sawNonRangeWindowSpine = true
		}
		for _, e := range v.GroupBy {
			p.walkExpr(e, sig)
		}
		for _, fn := range v.AggFuncs {
			for _, e := range fn.Params {
				p.walkExpr(e, sig)
			}
			for _, e := range fn.Args {
				p.walkExpr(e, sig)
			}
		}
		p.walkNode(v.Input, predStart, predEnd, depth, sig)
		return

	case *chplan.StepGrid:
		// StepGrid carries an eval grid (Start/End/Step) that ReanchorRange
		// clones VERBATIM — the grid-prediction guard cannot see it and the
		// slicer would leave every shard on the original full-grid bounds.
		// A StepGrid spine carrier is not in the routable set, so it
		// fails closed to route A.
		if v.Step > 0 {
			sig.sawNonRangeWindowSpine = true
		}
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
	// The RangeLWR matrix family is in the routable set: a
	// grid-carrying RangeLWR (Step > 0) is re-anchored by ReanchorRange
	// (its Start/End re-gridded, the input spine widened by the offset-aware
	// Offset+Lookback membership window) and zeroed/re-filled by the slicer's
	// unpinSpine, so it does not set sawNonRangeWindowSpine. The deriv /
	// idelta / irate / instant-LWR / negative-offset families that lower to a
	// bare RangeLWR spine route B. RangeBucketFanout / StepGrid stay
	// rejected — their grids are CloneNode'd verbatim by ReanchorRange.
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
		case *chplan.Aggregate:
			// scalar(sum(... now64 ...)) — the interior Aggregate's group
			// keys + agg args must be swept too, else a now64 inside a
			// ScalarSubquery-interior aggregate escapes the now64 gate.
			for _, e := range v.GroupBy {
				p.scanExprForNow64(e, sig)
			}
			for _, fn := range v.AggFuncs {
				for _, e := range fn.Params {
					p.scanExprForNow64(e, sig)
				}
				for _, e := range fn.Args {
					p.scanExprForNow64(e, sig)
				}
			}
		case *chplan.RangeBucketFanout:
			for _, e := range v.GroupBy {
				p.scanExprForNow64(e, sig)
			}
			for _, fn := range v.AggFuncs {
				for _, e := range fn.Params {
					p.scanExprForNow64(e, sig)
				}
				for _, e := range fn.Args {
					p.scanExprForNow64(e, sig)
				}
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
// be cheaply replicated. The scalar is not hoisted (executed once with the
// literal bound), so any scalar interior carrying its own windowed spine is
// conservatively treated as heavy — replicating it K× is the cost a hoist
// would avoid. A purely row-wise scalar (no windowed
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
