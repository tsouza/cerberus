package routerrules

import (
	"context"
	"fmt"
)

// Autotuner fits the solver's two auto-mode cost thresholds — MinFanout and
// MinAnchorPairs — from the router corpus, so a deployment whose route-A queries
// OOM at a lower anchor fan-out than the conservative shipped defaults assume is
// automatically given more protective thresholds. It is the "brain" the engine's
// self-driving loop calls each tick; it holds no state and performs only corpus
// reads through the existing CorpusSource.Aggregate seam (no new SQL).
//
// # Safety is structural, not statistical
//
// The fit only ever LOWERS the thresholds toward the observed route-A OOM line;
// it never raises them. Because route A and route B are result-identical
// (avb_chdb_lane_test.go) and route B is the OOM mitigation, lowering a threshold
// can only send MORE queries to the safe route — the worst case is added route-B
// overhead, never a wrong answer and never a new OOM. And the candidate is
// clamped so that:
//
//   - candidate MinFanout <= the minimum fan-out at which route A was observed to
//     OOM, and
//   - candidate MinAnchorPairs <= min(N)*min(F) over the OOM'd rows, which is a
//     lower bound on the minimum N x F product of any OOM'd shape
//     (min(N_i F_i) >= min(N_i)*min(F_i)).
//
// Together these mean EVERY route-A OOM shape in the corpus provably clears the
// candidate gate and would route B — whether or not the fit actually lowered a
// threshold (if the OOM line sits above the current gate, the current gate
// already dominates it). This structural guarantee replaces a fragile off-policy
// point estimate: production routing is deterministic, so the corpus carries no
// counterfactual A/B evidence to loosen a threshold with — v1 therefore only
// tightens. Loosening (de-protecting) is out of scope until an exploration
// posture supplies that evidence.
//
// Cold start: with no route-A OOM in the corpus there is no signal, and the fit
// returns the current thresholds unchanged.
type Autotuner struct {
	src  CorpusSource
	opts AutotuneOptions
}

// AutotuneOptions are the fit knobs. Defaults come from DefaultAutotuneOptions;
// they are algorithm constants, not per-deployment tuning — promote one to an
// env knob only if a deployment ever needs it (ponytail: unmeasured today).
type AutotuneOptions struct {
	// MinThresholdFloor is the hard floor a candidate threshold can never dip
	// below (matches the Config.MinFanout >= 1 invariant; a fan-out gate of 1
	// still leaves K driven by MinAnchorsPerSlice).
	MinThresholdFloor int

	// FanoutHysteresis / AnchorPairsHysteresis are the minimum absolute drop from
	// the current value required to treat a candidate as a real change, damping
	// threshold thrash under a non-stationary corpus.
	FanoutHysteresis      int
	AnchorPairsHysteresis int
}

// Default fit knobs (named — no magic constants).
const (
	defaultMinThresholdFloor     = 1
	defaultFanoutHysteresis      = 2
	defaultAnchorPairsHysteresis = 500
)

// DefaultAutotuneOptions returns the standard fit knobs.
func DefaultAutotuneOptions() AutotuneOptions {
	return AutotuneOptions{
		MinThresholdFloor:     defaultMinThresholdFloor,
		FanoutHysteresis:      defaultFanoutHysteresis,
		AnchorPairsHysteresis: defaultAnchorPairsHysteresis,
	}
}

// NewAutotuner builds an Autotuner over a corpus source and fit knobs.
func NewAutotuner(src CorpusSource, opts AutotuneOptions) *Autotuner {
	return &Autotuner{src: src, opts: opts}
}

// Thresholds is a (MinFanout, MinAnchorPairs) pair — the current active gate the
// loop passes in, and the fitted candidate it gets back.
type Thresholds struct {
	MinFanout      int
	MinAnchorPairs int
}

// FitResult is the outcome of one fit: the candidate thresholds, whether they
// differ from the current gate beyond hysteresis (Changed), a machine-readable
// Reason, and the OOM-line evidence the fit was derived from (for the shadow
// header / metric / logs).
type FitResult struct {
	Candidate Thresholds
	Changed   bool
	Reason    string

	// OOMMinFanout / OOMMinAnchors are the observed route-A OOM floor: the
	// minimum fan-out and the minimum anchor count among corpus rows where route
	// A OOM'd. Zero when HasOOMSignal is false.
	OOMMinFanout  int
	OOMMinAnchors int
	HasOOMSignal  bool
}

// Fit reads the corpus and returns a certified threshold candidate relative to
// the current gate. It never raises a threshold; see the type doc for the
// structural safety argument.
func (a *Autotuner) Fit(ctx context.Context, current Thresholds) (FitResult, error) {
	res := FitResult{Candidate: current, Reason: ReasonAutotuneNoChange}

	oomA := Scope{"route": "A", "exit_status": "oom"}

	minOOMFanout, err := a.scalar(ctx, AggSpec{Column: "fanout", Agg: AggMin, Scope: oomA})
	if err != nil {
		return FitResult{}, err
	}
	minOOMAnchors, err := a.scalar(ctx, AggSpec{Column: "n_anchors", Agg: AggMin, Scope: oomA})
	if err != nil {
		return FitResult{}, err
	}
	// No route-A OOM anywhere in the corpus → no signal → cold-start: hold the
	// configured thresholds. A NoSignal value on either aggregate means the
	// OOM-A population was empty.
	if minOOMFanout.NoSignal || minOOMAnchors.NoSignal {
		res.Reason = ReasonAutotuneNoSignal
		return res, nil
	}
	res.HasOOMSignal = true
	res.OOMMinFanout = int(minOOMFanout.Scalar)
	res.OOMMinAnchors = int(minOOMAnchors.Scalar)

	// Candidate MinFanout: clamp the observed OOM fan-out floor into
	// [floor, current] — only ever lowers, and never above the OOM floor, so
	// every OOM'd shape (fan-out >= OOMMinFanout) clears it.
	candFanout := clampInt(res.OOMMinFanout, a.opts.MinThresholdFloor, current.MinFanout)

	// Candidate MinAnchorPairs: min(N)*min(F) over the OOM-A population is a
	// conservative lower bound on the minimum N x F product of any OOM'd shape,
	// so clamping the pairs gate at or below it admits every OOM'd shape to route
	// B while never raising the gate.
	oomPairsLB := res.OOMMinAnchors * res.OOMMinFanout
	candPairs := clampInt(oomPairsLB, a.opts.MinThresholdFloor, current.MinAnchorPairs)

	res.Candidate = Thresholds{MinFanout: candFanout, MinAnchorPairs: candPairs}

	// Changed iff either gate dropped by at least its hysteresis margin. Damps
	// thrash: a one-unit wobble in the corpus does not churn the live thresholds.
	fanoutDropped := current.MinFanout-candFanout >= a.opts.FanoutHysteresis
	pairsDropped := current.MinAnchorPairs-candPairs >= a.opts.AnchorPairsHysteresis
	if fanoutDropped || pairsDropped {
		res.Changed = true
		res.Reason = ReasonAutotuneApplied
	}
	return res, nil
}

// scalar resolves one scalar aggregate, erroring if it unexpectedly resolves to a
// partition-keyed value (the fit never partitions).
func (a *Autotuner) scalar(ctx context.Context, spec AggSpec) (Value, error) {
	v, err := a.src.Aggregate(ctx, spec)
	if err != nil {
		return Value{}, fmt.Errorf("routerrules: autotune aggregate %s: %w", spec.Column, err)
	}
	if v.IsPartitioned() {
		return Value{}, fmt.Errorf("routerrules: autotune aggregate %s unexpectedly partitioned", spec.Column)
	}
	return v, nil
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
