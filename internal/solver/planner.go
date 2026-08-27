package solver

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Planner is pure, read-only classification of a post-optimize plan into a
// routing Decision. It never mutates the plan: every check reads the tree,
// and slicing (the only path that copies) goes through ReanchorRange, which
// deep-copies.
//
// The auto-mode cost thresholds (MinFanout, MinAnchorPairs) are static
// configuration read straight off Cfg, so classification is a pure function of
// (plan, meta, Cfg) — no per-request mutable state, no RNG, and identical
// across every replica running the same configuration.
type Planner struct {
	Cfg Config
}

// CurrentThresholds reports the configured auto-gate thresholds for the shadow
// header / metric.
func (p *Planner) CurrentThresholds() (minFanout, minAnchorPairs int) {
	return p.Cfg.MinFanout, p.Cfg.MinAnchorPairs
}

// Plan classifies plan against meta and returns the Decision plus whether the
// plan routes B. The Decision is always non-nil: even a non-route carries the
// Reason for the shadow header.
//
// Routing follows docs/solver.md §"Eligibility signals": a single pass walks
// both the node tree and every expression tree (including
// ScalarSubquery.Input, which chplan.Walk does NOT recurse into) gathering the
// eligibility signals, then the cost thresholds and the K clamp decide. Mode
// shapes the final gate:
//
//   - "single": classify but NEVER route — always returns (decision, false).
//   - "sharded": thresholds drop to the floor (K_min = 2) so every ELIGIBLE
//     plan routes; ineligible plans still stay on route A.
//   - "auto": route iff eligible AND F >= MinFanout AND N x F >= MinAnchorPairs
//     AND K >= 2.
func (p *Planner) Plan(plan chplan.Node, meta RequestMeta) (*Decision, bool) {
	sig, decision, k, eligible := p.classify(plan, meta)
	if !eligible {
		return decision, false
	}

	// "single" classifies but never routes. The reason is routing-disabled,
	// NOT below-threshold: no threshold was consulted, so this row is not
	// evidence about where the threshold sits and must not join a population
	// whose advice is "move the threshold".
	if p.Cfg.Mode == ModeSingle {
		return notRouted(ReasonRoutingDisabled).withGrid(sig, meta), false
	}

	if p.Cfg.Mode == ModeAuto {
		// Cost thresholds gate the auto path. Below threshold → route A.
		minFanout, minAnchorPairs := p.Cfg.MinFanout, p.Cfg.MinAnchorPairs
		// A per-rung carrier's F is unmeasurable before the scan runs (see
		// carrierGeometry.perRungIntermediate), so both F-derived comparisons
		// below would be reading a placeholder rather than a cost. Gate it on
		// the anchor axis instead — the axis that IS known at plan time, and
		// the one slicing actually divides.
		perRung := sig.sawPerRungCarrier && sig.outerN >= minAnchorsForPerRungShard
		if !perRung && sig.maxFanout < int64(minFanout) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if !perRung && int64(sig.outerN)*sig.maxFanout < int64(minAnchorPairs) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if k < 2 {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		// Last, so a plan that was already below threshold keeps THAT reason
		// and the shipped route-A analyzer's population does not shift
		// underneath it: only plans that would genuinely have routed change
		// verdict here.
		if sig.sawIndivisibleAnchorGrid {
			return notRouted(ReasonAnchorGridIndivisible).withGrid(sig, meta), false
		}
	}
	// "sharded": thresholds drop to the floor — every eligible plan routes
	// at K_min = 2 (k is already clamped to >= 2 by classify when upper >= 2).

	return p.sliceAndDecide(plan, meta, sig, k)
}

// Eligible reports whether plan is STRUCTURALLY eligible to route B —
// independent of Cfg.Mode's PREDICTIVE cost thresholds (MinFanout,
// MinAnchorPairs): every correctness gate below passes, a windowed anchor
// grid exists, and the high-D floor leaves K >= 2. On success it slices and
// returns a fully routed Decision, exactly as Plan would under ModeSharded's
// floor semantics (K clamped to >= 2, no fanout/anchor-pair gate).
//
// This is the re-derivation the failure-driven route memo calls at every
// non-baseline dispatch site (probe, retry, memo-hit): Cfg.Mode's auto-mode
// thresholds are a PROXY for cost, and a real route-A resource failure is
// stronger evidence than that proxy — re-applying the proxy on top of
// empirical evidence would defeat the reason the memo mechanism exists. The
// structural / correctness gates are NOT bypassed here: those protect
// answer correctness, not cost policy, and every one of them still applies
// unconditionally, via the same classify() every Plan() call already runs.
//
// Eligible respects ModeSingle: an operator who has explicitly disabled the
// solver has said route B never runs, full stop, and the memo mechanism —
// itself a form of route-B usage — honours that rather than routing around
// it as a side channel. It does NOT gate on ModeAuto's thresholds
// (MinFanout, MinAnchorPairs) at all — those stay load-bearing only for
// Plan's own ModeAuto branch above.
//
// Eligible answers ONLY "can this plan be sliced and answered identically
// via route B". It makes no promise about breaker state, admission budget,
// live-edge freshness, or corroboration — the caller applies its own gates
// for those before dispatching.
func (p *Planner) Eligible(plan chplan.Node, meta RequestMeta) (*Decision, bool) {
	// classify runs before the mode gate so this seam and Plan's cannot
	// disagree about the same plan's geometry. Classification is a pure
	// signal-gathering walk with no side effects, so running it on a
	// deployment that will refuse anyway costs one tree walk and buys a
	// corpus row that is honest about what was refused.
	sig, decision, k, eligible := p.classify(plan, meta)
	if !eligible {
		return decision, false
	}
	if p.Cfg.Mode == ModeSingle {
		return notRouted(ReasonRoutingDisabled).withGrid(sig, meta), false
	}
	return p.sliceAndDecide(plan, meta, sig, k)
}

// sliceAndDecide is the shared tail of Plan and Eligible once a mode-specific
// gate (or none, for Eligible) has cleared k >= 2: slice the plan at k shards
// and build the routed Decision, or fall back to a non-route Decision if
// slicing fails or collapses to a single produced slice.
func (p *Planner) sliceAndDecide(plan chplan.Node, meta RequestMeta, sig signals, k int64) (*Decision, bool) {
	slices, err := p.slice(plan, meta, int(k))
	if err != nil {
		// A slicing failure on a plan the Planner judged eligible is a
		// construction bug, not a routing outcome: fall back to route A
		// rather than emit a wrong shard set.
		return notRouted(ReasonNotSliceable).withGrid(sig, meta), false
	}

	// The doc's invariant is "route iff K >= 2". The clamp upstream keeps the
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

// classify runs every structural/correctness gate plus the cost-grid and the
// K clamp, independent of Cfg.Mode. It returns the eligibility signals, the
// clamped K, and whether the plan is STRUCTURALLY eligible to route: every
// gate passed, a windowed anchor grid exists, and the high-D floor left
// K >= 2.
//
// When eligible is false, decision is the fully-built non-route Decision the
// caller should return verbatim (carrying the Reason for the shadow header).
// When eligible is true, decision is nil — sig and k carry everything the
// caller needs to apply its OWN mode-specific or evidence-specific gate
// before slicing (Plan's ModeAuto threshold check; Eligible's unconditional
// floor). This is the single re-derivation both Plan and Eligible run: a
// plan's eligibility is a pure function of (plan, meta, Cfg.MaxK,
// Cfg.MinAnchorsPerSlice) — no caller trusts a Decision computed at an
// earlier point in time or against a different plan.
func (p *Planner) classify(plan chplan.Node, meta RequestMeta) (sig signals, decision *Decision, k int64, eligible bool) {
	// analyze runs FIRST, before any gate, so every Decision this classifier
	// returns carries the plan's real cost grid — including the ones that
	// refuse. The calibration corpus exists to replay routing decisions
	// offline, and a refusal recorded with a zero grid is unreplayable: it
	// says "we declined" without saying what we declined. analyze is a pure
	// signal-gathering walk (it makes no routing decision and mutates no
	// state), so hoisting it above the gates changes no outcome.
	sig = p.analyze(plan, meta)

	// (2)-prefix: instant queries are never time-slice routed. Step == 0 means
	// an instant evaluation: the anchor grid is a single point (N = 1), and
	// route B's only decomposition is anchor-grid partitioning (slicer.go),
	// which needs N >= 2 to produce more than one shard. The refusal is
	// definitional, so it is checked before the correctness gates.
	//
	// The grid this stamps is load-bearing for the corpus in two ways. It
	// records the spine lookback (cumulativeD) that decides whether an instant
	// query is heavy enough to be worth a different decomposition at all — a
	// question the zero grid could not even be asked. And it makes GridOf's
	// silent-miss failure mode (solver.go: a grid carrier not found leaves
	// step == 0, so a RANGE query is classified instant) visible in the
	// corpus: a genuine instant query records no anchor grid because its plan
	// carries none, whereas a missed carrier records a non-zero N/F/OuterRange
	// from the plan itself NEXT TO a zero Step.
	//
	// Both conjuncts are needed. reason=instant is also emitted further down
	// (the sawUnpinnedBound / sawInstantWindow gate), and that one fires on a
	// genuine RANGE request — non-zero Step, real grid — so reason=instant
	// beside a populated grid is ordinary, not a missed carrier. It is
	// reason=instant beside a ZERO Step and a non-zero grid that cannot happen
	// any other way.
	if meta.Step <= 0 {
		return sig, notRouted(ReasonInstant).withGrid(sig, meta), 0, false
	}

	// (0) Extraction: the request declares a real grid (meta.Step > 0 by the
	// guard above), so the plan must own at least one carrier the walk could
	// measure. When it does not, the cost grid stamped below is all zeros
	// because NOTHING WAS MEASURED — and every other refusal token would then
	// be a claim about numbers that were never read. Recording that separately
	// is the whole point: an unmeasured population averaged in with the
	// genuinely-cheap one silently drags every calibration threshold toward
	// zero. A carrier kind chplan grew ahead of carrierGeometryOf lands here
	// too — its cost is unknown, not zero.
	//
	// Route-neutral by construction: both this and the gates below refuse, so
	// the gate changes the recorded REASON and never the route.
	if !sig.sawGridCarrier {
		return sig, notRouted(ReasonExtractionFailed).withGrid(sig, meta), 0, false
	}

	// (1) Slice-invariance: any unmarked node anywhere → route A.
	if !sig.allSliceInvariant {
		return sig, notRouted(ReasonNotSliceable).withGrid(sig, meta), 0, false
	}
	// (1b) Routable-spine restriction: the routable spine families are the
	// RangeWindow matrix family, the RangeWindowGridNative timeSeries*ToGrid family,
	// the RangeLWR bare-selector last-with-respect-to family, and the
	// RangeBucketFanout array-aggregate fan-out behind the classic-histogram
	// families — ReanchorRange re-grids all four. Every other
	// [chplan.GridCarrier] kind (RangeWindowStaleResample, StepGrid,
	// AbsentOverTime) has its grid CloneNode'd verbatim, so each shard would
	// emit the original full-grid bounds; they fail closed to route A. Note
	// the asymmetry this gate exists to preserve: the extractor MEASURES all
	// seven kinds so the corpus can see what a refused plan costs, while only
	// the re-anchored families may be SLICED. Admitting a kind here means
	// teaching ReanchorRange (and the slicer's UnpinSpine) its grid first, not
	// widening carrierGeometryOf.
	if sig.sawNonRangeWindowSpine {
		return sig, notRouted(ReasonNotSliceable).withGrid(sig, meta), 0, false
	}
	// (3) now64 anywhere (predicate / projection / ScalarSubquery.Input).
	if sig.sawNow64 {
		return sig, notRouted(ReasonNow64).withGrid(sig, meta), 0, false
	}
	// (3b) An instant-mode (!StepAligned) VectorJoin. The slice-invariance
	// registry admits VectorJoin by node kind, so it ALSO admits the
	// instant-mode shape — whose emitter synthesizes the join-side timestamp
	// with now64(9) (internal/chsql/vector_join.go joinTimestampFrag), a
	// wall-clock the plan-level now64 scanner never sees (it is minted in the
	// SQL, not carried as a chplan FuncCall). Only the StepAligned (range-mode)
	// join step-aligns on the real per-anchor TimestampColumn and is safe to
	// slice; the instant shape fails closed to route A. The Step<=0 gate above
	// already rejects instant *queries*, but a !StepAligned join can appear
	// under a range-mode request, so this guard is both the load-bearing
	// fail-close AND honest telemetry.
	if sig.sawInstantVectorJoin {
		return sig, notRouted(ReasonInstantJoin).withGrid(sig, meta), 0, false
	}
	// (2) Both Start and End pinned on every windowed node, and no
	// instant-shape windowed node (OuterRange == 0 / Step == 0).
	if sig.sawUnpinnedBound || sig.sawInstantWindow {
		return sig, notRouted(ReasonInstant).withGrid(sig, meta), 0, false
	}
	// (4) Grid-prediction check: a windowed node whose bounds diverge from
	// the grid the request predicts at its spine depth (an @-pinned anchor).
	if sig.sawGridMismatch {
		return sig, notRouted(ReasonGridMismatch).withGrid(sig, meta), 0, false
	}
	// (7) A ScalarSubquery too expensive to replicate K× (and that cannot be
	// classified safe by the cheap interior bound) → route A.
	if sig.sawScalarHeavy {
		return sig, notRouted(ReasonScalarHeavy).withGrid(sig, meta), 0, false
	}

	// The plan is ELIGIBLE. Compute the cost grid and the K clamp.
	if !sig.hasWindow {
		// Eligible but no windowed node carries an anchor grid to slice —
		// nothing to gain from slicing.
		return sig, notRouted(ReasonBelowThreshold).withGrid(sig, meta), 0, false
	}

	n := sig.outerN              // N = OuterRange/Step + 1
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
	kk := int64(n / p.Cfg.MinAnchorsPerSlice)
	if kk < lower {
		kk = lower
	}
	if kk > upper {
		kk = upper
	}

	// If the high-D clamp ceiling fell below 2 there is no valid K — the
	// documented high-D floor.
	if upper < 2 {
		return sig, notRouted(ReasonHighD).withGrid(sig, meta), 0, false
	}
	// (6) An end-phased nested grid is generated backward from its own End,
	// so the slice quantum must preserve its phase — checked against the
	// same ceil(N/K) quantum the slicer will emit below.
	if !sliceQuantumCommensurate(sig, step, int(kk)) {
		return sig, notRouted(ReasonIncommensurate).withGrid(sig, meta), 0, false
	}

	return sig, nil, kk, true
}

// notRouted builds a non-route Decision carrying only the reason. Callers
// chain .withGrid(sig, meta) to attach the cost-grid readout. Every refusal in
// this package is raised after classify has run, so none of them reaches the
// corpus with an unexplained all-zero grid — a refused row is still a
// calibration datapoint, and a zero grid would otherwise be indistinguishable
// from a genuinely trivial query. The one refusal whose grid IS legitimately
// all zeros carries ReasonExtractionFailed, which says so in the row itself.
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

	sawNow64         bool
	sawUnpinnedBound bool
	sawInstantWindow bool
	sawGridMismatch  bool
	sawScalarHeavy   bool

	// sawInstantVectorJoin records a StepAligned==false VectorJoin — an
	// instant-mode vector-vector join. Its emitter synthesizes the join-side
	// timestamp with now64(9) (a per-shard-divergent wall-clock invisible to
	// the plan-level now64 scanner), so it must stay on route A even though the
	// VectorJoin node kind is registered slice-invariant. The StepAligned
	// (range-mode) join, which step-aligns on the real per-anchor
	// TimestampColumn, does not set this flag and remains routable.
	sawInstantVectorJoin bool

	// sawNonRangeWindowSpine records a grid bound-carrier whose grid
	// ReanchorRange does NOT re-anchor — every [chplan.GridCarrier] kind
	// outside the routable set, which ReanchorRange CloneNode's verbatim (so
	// every shard would emit stale bounds). The routable set is the
	// RangeWindow matrix family, the RangeWindowGridNative timeSeries*ToGrid family
	// and the RangeLWR bare-selector family: all three are re-gridded by
	// ReanchorRange and zeroed/re-filled by UnpinSpine, so none of them sets
	// this flag. RangeWindowStaleResample / RangeBucketFanout / StepGrid /
	// AbsentOverTime fail closed to route A until ReanchorRange learns their
	// grids — see carrierGeometry.reanchorable, which is the single place that
	// decision is recorded.
	//
	// The name is historical: the routable set was once the RangeWindow family
	// alone. It reads "saw a carrier outside the re-anchorable set", which is
	// what carrierGeometry.reanchorable answers.
	sawNonRangeWindowSpine bool

	// sawGridCarrier records that the walk found at least one
	// [chplan.GridCarrier] whose geometry carrierGeometryOf could derive. When
	// it stays false the cost grid on the resulting Decision is all zeros
	// because NOTHING WAS MEASURED, not because the plan is trivial — the
	// distinction ReasonExtractionFailed exists to make. Without it the corpus
	// cannot tell "looked at the plan and declined" from "extracted nothing",
	// and a carrier kind added to chplan ahead of carrierGeometryOf would
	// silently deflate every threshold calibrated off those rows.
	sawGridCarrier bool

	// sawIndivisibleAnchorGrid records that some routable carrier on this
	// plan's spine answers false to chplan.GridCarrier.AnchorGridDivides —
	// its peak intermediate does NOT shrink when the anchor grid is cut.
	// ModeAuto's thresholds are a PROXY for cost that reads F = lookback/Step
	// as a divisor; for such a carrier F is a redundancy multiplier instead,
	// so the proxy's own sign is inverted and clearing it is evidence AGAINST
	// routing. Recorded here and consulted only in Plan's ModeAuto branch —
	// Eligible() deliberately ignores it, so a real route-A resource failure
	// still routes through the failure-driven memo.
	sawIndivisibleAnchorGrid bool
	// sawPerRungCarrier records that some carrier amplifies per stored bucket
	// rung, so maxFanout is not a usable cost proxy for this plan — see
	// carrierGeometry.perRungIntermediate and minAnchorsForPerRungShard.
	sawPerRungCarrier bool

	// Cost grid, derived from the OUTERMOST windowed node (the spine root).
	outerN      int           // N = OuterRange/Step + 1
	outerRange  time.Duration // OuterRange of the outermost spine
	maxFanout   int64         // F over every windowed node
	cumulativeD time.Duration // D = Σ per-anchor lookback over every grid carrier

	// endPhasedResolutions records the step of every nested spine whose
	// anchor PHASE is tied to the node's own End — the grids the selected
	// slice quantum has to preserve. An epoch-aligned nested grid
	// (RangeWindow.StepAlign, the PromQL subquery inner-sample grid) is
	// phase-0 by construction and so invariant under any shift of End; it
	// contributes nothing here. See sliceQuantumCommensurate.
	endPhasedResolutions []time.Duration
}

// analyze runs the one eligibility pass over both the node tree and every
// expr tree (recursing into ScalarSubquery.Input, which chplan.Walk skips).
func (p *Planner) analyze(plan chplan.Node, meta RequestMeta) signals {
	sig := signals{allSliceInvariant: true}

	// depth tracks how deep on the windowed spine we are, so the
	// grid-prediction check can predict the right (start, end) per level.
	// The outermost windowed node predicts [meta.Start, meta.End]; each
	// nested matrix window widens its start by the parent's Range.
	p.walkNode(plan, meta.Start, meta.End, meta.Step, 0, &sig)

	return sig
}

// walkNode visits one node, threading the grid bounds predicted at this spine
// depth. predStart/predEnd are what the request grid predicts here; predStep
// is the evaluation cadence a windowed node embedded HERE would have to match
// to be judged anchor-compatible by checkScalarHeavy — a windowed node's own
// Step once one is found on the way down, the request Step everywhere above
// the first one; depth is the matrix-spine nesting level (0 = outermost). On
// a windowed node it records cost signals and recurses into its widened
// inner spine; off the spine it recurses into children with the same
// predicted bounds.
func (p *Planner) walkNode(n chplan.Node, predStart, predEnd time.Time, predStep time.Duration, depth int, sig *signals) {
	if n == nil {
		return
	}
	if !chplan.IsSliceInvariant(n) {
		sig.allSliceInvariant = false
	}

	switch v := n.(type) {
	case *chplan.RangeWindow:
		p.recordGridCarrier(v, depth, sig)
		p.checkRangeWindowGrid(v, predStart, predEnd, depth, sig)
		// Walk this node's exprs for now64 / scalar-heavy. GroupBy /
		// ScalarExprs are evaluated at THIS window's own level and cadence,
		// so any embedded ScalarSubquery / InSubquery is checked against
		// THIS (predStart, predEnd, v.Step) — not the widened inner-spine
		// bounds v.Input recurses into below, and not a step inherited from
		// further up (a nested subquery resolution can differ from it).
		for _, e := range v.GroupBy {
			p.walkExpr(e, predStart, predEnd, v.Step, sig)
		}
		for _, e := range v.ScalarExprs {
			p.walkExpr(e, predStart, predEnd, v.Step, sig)
		}
		// Recurse into the inner spine widened via the single shared owner
		// of this arithmetic (mirrors ReanchorRange / widenSubquerySpine) so
		// the grid this predicts for the child matches what re-anchoring
		// actually produces — including the Offset term (#1464).
		inStart, inEnd := v.InputWindow(predStart, predEnd)
		p.walkNode(v.Input, inStart, inEnd, v.Step, depth+1, sig)
		return

	case *chplan.RangeLWR:
		p.recordGridCarrier(v, depth, sig)
		p.checkRangeLWRGrid(v, predStart, predEnd, depth, sig)
		p.walkNode(v.Input, predStart, predEnd, v.Step, depth+1, sig)
		return

	case *chplan.RangeWindowGridNative:
		p.walkRangeWindowGridNative(v, predStart, predEnd, depth, sig)
		return

	case *chplan.RangeWindowStaleResample:
		// The native staleness-resample lowering of a bare selector at each
		// anchor. Same story as RangeWindowGridNative, with Lookback (the staleness
		// horizon) as the per-anchor window depth. Its inner spine is walked at
		// the unwidened bounds, mirroring the RangeLWR arm it is the native
		// sibling of.
		p.recordGridCarrier(v, depth, sig)
		p.walkNode(v.Input, predStart, predEnd, v.Step, depth+1, sig)
		return

	case *chplan.AbsentOverTime:
		// absent_over_time's own grid. It carries no group-key or agg-arg
		// exprs (only SynthLabels, which are literal label pairs), so the
		// default child recursion below the record call is the whole sweep.
		// Its per-anchor membership window is `(anchor - Offset - Range,
		// anchor - Offset]` (see chplan.AbsentOverTime.Offset), so the inner
		// spine widens by Offset+Range exactly as the RangeWindow arm does
		// (#1464). The Input of an `absent_over_time(<sub>)` lowering is a
		// Project over the subquery's OWN RangeWindow grid, so this walk
		// really does reach a carrier whose predicted bounds it decides.
		p.recordGridCarrier(v, depth, sig)
		p.walkNode(v.Input, predStart.Add(-v.Offset-v.Range), predEnd, v.Step, depth+1, sig)
		return

	case *chplan.Aggregate:
		// Aggregate is slice-invariant AND the OUTERMOST node of the
		// dominant routed shape sum(rate(m[5m])). Its key/value exprs are
		// off the windowed spine, so a now64 hidden in a group key or an
		// aggregate argument would otherwise never reach walkExpr and the
		// plan would route despite two shards seeing different now64
		// wall-clocks. Sweep them explicitly before recursing.
		for _, e := range v.GroupBy {
			p.walkExpr(e, predStart, predEnd, predStep, sig)
		}
		for _, fn := range v.AggFuncs {
			for _, e := range fn.Params {
				p.walkExpr(e, predStart, predEnd, predStep, sig)
			}
			for _, e := range fn.Args {
				p.walkExpr(e, predStart, predEnd, predStep, sig)
			}
		}
		p.walkNode(v.Input, predStart, predEnd, predStep, depth, sig)
		return

	case *chplan.RangeBucketFanout:
		p.walkRangeBucketFanout(v, predStart, predEnd, depth, sig)
		return

	case *chplan.RangeBucketGridNative:
		p.walkRangeBucketGridNative(v, predStart, predEnd, depth, sig)
		return

	case *chplan.HistogramQuantile:
		// The classic-histogram quantile interpolation: off-grid immutable
		// itself (see chplan.reanchor's *HistogramQuantile arm — a
		// pass-through mirroring *Project), so it carries no own grid to
		// record or widen by. But GroupBy and PhiExpr are exactly the
		// unswept-slot hazard walkScalarInterior's own doc already names by
		// example ("a histogram_quantile's phi... escaped the gate and
		// routed B") — see walkHistogramQuantile (factored out to keep
		// walkNode under funlen's statement cap).
		p.walkHistogramQuantile(v, predStart, predEnd, predStep, depth, sig)
		return

	case *chplan.StepGrid:
		// StepGrid carries an eval grid (Start/End/Step) that ReanchorRange
		// clones VERBATIM — the grid-prediction guard cannot see it and the
		// slicer would leave every shard on the original full-grid bounds.
		// A StepGrid spine carrier is not in the routable set, so it fails
		// closed to route A. It is a leaf (no Children), and it reads no
		// samples, so it contributes an anchor count and no lookback.
		p.recordGridCarrier(v, depth, sig)
		return

	case *chplan.Filter:
		p.walkExpr(v.Predicate, predStart, predEnd, predStep, sig)
		p.walkNode(v.Input, predStart, predEnd, predStep, depth, sig)
		return

	case *chplan.Project:
		for _, pr := range v.Projections {
			p.walkExpr(pr.Expr, predStart, predEnd, predStep, sig)
		}
		p.walkNode(v.Input, predStart, predEnd, predStep, depth, sig)
		return

	case *chplan.VectorJoin:
		// A vector-vector join is registered slice-invariant, but only the
		// StepAligned (range-mode) shape is actually safe to slice: the emitter
		// step-aligns on the real per-anchor TimestampColumn, so each
		// (match-key, anchor) pair joins independently and the many-to-one
		// dedup (throwIf(uniqExact>1)) + Include mapConcat are per-anchor. The
		// instant-mode (!StepAligned) shape synthesizes the join-side timestamp
		// with now64(9) in SQL (invisible to walkExpr's now64 scan), so it must
		// fail closed to route A — record the signal the Plan() gate reads.
		if !v.StepAligned {
			sig.sawInstantVectorJoin = true
		}
		// The join carries no own grid and no lookback: BOTH arms are
		// independent windowed spines evaluating over the SAME [predStart,
		// predEnd] at this depth (no widening at the join level — each arm's
		// own RangeWindow / RangeLWR widens its inner scan). Recurse both.
		p.walkNode(v.Left, predStart, predEnd, predStep, depth, sig)
		p.walkNode(v.Right, predStart, predEnd, predStep, depth, sig)
		return
	}

	// Default: no dedicated arm above. Extraction is keyed on the
	// [chplan.GridCarrier] INTERFACE, not on the arms of the switch, so a
	// carrier kind that grows a grid before this walk grows an arm for it is
	// still measured (and, if carrierGeometryOf does not know it, still fails
	// closed) instead of contributing a silent all-zero feature vector. The
	// arms above exist for the per-kind expr sweeps and recursion shapes;
	// each returns, so nothing is recorded twice.
	if gc, ok := n.(chplan.GridCarrier); ok {
		p.recordGridCarrier(gc, depth, sig)
	}

	// Recurse into every child with the same predicted bounds, and walk any
	// exprs the node carries via the generic node walk + a defensive expr
	// sweep of nested ScalarSubqueries.
	for _, c := range n.Children() {
		p.walkNode(c, predStart, predEnd, predStep, depth, sig)
	}
}

// walkRangeWindowGridNative sweeps the ClickHouse-native `timeSeries<fn>ToGrid`
// lowering of rate(m[Range]) and its siblings. The node owns a full eval grid,
// so on a purely natively-lowered plan it is the ONLY carrier and the sole
// source of that plan's n_anchors / outer_range / cumulative_d.
//
// Its GroupBy and Recollapse exprs are evaluated at THIS node's own level and
// cadence, so an embedded ScalarSubquery / InSubquery is checked against
// (predStart, predEnd, v.Step) rather than against the widened inner-spine
// bounds the recursion below descends into — the same rule the fan-out
// RangeWindow arm applies to its own exprs.
func (p *Planner) walkRangeWindowGridNative(v *chplan.RangeWindowGridNative, predStart, predEnd time.Time, depth int, sig *signals) {
	p.recordGridCarrier(v, depth, sig)
	p.checkRangeWindowGridNativeGrid(v, predStart, predEnd, depth, sig)
	for _, e := range v.GroupBy {
		p.walkExpr(e, predStart, predEnd, v.Step, sig)
	}
	for _, pr := range v.Recollapse {
		p.walkExpr(pr.Expr, predStart, predEnd, v.Step, sig)
	}
	// Recurse into the inner spine widened via the single shared owner of this
	// arithmetic (mirrors chplan.ReanchorRange) so the grid this predicts for
	// the child matches what re-anchoring actually produces — including the
	// Offset term (#1464).
	inStart, inEnd := v.InputWindow(predStart, predEnd)
	p.walkNode(v.Input, inStart, inEnd, v.Step, depth+1, sig)
}

// carrierGeometry is the measurement-only projection of one
// [chplan.GridCarrier]: the two spans every cost-grid feature (and therefore
// every calibration-corpus row) is derived from, plus the single correctness
// bit that says whether the slicer may re-grid the carrier at all.
//
// It exists so extraction is keyed on the GridCarrier INTERFACE rather than
// on a hand-maintained list of concrete node kinds. A carrier the extractor
// cannot see contributes nothing, and a Decision whose features are all zero
// is indistinguishable from a plan that genuinely has no grid — so a missed
// kind silently mislabels its whole population in the corpus rather than
// raising anything.
type carrierGeometry struct {
	// outerRange is the span of the carrier's OWN anchor grid: first anchor
	// to last. With the carrier's Step it yields n_anchors.
	outerRange time.Duration

	// lookback is how far back a SINGLE anchor reads — the [range] of a
	// range-vector carrier, the staleness horizon of a resample / LWR
	// carrier, zero for a carrier that emits a bare anchor axis and reads no
	// samples. It is what the corpus records as cumulative_d, and with Step
	// it yields the fan-out.
	//
	// It is the window's SPAN, and deliberately EXCLUDES the carrier's
	// `Offset`, even though every widening pass reaches back Offset+Range
	// (chplan.RangeWindow.InputWindow). Issue #1732 proposed folding Offset
	// in here for consistency with that arithmetic; the two answer different
	// questions and conflating them is wrong in both features this feeds:
	//
	//   - F = lookback/Step is how many (sample, anchor) pairs the carrier
	//     MATERIALISES per sample. Shifting a window does not change how many
	//     samples it holds, and F gates routing under ModeAuto, so folding
	//     Offset in would route plans on a sample count nobody reads. (For a
	//     singlePass carrier this derivation does not apply at all — see that
	//     field.)
	//   - D = Σ lookback drives the high-D floor, which measures per-slice
	//     REDUNDANCY: a slice narrower than D re-reads more than it computes,
	//     because ADJACENT slices' input windows overlap by exactly the
	//     window span. An `offset` shifts every slice's window back by the
	//     same amount, so adjacent slices overlap no more than they did at
	//     offset 0 and the redundancy is unchanged.
	//
	// TestPlan_RangeLWRSpineRoutes/positive_offset is the live pin on the
	// second point: a 1h-offset spine over a 1h grid still routes, which
	// folding Offset into D refuses as high-D despite costing no extra bytes.
	// TestCarrierGeometry_OffsetChangesNeitherFanoutNorD pins it for every
	// offset-bearing carrier kind at once.
	lookback time.Duration

	// singlePass reports that the carrier's emitter reduces each raw sample
	// EXACTLY ONCE, in one streaming aggregate pass, and never materialises the
	// (sample, anchor) matrix. It is the native timeSeries*ToGrid family: the
	// aggregate is handed (start, end, step, window) and fills a per-series
	// grid array in a single C++ walk of the samples.
	//
	// It exists because `lookback` answers TWO questions that coincide for the
	// fan-out family and diverge for the native one:
	//
	//   - D (Σ lookback) is per-slice SCAN redundancy: adjacent slices' input
	//     windows overlap by exactly the window span, whichever emitter reads
	//     them. A native carrier's shard widens its scan by Offset+Range
	//     exactly as the fan-out arm's does (chplan.RangeWindowGridNative.
	//     InputWindow), so its D is its Range and this field does NOT touch it.
	//   - F is the MEMORY proxy the ModeAuto gate reads: peak intermediate rows
	//     per raw row, which is what slicing the anchor grid K ways divides by
	//     K. The fan-out arm arrayJoins each sample across the lookback/Step
	//     anchors it covers, so its F is lookback/Step. The native aggregate
	//     builds no such intermediate — its state is one Array(Nullable(Float64))
	//     of N grid points per SERIES — so it materialises one row per sample and
	//     its F is singlePassFanout, flat in the window width.
	//
	// Reporting lookback/Step for a native carrier would claim a matrix that is
	// never built, and ModeAuto would shard a flat-memory single-pass statement
	// K ways — paying K scans of an Offset+Range-overlapping window for a memory
	// problem the statement does not have. Reporting singlePassFanout keeps the
	// native carrier below MinFanout on its own (so ModeAuto declines on cost),
	// while leaving it fully SLICEABLE: a mixed UnionAll whose other arm IS a
	// fan-out still routes on that arm's F (maxFanout is a max over carriers),
	// and the threshold-free Eligible seam — the failure-driven route memo,
	// which fires on a real route-A resource failure rather than on a cost proxy
	// — still slices a pure-native plan when the evidence says route A could not
	// hold it.
	singlePass bool

	// perRungIntermediate marks a carrier whose F is NOT representable as a
	// plan-time constant at all, because its amplification is per stored
	// BUCKET RUNG rather than per sample or per anchor.
	//
	// Only [chplan.RangeBucketGridNative] sets it. Its Level-0 arrayJoin emits
	// one intermediate row per (sample, `le` rung) and Level 1 holds one grid
	// state per (series, rung), so by singlePass's own definition — peak
	// intermediate rows per raw row — its F is the rung count. That count is
	// the stored `length(ExplicitBounds)`: real data, ranging 34-136 across
	// deployments measured for #2681, and unknowable before the scan runs.
	//
	// It reports singlePass too, which stays correct for what singlePass is
	// used for elsewhere (the cumulative-D derivation): the carrier really
	// does read each sample once. What it does NOT do is materialise one row
	// per sample, which is the part the ModeAuto fanout gate reads — and that
	// is the distinction this field exists to draw. Copying singlePass here
	// from the scalar native carrier without it told the planner an
	// arrayJoin-amplified shape costs the same as a flat-memory one, which is
	// what kept the expensive shape under a threshold designed to catch
	// expensive shapes (#2687).
	//
	// Guessing a constant F was rejected rather than left undone: real widths
	// span 34-136 while Prometheus's own DefBuckets is 10, so any value large
	// enough to clear MinFanout over-claims for a default-bucket histogram and
	// would shard cheap queries — exactly the waste singlePass's doc warns
	// about. The gate therefore switches axis instead of inventing a number;
	// see minAnchorsForPerRungShard.
	perRungIntermediate bool

	// reanchorable reports whether [chplan.ReanchorRange] re-grids this kind.
	// This is a CORRECTNESS bit, not a measurement one: a carrier
	// ReanchorRange clones verbatim hands every shard the original full-grid
	// bounds, so a live grid on such a kind must fail the plan closed to
	// route A. Exactly the RangeWindow, RangeWindowGridNative and RangeLWR
	// families are re-gridded.
	reanchorable bool
}

// singlePassFanout is the fan-out a carrier contributes when its emitter reads
// each raw sample exactly once and materialises no (sample, anchor) matrix: one
// intermediate row per sample, i.e. no amplification at all. It is the F of the
// native timeSeries*ToGrid family (carrierGeometry.singlePass), and 1 rather
// than 0 because the samples ARE read — a zero would say the carrier touches no
// data, which is StepGrid's answer, not this family's.
const singlePassFanout = int64(1)

// minAnchorsForPerRungShard is the anchor count at or above which a per-rung
// carrier (carrierGeometry.perRungIntermediate) is routed by ModeAuto without
// consulting the fanout thresholds, whose input it cannot supply.
//
// Chosen from measurement, not from the threshold it replaces. Real-ClickHouse
// 26.6 sweeps of the classic-histogram ladder at the bucket width production
// actually carries (68) put the per-query memory cliff at ~94 anchors under a
// 6 GiB cap and far lower under the 1 GiB default (issue #2681's calibration
// table). 120 sits above the widest measured safe grid, so a panel small
// enough to have never been observed to strain a per-query budget still runs
// unsliced on route A — the K-scans-for-no-benefit waste singlePass's own doc
// warns about — while the multi-hour dashboard windows that DO strain it
// route.
//
// It is expressed in anchors rather than in cost units on purpose: the anchor
// grid is the one axis of this carrier's cost that the planner can read before
// any data is touched, and it is precisely the axis a shard divides. Rungs and
// raw rows are the other two factors and both need the scan.
//
// Erring high is the safe direction. A per-rung carrier BELOW this line that
// nevertheless exhausts memory is not stranded: it fails on route A, and the
// failure-driven route memo escalates it to a sharded retry from real evidence
// (internal/engine/route_outcome.go). Erring low has no such backstop — it
// spends K scans on a query that never needed them.
const minAnchorsForPerRungShard = 120

// carrierGeometryOf derives the measurement geometry of one grid carrier,
// enumerating every [chplan.GridCarrier] implementation.
//
// The second return is false only for a kind added on the chplan side that
// this package has not learned yet. The caller turns that into a LOUD
// extraction-failed row plus a fail-closed route A rather than a silent
// all-zero feature vector — guessing a geometry would be worse than either.
// chplan's completeness ratchet closes the carrier set and
// TestCarrierGeometry_CoversEveryCarrier pins this enumeration against it, so
// the branch is unreachable in a consistent tree.
func carrierGeometryOf(gc chplan.GridCarrier) (carrierGeometry, bool) {
	switch v := gc.(type) {
	case *chplan.RangeWindow:
		// The fan-out matrix window. OuterRange is read explicitly rather
		// than derived from End-Start because a subquery-inner window leaves
		// both bounds zero for ReanchorRange to fill, and the span must stay
		// measurable there.
		return carrierGeometry{outerRange: v.OuterRange, lookback: v.Range, reanchorable: true}, true

	case *chplan.RangeLWR:
		// The bare-selector last-with-respect-to leaf: one sample per anchor,
		// chosen inside the Lookback staleness horizon. Re-gridded by
		// ReanchorRange and zeroed/re-filled by the slicer's UnpinSpine, so
		// the deriv / idelta / irate / instant-LWR / negative-offset families
		// that lower to a bare RangeLWR spine are routable.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Lookback, reanchorable: true}, true

	case *chplan.RangeBucketGridNative:
		// The native bucket-ladder aggregate: one single-pass grid per
		// (series, `le` rung) rather than a per-(series, anchor) fan-out, so
		// singlePass is set for the same reason RangeWindowGridNative sets it.
		// Re-anchorable since #2677: chplan.ReanchorRange's own
		// *RangeBucketGridNative arm re-grids (Start, End) and widens the input
		// spine by Offset+Range, so a routed shard evaluates ITS OWN sub-grid
		// rather than the full one. Together with the node's entry in
		// chplan.IsSliceInvariant's registry that admits this kind at both
		// gates — see TestRangeBucketGridNative_SlicingAdmittedAtBothGates,
		// which fails if either admission is withdrawn.
		//
		// Why it matters that BOTH moved together: the node's memory grows with
		// the anchor count and with the in-window raw rows, so a wide window
		// busts a single query's ClickHouse memory cap. Time-slicing divides
		// both axes per shard, which is the only relief that removes the window
		// width as a memory term rather than relocating the wall.
		return carrierGeometry{
			outerRange:          v.End.Sub(v.Start),
			lookback:            v.Range,
			singlePass:          true,
			perRungIntermediate: true,
			reanchorable:        true,
		}, true

	case *chplan.RangeWindowGridNative:
		// timeSeries<fn>ToGrid: the whole rate(m[Range]) collapses into one
		// ClickHouse aggregate, so this node ALONE carries the grid of a
		// natively-lowered plan — it is the sole source of that plan's
		// n_anchors / outer_range / cumulative_d. ReanchorRange re-grids it
		// (and UnpinSpine zeroes it), so it is sliceable; its per-anchor
		// lookback is the window Range, and it materialises no (sample, anchor)
		// matrix, so its fan-out is singlePassFanout rather than Range/Step.
		return carrierGeometry{
			outerRange:   v.End.Sub(v.Start),
			lookback:     v.Range,
			singlePass:   true,
			reanchorable: true,
		}, true

	case *chplan.RangeWindowStaleResample:
		// timeSeriesResampleToGridWithStaleness: the native sibling of
		// RangeLWR, with the staleness horizon as its per-anchor depth. Same
		// single-pass grid aggregate as RangeWindowGridNative, so the same fan-out
		// answer; it is NOT re-anchored, so the flag is telemetry-only here
		// until ReanchorRange grows an arm for it.
		return carrierGeometry{
			outerRange:   v.End.Sub(v.Start),
			lookback:     v.Lookback,
			singlePass:   true,
			reanchorable: false,
		}, true

	case *chplan.RangeBucketFanout:
		// The array-aggregate fan-out behind the classic-histogram families:
		// the RangeLWR sibling that collapses each (series, anchor)'s raw
		// BucketCounts/ExplicitBounds rows via GROUP BY instead of picking one
		// last sample. Re-gridded by chplan.ReanchorRange (and zeroed/re-filled
		// by the slicer's UnpinSpine) the same way RangeLWR is, so it is
		// routable.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Lookback, reanchorable: true}, true

	case *chplan.AbsentOverTime:
		// absent_over_time's own grid; Range is the per-anchor window.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Range, reanchorable: false}, true

	case *chplan.StepGrid:
		// A bare anchor axis for the data-free shapes (time(), vector(scalar),
		// the zero-arg date functions). It reads no samples at all, so its
		// per-anchor lookback is genuinely zero rather than unknown: it
		// contributes an anchor count and no cumulative_d.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), reanchorable: false}, true

	default:
		return carrierGeometry{}, false
	}
}

// recordGridCarrier extracts the cost-grid features from one carrier of ANY
// kind, and records the one correctness signal that is a property of the kind
// itself (whether the slicer can re-anchor it).
//
// Measurement and slice-invariance are deliberately separate concerns here.
// Every kind is MEASURED; only the re-anchorable kinds are ROUTABLE. A
// non-re-anchorable carrier with a live grid therefore contributes its real
// geometry to the corpus AND sets the fail-closed flag, so the row records
// both what the plan costs and why it stayed on route A. Nothing in this
// function can make a plan route that would not have routed before: a
// non-re-anchorable carrier either has step <= 0 (and then never reaches the
// hasWindow branch) or sets sawNonRangeWindowSpine, which gate (1b) reads
// ahead of every threshold.
func (p *Planner) recordGridCarrier(gc chplan.GridCarrier, depth int, sig *signals) {
	geom, ok := carrierGeometryOf(gc)
	if !ok {
		// A carrier kind this package does not enumerate. Refuse to guess a
		// geometry, and refuse to route a plan that cannot be measured. Leaving
		// sawGridCarrier unset is what turns this into a ReasonExtractionFailed
		// row rather than a refusal that claims numbers nobody derived.
		sig.sawNonRangeWindowSpine = true
		return
	}
	sig.sawGridCarrier = true

	_, _, step := gc.EvalGrid()

	// A live grid ReanchorRange would clone verbatim fails closed. A carrier
	// with no grid of its own (step <= 0) is a broadcast instant subtree —
	// sharing it verbatim across shards is exactly right, because its value
	// does not depend on the shard's bounds.
	if !geom.reanchorable && step > 0 {
		sig.sawNonRangeWindowSpine = true
	}

	// A routable carrier whose peak does not divide with the grid. Gated on
	// reanchorable so a kind that already fails closed above does not also
	// claim this reason, and on step > 0 because a gridless broadcast subtree
	// is shared verbatim rather than sliced.
	if geom.reanchorable && !gc.AnchorGridDivides() && step > 0 {
		sig.sawIndivisibleAnchorGrid = true
	}

	// The instant/degenerate-spine gate polices the ROUTABLE spine: an
	// outermost routable carrier must own a real anchor grid. The
	// non-routable kinds are already covered by the flag above.
	if geom.reanchorable && (step <= 0 || (depth == 0 && geom.outerRange <= 0)) {
		sig.sawInstantWindow = true
	}

	if step > 0 {
		// F is the per-sample intermediate-row count, so it is derived from the
		// window width ONLY for a carrier that materialises the (sample, anchor)
		// matrix. A single-pass grid aggregate reads each sample once whatever
		// its window width is; see carrierGeometry.singlePass.
		fan := singlePassFanout
		if !geom.singlePass {
			fan = int64(geom.lookback / step)
		}
		if fan > sig.maxFanout {
			sig.maxFanout = fan
		}
	}
	if geom.perRungIntermediate {
		sig.sawPerRungCarrier = true
	}
	// D is per-slice SCAN redundancy, which is the window SPAN regardless of
	// which emitter reads it — a single-pass carrier's shard widens its input
	// by Offset+Range exactly as a fan-out carrier's does, so singlePass does
	// not enter here.
	sig.cumulativeD += geom.lookback

	if depth == 0 && geom.outerRange > 0 && step > 0 {
		sig.hasWindow = true
		sig.outerRange = geom.outerRange
		sig.outerN = int(geom.outerRange/step) + 1
	}
}

// checkRangeWindowGrid runs the bound-pinning, grid-prediction and inner-
// resolution gates that belong to the fan-out RangeWindow family specifically
// — the ones that read the node's own Start/End against the bounds predicted
// at this spine depth, which no cross-kind geometry can express.
func (p *Planner) checkRangeWindowGrid(v *chplan.RangeWindow, predStart, predEnd time.Time, depth int, sig *signals) {
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
		if !rangeWindowGridMatches(v, predStart, predEnd) {
			sig.sawGridMismatch = true
		}
	}

	// (5) Nested-grid phase for the slice-quantum check. Only a nested grid
	// generated backward from this node's own End is phase-sensitive to
	// where the shard boundaries fall. StepAlign snaps the anchor base to an
	// absolute-epoch multiple of Step (reference PromQL's subquery
	// inner-sample grid — see chplan.RangeWindow.StepAlign and chsql's
	// epochAlignedEndFrag), so the fanned anchors land on phase 0 no matter
	// what End is: shifting End by a NON-multiple of Step selects a
	// different newest anchor but never moves the grid off phase, and the
	// per-shard anchor set stays a subset of the unsliced one. Such a spine
	// therefore imposes no quantum constraint and must not be recorded here.
	if depth > 0 && v.Step > 0 && !v.StepAlign {
		sig.endPhasedResolutions = append(sig.endPhasedResolutions, v.Step)
	}
}

// checkRangeLWRGrid runs the RangeLWR family's bound-pinning and
// grid-prediction gates. It differs from the RangeWindow form in two ways
// that are properties of the node, not of the check: RangeLWR has no separate
// OuterRange field (its span IS End-Start), and it is a spine LEAF, so the
// grid-prediction comparison is meaningful only at depth 0 where both bounds
// are pinned.
func (p *Planner) checkRangeLWRGrid(v *chplan.RangeLWR, predStart, predEnd time.Time, depth int, sig *signals) {
	startZero := v.Start.IsZero()
	endZero := v.End.IsZero()
	if depth == 0 {
		if startZero || endZero {
			sig.sawUnpinnedBound = true
		}
		if !startZero && !endZero && !rangeLWRGridMatches(v, predStart, predEnd) {
			sig.sawGridMismatch = true
		}
	} else if startZero != endZero {
		sig.sawUnpinnedBound = true
	}
	// (5) Nested-grid phase for the slice-quantum check — mirrors
	// checkRangeWindowGrid's StepAlign carve-out above. A plain RangeLWR
	// generates its anchors as `End - i*Step` with no epoch snap, so a
	// nested one's phase moves with the shard's End and always constrains
	// the slice quantum. A StepAlign RangeLWR (the subquery inner-sample
	// grid — see chplan.RangeLWR.StepAlign) is re-anchored by
	// chplan.ReanchorRange to a fresh epoch-floored [Start, End] derived
	// from this shard's own predicted bounds, so its anchors land on
	// phase 0 no matter what End is and it imposes no quantum constraint.
	if depth > 0 && v.Step > 0 && !v.StepAlign {
		sig.endPhasedResolutions = append(sig.endPhasedResolutions, v.Step)
	}
}

// checkRangeWindowGridNativeGrid runs the bound-pinning and grid-prediction gates
// for the native timeSeries*ToGrid family. It mirrors checkRangeWindowGrid with
// the one structural difference the node imposes: no separate OuterRange field,
// so the predicted span is End-Start directly (the RangeLWR form of the same
// equality).
//
// The node is pinned in range mode by construction, so in a plan built by the
// lowering neither the unpinned nor the half-pinned branch can fire. They are
// here because this gate is what stands between an @-pinned anchor and a shard
// set that silently disagrees with @ semantics: before this node was routable
// the check was unnecessary, and "the lowering cannot produce that shape today"
// is not a property the slicer may rest correctness on.
func (p *Planner) checkRangeWindowGridNativeGrid(v *chplan.RangeWindowGridNative, predStart, predEnd time.Time, depth int, sig *signals) {
	startZero := v.Start.IsZero()
	endZero := v.End.IsZero()
	switch {
	case startZero && endZero:
		// Fully unpinned. ReanchorRange fills this shape, but no lowering emits
		// it for this node kind, so it is a plan whose grid nothing pinned:
		// refuse rather than invent one.
		sig.sawUnpinnedBound = true
	case startZero != endZero:
		// Half-pinned: malformed.
		sig.sawUnpinnedBound = true
	default:
		if !rangeWindowGridNativeGridMatches(v, predStart, predEnd) {
			sig.sawGridMismatch = true
		}
	}

	// (5) Nested-grid phase for the slice-quantum check. The node's anchor axis
	// is `timeSeriesRange(Start, End, Step)` with no epoch snap — there is no
	// StepAlign carve-out on this kind — so a NESTED one's phase moves with the
	// shard's bounds exactly as a non-StepAlign'd nested RangeWindow's does, and
	// it constrains the slice quantum the same way.
	if depth > 0 && v.Step > 0 {
		sig.endPhasedResolutions = append(sig.endPhasedResolutions, v.Step)
	}
}

// rangeWindowGridNativeGridMatches is rangeWindowGridMatches for a
// RangeWindowGridNative: it carries no separate OuterRange field, so the predicted
// span is End-Start directly.
func rangeWindowGridNativeGridMatches(v *chplan.RangeWindowGridNative, predStart, predEnd time.Time) bool {
	return v.Start.Equal(predStart) && v.End.Equal(predEnd)
}

// rangeWindowGridMatches reports whether v's own (Start, End, OuterRange)
// sit exactly on the grid predicted at (predStart, predEnd) — the signal-4
// equality checkRangeWindowGrid applies to a matrix RangeWindow on the main
// spine. Factored out so checkScalarHeavy's interior walk
// (scalarInteriorAnchorCompatible, signal 7) can apply the IDENTICAL test to
// a windowed node reached through an Expr slot instead of a Node child —
// the two call sites must never drift apart, since the interior walk's
// whole safety argument is "the same grid-prediction proof the main spine
// already relies on".
func rangeWindowGridMatches(v *chplan.RangeWindow, predStart, predEnd time.Time) bool {
	return v.Start.Equal(predStart) && v.End.Equal(predEnd) && v.OuterRange == predEnd.Sub(predStart)
}

// rangeLWRGridMatches is rangeWindowGridMatches for a RangeLWR: it carries
// no separate OuterRange field, so the predicted span is End-Start directly.
func rangeLWRGridMatches(v *chplan.RangeLWR, predStart, predEnd time.Time) bool {
	return v.Start.Equal(predStart) && v.End.Equal(predEnd)
}

// walkHistogramQuantile sweeps a HistogramQuantile's GroupBy and PhiExpr for
// now64 / scalar-heavy hazards, then recurses into Input at the same depth
// (the node adds no grid nesting of its own — see chplan.reanchor's
// *HistogramQuantile arm, a pass-through mirroring *Project). PhiExpr is
// typically a ScalarSubquery for a computed phi (see
// chplan.HistogramQuantile.PhiExpr's doc), so leaving it unswept would let an
// embedded now64 or scalar-heavy interior escape every gate now that this
// kind is registered slice-invariant — mirrors the Aggregate arm's sweep.
// Factored out of walkNode's switch to keep that function under funlen's
// statement cap.
func (p *Planner) walkHistogramQuantile(v *chplan.HistogramQuantile, predStart, predEnd time.Time, predStep time.Duration, depth int, sig *signals) {
	for _, e := range v.GroupBy {
		p.walkExpr(e, predStart, predEnd, predStep, sig)
	}
	p.walkExpr(v.PhiExpr, predStart, predEnd, predStep, sig)
	p.walkNode(v.Input, predStart, predEnd, predStep, depth, sig)
}

// walkRangeBucketFanout sweeps the array-aggregate fan-out behind the
// classic-histogram families — the array-aggregate sibling of RangeLWR. Like
// Aggregate it carries group keys + agg args the spine recursion never sweeps,
// so it covers the same now64 gap; and it carries its own eval grid
// (Start/End/Step), re-anchored by chplan.ReanchorRange the same way RangeLWR
// is (checkRangeBucketFanoutGrid runs the same bound-pinning / grid-prediction
// gates checkRangeLWRGrid does).
//
// Factored out of walkNode's switch — together with its native sibling
// walkRangeBucketGridNative — to keep that function under funlen's statement
// cap, mirroring walkHistogramQuantile.
func (p *Planner) walkRangeBucketFanout(v *chplan.RangeBucketFanout, predStart, predEnd time.Time, depth int, sig *signals) {
	p.recordGridCarrier(v, depth, sig)
	p.checkRangeBucketFanoutGrid(v, predStart, predEnd, depth, sig)
	for _, e := range v.GroupBy {
		p.walkExpr(e, predStart, predEnd, v.Step, sig)
	}
	for _, fn := range v.AggFuncs {
		for _, e := range fn.Params {
			p.walkExpr(e, predStart, predEnd, v.Step, sig)
		}
		for _, e := range fn.Args {
			p.walkExpr(e, predStart, predEnd, v.Step, sig)
		}
	}
	// Each anchor's membership window looks back Offset+Lookback (mirrors
	// the RangeLWR arm above); widen the input spine by that much so the
	// grid this predicts for the child matches what ReanchorRange actually
	// produces (see reanchorRangeBucketFanout). depth+1 mirrors every
	// other GridCarrier arm above (RangeWindow / RangeLWR /
	// RangeWindowGridNative / AbsentOverTime): this node's own grid is a
	// nesting boundary, so anything a further-nested carrier inside
	// v.Input predicts a grid against must see itself as nested, not as
	// depth 0.
	p.walkNode(v.Input, predStart.Add(-v.Offset-v.Lookback), predEnd, v.Step, depth+1, sig)
}

// walkRangeBucketGridNative sweeps the ClickHouse-native sibling of
// RangeBucketFanout: same eval grid, same per-anchor membership window, so it
// is recorded and its input spine widened by Offset+Range exactly as the
// fan-out arm's is.
//
// Since #2677 this kind IS sharded — it is registered in
// chplan.IsSliceInvariant and carrierGeometryOf reports it re-anchorable, the
// two admissions TestRangeBucketGridNative_SlicingAdmittedAtBothGates pins — so
// it carries the same grid-prediction check its fan-out sibling does, in the
// two-bound form the node's own field set (no OuterRange) calls for.
//
// Factored out of walkNode's switch to keep that function under funlen's
// statement cap, mirroring walkHistogramQuantile.
func (p *Planner) walkRangeBucketGridNative(v *chplan.RangeBucketGridNative, predStart, predEnd time.Time, depth int, sig *signals) {
	p.recordGridCarrier(v, depth, sig)
	for _, e := range v.GroupBy {
		p.walkExpr(e, predStart, predEnd, v.Step, sig)
	}
	// depth+1 mirrors every other GridCarrier arm: this node's own grid is a
	// nesting boundary, so anything further nested inside v.Input sees itself
	// as nested rather than as depth 0.
	p.walkNode(v.Input, predStart.Add(-v.Offset-v.Range), predEnd, v.Step, depth+1, sig)
}

// checkRangeBucketFanoutGrid is checkRangeLWRGrid for RangeBucketFanout —
// same shape (no OuterRange field, a spine leaf whose grid-prediction
// comparison is meaningful only at depth 0), since RangeBucketFanout carries
// no StepAlign mode.
func (p *Planner) checkRangeBucketFanoutGrid(v *chplan.RangeBucketFanout, predStart, predEnd time.Time, depth int, sig *signals) {
	startZero := v.Start.IsZero()
	endZero := v.End.IsZero()
	if depth == 0 {
		if startZero || endZero {
			sig.sawUnpinnedBound = true
		}
		if !startZero && !endZero && !rangeBucketFanoutGridMatches(v, predStart, predEnd) {
			sig.sawGridMismatch = true
		}
	} else if startZero != endZero {
		sig.sawUnpinnedBound = true
	}
	// A RangeBucketFanout's anchors are generated backward from End with no
	// epoch snap (mirrors the plain RangeLWR case checkRangeLWRGrid handles),
	// so a nested one always constrains the slice quantum.
	if depth > 0 && v.Step > 0 {
		sig.endPhasedResolutions = append(sig.endPhasedResolutions, v.Step)
	}
}

func rangeBucketFanoutGridMatches(v *chplan.RangeBucketFanout, predStart, predEnd time.Time) bool {
	return v.Start.Equal(predStart) && v.End.Equal(predEnd)
}

// sliceQuantumCommensurate reports whether the quantum emitted by slice() is
// phase-compatible with every END-PHASED nested grid — the ones whose anchors
// are generated backward from the node's own End (RangeLWR always; a nested
// matrix RangeWindow that is not StepAlign'd). Shard j ends at
// `End - j*m*Step`, so every such grid keeps its phase iff `m*Step ≡ 0 (mod
// lcm(resolutions))`, i.e. iff m is a multiple of `lcm/gcd(Step, lcm)`.
//
// The planner chooses K and the slicer then deterministically derives
// m = ceil(N/K), so this checks the quantum that will ACTUALLY be emitted:
// asking only whether SOME valid m exists would admit plans whose real shard
// boundaries move a nested grid. Epoch-aligned nested grids are phase-0
// regardless of End and are excluded upstream, so a plan carrying only those
// reaches the empty-list fast path and routes.
func sliceQuantumCommensurate(sig signals, outerStep time.Duration, k int) bool {
	if len(sig.endPhasedResolutions) == 0 {
		return true
	}
	resLcm := time.Duration(1)
	for _, r := range sig.endPhasedResolutions {
		resLcm = lcmDuration(resLcm, r)
	}
	if resLcm <= 0 || outerStep <= 0 || k < 2 || sig.outerN < 2 {
		return false
	}
	requiredAnchors := resLcm / gcdDuration(outerStep, resLcm)
	quantum := (sig.outerN + k - 1) / k
	return int64(quantum)%int64(requiredAnchors) == 0
}

// walkExpr sweeps an expr tree for now64 and recurses into any embedded
// ScalarSubquery.Input / InSubquery.Subquery plan (which chplan.Walk does not
// reach), running the scalar-heavy cost check and the full node walk inside
// the subquery. predStart/predEnd/predStep is the grid predicted at THIS
// point in the main spine (the same bounds+cadence walkNode threads to e's
// owning node) — the anchor-compatibility check inside checkScalarHeavy needs
// them to judge a windowed node buried in the embedded plan against the grid
// actually predicted here, not against the top-level request grid
// unconditionally (a scalar embedded under a subquery's inner spine must be
// judged against the SUBQUERY's own inner grid and resolution).
func (p *Planner) walkExpr(e chplan.Expr, predStart, predEnd time.Time, predStep time.Duration, sig *signals) {
	chplan.InspectExprNodes(e, func(x chplan.Expr) bool {
		if fc, ok := x.(*chplan.FuncCall); ok && fc.Fn == chplan.FnNow64 {
			sig.sawNow64 = true
		}
		return true
	}, func(inner chplan.Node) {
		// A ScalarSubquery / InSubquery interior. Walk it for
		// slice-invariance / now64, and apply the scalar-heavy cost gate.
		p.checkScalarHeavy(inner, predStart, predEnd, predStep, sig)
		// The interior is below the spine and anchor-independent; walk it
		// for now64 / un-sliceable markers but pin its depth so its bounds
		// are not treated as a grid level.
		p.walkScalarInterior(inner, sig)
	})
}

// walkScalarInterior walks a ScalarSubquery's plan for now64 and unmarked
// nodes only — its bounds do not participate in the outer anchor grid, so the
// grid-prediction / commensurability checks are intentionally skipped here.
//
// It ALSO carries the sawInstantVectorJoin fail-close (mirroring walkNode's
// VectorJoin case): the slice-invariance registry admits VectorJoin by node
// kind, so an instant-mode (!StepAligned) join buried in a scalar interior —
// e.g. scalar(sum(up_a)/sum(up_b)), whose Aggregate-rooted arms carry no
// windowed node and so never trip checkScalarHeavy, and whose now64(9)
// join-side timestamp is minted in SQL and so never trips the now64 scan —
// would otherwise pass every gate and route B, replicating a per-shard
// wall-clock scalar across time-slices. Flag it here so it fails closed to
// route A exactly as a top-level instant join does.
//
// It reads the interior through chplan.WalkDeep and chplan.InspectNodeExprs —
// the IR's own total traversal and its single enumeration of the Expr slots a
// node carries — rather than naming the node kinds whose expressions can hold
// a now64. Naming them left the unlisted slots (an Aggregate's HAVING, a
// TopK's sort key, a histogram_quantile's phi) unswept, so a now64 reachable
// only through one of those escaped the gate and routed B.
func (p *Planner) walkScalarInterior(n chplan.Node, sig *signals) {
	chplan.WalkDeep(n, func(node chplan.Node) bool {
		if !chplan.IsSliceInvariant(node) {
			sig.allSliceInvariant = false
		}
		if v, ok := node.(*chplan.VectorJoin); ok && !v.StepAligned {
			sig.sawInstantVectorJoin = true
		}
		chplan.InspectNodeExprs(node, func(e chplan.Expr) {
			chplan.InspectExpr(e, func(x chplan.Expr) bool {
				if fc, ok := x.(*chplan.FuncCall); ok && fc.Fn == chplan.FnNow64 {
					sig.sawNow64 = true
				}
				return true
			})
		})
		return true
	})
}

// checkScalarHeavy implements signal (7): a ScalarSubquery / InSubquery
// interior whose own windowed spine cannot be proven anchor-compatible with
// the grid predicted at the point it is embedded is too expensive to
// replicate K×, since the scalar itself is never hoisted (route B never
// re-anchors an Expr-embedded plan — see scalarInteriorAnchorCompatible's
// doc for why that is exactly what makes the equality test below safe) and
// is instead re-evaluated once per shard.
//
// A windowed node whose own (Start, End[, OuterRange], Step) sit EXACTLY on
// the grid+cadence predicted at that point is evaluating one value per OUTER
// anchor — the same per-step cadence #1455/#1886 made computed scalar
// arguments bind at — so its own read span is bounded by what a single
// anchor of the outer spine already reads, not by an independent,
// unboundedly wide scan. The (Start, End[, OuterRange]) half of that is the
// identical slice-invariance argument signal 4/5's grid-prediction guard
// already accepts for the main spine (checkRangeWindowGrid /
// checkRangeLWRGrid), applied to a node reached through an Expr slot instead
// of a Node child; the Step equality is this check's OWN addition, absent
// from the main-spine guard because a nested subquery resolution there is
// legitimately allowed to differ from its parent's Step — a freedom this
// check does NOT extend, because nothing here re-derives the interior's own
// fan-out (F = Range/Step) the way the main spine's recordGridCarrier does,
// so a same-bounds-different-cadence interior (e.g. a nested one-minute grid
// under a fifteen-second outer request) is NOT provably a bounded one value
// per OUTER anchor and stays heavy. A node that does NOT match — a
// different span, a different cadence, an @-pinned anchor, a
// RangeBucketFanout, or a Step<=0 instant leaf with no established per-step
// argument — is conservatively still heavy: see scalarInteriorAnchorCompatible.
func (p *Planner) checkScalarHeavy(inner chplan.Node, predStart, predEnd time.Time, predStep time.Duration, sig *signals) {
	if !scalarInteriorAnchorCompatible(inner, predStart, predEnd, predStep) {
		sig.sawScalarHeavy = true
	}
}

// scalarInteriorAnchorCompatible walks a scalar/subquery interior's node
// tree threading grid predictions exactly as walkNode threads them down the
// main spine — a matrix RangeWindow widens the prediction via InputWindow
// and recurses into Input (mirroring the RangeWindow arm of walkNode /
// chplan.ReanchorRange / promql.widenSubquerySpine, the three consumers
// RangeWindow.InputWindow's doc names as the single shared owner of that
// arithmetic), a RangeLWR is a spine leaf checked against the current
// predicted grid — and reports false the moment it finds a windowed node
// that is NOT provably anchor-compatible. predStep is carried UNCHANGED
// through the whole walk (never re-derived from a nested node's own Step,
// unlike predStart/predEnd): every windowed node anywhere in the interior
// must match the SAME cadence the ScalarSubquery / InSubquery was embedded
// at, because that single cadence is the only one this check has an argument
// for — see checkScalarHeavy's doc for why a same-bounds-different-cadence
// nested node does not get the same argument.
//
// It never needs to handle an UNPINNED (zero Start/End) windowed node: the
// plan reaching the Planner has already been through lowering's own
// subquery-widening pass (promql.widenSubquerySpine, invoked self-contained
// by every subquery lowering, including one nested inside a computed scalar
// argument), so every windowed node anywhere in a route-A-emittable plan —
// on the main spine or inside an Expr-embedded interior — already carries
// concrete bounds. A zero-bound comparison here would therefore only ever
// fail the equality test below, which is the conservative outcome anyway.
//
// "Anchor-compatible" is deliberately narrower than "correct": route B never
// re-anchors an Expr-embedded interior (chplan.ReanchorRange's default case
// shares any node reached only through a Node's Children() verbatim, and
// Expr slots — ScalarSubquery.Input, InSubquery.Subquery — are invisible to
// that walk entirely; unpinSpineCOW's own doc says the same), so whatever
// this function admits keeps evaluating the FULL span it always evaluated
// under route A, identically in every shard — correctness cannot regress
// from admitting a node here. What DOES vary is cost: an interior that
// matches the predicted grid AND cadence costs route B at most what the
// outer spine's own per-shard share already costs; the equality test is how
// this package tells that shape apart from a genuinely independent,
// unboundedly wide scan (an @-pinned interior, an interior whose span or
// cadence has nothing to do with the outer grid) that really would multiply
// K× into real extra cost. A RangeBucketFanout is never admitted regardless
// of its bounds: unlike the MAIN spine — where it IS now routable (signal
// 1b, carrierGeometry.reanchorable — the classic-histogram OOM fix) — route
// B never re-anchors an Expr-embedded interior at all, so admitting it here
// would replicate its full unsliced grid K× rather than a per-shard slice;
// no equality argument bounding that replication cost has been built for it
// here, unlike the RangeWindow / RangeLWR / RangeWindowGridNative arms
// above. RangeWindowStaleResample and AbsentOverTime are absent from
// chplan.IsSliceInvariant's registry, so one appearing inside an interior
// already fails the whole plan via walkScalarInterior's slice-invariance
// sweep before this function's answer can matter. RangeWindowGridNative IS
// registered (issue #2117), so that sweep no longer covers it and this function
// answers for it directly — under the same equality the fan-out RangeWindow
// gets, since a full-span native grid replicated K× is exactly as wide a scan
// as a full-span fan-out one. StepGrid IS registered
// slice-invariant and falls through the switch below to the generic
// Children() walk unchecked — deliberately: it is the bare, data-free anchor
// axis behind `time()` / `vector(scalar)` / the zero-arg date functions, it
// reads no samples at all, so replicating it K× costs nothing regardless of
// where its own anchors sit, and this was already true of the pre-existing
// (kind-based) check this function replaces.
func scalarInteriorAnchorCompatible(n chplan.Node, predStart, predEnd time.Time, predStep time.Duration) bool {
	if n == nil {
		return true
	}
	switch v := n.(type) {
	case *chplan.RangeWindow:
		// A Step<=0 instant leaf carries no established per-step argument
		// (no anchor grid to match against) and stays conservatively heavy.
		if v.Step <= 0 || v.Step != predStep || !rangeWindowGridMatches(v, predStart, predEnd) {
			return false
		}
		inStart, inEnd := v.InputWindow(predStart, predEnd)
		return scalarInteriorAnchorCompatible(v.Input, inStart, inEnd, predStep)
	case *chplan.RangeLWR:
		if v.Step <= 0 || v.Step != predStep || !rangeLWRGridMatches(v, predStart, predEnd) {
			return false
		}
		return scalarInteriorAnchorCompatible(v.Input, predStart.Add(-v.Offset-v.Lookback), predEnd, predStep)
	case *chplan.RangeWindowGridNative:
		// The native timeSeries*ToGrid family, held to the SAME equality as the
		// fan-out RangeWindow above — in its two-bound form, since the node has
		// no OuterRange field. This arm became load-bearing the moment the node
		// entered chplan.IsSliceInvariant's registry (issue #2117): before that,
		// walkScalarInterior's slice-invariance sweep refused any plan whose
		// interior carried one, so the switch never had to answer. Now the
		// sweep is silent and the generic Children() fall-through below would
		// admit a native interior as CHEAP whatever its bounds — replicating a
		// full-span single-pass grid aggregate K times, which is exactly the
		// unboundedly-wide interior the gate exists to keep on route A.
		if v.Step <= 0 || v.Step != predStep || !rangeWindowGridNativeGridMatches(v, predStart, predEnd) {
			return false
		}
		inStart, inEnd := v.InputWindow(predStart, predEnd)
		return scalarInteriorAnchorCompatible(v.Input, inStart, inEnd, predStep)
	case *chplan.RangeBucketFanout:
		return false
	}
	for _, c := range n.Children() {
		if !scalarInteriorAnchorCompatible(c, predStart, predEnd, predStep) {
			return false
		}
	}
	return true
}
