package solver

// Self-tuning loop: the GENERIC background driver that, when enabled, reads
// THIS deployment's router corpus on a fixed cadence, runs the pure Calibrate
// over it, and atomically swaps the freshly calibrated Config into the Planner.
//
// What ships vs what is learned (mirrors calibrate.go):
//
//   - GENERIC (shipped): this loop, its cadence, the fail-open behaviour, and
//     the conservative defaults it starts from. Deployment-independent.
//   - LOCAL (never shipped): the Config the loop installs, derived at runtime
//     from FrontierReader.ReadFrontier (this deployment's corpus). Off by
//     default — nothing starts unless the operator flips CERBERUS_ROUTE_SELFTUNE.
//
// The loop derives its context from a cancelable parent and exits on cancel —
// it does NOT root anything in context.Background() without cancellation (the
// C1 breaker-recovery bug class). A read or calibrate error keeps the current
// Config, logs, and never crashes (fail-open).

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// EnvRouteSelfTune is the master flag for per-deployment self-tuning. Default
// OFF: unset / false means the Planner runs the static shipped Config and this
// loop never starts, so existing deployments see ZERO behaviour change. Set to
// a strconv.ParseBool-true value to enable.
const EnvRouteSelfTune = "CERBERUS_ROUTE_SELFTUNE"

// defaultSelfTuneInterval is the recalibration cadence. Sized long: the corpus
// frontier moves on the timescale of deployment workload shifts, not seconds,
// and each tick runs a rate-limited corpus read, so a slow cadence keeps the
// data-plane impact negligible while still adapting within an hour.
const defaultSelfTuneInterval = 15 * time.Minute

// FrontierReader is the GENERIC seam between the loop and a deployment's
// corpus. ReadFrontier returns the aggregated frontier buckets (a few
// aggregate SELECTs per (N,F[,D]) bucket, NOT every row) for THIS deployment.
// The concrete implementation lives in the corpus-reader wiring (cmd/cerberus
// maps cerberus_router_corpus rows into []CorpusSample); the loop depends only
// on this interface so the solver package stays free of any CH driver or
// optcorpus dependency.
//
// A nil error with an empty slice is a legitimate "no signal yet" — Calibrate
// fails open on it. A non-nil error is logged and the current Config is kept.
type FrontierReader interface {
	ReadFrontier(ctx context.Context) ([]CorpusSample, error)
}

// SelfTuner is the lifecycle handle for one Planner's background calibration
// goroutine. Created and started by StartSelfTuner; Stop cancels the loop ctx
// and joins the goroutine so shutdown is goleak-clean.
type SelfTuner struct {
	planner  *Planner
	reader   FrontierReader
	defaults Config
	interval time.Duration
	logger   *slog.Logger

	cancel   context.CancelFunc
	doneCh   chan struct{}
	stopOnce sync.Once
}

// SelfTuneParams groups StartSelfTuner's inputs. defaults is the SHIPPED
// conservative Config (the calibration floor / fail-open fallback); it is the
// only Config the loop trusts as a baseline and the value Calibrate returns
// verbatim on a no-op. interval <= 0 falls back to defaultSelfTuneInterval.
// A nil logger falls back to slog.Default so the loop always logs its swaps.
type SelfTuneParams struct {
	Planner  *Planner
	Reader   FrontierReader
	Defaults Config
	Interval time.Duration
	Logger   *slog.Logger
}

// StartSelfTuner launches the background calibration loop against a CANCELABLE
// child of parent and returns the handle. The loop runs one calibration pass
// immediately (so a restart picks up the learned frontier without waiting a
// full interval), then on every interval tick, until Stop or parent cancel.
//
// Returns nil if planner or reader is nil — callers may then skip Stop safely
// (Stop is a no-op on a nil receiver). The default-off wiring in cmd/cerberus
// simply never calls this.
func StartSelfTuner(parent context.Context, params SelfTuneParams) *SelfTuner {
	if params.Planner == nil || params.Reader == nil {
		return nil
	}
	interval := params.Interval
	if interval <= 0 {
		interval = defaultSelfTuneInterval
	}
	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(parent)
	st := &SelfTuner{
		planner:  params.Planner,
		reader:   params.Reader,
		defaults: params.Defaults,
		interval: interval,
		logger:   logger,
		cancel:   cancel,
		doneCh:   make(chan struct{}),
	}
	go st.run(ctx)
	return st
}

// run is the goroutine body: an immediate pass, then a ticker loop. It exits
// (closing doneCh) as soon as ctx is cancelled.
func (st *SelfTuner) run(ctx context.Context) {
	defer close(st.doneCh)

	st.recalibrate(ctx)

	ticker := time.NewTicker(st.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.recalibrate(ctx)
		}
	}
}

// recalibrate runs one read → Calibrate → atomic-swap cycle. Fail-open: any
// reader error keeps the current Config and returns; Calibrate's own no-op
// path returns the defaults verbatim, so a no-signal corpus reinstalls the
// conservative defaults (never a partially-learned Config). Every swap and
// no-op is logged so operators can SEE what their deployment self-tuned to.
func (st *SelfTuner) recalibrate(ctx context.Context) {
	samples, err := st.reader.ReadFrontier(ctx)
	if err != nil {
		// Fail-open: a corpus-read failure (CH busy, timeout, table missing)
		// must never disturb the data plane or crash the loop. Keep the
		// current Config and try again next tick.
		st.logger.Warn("route self-tune: corpus read failed; keeping current config",
			"err", err)
		return
	}

	calibrated, report := Calibrate(samples, st.defaults)

	if report.NoOp {
		// Reinstall the conservative defaults verbatim (Calibrate returned
		// them unchanged). This is the no-signal path: all route A,
		// below-threshold=0, no failures → defaults stay in force.
		st.planner.SetConfig(calibrated)
		st.logger.Info("route self-tune: no-op (local corpus carries no actionable signal)",
			"samples", report.SampleCount,
			"reason", report.NoOpReason,
			"min_fanout", calibrated.MinFanout,
			"min_anchor_pairs", calibrated.MinAnchorPairs)
		return
	}

	st.planner.SetConfig(calibrated)
	st.logger.Info("route self-tune: installed locally-calibrated thresholds",
		"samples", report.SampleCount,
		"frontier_fanout", report.FrontierFanout,
		"frontier_anchor_pairs", report.FrontierAnchorPairs,
		"min_fanout", calibrated.MinFanout,
		"min_anchor_pairs", calibrated.MinAnchorPairs,
		"changes", changeStrings(report.Changes))

	// Surface the floor/margin interaction: when the shipped safety floor bound
	// a calibrated threshold at or above the margin-reduced frontier, the safety
	// margin is reduced (or, in the sub-floor case, the floor would have gated
	// above the OOM coordinate and the calibrator capped strictly below the
	// frontier instead). Operators must SEE this — the floor previously moved
	// silently, defeating the safety margin near the floor without a trace.
	if report.FloorClampedFanout || report.FloorClampedAnchorPairs {
		st.logger.Warn("route self-tune: safety floor swallowed the margin near the frontier",
			"floor_clamped_fanout", report.FloorClampedFanout,
			"floor_clamped_anchor_pairs", report.FloorClampedAnchorPairs,
			"frontier_fanout", report.FrontierFanout,
			"frontier_anchor_pairs", report.FrontierAnchorPairs,
			"min_fanout", calibrated.MinFanout,
			"min_anchor_pairs", calibrated.MinAnchorPairs,
			"min_calibrated_fanout", minCalibratedFanout,
			"min_calibrated_anchor_pairs", minCalibratedAnchorPairs)
	}
}

// changeStrings renders the report's threshold moves into a flat []string for
// structured logging (def→calibrated per field with its reason).
func changeStrings(changes []ThresholdChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

// String renders one ThresholdChange as "Field: from→to (why)".
func (c ThresholdChange) String() string {
	return c.Field + ": " + strconv.Itoa(c.From) + "→" + strconv.Itoa(c.To) + " (" + c.Why + ")"
}

// Stop cancels the loop ctx and blocks until the goroutine has exited, so a
// caller's Close is goleak-clean. Idempotent and nil-safe.
func (st *SelfTuner) Stop() {
	if st == nil {
		return
	}
	st.stopOnce.Do(func() {
		st.cancel()
	})
	<-st.doneCh
}
