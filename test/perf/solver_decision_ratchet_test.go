// Component D of the perf-assessment framework: the BIDIRECTIONAL
// routing-DECISION ratchet.
//
// Component B (`test/perf/profile`) measures compute fan-out; Component C
// (`cardinality_ratchet_test.go`) freezes the fan-factor ceiling. This file
// freezes the OTHER half of the solver story — the *routing decision* the
// solver's Planner reaches for every real query shape in the corpus. It is the
// regression net for `internal/solver`: a one-line change to a Planner
// threshold, a K-clamp, or an eligibility signal silently re-routes a slice of
// the corpus, and without a pinned baseline that change ships invisibly.
//
// # The corpus + the fixed grid
//
// The query shapes are the curated PromQL TXTAR corpus (`test/spec/promql`,
// the `-- query.promql --` section of every fixture). Reusing the spec corpus
// keeps the ratchet anchored to REAL query shapes that already round-trip
// through the engine — we never invent synthetic queries that could drift from
// what cerberus actually serves.
//
// Every query is evaluated on a SINGLE deterministic grid so the decision is a
// pure function of the query shape + the Planner:
//
//	end  = 2026-01-01T00:00:00Z   (fixed wall-clock; @-modifiers resolve against it)
//	range = 1h  → start = end - 1h
//	step = 15s  → N = 241 anchors
//
// Each query is parsed (reference Prometheus parser) → lowered
// (`promql.LowerAtRange` at the fixed grid) → optimized (`optimizer.Default`)
// → classified (`solver.Planner.Plan` under Mode=auto, the DefaultConfig
// thresholds). The recorded decision is {routed, K, reason}.
//
// # The ratchet semantics (bidirectional, no escape hatch)
//
// The committed baseline (`solver-decision-baseline.json`) is the reviewed
// snapshot of every query's decision. The test FAILS on ANY drift — route
// flipped, K changed, or reason changed — in EITHER direction. There is NO
// allow-list, NO tolerance band, NO "expected drift" set: a silent change to a
// routing decision must never pass.
//
// To make review trivial, every drift is CLASSIFIED in the failure message:
//
//   - ADVANCEMENT — route A→B (a query that stayed single now shards), or K
//     grew (more memory headroom per request), or the reason moved toward
//     "routed". More of the corpus is now memory-safe under sharding.
//   - REGRESSION — route B→A (a query that sharded now stays single), or K
//     shrank, or a previously-"routed" query is now rejected. Fewer queries
//     get the sharding safety net.
//
// The classification is advisory: the test FAILS either way. An advancement is
// still a deliberate baseline regeneration (the diff shows the intent); a
// regression is held to a higher bar — see below. This is the
// cardinality-ratchet model: the baseline is regenerated DELIBERATELY by a
// maintainer (`just update-solver-decision-baseline`) and reviewed in the PR
// diff, never auto-relaxed by the test.
//
// # Regressions must be justified
//
// A REGRESSION in a regenerated baseline diff (route B→A, or K down, or a
// routed query now rejected) MUST be justified in the PR description with a
// REAL reason — e.g. a correctness fix that disqualifies the query from
// slicing (a now64 leak the old Planner missed, a grid-mismatch it failed to
// catch). It is NEVER an acceptable silent relaxation: "the threshold felt too
// aggressive" is not a justification; "this shape can't be sliced without
// breaking @-modifier semantics, here's the failing case" is. The
// advancement/regression tag exists precisely so a reviewer can see which
// rows moved which way and demand that story.
//
// # New / removed fixtures force a baseline edit
//
// The baseline key-set must match the corpus exactly. A NEW query fails with a
// "run just update-solver-decision-baseline" hint — which records its decision
// so the routing outcome of the new shape lands in the PR diff. A REMOVED
// query fails until its stale row is dropped. Same discipline as
// `update-golden` and the cardinality baseline: drift in either direction is a
// deliberate, reviewed baseline update.

package perf

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/test/spec"
)

// promqlSpecDir is the curated PromQL TXTAR corpus, relative to this package
// directory (test/perf → test/spec/promql).
const promqlSpecDir = "../spec/promql"

// decisionBaselinePath is the committed routing-decision snapshot, relative to
// this package.
const decisionBaselinePath = "solver-decision-baseline.json"

// updateDecisionEnv, when set to "1", regenerates the baseline from the
// current corpus classification instead of asserting against it. Mirrors the
// repo's GOLDEN_UPDATE convention; driven by
// `just update-solver-decision-baseline`.
const updateDecisionEnv = "UPDATE_SOLVER_DECISION_BASELINE"

// The single deterministic eval grid every corpus query is classified on. See
// the file doc: end is a fixed wall-clock so @-modifiers resolve identically
// run-to-run; range=1h / step=15s gives N=241 anchors.
var (
	decisionGridEnd   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionGridStart = decisionGridEnd.Add(-time.Hour)
	decisionGridStep  = 15 * time.Second
)

// decisionEntry is the committed per-query routing decision. It is a pure
// function of the query shape, the fixed grid, and the solver's DefaultConfig
// (Mode=auto), so it reproduces run-to-run with no environment noise.
type decisionEntry struct {
	// Query is the fixture id ("<name>" sans .txtar) — the stable baseline key.
	Query string `json:"query"`

	// Routed is whether the Planner routed the plan B (sharded-timeslice).
	Routed bool `json:"routed"`

	// K is the produced shard count on a route, 0 otherwise.
	K int `json:"k"`

	// Reason is the shadow-header vocabulary value (one of solver.Reason*).
	Reason string `json:"reason"`
}

// classifyCorpus parses, lowers, optimizes, and classifies every PromQL
// fixture in the corpus on the fixed grid, returning a query-id-keyed map of
// decisions. A fixture without a `-- query.promql --` section (or with an
// empty one) is not a query shape and is excluded. A parse / lower failure is
// returned as a hard error: the corpus is curated, so every query must lower.
func classifyCorpus(t *testing.T) map[string]decisionEntry {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(promqlSpecDir, "*.txtar"))
	if err != nil {
		t.Fatalf("glob %q: %v", promqlSpecDir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no *.txtar fixtures under %s", promqlSpecDir)
	}
	sort.Strings(matches)

	sm := schema.DefaultOTelMetrics()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto // the production routing mode the ratchet pins.
	planner := &solver.Planner{Cfg: cfg}
	// Mirror the production PromQL head, which builds its parser with
	// EnableExperimentalFunctions=true (see internal/api/prom/lang.go) so
	// the deliberately-supported experimental subset
	// (sort_by_label / sort_by_label_desc / mad_over_time /
	// ts_of_*_over_time / range() / step() / limitk() /
	// histogram_quantiles / info() / …) parses here exactly as it does in
	// production and contributes its routing decision to the ratchet. Without
	// this the ratchet rejects those corpus fixtures at parse time even though
	// the engine accepts them.
	parse := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	meta := solver.RequestMeta{
		Lang:  "promql",
		Start: decisionGridStart,
		End:   decisionGridEnd,
		Step:  decisionGridStep,
	}

	out := make(map[string]decisionEntry, len(matches))
	for _, path := range matches {
		c, lerr := spec.Load(path)
		if lerr != nil {
			t.Fatalf("load %s: %v", path, lerr)
		}
		raw, ok := c.Section("query.promql")
		if !ok {
			continue // not a query-shape fixture (e.g. exemplars-only).
		}
		query := strings.TrimSpace(raw)
		if query == "" {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(path), ".txtar")

		expr, perr := parse.ParseExpr(query)
		if perr != nil {
			t.Fatalf("%s: parse %q: %v", id, query, perr)
		}
		plan, lerr := promql.LowerAtRange(context.Background(), expr, sm,
			decisionGridStart, decisionGridEnd, decisionGridStep)
		if lerr != nil {
			t.Fatalf("%s: lower %q: %v", id, query, lerr)
		}
		plan = optimizer.Default().Run(context.Background(), plan)

		d, routed := planner.Plan(plan, meta)
		out[id] = decisionEntry{Query: id, Routed: routed, K: d.K, Reason: d.Reason}
	}
	if len(out) == 0 {
		t.Fatalf("classified zero queries from %d fixtures — corpus seam broke", len(matches))
	}
	return out
}

func TestSolverDecisionRatchet(t *testing.T) {
	current := classifyCorpus(t)

	if os.Getenv(updateDecisionEnv) == "1" {
		writeDecisionBaseline(t, current)
		routed := 0
		for _, e := range current {
			if e.Routed {
				routed++
			}
		}
		t.Logf("wrote %s with %d queries (%d routed / %d route-A)",
			decisionBaselinePath, len(current), routed, len(current)-routed)
		return
	}

	baseline := loadDecisionBaseline(t)

	// New queries (in corpus, not in baseline) — the diff must surface their
	// routing decision for review.
	var added []string
	for id := range current {
		if _, ok := baseline[id]; !ok {
			added = append(added, id)
		}
	}
	// Removed queries (in baseline, not in corpus) — drop the stale row.
	var removed []string
	for id := range baseline {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	for _, id := range added {
		c := current[id]
		t.Errorf("new query %q not in the routing-decision baseline "+
			"(routed=%v K=%d reason=%q) — run `just update-solver-decision-baseline` to record it",
			id, c.Routed, c.K, c.Reason)
	}
	for _, id := range removed {
		t.Errorf("baseline query %q no longer in the corpus — run "+
			"`just update-solver-decision-baseline` to drop the stale row", id)
	}

	// Bidirectional per-query ratchet over the matched set. ANY drift fails;
	// the message classifies advancement vs regression so review is trivial.
	matched := make([]string, 0, len(current))
	for id := range current {
		if _, ok := baseline[id]; ok {
			matched = append(matched, id)
		}
	}
	sort.Strings(matched)
	for _, id := range matched {
		cur, base := current[id], baseline[id]
		if cur == base {
			continue
		}
		t.Errorf("%s: routing decision drifted [%s]\n"+
			"      baseline: routed=%v K=%d reason=%q\n"+
			"      current:  routed=%v K=%d reason=%q\n"+
			"      A drift is NEVER auto-accepted: run `just update-solver-decision-baseline` to "+
			"regenerate, review the diff, and (if this row is a REGRESSION) justify it in the PR "+
			"with a real reason — a correctness fix that disqualifies the query, not a threshold tweak.",
			id, classifyDrift(base, cur),
			base.Routed, base.K, base.Reason,
			cur.Routed, cur.K, cur.Reason)
	}
}

// classifyDrift labels a baseline→current decision change as ADVANCEMENT or
// REGRESSION (or MIXED when the signals disagree). Advancement = more / deeper
// routing (A→B, K up, reason toward routed); regression = less / shallower
// (B→A, K down, routed→rejected). The label is advisory — the test fails
// either way — but it makes the direction of every moved row machine-visible
// in the failure output so a reviewer can demand a justification for the
// regressions specifically.
func classifyDrift(base, cur decisionEntry) string {
	adv, reg := false, false

	switch {
	case !base.Routed && cur.Routed:
		adv = true // A→B: a query gained the sharding safety net.
	case base.Routed && !cur.Routed:
		reg = true // B→A: a query lost it.
	case base.Routed && cur.Routed:
		// Both route: K is the headroom signal.
		switch {
		case cur.K > base.K:
			adv = true
		case cur.K < base.K:
			reg = true
		}
	}

	// Reason movement is a tiebreaker / extra signal when the route bit and K
	// did not already settle the direction (e.g. a non-route reason changed
	// from "below-threshold" to "not-sliceable", or vice-versa).
	if !adv && !reg && base.Reason != cur.Reason {
		switch {
		case cur.Reason == solver.ReasonRouted:
			adv = true
		case base.Reason == solver.ReasonRouted:
			reg = true
		default:
			// Two non-route reasons swapped — neither strictly better; flag it
			// as a drift the reviewer must read, not silently ranked.
			return "REASON-CHANGE"
		}
	}

	switch {
	case adv && reg:
		return "MIXED (advancement+regression — read both rows)"
	case adv:
		return "ADVANCEMENT"
	case reg:
		return "REGRESSION"
	default:
		return "DRIFT"
	}
}

// writeDecisionBaseline serialises the current classification as a
// deterministically-ordered JSON array (sorted by query id) so the committed
// file diffs cleanly.
func writeDecisionBaseline(t *testing.T, entries map[string]decisionEntry) {
	t.Helper()
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]decisionEntry, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, entries[id])
	}
	buf, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(decisionBaselinePath, buf, 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

// loadDecisionBaseline reads the committed baseline into a query-keyed map.
func loadDecisionBaseline(t *testing.T) map[string]decisionEntry {
	t.Helper()
	buf, err := os.ReadFile(decisionBaselinePath)
	if err != nil {
		t.Fatalf("read baseline %s: %v — run `just update-solver-decision-baseline` to create it",
			decisionBaselinePath, err)
	}
	var entries []decisionEntry
	if err := json.Unmarshal(buf, &entries); err != nil {
		t.Fatalf("parse baseline %s: %v", decisionBaselinePath, err)
	}
	m := make(map[string]decisionEntry, len(entries))
	for _, e := range entries {
		m[e.Query] = e
	}
	if len(m) == 0 {
		t.Fatalf("baseline %s is empty", decisionBaselinePath)
	}
	return m
}
