package solver

// Per-deployment self-tuning of the route A/B thresholds.
//
// ARCHITECTURE (the whole point of this file — keep it explicit):
//
//   - GENERIC (shipped to every deployment): everything in THIS file — the
//     Calibrate logic, the terminal-OOM frontier model, the safety rails, and
//     the CONSERVATIVE DEFAULT thresholds (DefaultConfig in config.go). This code
//     is deployment-independent: no deployment's learned constants are ever
//     baked into it. A fresh install runs the same Calibrate over its own
//     (initially empty) corpus.
//
//   - LOCAL (never shipped): the CALIBRATED constants Calibrate returns. They
//     are derived AT RUNTIME, exclusively from `samples` — THIS deployment's
//     own cerberus_router_corpus rows. A deployment's corpus tunes that deployment only;
//     another install tunes itself. A single deployment's corpus deliberately
//     never leaks into the shipped defaults (that would be cross-deployment
//     over-fit), which is why the only deployment-specific input to Calibrate
//     is `samples` and nothing else.
//
// Calibrate is PURE and DETERMINISTIC: same (samples, defaults) → same
// (Config, CalibrationReport), no clock / RNG / I/O. The corpus read lives
// behind a reader interface (selftune.go); the calibration math is here.

import (
	"sort"
)

// CorpusSample is one aggregated frontier bucket read from THIS deployment's
// cerberus_router_corpus — the ONLY deployment-specific input to Calibrate.
// Each sample summarises the queries that classified at a given (N, F, D)
// cost-grid point and whether any of them hit a TERMINAL OOM, so the
// calibrator can locate where the deployment's own queries actually fall over.
//
// It is intentionally a flat value type (no chplan / optcorpus dependency) so
// the calibrator stays pure and the reader that fills it can live anywhere
// (in practice cmd/cerberus maps corpus rows into this shape — see
// docs/solver.md).
type CorpusSample struct {
	// NAnchors / Fanout / CumulativeD are the cost-grid coordinates the
	// router stamped on the decision (Decision.NAnchors / Fanout /
	// CumulativeD). They mirror the corpus n_anchors / fanout / cumulative_d
	// columns.
	NAnchors    int
	Fanout      int
	CumulativeD int

	// Route is the routing read-out captured at dispatch: "A" (single) or
	// "B" (sharded). Mirrors the corpus route Enum8.
	Route string

	// BelowThreshold is true when the router classified the plan ELIGIBLE but
	// kept it on route A because it had not cleared the cost thresholds
	// (DecisionReason == ReasonBelowThreshold). These are the samples that
	// tell us where the CURRENT thresholds sit relative to the frontier.
	BelowThreshold bool

	// OOM is true when this sample's query terminated in the OOM / cost danger
	// zone — ExitStatus oom / timeout / sample_budget (the three terminal
	// outcomes route B exists to avoid). This is the only cost read-out the
	// calibrator consults: it locates the frontier from the TERMINAL-OOM
	// coordinate alone. The corpus still captures peak memory / wall-clock per
	// bucket (see optcorpus.FrontierBucket), but the calibrator deliberately
	// does NOT read a soft cost gradient — matching the shipped behaviour, which
	// gates purely on the terminal outcome.
	OOM bool

	// Count is how many real dispatches this aggregated bucket summarises, so
	// the calibrator can weight rates and apply the min-sample floor. A
	// bucket built from a single SELECT-GROUP-BY row carries its group count.
	Count int
}

// CalibrationReport is the human-readable, GENERIC explanation of what a
// single Calibrate run did — logged on every recalibration so operators can
// SEE what their deployment self-tuned to (and that it is local, derived from
// their own corpus). It carries no deployment identity; it is a diff of
// Config fields plus the evidence that justified each move.
type CalibrationReport struct {
	// SampleCount is the total number of real dispatches across all samples
	// (Σ CorpusSample.Count) the run considered.
	SampleCount int

	// NoOp is true when Calibrate returned the defaults verbatim because the
	// corpus carried no actionable signal (fail-open). NoOpReason names why.
	NoOp       bool
	NoOpReason string

	// Changes lists each threshold that moved, in defaults → calibrated form,
	// with the evidence. Empty when NoOp or when the frontier confirmed the
	// defaults already route safely.
	Changes []ThresholdChange

	// FrontierFanout / FrontierAnchorPairs record the empirical frontier the
	// run detected: the COUPLED (single-OOM-coordinate) Fanout / (N×F)
	// anchor-pair product that defines the binding danger corner. Zero when no
	// danger frontier was observed. Reported even on a no-op so operators see
	// the evidence.
	FrontierFanout      int
	FrontierAnchorPairs int

	// FloorClampedFanout / FloorClampedAnchorPairs are set when the shipped
	// safety floor (minCalibratedFanout / minCalibratedAnchorPairs) bound the
	// calibrated threshold at or ABOVE the margin-reduced frontier — i.e. the
	// floor swallowed the safety margin (and, in the sub-floor case, would have
	// installed a gate above the actual OOM coordinate). When set, the
	// calibrated threshold no longer sits a full margin below the frontier; the
	// loop logs a WARN so operators see the rail has been reached. This is the
	// honest surfacing the floor previously hid (it moved silently before).
	FloorClampedFanout      bool
	FloorClampedAnchorPairs bool
}

// ThresholdChange is one field's defaults → calibrated movement plus a short
// reason. The Field names match Config field names so the log is greppable.
type ThresholdChange struct {
	Field string
	From  int
	To    int
	Why   string
}

// Safety-rail constants (GENERIC). These bound how far Calibrate may move a
// threshold in the SAFER direction, and how much evidence it demands before
// moving at all. None of them encodes a deployment's data — they are the
// shipped conservative envelope.
const (
	// minCalibrationSamples is the corpus-thinness floor: below this many
	// total real dispatches the corpus cannot describe a frontier, so
	// Calibrate fails open and returns defaults unchanged. Sized so a handful
	// of stray queries can never move a production threshold.
	minCalibrationSamples = 200

	// minFrontierDangerSamples is how many OOM/cost-danger dispatches must
	// exist before the frontier is considered real. One unlucky OOM is noise;
	// a cluster of them at a consistent (F, N×F) is signal.
	minFrontierDangerSamples = 5

	// frontierSafetyMargin tightens the calibrated threshold to route B
	// strictly BEFORE the observed danger frontier rather than AT it
	// (PARQO asymmetric penalty: under-shard→OOM is catastrophic, over-shard
	// merely wastes a connection). A calibrated threshold is set to the
	// frontier value scaled down by this fraction, expressed as a percent so
	// it stays an integer-relative term, not an absolute tuned to one install.
	frontierSafetyMarginPercent = 75

	// minCalibratedFanout / minCalibratedAnchorPairs are the SHIPPED SAFETY
	// FLOOR: the calibrated thresholds may tighten toward route B but never
	// past these, so a pathological corpus can never drive routing to shard
	// trivially-small queries (which would waste connections for no memory
	// benefit). Tighten-only is bounded below by this floor.
	minCalibratedFanout      = 2
	minCalibratedAnchorPairs = 500
)

// Calibrate derives per-deployment route thresholds from THIS deployment's
// corpus samples, starting from the shipped conservative defaults. It is the
// GENERIC calibration logic; the only deployment-specific input is `samples`.
//
// SAFETY RAILS (all tested):
//
//   - Conservative-floor / tighten-only: a calibrated threshold may only move
//     in the SAFER direction — i.e. LOWER MinFanout / MinAnchorPairs so the
//     deployment shards MORE readily near its own OOM frontier. Calibrate
//     never RAISES a threshold (which would route B less readily and risk the
//     under-shard→OOM catastrophe), and never lowers past the shipped safety
//     floor (minCalibratedFanout / minCalibratedAnchorPairs).
//   - Fail-open / no-op without signal: a thin corpus (< minCalibrationSamples
//     real dispatches), no below-threshold decisions, or no OOM/cost-danger
//     exemplars → return `defaults` UNCHANGED with NoOp set. (A no-signal
//     corpus is exactly this shape — all route A, below-threshold=0, zero
//     failures — so Calibrate is a true verbatim no-op there.)
//   - Relative terms: the frontier is expressed in the corpus's own observed
//     (F, N×F) coordinates scaled by a shipped margin percent, not absolutes
//     hardcoded to one deployment.
//
// Only MinFanout and MinAnchorPairs are calibrated; the geometric knobs (MaxK,
// MinAnchorsPerSlice, high-D clamp) are structural grid invariants the corpus
// does not re-derive, so they pass through from defaults untouched.
func Calibrate(samples []CorpusSample, defaults Config) (Config, CalibrationReport) {
	total := 0
	for _, s := range samples {
		c := s.Count
		if c <= 0 {
			c = 1
		}
		total += c
	}

	report := CalibrationReport{SampleCount: total}

	// Fail-open rail 1: thin corpus. A handful of samples cannot describe a
	// frontier; return defaults verbatim.
	if total < minCalibrationSamples {
		report.NoOp = true
		report.NoOpReason = "thin corpus: below min-sample floor"
		return defaults, report
	}

	// Find the empirical danger frontier: the SMALLEST Fanout and smallest
	// (N×F) anchor-pair product at which the deployment actually observed
	// OOM/cost-danger dispatches, weighted by a min-danger-sample floor so a
	// single unlucky OOM is treated as noise.
	frontierF, frontierPairs, dangerCount, sawBelowThreshold := scanFrontier(samples)

	report.FrontierFanout = frontierF
	report.FrontierAnchorPairs = frontierPairs

	// Fail-open rail 2: no below-threshold decisions means the current
	// thresholds never held a plan back — there is nothing to tighten toward.
	if !sawBelowThreshold {
		report.NoOp = true
		report.NoOpReason = "no below-threshold decisions: thresholds never gated a plan"
		return defaults, report
	}

	// Fail-open rail 3: no OOM/cost-danger frontier observed. Without a danger
	// signal there is no evidence that routing A near these thresholds is
	// risky, so tightening would be speculative. Return defaults.
	if dangerCount < minFrontierDangerSamples || frontierF <= 0 || frontierPairs <= 0 {
		report.NoOp = true
		report.NoOpReason = "no OOM/cost-danger frontier: corpus shows no danger signal"
		return defaults, report
	}

	// We have a real frontier. Derive calibrated thresholds that route B
	// strictly BEFORE it, then apply tighten-only + safety-floor clamps. Each
	// axis reports whether the floor swallowed the safety margin so the loop can
	// WARN (and so a genuine sub-floor OOM caps strictly below the frontier
	// rather than installing a gate above the OOM coordinate).
	calibrated := defaults

	calibrated.MinFanout, report.FloorClampedFanout = calibrateAxis(
		defaults.MinFanout, frontierF, minCalibratedFanout,
	)
	calibrated.MinAnchorPairs, report.FloorClampedAnchorPairs = calibrateAxis(
		defaults.MinAnchorPairs, frontierPairs, minCalibratedAnchorPairs,
	)

	if calibrated.MinFanout != defaults.MinFanout {
		report.Changes = append(report.Changes, ThresholdChange{
			Field: "MinFanout",
			From:  defaults.MinFanout,
			To:    calibrated.MinFanout,
			Why:   axisWhy("fanout", report.FloorClampedFanout),
		})
	}
	if calibrated.MinAnchorPairs != defaults.MinAnchorPairs {
		report.Changes = append(report.Changes, ThresholdChange{
			Field: "MinAnchorPairs",
			From:  defaults.MinAnchorPairs,
			To:    calibrated.MinAnchorPairs,
			Why:   axisWhy("anchor-pairs", report.FloorClampedAnchorPairs),
		})
	}

	// If the frontier confirmed the defaults already route safely (both clamps
	// left the defaults in place), this is a confirming no-op: still return
	// defaults so the swap is a literal identity, and record why.
	if len(report.Changes) == 0 {
		report.NoOp = true
		report.NoOpReason = "frontier above current thresholds: defaults already route safely"
		return defaults, report
	}

	return calibrated, report
}

// scanFrontier finds the deployment's binding danger corner: ONE OOM sample's
// COUPLED (Fanout, N×F) coordinate, plus the total danger-sample count and
// whether any below-threshold decision exists. Deterministic: samples are
// sorted before scanning so the result never depends on input order.
//
// COUPLED, not per-axis-independent. An earlier version minimised frontierF and
// frontierPairs separately, so the two could come from two DIFFERENT OOM
// samples — a synthetic corner that exists at no real query, and that let a
// fanout-gated OOM (small fanout, large N → large pairs) drag the pairs
// frontier down below where the pairs axis is actually dangerous. Both frontier
// values now come from the SAME sample: the OOM coordinate with the smallest
// anchor-pair product (ties broken by smaller fanout, then D, then Route for
// determinism). Anchor-pairs is the dominant cost proxy (route B exists to cap
// the N×F scan), so the smallest-pairs OOM is the corner closest to the safe
// region — the conservative binding frontier.
func scanFrontier(samples []CorpusSample) (frontierF, frontierPairs, dangerCount int, sawBelowThreshold bool) {
	ordered := make([]CorpusSample, len(samples))
	copy(ordered, samples)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		ap, bp := a.NAnchors*a.Fanout, b.NAnchors*b.Fanout
		if ap != bp {
			return ap < bp
		}
		if a.Fanout != b.Fanout {
			return a.Fanout < b.Fanout
		}
		if a.CumulativeD != b.CumulativeD {
			return a.CumulativeD < b.CumulativeD
		}
		return a.Route < b.Route
	})

	frontierF = 0
	frontierPairs = 0
	for _, s := range ordered {
		if s.BelowThreshold {
			sawBelowThreshold = true
		}
		if !s.OOM {
			continue
		}
		c := s.Count
		if c <= 0 {
			c = 1
		}
		dangerCount += c

		pairs := s.NAnchors * s.Fanout
		if pairs <= 0 || s.Fanout <= 0 {
			continue
		}
		// First valid OOM sample in (pairs, fanout, …) order is the binding
		// corner; both axes are read from it so the coordinate is real.
		if frontierPairs == 0 {
			frontierF = s.Fanout
			frontierPairs = pairs
		}
	}
	return frontierF, frontierPairs, dangerCount, sawBelowThreshold
}

// applyMargin scales a frontier value down by frontierSafetyMarginPercent so
// the calibrated threshold sits strictly BELOW the observed danger frontier
// (route B fires before the deployment hits the danger zone). Floors at 1 so a
// tiny frontier never collapses to zero.
func applyMargin(frontierValue int) int {
	scaled := frontierValue * frontierSafetyMarginPercent / 100
	if scaled < 1 {
		return 1
	}
	return scaled
}

// axisWhy renders the reason string for one axis's threshold move, calling out
// when the safety floor swallowed the margin so the change log is honest.
func axisWhy(axis string, floorClamped bool) string {
	if floorClamped {
		return "observed OOM/cost-danger at low " + axis +
			"; calibrated threshold pinned by the safety floor — margin reduced (see floor-clamp WARN)"
	}
	return "observed OOM/cost-danger at lower " + axis + "; shard before the local frontier"
}

// calibrateAxis derives one axis's calibrated threshold from its observed
// frontier and reports whether the safety floor swallowed the margin.
//
// It enforces the tighten-only + safety-floor rails: the result is accepted
// only if it tightens (strictly LOWER than the shipped default — shard more
// readily) and never sits below the shipped floor; otherwise the default is
// kept. Calibrate can never RAISE a threshold.
//
// The floor/margin interaction (the bug this replaces hid): the margin-reduced
// target `want` is the value that puts route B a full safety margin below the
// frontier. When the floor would raise `want` back up to or above the frontier,
// the margin is gone — and a genuine SUB-FLOOR frontier (frontier ≤ floor) is
// worse still: pinning to the floor installs a gate ABOVE the OOM coordinate,
// keeping an already-OOMing query on route A. Both cases set floorClamped=true
// so the loop WARNs instead of moving silently:
//
//   - frontier ≤ floor (sub-floor OOM): cap STRICTLY BELOW the frontier
//     (frontier-1) so route B still fires before the danger zone, rather than
//     pinning to a floor that sits above it.
//   - floor < frontier but floor ≥ want (floor ate the margin): pin to the
//     floor — route B still fires before the frontier, just with less headroom.
func calibrateAxis(def, frontier, floor int) (value int, floorClamped bool) {
	want := applyMargin(frontier)
	if want >= def {
		// Not a tightening (frontier above current threshold) — keep default.
		return def, false
	}
	if want >= floor {
		// Clean tightening: full margin preserved, floor not reached.
		return want, false
	}
	// The floor would raise `want`. Surface it.
	if frontier <= floor {
		// Sub-floor OOM: floor sits at/above the OOM coordinate. Cap strictly
		// below the frontier so route B fires before the danger zone.
		capped := frontier - 1
		if capped < 1 {
			capped = 1
		}
		return capped, true
	}
	// Floor is still below the frontier — pin to it, but flag the lost margin.
	return floor, true
}
