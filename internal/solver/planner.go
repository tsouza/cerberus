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
		if sig.maxFanout < int64(minFanout) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if int64(sig.outerN)*sig.maxFanout < int64(minAnchorPairs) {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
		}
		if k < 2 {
			return notRouted(ReasonBelowThreshold).withGrid(sig, meta), false
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
	// RangeWindow matrix family and the RangeLWR bare-selector
	// last-with-respect-to family — ReanchorRange re-grids both. Every other
	// [chplan.GridCarrier] kind (RangeWindowNative, RangeWindowResample,
	// RangeBucketFanout, StepGrid, AbsentOverTime) has its grid CloneNode'd
	// verbatim, so each shard would emit the original full-grid bounds; they
	// fail closed to route A. Note the asymmetry this gate exists to preserve:
	// the extractor MEASURES all seven kinds so the corpus can see what a
	// refused plan costs, while only the two re-anchored families may be
	// SLICED. Admitting a kind here means teaching ReanchorRange its grid
	// first, not widening carrierGeometryOf.
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
	// (5) Grid commensurability for nested spines.
	if sig.sawIncommensurate {
		return sig, notRouted(ReasonIncommensurate).withGrid(sig, meta), 0, false
	}
	// (6) A ScalarSubquery too expensive to replicate K× (and that cannot be
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

	sawNow64          bool
	sawUnpinnedBound  bool
	sawInstantWindow  bool
	sawGridMismatch   bool
	sawIncommensurate bool
	sawScalarHeavy    bool

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
	// RangeWindow matrix family plus the RangeLWR bare-selector family: both
	// are re-gridded by ReanchorRange and zeroed/re-filled by unpinSpine, so
	// neither sets this flag. RangeWindowNative / RangeWindowResample /
	// RangeBucketFanout / StepGrid / AbsentOverTime fail closed to route A
	// until ReanchorRange learns their grids — see carrierGeometry.reanchorable,
	// which is the single place that decision is recorded.
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

	// Cost grid, derived from the OUTERMOST windowed node (the spine root).
	outerN      int           // N = OuterRange/Step + 1
	outerRange  time.Duration // OuterRange of the outermost spine
	maxFanout   int64         // F over every windowed node
	cumulativeD time.Duration // D = Σ per-anchor lookback over every grid carrier

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
		p.recordGridCarrier(v, depth, sig)
		p.checkRangeWindowGrid(v, predStart, predEnd, depth, sig)
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
		p.recordGridCarrier(v, depth, sig)
		p.checkRangeLWRGrid(v, predStart, predEnd, depth, sig)
		p.walkNode(v.Input, predStart, predEnd, depth+1, sig)
		return

	case *chplan.RangeWindowNative:
		// The ClickHouse-native `timeSeriesRateToGrid` lowering of
		// rate(m[Range]). It owns a full eval grid, so it is the ONLY carrier
		// in a natively-lowered plan and the sole source of that plan's
		// n_anchors / outer_range / cumulative_d. Its bounds are pinned by
		// construction (range mode only), so no bound-pinning gate applies;
		// recordGridCarrier's reanchorable check is what keeps it on route A.
		p.recordGridCarrier(v, depth, sig)
		for _, e := range v.GroupBy {
			p.walkExpr(e, sig)
		}
		for _, pr := range v.Recollapse {
			p.walkExpr(pr.Expr, sig)
		}
		// Each anchor reduces the samples in its own `Range` window, so the
		// inner spine reaches back that far — the same widening the RangeWindow
		// arm above applies.
		p.walkNode(v.Input, predStart.Add(-v.Range), predEnd, depth+1, sig)
		return

	case *chplan.RangeWindowResample:
		// The native staleness-resample lowering of a bare selector at each
		// anchor. Same story as RangeWindowNative, with Lookback (the staleness
		// horizon) as the per-anchor window depth. Its inner spine is walked at
		// the unwidened bounds, mirroring the RangeLWR arm it is the native
		// sibling of.
		p.recordGridCarrier(v, depth, sig)
		p.walkNode(v.Input, predStart, predEnd, depth+1, sig)
		return

	case *chplan.AbsentOverTime:
		// absent_over_time's own grid. It carries no group-key or agg-arg
		// exprs (only SynthLabels, which are literal label pairs), so the
		// default child recursion below the record call is the whole sweep.
		p.recordGridCarrier(v, depth, sig)
		p.walkNode(v.Input, predStart.Add(-v.Range), predEnd, depth+1, sig)
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
		// would emit stale bounds — recordGridCarrier fails it closed to
		// route A while still measuring the grid it does carry.
		p.recordGridCarrier(v, depth, sig)
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
		// A StepGrid spine carrier is not in the routable set, so it fails
		// closed to route A. It is a leaf (no Children), and it reads no
		// samples, so it contributes an anchor count and no lookback.
		p.recordGridCarrier(v, depth, sig)
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
		p.walkNode(v.Left, predStart, predEnd, depth, sig)
		p.walkNode(v.Right, predStart, predEnd, depth, sig)
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
		p.walkNode(c, predStart, predEnd, depth, sig)
	}
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
	lookback time.Duration

	// reanchorable reports whether [chplan.ReanchorRange] re-grids this kind.
	// This is a CORRECTNESS bit, not a measurement one: a carrier
	// ReanchorRange clones verbatim hands every shard the original full-grid
	// bounds, so a live grid on such a kind must fail the plan closed to
	// route A. Exactly the RangeWindow and RangeLWR families are re-gridded.
	reanchorable bool
}

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
		// ReanchorRange and zeroed/re-filled by the slicer's unpinSpine, so
		// the deriv / idelta / irate / instant-LWR / negative-offset families
		// that lower to a bare RangeLWR spine are routable.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Lookback, reanchorable: true}, true

	case *chplan.RangeWindowNative:
		// timeSeries<fn>ToGrid: the whole rate(m[Range]) collapses into one
		// ClickHouse aggregate, so this node ALONE carries the grid of a
		// natively-lowered plan — it is the sole source of that plan's
		// n_anchors / outer_range / cumulative_d. ReanchorRange has no arm
		// for it, so it stays route A.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Range, reanchorable: false}, true

	case *chplan.RangeWindowResample:
		// timeSeriesResampleToGridWithStaleness: the native sibling of
		// RangeLWR, with the staleness horizon as its per-anchor depth.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Lookback, reanchorable: false}, true

	case *chplan.RangeBucketFanout:
		// The array-aggregate fan-out behind the histogram families. Its
		// per-node shape is slice-invariant, but its grid is cloned verbatim,
		// which is why measuring it must not be confused with routing it.
		return carrierGeometry{outerRange: v.End.Sub(v.Start), lookback: v.Lookback, reanchorable: false}, true

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

	// The instant/degenerate-spine gate polices the ROUTABLE spine: an
	// outermost routable carrier must own a real anchor grid. The
	// non-routable kinds are already covered by the flag above.
	if geom.reanchorable && (step <= 0 || (depth == 0 && geom.outerRange <= 0)) {
		sig.sawInstantWindow = true
	}

	if step > 0 {
		if fan := int64(geom.lookback / step); fan > sig.maxFanout {
			sig.maxFanout = fan
		}
	}
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
		predOuter := predEnd.Sub(predStart)
		if !v.Start.Equal(predStart) || !v.End.Equal(predEnd) || v.OuterRange != predOuter {
			sig.sawGridMismatch = true
		}
	}

	// (5) Inner spine resolution for the lcm commensurability check.
	if depth > 0 && v.Step > 0 {
		sig.innerResolutions = append(sig.innerResolutions, v.Step)
		p.checkCommensurability(sig)
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
		if !startZero && !endZero {
			predOuter := predEnd.Sub(predStart)
			if !v.Start.Equal(predStart) || !v.End.Equal(predEnd) || v.End.Sub(v.Start) != predOuter {
				sig.sawGridMismatch = true
			}
		}
	} else if startZero != endZero {
		sig.sawUnpinnedBound = true
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
func (p *Planner) walkScalarInterior(n chplan.Node, sig *signals) {
	chplan.Walk(n, func(node chplan.Node) bool {
		if !chplan.IsSliceInvariant(node) {
			sig.allSliceInvariant = false
		}
		switch v := node.(type) {
		case *chplan.VectorJoin:
			if !v.StepAligned {
				sig.sawInstantVectorJoin = true
			}
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
