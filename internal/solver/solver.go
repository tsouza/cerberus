package solver

import (
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Solver bundles the policy half (Planner) with the data-plane half
// (Executor) behind one engine-facing handle. The engine holds a
// *Solver and, at the QueryPlan / QueryPlanCursor seam between
// Optimizer.Run and chsql.Emit, calls Classify to compute the routing
// Decision (the shadow-header signal). A nil *Solver means the feature
// is fully off and every existing call path is byte-unchanged.
//
// Under the phase-1 default (Config.Mode == ModeSingle) the Planner
// classifies but NEVER routes, so Classify always returns routed=false
// and the engine stays on route A. The Executor is wired (so the
// phase-2 flip is a config change, not a code change) but dormant: the
// engine only reaches Executor.Execute when Classify reports routed,
// which never happens under ModeSingle.
type Solver struct {
	// Planner is the pure, read-only eligibility classifier. Required.
	Planner *Planner

	// Executor dispatches the K shard cursors on a true route. May be
	// nil in tests that exercise classification only; the engine guards
	// the routed branch on a non-nil Executor as belt-and-braces, but
	// under ModeSingle the routed branch is never taken regardless.
	Executor *Executor

	// Cfg is the resolved solver configuration. Carried here so the
	// engine can read Cfg.Mode without reaching into Planner.Cfg.
	Cfg Config
}

// New builds a Solver from a validated Config plus the data-plane
// dependencies the Executor needs. The Planner is constructed from cfg;
// the Executor is wired with the same cfg plus the gate / breaker /
// admit / emitter hooks. Passing a nil emitter / gate / breaker / admit
// is legal — the Executor degrades each to its documented no-op — but a
// production wiring supplies all four. The returned Solver is dormant
// until cfg.Mode flips off "single".
func New(cfg Config, emitter SQLEmitter, deps ExecDeps) *Solver {
	return &Solver{
		Planner: &Planner{Cfg: cfg},
		Executor: &Executor{
			Client:  deps.Client,
			Emitter: emitter,
			Cfg:     cfg,
			Gate:    deps.Gate,
			GateCap: deps.GateCap,
			Breaker: deps.Breaker,
			Admit:   deps.Admit,
		},
		Cfg: cfg,
	}
}

// ExecDeps groups the data-plane hooks the Executor needs so New stays a
// two-argument constructor (the emitter is split out because the engine
// adapter owns it). Every field is optional in the documented-no-op
// sense; production wiring supplies all of them.
type ExecDeps struct {
	// Client opens the per-shard cursors (*chclient.Client in prod).
	Client CursorQuerier
	// Gate is the GLOBAL connection semaphore shared across heads.
	Gate *semaphore.Weighted
	// GateCap is the Gate's total size (semaphore.Weighted hides it).
	GateCap int64
	// Breaker peeks the circuit state pre-flight (*chclient.Client).
	Breaker breakerPeeker
	// Admit is the two-stage weighted-admission hook (*admit.Limiter).
	Admit admitTopUp
}

// Classify runs the Planner over the optimized plan + request grid and
// returns the Decision (always non-nil) plus whether the plan routes B.
// The Decision carries the Strategy / K / Reason the engine stamps onto
// the shadow header regardless of the route outcome.
//
// classification is gated to the PromQL head: meta.Lang must be
// LangPromQL. For any other head Classify returns a not-classified
// sentinel so the engine skips the solver entirely and the shadow
// header is omitted (the foundation deferred the Lang gate to the other
// two heads).
func (s *Solver) Classify(plan chplan.Node, meta RequestMeta) (*Decision, bool) {
	if s == nil || s.Planner == nil {
		return nil, false
	}
	if meta.Lang != LangPromQL {
		return nil, false
	}
	return s.Planner.Plan(plan, meta)
}

// LangPromQL is the head name the solver classifies. Phase 1 routes the
// PromQL query_range matrix family only; the other heads skip the solver
// entirely (Classify returns not-classified for them). It mirrors the
// engine's Lang.Name() string for the Prom head.
const LangPromQL = "promql"

// GridOf extracts the request's OUTER eval grid (Start, End, Step) from
// the optimized plan by finding the outermost grid-bearing carrier. The
// engine.Meta type intentionally does NOT carry the grid (it stays an
// engine-internal type), so the solver derives the grid from the plan
// the same way the emitter reads it.
//
// The carrier is the first (outermost, depth-first pre-order) node that
// owns an eval grid: a StepGrid, or a RangeWindow / RangeLWR /
// RangeBucketFanout with Step > 0. The dominant routed shape
// sum(rate(m[5m])) carries its grid on the outermost RangeWindow. For an
// instant / non-range plan no carrier with Step > 0 exists, so GridOf
// returns a zero grid (Step == 0) and the Planner routes A on the
// (2)-prefix instant guard.
//
// GridOf only reads the OUTER bounds; the Planner re-walks the full
// spine to validate inner-grid commensurability and grid-prediction.
func GridOf(plan chplan.Node) (start, end time.Time, step time.Duration) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		if step > 0 {
			// Outer carrier already found; stop descending.
			return false
		}
		switch v := n.(type) {
		case *chplan.StepGrid:
			if v.Step > 0 {
				start, end, step = v.Start, v.End, v.Step
			}
		case *chplan.RangeWindow:
			if v.Step > 0 {
				start, end, step = v.Start, v.End, v.Step
			}
		case *chplan.RangeLWR:
			if v.Step > 0 {
				start, end, step = v.Start, v.End, v.Step
			}
		case *chplan.RangeBucketFanout:
			if v.Step > 0 {
				start, end, step = v.Start, v.End, v.Step
			}
		}
		return true
	})
	return start, end, step
}
