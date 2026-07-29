package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The two distinct spine lookbacks the instant table below uses. Two values,
// not one, so a classifier that stamped a single hard-coded duration onto every
// instant refusal could not satisfy the table.
const (
	instantLookback     = 5 * time.Minute
	longInstantLookback = time.Hour
)

// instantRangeWindow is how internal/promql lowers an instant range-vector call
// (`rate(m[<lookback>])` evaluated through LowerAt): a matrix RangeWindow whose
// Range is the lookback, whose End is the single eval instant, and whose Step /
// Start / OuterRange are all left unset. That unset Step is exactly why GridOf
// finds no carrier and reports a zero grid, which is what makes the request
// meta's Step zero and the classification "instant".
func instantRangeWindow(lookback time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           lookback,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// instantAggregate wraps an instant range-vector call in the outer `sum(...)`
// of the dominant dashboard shape.
func instantAggregate(lookback time.Duration) chplan.Node {
	return &chplan.Aggregate{
		Input:    instantRangeWindow(lookback),
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// TestDecision_CostGrid_PopulatedOnInstant pins the third corner of the
// cost-grid contract, alongside the routed and route-A cases in
// decision_grid_test.go: an INSTANT query's refusal must carry the plan's real
// spine lookback.
//
// Every Decision reaches the calibration corpus, refusals included, and a
// refusal recorded with an all-zero grid is unreplayable — it says "we
// declined" without saying what we declined. CumulativeD is the whole answer
// for an instant query (there is no anchor grid to measure), so a zero D makes
// the instant population uninterpretable in aggregate.
//
// Each fixture derives its RequestMeta from GridOf exactly as engine.classify
// derives it in production, never hand-setting Step, so every row proves it is
// a GENUINE instant plan — one whose own grid is zero — rather than a
// meta/plan disagreement that merely simulates one.
func TestDecision_CostGrid_PopulatedOnInstant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		plan  func() chplan.Node
		wantD time.Duration
	}{
		{
			name:  "bare instant range-vector call",
			plan:  func() chplan.Node { return instantRangeWindow(instantLookback) },
			wantD: instantLookback,
		},
		{
			name:  "instant range-vector under an aggregate",
			plan:  func() chplan.Node { return instantAggregate(instantLookback) },
			wantD: instantLookback,
		},
		{
			// A different lookback must produce a different D: the recorded
			// value tracks the plan, it is not a constant.
			name:  "long-lookback instant range-vector",
			plan:  func() chplan.Node { return instantAggregate(longInstantLookback) },
			wantD: longInstantLookback,
		},
		{
			// An instant vector-vector binop lowers to a !StepAligned
			// VectorJoin over two independent instant spines. D is the SUM of
			// both arms' lookbacks, which is the cost the corpus needs — one
			// arm's Range would understate it by half. The refusal reason
			// stays "instant" (not "instant-join"): the Step<=0 gate is
			// definitional and precedes the join gate.
			name: "instant vector-vector binop over two spines",
			plan: func() chplan.Node {
				return &chplan.VectorJoin{
					Left:             instantAggregate(instantLookback),
					Right:            instantAggregate(instantLookback),
					Op:               chplan.OpDiv,
					MetricNameColumn: "MetricName",
				}
			},
			wantD: 2 * instantLookback,
		},
		{
			// A bare instant selector carries no windowed spine node at all,
			// so its honest D is zero — the plan has no lookback to report.
			// Pinned so a future reader knows an all-zero instant corpus row
			// is still legitimate for this family, and is not evidence the
			// grid was dropped.
			name: "bare instant selector carries no spine lookback",
			plan: func() chplan.Node {
				return &chplan.Filter{
					Input:     leafScan(),
					Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitString{V: "m"}},
				}
			},
			wantD: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan := tc.plan()

			// engine.classify builds RequestMeta from the plan's own grid
			// carrier; mirroring that here is what makes the fixture honest.
			start, end, step := GridOf(plan)
			if step != 0 {
				t.Fatalf("fixture is not an instant plan: GridOf step = %s, want 0", step)
			}
			meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

			p := &Planner{Cfg: autoCfg()}
			_, decision, k, eligible := p.classify(plan, meta)

			// The refusal itself is unchanged: instant queries never route.
			if eligible {
				t.Fatalf("instant plan reported eligible; it can never route B")
			}
			if k != 0 {
				t.Errorf("k = %d, want 0 on an instant refusal", k)
			}
			if decision.Reason != ReasonInstant {
				t.Fatalf("reason = %q, want %q", decision.Reason, ReasonInstant)
			}
			if decision.Strategy != "" {
				t.Errorf("Strategy = %q, want empty on a refusal", decision.Strategy)
			}

			// The positive claim: the refusal carries the plan's real lookback.
			if decision.CumulativeD != tc.wantD {
				t.Errorf("CumulativeD = %s, want %s — an instant refusal recorded with a zero "+
					"lookback is unreplayable in the calibration corpus",
					decision.CumulativeD, tc.wantD)
			}

			// A genuine instant plan carries no anchor grid, so N/F/OuterRange
			// and the request Step are zero because the PLAN and the REQUEST
			// say so — not because analyze was skipped. Non-zero here is
			// GridOf's silent-miss signature, pinned by the test below.
			if decision.NAnchors != 0 || decision.Fanout != 0 ||
				decision.OuterRange != 0 || decision.Step != 0 {
				t.Errorf("instant decision carries an anchor grid: N=%d F=%d OuterRange=%s Step=%s, want all zero",
					decision.NAnchors, decision.Fanout, decision.OuterRange, decision.Step)
			}

			// The same grid must survive both public entry points, which is
			// where the engine actually reads the Decision from.
			planD, planRouted := p.Plan(plan, meta)
			eligD, eligRouted := p.Eligible(plan, meta)
			for _, entry := range []struct {
				name   string
				d      *Decision
				routed bool
			}{
				{"Plan", planD, planRouted},
				{"Eligible", eligD, eligRouted},
			} {
				if entry.routed {
					t.Errorf("%s: instant plan routed (K=%d)", entry.name, entry.d.K)
					continue
				}
				if entry.d.Reason != ReasonInstant || entry.d.CumulativeD != tc.wantD {
					t.Errorf("%s: reason=%q CumulativeD=%s, want %q / %s",
						entry.name, entry.d.Reason, entry.d.CumulativeD, ReasonInstant, tc.wantD)
				}
			}
		})
	}
}

// TestDecision_CostGrid_InstantWithAnchorGridIsAMissedCarrier pins the
// DETECTION signature the instant grid buys: a RANGE plan the classifier was
// told is instant records reason=instant AND a non-zero anchor grid, so the two
// populations stay separable in the corpus instead of collapsing onto one
// all-zero row.
//
// The injection point is faithful, not contrived. engine.classify builds
// RequestMeta.Step from solver.GridOf(plan) verbatim, so "GridOf missed the
// carrier" IS "meta.Step == 0 for a plan whose own grid step is non-zero" —
// exactly the state constructed here. What is NOT constructible is a plan that
// makes GridOf miss: chplan's completeness ratchet closes the GridCarrier set,
// so every grid-declaring node is visible to GridOf by construction
// (TestGridOf_ReadsEveryCarrier pins that). This test therefore pins the
// weaker, honest property — that such a miss would be DIAGNOSABLE from the
// corpus rather than silent — which is the only half that is testable.
func TestDecision_CostGrid_InstantWithAnchorGridIsAMissedCarrier(t *testing.T) {
	t.Parallel()

	plan := oomWindow()
	if _, _, step := GridOf(plan); step == 0 {
		t.Fatalf("fixture must be a RANGE plan for this to model a missed carrier")
	}

	// The meta a missed carrier would hand the Planner: zero step, real plan.
	missedMeta := oomMeta()
	missedMeta.Step = 0

	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, missedMeta)
	if routed {
		t.Fatalf("a zero-step request must never route (K=%d)", d.K)
	}
	if d.Reason != ReasonInstant {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInstant)
	}
	// The smoking gun: an instant row that nonetheless carries an anchor grid.
	if d.NAnchors == 0 || d.Fanout == 0 || d.OuterRange == 0 {
		t.Errorf("missed-carrier decision recorded N=%d F=%d OuterRange=%s; a corpus row with "+
			"reason=instant and an all-zero grid is indistinguishable from a genuine instant query",
			d.NAnchors, d.Fanout, d.OuterRange)
	}
	if d.Step != 0 {
		t.Errorf("Step = %s, want 0 — Step mirrors the REQUEST grid, which is what went missing", d.Step)
	}
}
