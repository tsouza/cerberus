package routerrules

import "context"

// Autotuner fits the solver's two auto-mode cost thresholds — MinFanout and
// MinAnchorPairs — from the router corpus, so a deployment whose route-A queries
// OOM at a lower anchor fan-out than the conservative shipped defaults assume is
// automatically given more protective thresholds. It is the "brain" the engine's
// self-driving loop calls each tick; it holds no state and reads only the
// observed OOM floor through the OOMFloorSource seam (no new generic SQL).
//
// # Safety is structural, not statistical
//
// The fit only ever LOWERS the thresholds toward the observed route-A OOM line;
// it never raises them, and — with no hysteresis — whatever it computes is what
// the loop applies, so the guarantee below holds for the LIVE gate, not merely a
// notional candidate. Because route A and route B are result-identical
// (avb_chdb_lane_test.go) and route B is the OOM mitigation, lowering a threshold
// can only send more queries to the safe route: the worst case is added route-B
// overhead, never a wrong answer and never a new OOM. And the candidate is
// clamped so that:
//
//   - candidate MinFanout <= the minimum fan-out at which route A was observed to
//     OOM (over the eligible, grid-bearing population — see OOMFloor), and
//   - candidate MinAnchorPairs <= min(N)*min(F) over those rows, a lower bound on
//     the minimum N x F product of any such shape (min(N_i F_i) >= min(N_i)*min(F_i)).
//
// Together these mean EVERY eligible route-A OOM shape in the corpus provably
// clears the live gate and would route B. Because the fit reads only the
// below-threshold, grid-bearing OOM population (OOMFloor's predicate), an
// ineligible OOM (instant / not-sliceable, recorded with a zero grid) can never
// crater the floor to 0.
//
// Monotone convergence: each tick refits against the CURRENTLY active thresholds,
// so the gate only ratchets downward and settles once it reaches the OOM floor —
// no oscillation, hence no hysteresis is needed. Because production routing is
// deterministic, the corpus carries no counterfactual A/B evidence to loosen a
// threshold with, so v1 only tightens; a restart drops the in-memory overlay back
// to the configured values and re-derives from a fresh corpus window.
//
// Cold start: with no eligible route-A OOM in the corpus there is no signal, and
// the fit returns the current thresholds unchanged.
type Autotuner struct {
	src OOMFloorSource
}

// minThresholdFloor is the hard floor a candidate threshold can never dip below
// (matches the Config.MinFanout >= 1 invariant). The OOMFloor predicate excludes
// fanout==0 rows, so a real signal is always >= 1 and the floor never has to
// rescue the candidate from 0.
const minThresholdFloor = 1

// NewAutotuner builds an Autotuner over an OOM-floor source.
func NewAutotuner(src OOMFloorSource) *Autotuner {
	return &Autotuner{src: src}
}

// Thresholds is a (MinFanout, MinAnchorPairs) pair — the current active gate the
// loop passes in, and the fitted candidate it gets back.
type Thresholds struct {
	MinFanout      int
	MinAnchorPairs int
}

// FitResult is the outcome of one fit: the candidate thresholds, whether they
// differ from the current gate (Changed), a machine-readable Reason, and the
// OOM-line evidence the fit was derived from (for the shadow header / logs).
type FitResult struct {
	Candidate Thresholds
	Changed   bool
	Reason    string

	OOMMinFanout  int
	OOMMinAnchors int
	HasOOMSignal  bool

	// Rolling-window outcome counts over the corpus window, passed through from
	// the OOM-floor read for introspection (they do not affect the fit).
	RouteAOomCount   int64
	RouteBExecutions int64
	RouteBOomCount   int64
}

// Fit reads the corpus OOM floor and returns a threshold candidate relative to
// the current gate. It never raises a threshold; see the type doc for the
// structural safety argument.
func (a *Autotuner) Fit(ctx context.Context, current Thresholds) (FitResult, error) {
	floor, err := a.src.OOMFloor(ctx)
	if err != nil {
		return FitResult{}, err
	}

	// Outcome counts are carried on every result, signal or not (route-B volume
	// is meaningful even when no route-A OOMs remain).
	res := FitResult{
		Candidate:        current,
		RouteAOomCount:   floor.RouteAOomCount,
		RouteBExecutions: floor.RouteBExecutions,
		RouteBOomCount:   floor.RouteBOomCount,
	}
	if !floor.HasSignal {
		res.Reason = ReasonAutotuneNoSignal
		return res, nil
	}
	res.HasOOMSignal = true
	res.OOMMinFanout = floor.MinFanout
	res.OOMMinAnchors = floor.MinAnchors

	// Candidate MinFanout: clamp the observed OOM fan-out floor into
	// [floor, current] — only ever lowers, never above the OOM floor.
	candFanout := clampInt(floor.MinFanout, minThresholdFloor, current.MinFanout)

	// Candidate MinAnchorPairs: min(N)*min(F) over the OOM population is a
	// conservative lower bound on the minimum N x F product of any OOM'd shape.
	oomPairsLB := floor.MinAnchors * floor.MinFanout
	candPairs := clampInt(oomPairsLB, minThresholdFloor, current.MinAnchorPairs)

	res.Candidate = Thresholds{MinFanout: candFanout, MinAnchorPairs: candPairs}
	if candFanout < current.MinFanout || candPairs < current.MinAnchorPairs {
		res.Changed = true
		res.Reason = ReasonAutotuneApplied
	} else {
		res.Reason = ReasonAutotuneNoChange
	}
	return res, nil
}

// Autotune fit-result reason vocabulary (mirrors the solver Reason* style so the
// shadow header / metric can carry it verbatim).
const (
	ReasonAutotuneApplied  = "autotune-applied"
	ReasonAutotuneNoChange = "autotune-no-change"
	ReasonAutotuneNoSignal = "autotune-no-signal"
)

// clampInt clamps v to [lo, hi]. When lo > hi (a degenerate corpus where the
// floor sits above the current gate) hi wins, preserving the never-raise
// invariant.
func clampInt(v, lo, hi int) int {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}
