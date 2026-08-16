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
// thresholds). The recorded decision is {routed, K, reason} PLUS the
// classifier's cost grid {n_anchors, fanout, cumulative_d, outer_range}.
//
// # Why the cost grid is part of the pin
//
// The route bit, K and the reason are the classifier's OUTPUT; the cost grid is
// its INPUT — the features the signal walk extracted from the plan. Pinning
// only the output leaves the extractor unratcheted over the corpus, and the
// extractor's failure mode is silent by construction: a carrier kind the walk
// stops measuring contributes an all-zero feature vector, which refuses the plan
// for the SAME reason with the SAME K as measuring it properly would. Every
// natively-lowered rate, every histogram bucket fan-out, every
// `absent_over_time` in the corpus is refused either way, so an output-only
// baseline is byte-identical whether those plans are measured or invisible.
//
// The cost grid is also what the calibration corpus records per query
// (optcorpus's n_anchors / fanout / cumulative_d / outer_range columns), so
// these rows are the offline replay of exactly the numbers a routing threshold
// is fitted against. A drift here is a drift in every threshold's evidence base
// even when no query changed route.
//
// # Two lowering tables, not one (#2120)
//
// Every corpus query is classified TWICE — once under `loweringFanout` (the
// generic RangeWindow lowering `promql.RangeLowerers{}.withDefaults()`
// resolves to) and once under `loweringNative` (every AutoSelect
// `chopt.Registry()` feature wired to its native timeSeries*ToGrid strategy —
// see `nativeLowerers`) — and the baseline key is `<query>/<lowering>`, not
// the bare query id.
//
// Before this axis existed the ratchet called `promql.LowerAtRange`, which
// resolves to the all-fan-out table unconditionally, so it classified the
// configuration that is NOT what a capable server actually runs
// (docs/performance.md: "the native path is the default on a capable server;
// you only need to act to opt *out* of it"). #2117's `RangeWindowGridNative`
// re-anchor fix changed that node's OWN routing classification —
// not-sliceable to sliceable — and this ratchet regenerated with ZERO drift,
// because it had never classified a single `RangeWindowGridNative` plan to begin
// with (a 590-fixture census found exactly 0 occurrences). A native-routing
// regression is invisible to a baseline that never records a native decision;
// classifying under both tables puts the native rows beside the fan-out ones
// in the same reviewed diff.
//
// # The ratchet semantics (bidirectional, no escape hatch)
//
// The committed baseline (`solver-decision-baseline/`, one JSON shard per
// (query, lowering) pair — see baseline_shards_test.go) is the reviewed
// snapshot of every classified row's decision. The test FAILS on ANY drift — route
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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chopt"
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
// this package. It is a TREE of one shard per (query, lowering) pair, not a
// single file — see baseline_shards_test.go for why the roster is stored that
// way, and #2120 for why the key has two segments rather than one.
const decisionBaselinePath = "solver-decision-baseline"

// decisionShardDepth is how many path segments a baseline key maps to. A key
// is "<query id>/<lowering>" — see decisionKey — so its shard is
// solver-decision-baseline/<name>/<lowering>.json. Two segments, not one,
// because #2120 found the ratchet classified every query under exactly ONE
// lowering table (the all-fan-out default `withDefaults` resolves the zero
// value to), leaving the native timeSeries*ToGrid configuration — the one
// production actually runs on a capable server, per docs/performance.md — with
// zero coverage. Keying on (query, lowering) puts both configurations' rows
// side by side in the same reviewed diff instead of one silently shadowing
// the other.
const decisionShardDepth = 2

// decisionRegen is the recipe that rewrites the tree, quoted in the failure
// messages the shard store raises.
const decisionRegen = "just update-solver-decision-baseline"

// loweringFanout / loweringNative name the two lowering tables every corpus
// query is classified under — the second baseline-key segment (decisionKey).
//
//   - loweringFanout is the all-fan-out configuration: the generic RangeWindow
//     lowering `withDefaults` resolves promql.RangeLowerers{} (the zero value)
//     to. This is what the ratchet classified exclusively before #2120.
//   - loweringNative is the configuration production actually runs on a
//     capable ClickHouse server (>= 25.9, experimental TS-grid permitted):
//     every AutoSelect feature in chopt.Registry() wired to its native
//     timeSeries*ToGrid strategy — see nativeLowerers.
const (
	loweringFanout = "fanout"
	loweringNative = "native"
)

// decisionKey composes a baseline key from a query id and a lowering name —
// the inverse the shard store's [baselineShards.keyOf] must agree with.
func decisionKey(id, lowering string) string {
	return fmt.Sprintf("%s/%s", id, lowering)
}

// nativeLowerers builds the promql.RangeLowerers table production wires on a
// fully capable ClickHouse server: every AutoSelect feature in
// chopt.Registry() resolved to its native strategy, mirroring
// cmd/cerberus/main.go's nativeRangeLowerers with every optSet.Has(...) check
// answering true. Built FROM the registry, not hand-copied, so a new
// AutoSelect ts_grid feature fails this helper loudly — via the default case
// below — instead of silently classifying only under the stale table, which
// is exactly the blind spot #2120 reported (RangeWindowGridNative going from
// not-sliceable to sliceable when #2117 shipped moved the ratchet's own
// classification with zero recorded drift).
func nativeLowerers(t *testing.T) promql.RangeLowerers {
	t.Helper()

	var l promql.RangeLowerers
	recollapse := false
	for _, f := range chopt.Registry() {
		if !f.AutoSelect {
			continue // not wired on any server — chopt.FeatureTSGridChanges today.
		}
		switch f.ID {
		case chopt.FeatureTSGridRange:
			// Composed below, once FeatureTSGridRecollapse (which narrows it)
			// is known — Recollapse is a modifier on Rate, not its own field.
		case chopt.FeatureTSGridResample:
			l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}
		case chopt.FeatureTSGridResets:
			l.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
		case chopt.FeatureTSGridDeriv:
			l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
		case chopt.FeatureTSGridPredictLinear:
			l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
		case chopt.FeatureTSGridRecollapse:
			recollapse = true
		case chopt.FeatureAggregationInOrder, chopt.FeatureConditionCache:
			// CH SETTINGS stamped at emit time, not a RangeLowerers dispatch
			// strategy — no effect on which lowering table a query takes.
		default:
			t.Fatalf("chopt feature %q is AutoSelect but nativeLowerers does not know how to wire it "+
				"into promql.RangeLowerers — update this helper (see issue #2120)", f.ID)
		}
	}
	l.Rate = promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}, Recollapse: recollapse}
	return l
}

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

// decisionEntry is the committed per-(query, lowering) routing decision. It
// is a pure function of the query shape, the lowering table, the fixed grid,
// and the solver's DefaultConfig (Mode=auto), so it reproduces run-to-run
// with no environment noise.
type decisionEntry struct {
	// Query is the FULL baseline key — decisionKey(id, lowering), e.g.
	// "rate_basic/native" — mirroring how the sibling cardinality baseline's
	// Fixture field holds its own full "<head>/<name>" path rather than a
	// bare name. The shard tree's structural guard
	// (.github/scripts/generated-baseline-structural-guard.mjs) asserts this
	// field equals the shard's own path verbatim, so it cannot be just the
	// fixture id once a query has more than one row.
	Query string `json:"query"`

	// Lowering is loweringFanout or loweringNative, restated from the second
	// half of Query for a diff/grep that does not want to split the key —
	// naming which promql.RangeLowerers table classified this row. See
	// #2120: without this axis the ratchet had exactly one row per query and
	// it was always the fan-out one, leaving the native configuration
	// production runs on a capable server unratcheted.
	Lowering string `json:"lowering"`

	// Routed is whether the Planner routed the plan B (sharded-timeslice).
	Routed bool `json:"routed"`

	// K is the produced shard count on a route, 0 otherwise.
	K int `json:"k"`

	// Reason is the shadow-header vocabulary value (one of solver.Reason*).
	Reason string `json:"reason"`

	// NAnchors / Fanout / CumulativeD / OuterRange are the classifier's cost
	// grid — the features the signal walk extracted from the plan, and the
	// same four scalars the calibration corpus stores per query. They are
	// pinned because they are the only place an extraction regression is
	// visible: a plan the walk cannot measure is refused with the same
	// reason and the same K as one it measures, so the decision half of this
	// row cannot tell the two apart.
	//
	// The two durations are recorded in Go's canonical duration form
	// ("5m0s", "1h0m0s", "0s") rather than as raw nanoseconds so a moved row
	// is readable in the diff without conversion.
	NAnchors    int    `json:"n_anchors"`
	Fanout      int64  `json:"fanout"`
	CumulativeD string `json:"cumulative_d"`
	OuterRange  string `json:"outer_range"`
}

// classifyCorpus parses, lowers under `lowerers`, optimizes, and classifies
// every PromQL fixture in the corpus on the fixed grid, returning a
// decisionKey(id, lowering)-keyed map of decisions. A fixture without a
// `-- query.promql --` section (or with an empty one) is not a query shape
// and is excluded. A parse / lower failure is returned as a hard error: the
// corpus is curated, so every query must lower under EVERY lowering table —
// see [nativeLowerers] on why the native strategies always delegate to their
// fan-out fallback instead of erroring on an ineligible shape.
func classifyCorpus(t *testing.T, lowering string, lowerers promql.RangeLowerers) map[string]decisionEntry {
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
	opts := promql.LowerOpts{Lowerers: lowerers}

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
		plan, lerr := promql.LowerAtRangeOpts(context.Background(), expr, sm,
			decisionGridStart, decisionGridEnd, decisionGridStep, opts)
		if lerr != nil {
			t.Fatalf("%s [%s]: lower %q: %v", id, lowering, query, lerr)
		}
		plan = optimizer.Default().Run(context.Background(), plan)

		d, routed := planner.Plan(plan, meta)
		key := decisionKey(id, lowering)
		out[key] = decisionEntry{
			Query:       key,
			Lowering:    lowering,
			Routed:      routed,
			K:           d.K,
			Reason:      d.Reason,
			NAnchors:    d.NAnchors,
			Fanout:      d.Fanout,
			CumulativeD: d.CumulativeD.String(),
			OuterRange:  d.OuterRange.String(),
		}
	}
	if len(out) == 0 {
		t.Fatalf("classified zero queries from %d fixtures — corpus seam broke", len(matches))
	}
	return out
}

func TestSolverDecisionRatchet(t *testing.T) {
	// Classify every corpus query under BOTH lowering tables — the all-fan-out
	// default and the native timeSeries*ToGrid configuration production auto-
	// selects on a capable server — and merge into one decisionKey-keyed map.
	// See #2120: a single-table classification left whichever configuration
	// was NOT default with zero coverage, so a routing regression confined to
	// it shipped with this ratchet reporting no drift.
	current := make(map[string]decisionEntry)
	for k, v := range classifyCorpus(t, loweringFanout, promql.RangeLowerers{}) {
		current[k] = v
	}
	for k, v := range classifyCorpus(t, loweringNative, nativeLowerers(t)) {
		current[k] = v
	}

	if os.Getenv(updateDecisionEnv) == "1" {
		writeDecisionBaseline(t, current)
		routed := 0
		for _, e := range current {
			if e.Routed {
				routed++
			}
		}
		t.Logf("wrote %s with %d rows across %s/%s (%d routed / %d route-A)",
			decisionBaselinePath, len(current), loweringFanout, loweringNative, routed, len(current)-routed)
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
			"      baseline: routed=%v K=%d reason=%q N=%d F=%d D=%s outer=%s\n"+
			"      current:  routed=%v K=%d reason=%q N=%d F=%d D=%s outer=%s\n"+
			"      A drift is NEVER auto-accepted: run `just update-solver-decision-baseline` to "+
			"regenerate, review the diff, and (if this row is a REGRESSION) justify it in the PR "+
			"with a real reason — a correctness fix that disqualifies the query, not a threshold tweak.",
			id, classifyDrift(base, cur),
			base.Routed, base.K, base.Reason, base.NAnchors, base.Fanout, base.CumulativeD, base.OuterRange,
			cur.Routed, cur.K, cur.Reason, cur.NAnchors, cur.Fanout, cur.CumulativeD, cur.OuterRange)
	}
}

// classifyDrift labels a baseline→current decision change as ADVANCEMENT or
// REGRESSION (or MIXED when the signals disagree). Advancement = more / deeper
// routing (A→B, K up, reason toward routed); regression = less / shallower
// (B→A, K down, routed→rejected). The label is advisory — the test fails
// either way — but it makes the direction of every moved row machine-visible
// in the failure output so a reviewer can demand a justification for the
// regressions specifically.
//
// COST-GRID is the label for a row whose route, K and reason all held while the
// extracted features moved. It is not ranked as advancement or regression
// because it is neither: the routing outcome is identical and what changed is
// what the signal walk SAW. That is the direction an extraction regression
// arrives from, so the label names it explicitly rather than letting it land in
// the generic bucket.
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

	// classifyDrift is only called on a row that moved, so route, K and reason
	// all holding means the extracted cost grid is what moved.
	if !adv && !reg && base.Reason == cur.Reason {
		return "COST-GRID"
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

// decisionShards is the committed baseline's shard store: one file per corpus
// query, keyed by the query id, with the tree pruned on regeneration so a
// fixture dropped from the corpus cannot leave a decision row behind.
var decisionShards = baselineShards[decisionEntry]{
	dir:   decisionBaselinePath,
	depth: decisionShardDepth,
	keyOf: func(e decisionEntry) string { return e.Query },
	regen: decisionRegen,
}

// writeDecisionBaseline rewrites the shard tree from the current
// classification.
func writeDecisionBaseline(t *testing.T, entries map[string]decisionEntry) {
	t.Helper()
	decisionShards.mustWrite(t, entries)
}

// loadDecisionBaseline reads the committed shard tree into a query-keyed map.
func loadDecisionBaseline(t *testing.T) map[string]decisionEntry {
	t.Helper()

	return decisionShards.mustLoad(t)
}
