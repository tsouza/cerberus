// Package actuals implements the bounded predicted-vs-actual drift tracker
// for cerberus issue #2789: it closes the loop EXPLAIN ESTIMATE's pre-flight
// advisor (issue #2787) and the cardinality pre-probe (issue #2788) leave
// open — both predict a plan's scan cost before dispatch, and neither is
// ever checked against what the query actually consumed. Without that check,
// EXPLAIN ESTIMATE's marks-level bias under PREWHERE, skip indexes, and the
// filesystem cache is invisible, and mis-admission is silent.
//
// A Tracker records, per literal-free plan-shape id (the SAME "cerb:..." id
// internal/engine's SettingsRules already stamps into ClickHouse's
// log_comment — see internal/engine/plan_shape_id.go), the most recent
// advisory prediction and a BOUNDED exponential moving average of the real
// resource usage actually observed for that shape. Two independent sources
// feed the actual side: the native-protocol progress/ProfileEvents packets
// (internal/chclient/progress.go, the fast path — free, since the
// production deployment's driver already streams them) and
// system.query_log (internal/engine's query-log reconciler, the slow
// batch/fallback path for a query packet capture missed — a query that
// failed before completing, or a deployment mode where packet capture is
// not wired).
//
// Package is a pure, dependency-free leaf (stdlib only, exactly like
// internal/routememo) so it is importable from both chclient (the packet
// capture site, BELOW engine in the layering) and engine (every consumer)
// without an import cycle — see .go-arch-lint.yml's own comment on this
// component.
//
// Anti-autotune stance (issue #2788's own precedent, restated here because
// this package inherits it): a Tracker is a bounded, advisory INPUT to
// existing policies — the calibration factor it reports is clamped
// (minCalibrationFactor/maxCalibrationFactor) and every EMA update is capped
// per-observation influence (Config.EMAAlpha) — never a new curve-fitting
// loop. It does not decide anything itself; it only tells an existing
// mechanism (the K clamp, the per-rung admission learner, the route memo,
// an operator's dashboard) that a shape's prior looks wrong. This is the
// same anti-autotune boundary the retired threshold-fitting autotune
// (issue #1273) crossed and cerberus does not repeat.
package actuals

import (
	"fmt"
	"time"
)

// Config tunes the Tracker. Every field maps to a CERBERUS_QUERY_ACTUALS_*
// env var parsed by ConfigFromEnv (config_env.go) — kept in this package
// rather than internal/config, mirroring internal/solver's own Config
// (solver/config.go: "kept in this package rather than internal/config to
// avoid an import cycle; this package owns the defaults and the
// invariants").
//
// Enabled is the master kill switch (CERBERUS_QUERY_ACTUALS_ENABLED,
// DEFAULT FALSE): this is a brand-new, unvalidated advisory layer over
// mechanisms that already work correctly without it (the EXPLAIN ESTIMATE
// advisor, the cardinality pre-probe, the failure-driven route memo, the
// per-rung admission learner all remain reachable and correct with Enabled
// false — see cmd/cerberus's wiring). It is NOT a chopt registry entry —
// ProfileEvents on the native protocol and system.query_log are both
// ancient, always-available ClickHouse surfaces with no version floor to
// probe — so this is a plain solver-policy config knob, mirroring
// solver.Config.AdaptiveEnabled's own CERBERUS_SOLVER_ADAPTIVE_ENABLED
// pattern rather than an chopt.EnabledSet feature.
type Config struct {
	// Enabled is the master switch. See this type's own doc.
	Enabled bool

	// DriftLowerRatio / DriftUpperRatio (CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO
	// / CERBERUS_QUERY_ACTUALS_DRIFT_UPPER_RATIO) bound the "expected" band for
	// actualEMA/predicted. A ratio outside [DriftLowerRatio, DriftUpperRatio]
	// after MinObservations flags the shape as drift-alerting (see
	// DriftReport.Alerting). EXPLAIN ESTIMATE is a granule-resolution UPPER
	// BOUND (internal/chclient/explain_estimate.go's own doc), so the actual is
	// EXPECTED to run below the prediction most of the time — the band is
	// deliberately asymmetric around 1.0 rather than a symmetric +/-X%.
	DriftLowerRatio float64
	DriftUpperRatio float64

	// MinObservations (CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS) is the
	// corroboration floor before a shape's drift verdict is trusted at all —
	// mirroring internal/engine's own perRungEvidenceMinObservations /
	// routememo's minCorroboratingFailures: a single anomalous actual must
	// never, by itself, flip a shape's verdict. This is the anti-autotune
	// bound on OBSERVATION COUNT; EMAAlpha below is the matching bound on
	// per-observation INFLUENCE.
	MinObservations int

	// EMAAlpha (CERBERUS_QUERY_ACTUALS_EMA_ALPHA) is the exponential moving
	// average's smoothing factor for the actual-rows side: each new
	// observation moves the tracked average by at most EMAAlpha of the
	// distance to that single sample
	// (emaRows += EMAAlpha * (observedRows - emaRows)). Bounding this below
	// 1.0 is what makes a burst of anomalous actuals unable to swing the
	// tracked state violently in one shot — the issue's own "keep feedback
	// advisory with BOUNDED influence" requirement, applied at the
	// per-observation level.
	EMAAlpha float64

	// EntryTTL (CERBERUS_QUERY_ACTUALS_ENTRY_TTL) bounds how long a shape's
	// tracked state is trusted before it ages out — mirroring
	// routememo.memoEntryTTL / the per-rung admission learner's
	// perRungEvidenceTTL: a shape's real cardinality can grow, and a verdict
	// computed against a stale window must not silently suppress recalibration
	// forever once that has happened.
	EntryTTL time.Duration

	// QueryLogPollInterval (CERBERUS_QUERY_ACTUALS_QUERY_LOG_POLL_INTERVAL) is
	// how often internal/engine's QueryLogActualsReconciler polls
	// system.query_log for the batch/fallback actuals source — mirroring
	// optcorpus's own CERBERUS_CH_OPT_CORPUS_INTERVAL default (60s).
	// query_log's own async flush lag (docs/operations.md) makes this
	// inherently a slow path; the reconciler is designed for that latency, not
	// for actuals to arrive synchronously with the query that produced them.
	QueryLogPollInterval time.Duration

	// QueryLogLookback (CERBERUS_QUERY_ACTUALS_QUERY_LOG_LOOKBACK) is how far
	// back the FIRST poll looks before any watermark exists, and the overlap
	// margin used if a poll ever needs to recover after an error — sized well
	// above QueryLogPollInterval so a slow query_log flush (or one missed poll
	// tick) never drops a row between two polls.
	QueryLogLookback time.Duration
}

// Default tuning constants (this package's own calibration surface — no
// production measurement exists yet for THIS feature, unlike
// solver.defaultMaxK's measured history, because the feature does not exist
// before this issue; these are conservative starting points an operator
// tunes from real deployment data via the env vars above).
const (
	// defaultDriftLowerRatio: an actual EMA at or below 10% of the EXPLAIN
	// ESTIMATE prediction is ordinary, not alerting — EXPLAIN ESTIMATE is a
	// granule-resolution upper bound (typically 8192-row granules), so a
	// tight predicate or an effective skip index routinely prunes far more
	// than a granule-only estimate can see. Below this floor the estimate is
	// still "in the right direction" (a real overestimate), just imprecise —
	// exactly the class of drift the issue says is expected and not the
	// dangerous direction.
	defaultDriftLowerRatio = 0.1

	// defaultDriftUpperRatio: an actual EMA at or above 3x the prediction is
	// the DANGEROUS direction — EXPLAIN ESTIMATE was supposed to be an upper
	// bound and real work exceeded it, which is exactly the "silent
	// mis-admission" the issue exists to surface. 3x is deliberately much
	// tighter than the lower band's 10x-in-the-safe-direction slack, because
	// this is the side that actually costs an incident.
	defaultDriftUpperRatio = 3.0

	// defaultMinObservations mirrors internal/engine's own
	// perRungEvidenceMinObservations and routememo's minCorroboratingFailures
	// (both 2): a single observation is never enough evidence to flip a
	// shape's advisory verdict.
	defaultMinObservations = 2

	// defaultEMAAlpha: each new observation moves the tracked average at most
	// 20% of the way toward that single sample, so a lone wildly-anomalous
	// actual can shift the EMA by at most 1/5 of the gap in one step —
	// several corroborating observations are needed to move it far, which is
	// the bounded-influence property the issue's anti-autotune stance
	// requires.
	defaultEMAAlpha = 0.2

	// defaultEntryTTL mirrors routememo.memoEntryTTL / the per-rung admission
	// learner's perRungEvidenceTTL (both 30m).
	defaultEntryTTL = 30 * time.Minute

	// defaultQueryLogPollInterval mirrors optcorpus's own
	// CERBERUS_CH_OPT_CORPUS_INTERVAL default (60s) — the established cadence
	// for a system.query_log background reconciler in this codebase.
	defaultQueryLogPollInterval = 60 * time.Second

	// defaultQueryLogLookback: 3x the poll interval gives two full missed
	// polls of overlap margin before a row could be dropped between two
	// watermarks — generous against query_log's own flush lag
	// (docs/operations.md), which is measured in seconds, not minutes.
	defaultQueryLogLookback = 3 * defaultQueryLogPollInterval
)

// DefaultConfig returns the conservative library defaults. Enabled is false
// — the feature ships dark, mirroring solver.DefaultConfig's Mode ==
// ModeSingle — so DefaultConfig is safe to wire as the in-process default
// without turning the feature on.
func DefaultConfig() Config {
	return Config{
		Enabled:              false,
		DriftLowerRatio:      defaultDriftLowerRatio,
		DriftUpperRatio:      defaultDriftUpperRatio,
		MinObservations:      defaultMinObservations,
		EMAAlpha:             defaultEMAAlpha,
		EntryTTL:             defaultEntryTTL,
		QueryLogPollInterval: defaultQueryLogPollInterval,
		QueryLogLookback:     defaultQueryLogLookback,
	}
}

// Validate fail-fast checks the invariants Tracker depends on. Mirrors
// solver.Config.Validate's split: ConfigFromEnv does not call this itself,
// the caller (cmd/cerberus) does, at startup.
func (c Config) Validate() error {
	if c.DriftLowerRatio <= 0 {
		return fmt.Errorf("actuals: DriftLowerRatio must be > 0, got %g", c.DriftLowerRatio)
	}
	if c.DriftUpperRatio <= c.DriftLowerRatio {
		return fmt.Errorf("actuals: DriftUpperRatio (%g) must be > DriftLowerRatio (%g)", c.DriftUpperRatio, c.DriftLowerRatio)
	}
	if c.MinObservations < 1 {
		return fmt.Errorf("actuals: MinObservations must be >= 1, got %d", c.MinObservations)
	}
	if c.EMAAlpha <= 0 || c.EMAAlpha > 1 {
		return fmt.Errorf("actuals: EMAAlpha must be in (0, 1], got %g", c.EMAAlpha)
	}
	if c.EntryTTL <= 0 {
		return fmt.Errorf("actuals: EntryTTL must be > 0, got %s", c.EntryTTL)
	}
	if c.QueryLogPollInterval <= 0 {
		return fmt.Errorf("actuals: QueryLogPollInterval must be > 0, got %s", c.QueryLogPollInterval)
	}
	if c.QueryLogLookback <= 0 {
		return fmt.Errorf("actuals: QueryLogLookback must be > 0, got %s", c.QueryLogLookback)
	}
	return nil
}
