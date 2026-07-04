// Package autotune is the solver's self-driving threshold controller. On a fixed
// cadence it refits the auto-mode cost thresholds (MinFanout, MinAnchorPairs)
// from the router corpus via routerrules.Autotuner and hot-swaps any certified
// change into the running solver.Planner.
//
// It is the composition of the corpus-fit "brain" (routerrules) with the policy
// half (solver): it depends on both and is imported by nothing but cmd/, which
// wires it with a corpus source and launches Run on the server lifecycle context.
// The fit only ever LOWERS a threshold toward the observed route-A OOM line, so a
// mis-fit adds route-B overhead but never a wrong answer or a new OOM (route A
// and route B are result-identical); see routerrules.Autotuner for the structural
// safety argument. The hot-swap is a single atomic pointer store
// (Planner.SetThresholds) — no lock touches the request hot path.
package autotune

import (
	"context"
	"log/slog"
	"time"

	"github.com/tsouza/cerberus/internal/routerrules"
	"github.com/tsouza/cerberus/internal/solver"
)

// Loop drives periodic corpus-fit → certify → hot-reload of the Planner's
// auto-mode thresholds.
type Loop struct {
	planner  *solver.Planner
	tuner    *routerrules.Autotuner
	interval time.Duration
	logger   *slog.Logger
}

// New builds a Loop. interval must be > 0 (solver.Config.Validate enforces this
// whenever Autotune is set, so cmd/ has already failed fast on a bad value).
func New(planner *solver.Planner, tuner *routerrules.Autotuner, interval time.Duration, logger *slog.Logger) *Loop {
	return &Loop{planner: planner, tuner: tuner, interval: interval, logger: logger}
}

// Run drives the loop until ctx is cancelled, then returns — leaving no
// goroutine behind (goleak-clean). A transient corpus-read failure is logged and
// skipped so one bad tick never stops self-tuning.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.tick(ctx)
		}
	}
}

// tick performs one fit-and-apply against the Planner's currently active
// thresholds. The fit is relative to the live gate, so successive ticks ratchet
// the thresholds down toward the observed OOM line and never churn once settled.
func (l *Loop) tick(ctx context.Context) {
	minFanout, minAnchorPairs := l.planner.CurrentThresholds()
	cur := routerrules.Thresholds{MinFanout: minFanout, MinAnchorPairs: minAnchorPairs}

	res, err := l.tuner.Fit(ctx, cur)
	if err != nil {
		l.logger.WarnContext(ctx, "autotune fit failed", "err", err)
		return
	}
	if !res.Changed {
		l.logger.DebugContext(ctx, "autotune no change",
			"reason", res.Reason,
			"min_fanout", cur.MinFanout,
			"min_anchor_pairs", cur.MinAnchorPairs)
		return
	}

	l.planner.SetThresholds(res.Candidate.MinFanout, res.Candidate.MinAnchorPairs)
	l.logger.InfoContext(ctx, "autotune applied",
		"reason", res.Reason,
		"prev_min_fanout", cur.MinFanout,
		"min_fanout", res.Candidate.MinFanout,
		"prev_min_anchor_pairs", cur.MinAnchorPairs,
		"min_anchor_pairs", res.Candidate.MinAnchorPairs,
		"oom_min_fanout", res.OOMMinFanout,
		"oom_min_anchors", res.OOMMinAnchors)
}
