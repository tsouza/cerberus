//go:build chdb

// autotune_chdb_lane_test.go — the self-driving autotune loop's safety property
// proven END TO END through a real ClickHouse SQL path (chDB), closing the loop
// the fakes-only unit lanes leave open.
//
// # WHY THIS LANE EXISTS
//
// The autotune loop's safety property — "every eligible route-A OOM shape in the
// corpus provably clears the LIVE gate and would route B after a fit" — is
// already covered structurally by two fakes-based unit tests:
//
//   - autotune_test.go: the Fit clamp math over a fake OOMFloorSource;
//   - solver/planner_autotune_test.go (TestPlan_SetThresholds_FlipsRouting): the
//     SetThresholds hot-swap flips one plan A→B, with hand-set thresholds.
//
// Neither drives the OOM floor through the production ClickHouse read path. The
// floor is computed by a real aggregate SELECT (min(fanout), min(n_anchors),
// count() under the route='A' AND exit_status='oom' AND
// decision_reason='below-threshold' AND fanout>0 AND n_anchors>0 predicate) whose
// wire-type / predicate behaviour only a real CH engine exercises. This test
// seeds that exact corpus table in chDB, fits the thresholds from it through
// NewCHOOMFloorSource, hot-swaps them onto a live *solver.Planner, and asserts the
// known-OOM shape — left on route A by the shipped default gate — now routes B.
//
// It is the chDB-backed counterpart of the in-process flip test: same property,
// but the gate the fit derives comes from real SQL over a seeded corpus, not a
// fake floor. Gated by the `chdb` build tag (libchdb.so required); it runs in the
// `just test-chdb` handler-tests lane (chdb.yml), alongside the corpus parity
// tests whose harness (openParityChDB / seedParityTableFromRows / sqlDBConn) it
// reuses.
package routerrules

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/solver"
)

// The seeded corpus + the plan under test share one grid so the story is exact:
// a 1h window at a 15s step yields N = 3600/15 + 1 = 241 anchors, and a shape's
// fan-out is F = Range/Step. The OOM floor the corpus records is oomFloorFanout,
// deliberately BELOW the shipped default MinFanout (16): the shape is left on
// route A by the default gate and only flips to B once the fit lowers the gate to
// the observed floor.
const (
	autotuneGridStep    = 15 * time.Second
	autotuneGridWindow  = time.Hour
	autotuneGridAnchors = 241 // N = autotuneGridWindow/autotuneGridStep + 1

	// oomFloorFanout is the single source of truth tying the seeded OOM signal
	// to the plan under test: the corpus's minimum route-A OOM fan-out AND the
	// plan's F. It is below defaultMinFanout(16) so the default gate routes it A.
	oomFloorFanout = 8

	// A second, HIGHER eligible OOM row proves min(fanout)/min(n_anchors) pick
	// the floor row, not this one.
	higherOOMFanout  = 12
	higherOOMAnchors = 300

	// An excluded route-A OOM: same route+status but decision_reason is NOT
	// 'below-threshold', so the OOMFloor predicate must filter it out. Its
	// fan-out (4) is below the floor, so a broken predicate that let it through
	// would drop OOMMinFanout to 4 — caught by the exact OOMMinFanout assertion.
	excludedOOMFanout  = 4
	excludedOOMAnchors = 100

	// belowFloorFanout is the negative-control plan's F: below the fitted gate
	// (oomFloorFanout), so it must STAY on route A even after the fit — proof the
	// lowered gate still discriminates rather than routing everything.
	belowFloorFanout = 4
)

// TestAutotune_CHFit_FlipsKnownOOMShapeToRouteB is the end-to-end safety proof:
// a route-A, below-threshold OOM shape recorded in the corpus at a fan-out below
// the shipped default gate is, after the loop fits and applies thresholds from
// that corpus through the real ClickHouse SQL path, no longer left on route A —
// it routes B. The fit's floor is derived by NewCHOOMFloorSource's aggregate
// SELECT over a chDB-seeded corpus, not a fake.
func TestAutotune_CHFit_FlipsKnownOOMShapeToRouteB(t *testing.T) {
	ctx := context.Background()

	// --- Seed the corpus with a route-A OOM floor below the default gate. ---
	db := openParityChDB(t)
	seedParityTableFromRows(t, db, []jsonlRow{
		oomRow(oomFloorFanout, autotuneGridAnchors, reasonBelowThreshold),
		oomRow(higherOOMFanout, higherOOMAnchors, reasonBelowThreshold),
		// Excluded: not a below-threshold cost-gate OOM, so it must not lower
		// the floor to excludedOOMFanout(4).
		oomRow(excludedOOMFanout, excludedOOMAnchors, "not-sliceable"),
	})

	// --- Fit thresholds from that corpus through the real CH read path. ---
	// The current gate is the shipped default so the fit is what a fresh
	// deployment's first tick sees.
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	current := Thresholds{MinFanout: cfg.MinFanout, MinAnchorPairs: cfg.MinAnchorPairs}

	src := NewCHOOMFloorSource(&sqlDBConn{db: db}, 0)
	res, err := NewAutotuner(src).Fit(ctx, current)
	if err != nil {
		t.Fatalf("fit over chDB corpus: %v", err)
	}

	// The floor is the MINIMUM over the eligible below-threshold OOM population:
	// fan-out 8 (not the higher row's 12, not the excluded row's 4), anchors 241.
	if !res.HasOOMSignal {
		t.Fatalf("expected an OOM signal from the seeded corpus, got none: %+v", res)
	}
	if res.OOMMinFanout != oomFloorFanout {
		t.Fatalf("OOMMinFanout = %d, want %d (predicate must pick the floor row, excluding non-below-threshold OOMs)",
			res.OOMMinFanout, oomFloorFanout)
	}
	if res.OOMMinAnchors != autotuneGridAnchors {
		t.Fatalf("OOMMinAnchors = %d, want %d", res.OOMMinAnchors, autotuneGridAnchors)
	}
	if !res.Changed {
		t.Fatalf("fit must lower the gate below the default; Changed=false, candidate=%+v", res.Candidate)
	}
	// The structural safety clamp: the candidate never sits above the observed
	// OOM fan-out floor, so the OOM shape provably clears the live gate.
	if res.Candidate.MinFanout > res.OOMMinFanout {
		t.Fatalf("candidate MinFanout %d exceeds observed OOM floor %d (safety clamp violated)",
			res.Candidate.MinFanout, res.OOMMinFanout)
	}
	if res.Candidate.MinFanout >= current.MinFanout {
		t.Fatalf("candidate MinFanout %d did not lower below the default %d",
			res.Candidate.MinFanout, current.MinFanout)
	}

	// --- The known-OOM shape: below the default gate, so it starts on route A. ---
	p := &solver.Planner{Cfg: cfg}
	oomShape := rangeWindowFanout(oomFloorFanout)
	meta := autotuneMeta()

	d, routed := p.Plan(oomShape, meta)
	if routed || d.Reason != solver.ReasonBelowThreshold {
		t.Fatalf("pre-fit: routed=%v reason=%q, want route A (below-threshold) under the default gate",
			routed, d.Reason)
	}

	// --- Apply the fitted thresholds and re-plan: the OOM shape must now route B. ---
	p.SetThresholds(res.Candidate.MinFanout, res.Candidate.MinAnchorPairs)
	if gotF, gotP := p.CurrentThresholds(); gotF != res.Candidate.MinFanout || gotP != res.Candidate.MinAnchorPairs {
		t.Fatalf("CurrentThresholds = (%d, %d), want the fitted (%d, %d)",
			gotF, gotP, res.Candidate.MinFanout, res.Candidate.MinAnchorPairs)
	}

	d, routed = p.Plan(oomShape, meta)
	if !routed || d.Reason != solver.ReasonRouted {
		t.Fatalf("post-fit: routed=%v reason=%q, want route B (routed) — the known-OOM shape must NOT be left on route A",
			routed, d.Reason)
	}

	// --- Negative control: a shape below the FITTED gate still stays on route A,
	// proving the lowered gate is still a gate, not "route everything". ---
	d, routed = p.Plan(rangeWindowFanout(belowFloorFanout), meta)
	if routed || d.Reason != solver.ReasonBelowThreshold {
		t.Fatalf("post-fit control: routed=%v reason=%q, want route A (below-threshold) for a shape under the fitted gate",
			routed, d.Reason)
	}
}

// oomRow builds a corpus row for a route-A OOM at the given fan-out / anchor
// count and decision_reason, with the other cost-grid columns filled sanely (a
// 1h window at the shared grid step). Only route/exit_status/decision_reason/
// fanout/n_anchors are load-bearing for the OOMFloor predicate.
func oomRow(fanout, anchors float64, reason string) jsonlRow {
	return jsonlRow{
		EventTime:           1_700_000_000,
		ShapeID:             "shape",
		Language:            "promql",
		NormalizedQueryHash: 1,
		NAnchors:            anchors,
		Fanout:              fanout,
		CumulativeD:         float64((oomFloorFanout * autotuneGridStep).Seconds()),
		OuterRange:          float64(autotuneGridWindow.Seconds()),
		Step:                float64(autotuneGridStep.Seconds()),
		Route:               "A",
		KShards:             0,
		DecisionReason:      reason,
		ReadRows:            1,
		ReadBytes:           1,
		QueryDurationMS:     1,
		MemoryUsage:         1,
		ExitStatus:          "oom",
	}
}

// rangeWindowFanout builds a bare, slice-invariant RangeWindow on the shared grid
// whose fan-out F = Range/Step is exactly the requested value (Range = fanout ×
// step). A bare RangeWindow is eligible (see solver's rejection table), so the
// only thing standing between it and route B is the cost gate.
func rangeWindowFanout(fanout int) chplan.Node {
	start := time.Unix(1_700_000_000, 0).UTC()
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix", "Attributes"}},
		Func:            "rate",
		Range:           time.Duration(fanout) * autotuneGridStep,
		Step:            autotuneGridStep,
		OuterRange:      autotuneGridWindow,
		Start:           start,
		End:             start.Add(autotuneGridWindow),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// autotuneMeta is the request window matching the shared grid: 1h at a 15s step.
func autotuneMeta() solver.RequestMeta {
	start := time.Unix(1_700_000_000, 0).UTC()
	return solver.RequestMeta{
		Lang:  "promql",
		Start: start,
		End:   start.Add(autotuneGridWindow),
		Step:  autotuneGridStep,
	}
}
