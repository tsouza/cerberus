package solver

// Per-deployment self-tuning of the route A/B thresholds.
//
// ARCHITECTURE (the whole point of this file — keep it explicit):
//
//   - GENERIC (shipped to every deployment): everything in THIS file — the
//     Calibrate logic, the cost-frontier model, the safety rails, and the
//     CONSERVATIVE DEFAULT thresholds (DefaultConfig in config.go). This code
//     is deployment-independent: no deployment's learned constants are ever
//     baked into it. A fresh install runs the same Calibrate over its own
//     (initially empty) corpus.
//
//   - LOCAL (never shipped): the CALIBRATED constants Calibrate returns. They
//     are derived AT RUNTIME, exclusively from `samples` — THIS deployment's
//     own cerberus_router_corpus rows. Squid's corpus tunes squid only;
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
// Each sample summarises the observed cost of queries that classified at a
// given (N, F, D) cost-grid point, so the calibrator can find where the
// deployment's own cost frontier approaches the danger zone.
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

	// MemoryUsage is the peak server-side memory the dispatched query used
	// (bytes), from the corpus memory_usage column. Zero for decision-only
	// rows that never dispatched a CH query.
	MemoryUsage uint64

	// QueryDurationMS is the server-side wall-clock duration, from the corpus
	// query_duration_ms column. Zero for decision-only rows.
	QueryDurationMS uint64

	// OOM is true when this sample's query terminated in the OOM / cost danger
	// zone — ExitStatus oom / timeout / sample_budget (the three terminal
	// outcomes route B exists to avoid). These are the frontier-defining
	// catastrophes.
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
	// run detected: the smallest Fanout / (N×F) anchor-pair product at which
	// the corpus showed OOM/cost-danger samples. Zero when no danger frontier
	// was observed. Reported even on a no-op so operators see the evidence.
	FrontierFanout      int
	FrontierAnchorPairs int
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
//     exemplars → return `defaults` UNCHANGED with NoOp set. (Squid today is
//     exactly this shape — all route A, below-threshold=0, zero failures — so
//     Calibrate is a true verbatim no-op there.)
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
	// strictly BEFORE it, then apply tighten-only + safety-floor clamps.
	calibrated := defaults

	wantFanout := applyMargin(frontierF)
	wantPairs := applyMargin(frontierPairs)

	calibrated.MinFanout = clampTighten(
		defaults.MinFanout, wantFanout, minCalibratedFanout,
	)
	calibrated.MinAnchorPairs = clampTighten(
		defaults.MinAnchorPairs, wantPairs, minCalibratedAnchorPairs,
	)

	if calibrated.MinFanout != defaults.MinFanout {
		report.Changes = append(report.Changes, ThresholdChange{
			Field: "MinFanout",
			From:  defaults.MinFanout,
			To:    calibrated.MinFanout,
			Why:   "observed OOM/cost-danger at lower fanout; shard before the local frontier",
		})
	}
	if calibrated.MinAnchorPairs != defaults.MinAnchorPairs {
		report.Changes = append(report.Changes, ThresholdChange{
			Field: "MinAnchorPairs",
			From:  defaults.MinAnchorPairs,
			To:    calibrated.MinAnchorPairs,
			Why:   "observed OOM/cost-danger at fewer anchor-pairs; shard before the local frontier",
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

// scanFrontier finds, across all samples, the smallest Fanout and smallest
// (N×F) anchor-pair product at which the deployment observed OOM/cost-danger
// dispatches, plus the total danger-sample count and whether any
// below-threshold decision exists. Deterministic: samples are sorted before
// scanning so the result never depends on input order.
func scanFrontier(samples []CorpusSample) (frontierF, frontierPairs, dangerCount int, sawBelowThreshold bool) {
	ordered := make([]CorpusSample, len(samples))
	copy(ordered, samples)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Fanout != b.Fanout {
			return a.Fanout < b.Fanout
		}
		ap, bp := a.NAnchors*a.Fanout, b.NAnchors*b.Fanout
		if ap != bp {
			return ap < bp
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

		if s.Fanout > 0 && (frontierF == 0 || s.Fanout < frontierF) {
			frontierF = s.Fanout
		}
		pairs := s.NAnchors * s.Fanout
		if pairs > 0 && (frontierPairs == 0 || pairs < frontierPairs) {
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

// clampTighten enforces the tighten-only + safety-floor rails for an int
// threshold: the calibrated value `want` is accepted ONLY if it is strictly
// LOWER than the shipped default (the safer direction — shard more readily)
// AND not below the shipped floor. Otherwise the default is kept verbatim.
// This is the load-bearing safety invariant: Calibrate can never RAISE a
// threshold, and never lower past the floor.
func clampTighten(def, want, floor int) int {
	if want < floor {
		want = floor
	}
	if want >= def {
		// Not a tightening (would loosen or no-op) — keep the default.
		return def
	}
	return want
}
