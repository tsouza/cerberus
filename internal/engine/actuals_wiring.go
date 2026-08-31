package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// actuals_wiring.go is the engine-side half of cerberus issue #2789: it
// closes the loop the EXPLAIN ESTIMATE advisor (issue #2787,
// explain_estimate_wiring.go) and the cardinality pre-probe (issue #2788,
// cardinality_probe_wiring.go) leave open by feeding each advisory
// prediction, and every dispatch's real measured resource usage, into an
// Engine.Actuals tracker (internal/actuals) — the drift-detection core plus
// three thinner consumer hooks:
//
//  1. calibrateEstimate: a bounded correction to the K-clamp's advisory
//     estimate, folded into RequestMeta.Estimate at the SAME point
//     ScanEstimateAdvisor/CardinalityProbeAdvisor already populate it
//     (engine.go's classify) — "calibrate the solver's carrier-geometry
//     cost model" without solver itself needing to import actuals at all.
//  2. Route-memo priors with real magnitudes: routememo.Memo.
//     RecordActualMagnitude (internal/routememo/magnitude.go), wired from
//     per_rung_admission.go's existing perRungObservingCursor — see that
//     file's own doc.
//  3. Per-rung admission tightening: reuses PerRungAdmissionLearner.
//     SeedPriorFromEstimate (per_rung_admission.go, issue #2787's own
//     seeding mechanism) with a verdict derived from this shape's OWN
//     measured drift, one-directional exactly like the EXPLAIN ESTIMATE
//     seeding it sits beside — see maybeSeedPerRungAdmissionFromDrift.
//  4. The drift-detection core itself: applyActualsCapture tags every
//     dispatch so internal/chclient's packet fast path
//     (WithActualsCapture) and query_log_actuals.go's batch fallback both
//     feed the SAME Tracker, which alerts via
//     telemetry.RecordEstimateDrift whenever a shape's real usage diverges
//     from its EXPLAIN ESTIMATE prediction beyond the configured band.
//
// Every hook is reached ONLY through Engine.Actuals' own nil guard (the
// functions below), so an Engine that never wires a Tracker (the default —
// see actuals.Config.Enabled's own doc) is byte-unchanged, exactly like
// every other OPTIONAL mechanism in this package (RouteMemo,
// PerRungAdmission, ScanEstimateAdvisor, CardinalityProbeAdvisor).

// applyActualsCapture tags ctx for issue #2789's actuals capture: it
// records decision's advisory prediction (if any) into tracker, forces
// ClickHouse's log_comment to carry decision.ShapeID (independent of
// SettingsRules.LogCommentShape — see below), and layers
// chclient.WithActualsCapture so the native-protocol packet fast path can
// record the real observation once the dispatch completes.
//
// A no-op (ctx returned unchanged) when tracker is nil, decision is nil, or
// decision.ShapeID is empty (engine.classify never set RequestMeta.ShapeID
// because Actuals was nil at classification time) — every existing
// dispatch path is reachable and byte-identical with the feature off.
//
// log_comment is stamped here REGARDLESS of SettingsRules.LogCommentShape:
// that flag governs a SEPARATE, purely-observability concern (an operator
// manually clustering system.query_log by log_comment), while actuals
// capture's OWN query_log fallback path (query_log_actuals.go) needs
// log_comment populated to correlate a row back to its shape whether or not
// the operator separately opted into the observability flag. Both write
// through the same chclient.WithQuerySetting key, so an operator who has
// BOTH flags on pays no double-stamp cost — the second write is an
// idempotent overwrite with the identical value (SettingsRules.apply always
// derives the SAME planShapeID for the SAME plan).
func applyActualsCapture(ctx context.Context, tracker *actuals.Tracker, decision *solver.Decision) context.Context {
	if tracker == nil || decision == nil || decision.ShapeID == "" {
		return ctx
	}
	if decision.HasPredictedEstimate {
		tracker.RecordPredicted(decision.ShapeID, decision.PredictedRows)
	}
	ctx = chclient.WithQuerySetting(ctx, settingLogComment, decision.ShapeID)
	return chclient.WithActualsCapture(ctx, tracker, decision.ShapeID)
}

// calibrateEstimate applies Hook 1 (this file's own doc) to est: a bounded
// correction (actuals.Tracker.CalibrationFactor's own [0.5, 2.0] clamp)
// derived from shapeID's tracked predicted-vs-actual history, folded into a
// COPY of est (the caller's original is never mutated) before it reaches
// the K clamp.
//
// Returns est UNCHANGED (same pointer, not even a copy) when e.Actuals is
// nil, est is nil, est.Rows is already zero (nothing to scale), or the
// shape has not yet accumulated enough evidence for a factor
// (CalibrationFactor's own ok=false) — the overwhelmingly common case,
// exactly like every other advisory signal in this file staying inert
// until the feature is both wired AND has real evidence to act on.
func (e *Engine) calibrateEstimate(shapeID string, est *solver.ScanEstimate) *solver.ScanEstimate {
	if e.Actuals == nil || est == nil || est.Rows == 0 {
		return est
	}
	factor, ok := e.Actuals.CalibrationFactor(shapeID)
	if !ok {
		return est
	}
	// factor is always in [0.5, 2.0] (CalibrationFactor's own clamp) and
	// est.Rows is unsigned, so the product can never go negative — no
	// defensive floor needed before the conversion below.
	calibrated := *est
	calibrated.Rows = uint64(float64(est.Rows) * factor)
	return &calibrated
}

// maybeSeedPerRungAdmissionFromActuals applies Hook 3 (this file's own
// doc): seeds e.PerRungAdmission with a prior derived from shapeID's OWN
// tracked actual-rows EMA, reusing PerRungAdmissionLearner.
// SeedPriorFromEstimate's existing one-directional (cheap=true only)
// contract — the SAME mechanism explain_estimate_wiring.go's own
// maybeSeedPerRungPrior already uses for a live EXPLAIN ESTIMATE round
// trip, applied here to a ZERO-I/O read of state a PAST dispatch already
// recorded. The comparison mirrors PerRungAdmissionLearner.Observe's own
// (outputRows < nAnchors*perRungCheapRowsPerAnchor) — same threshold, same
// "cheap relative to anchor count" reasoning — but against the tracked
// ACTUAL scan-rows EMA rather than a freshly-drained composed-output count,
// so it can fire on the shape's FIRST request of a session instead of
// waiting for that same shape to drain cleanly through the per-rung bypass
// at all.
//
// No-op when e.PerRungAdmission or e.Actuals is nil, baseline carries no
// anchor count, or the shape's actuals snapshot has not yet reached
// perRungEvidenceMinObservations (the SAME corroboration floor
// Observe/ShouldDeclineBypass already require) — a single anomalous
// actuals reading must never seed a prior any more than a single real
// drain would.
func (e *Engine) maybeSeedPerRungAdmissionFromActuals(plan chplan.Node, shapeID string, baseline *solver.Decision) {
	if e.PerRungAdmission == nil || e.Actuals == nil || shapeID == "" || baseline == nil || baseline.NAnchors <= 0 {
		return
	}
	report, ok := e.Actuals.Snapshot(shapeID)
	if !ok || report.Observations < perRungEvidenceMinObservations {
		return
	}
	// Real actual-rows EMAs never approach int64 overflow (~9.2e18).
	if int64(report.ActualEMARows) >= int64(baseline.NAnchors)*perRungCheapRowsPerAnchor { //nolint:gosec // G115
		return
	}
	e.PerRungAdmission.SeedPriorFromEstimate(shapeKey(plan, baseline), true)
}

// recordEstimateDriftFromQueryLog forwards one query_log-sourced
// DriftReport to telemetry.RecordEstimateDrift, tagged
// actuals.SourceQueryLog — the slow-path counterpart to
// internal/chclient/progress.go's own packet-path call, kept as a tiny
// named wrapper (rather than inlined at query_log_actuals.go's own call
// site) purely so both drift-emission call sites read as the SAME one-line
// shape.
func recordEstimateDriftFromQueryLog(ctx context.Context, report actuals.DriftReport) {
	telemetry.RecordEstimateDrift(ctx, report.Ratio, report.Alerting, actuals.SourceQueryLog.String())
}
