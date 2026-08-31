package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// boundClass records HOW a chplan plan-node's worst-case resource consumption
// is bounded. This is the resource-bound audit's Step-5 enumeration made
// executable: every plan node must declare a class, so a newly-added node type
// FAILS TestResourceBoundClassification_Exhaustive until its author has thought
// about (and recorded) how it is bounded — the "can't ship an unbounded
// operator" guarantee.
type boundClass int

const (
	// boundStructural: the bound is intrinsic to the node's own IR/SQL shape
	// (a LIMIT, a 1:1 projection, a single row, a top-N) — it cannot run away.
	boundStructural boundClass = iota
	// boundGated: an axis that CAN be unbounded, caught by a fail-closed
	// plan-time gate that rejects (422) when the bound is absent.
	boundGated
	// boundRuntimeNet: bounded only at execution time by ClickHouse
	// (max_memory_usage / external-spill) and/or the Go-side result drain
	// (SampleBudget). No plan-time gate; a runaway is a graceful resource
	// rejection, never a process crash. Declared, not silently omitted.
	boundRuntimeNet
)

// resourceBoundClassification classifies every chplan plan node (every type
// with a planNode() marker). Keep it in sync with internal/chplan — the
// exhaustiveness test below fails if a plan node is missing here.
var resourceBoundClassification = map[string]struct {
	class boundClass
	note  string
}{
	// Leaf + structurally-bounded shapes.
	"Scan":             {boundGated, "axis 1: leaf scan bounded by the request time window; instant windowed leaves fail-close via requireInstantScanBound"},
	"Filter":           {boundStructural, "≤ input rows; the scan-time predicate it carries bounds the leaf below"},
	"Project":          {boundStructural, "1:1 row map"},
	"OneRow":           {boundStructural, "exactly one row"},
	"Limit":            {boundStructural, "explicit LIMIT N"},
	"TopK":             {boundStructural, "LIMIT N over the ordered input"},
	"SearchTraceLimit": {boundStructural, "axis 2: top-N traces (the structural row-source bound)"},
	"OrderBy":          {boundRuntimeNet, "axis 4: sort buffer; spill + max_memory_usage"},

	// Subquery / windowed grids — axis 5, the anchor budget.
	"RangeWindow":                  {boundGated, "axis 5: subquery anchor grid (incl. nested product) gated by requireSubquerySampleBudget; axis 1 instant leaf by requireInstantScanBound"},
	"RangeWindowGridNative":        {boundGated, "axis 5: native timeSeries*ToGrid variant of RangeWindow; same anchor-grid bound"},
	"RangeWindowGridNativeInstant": {boundGated, "axis 1: instant-mode native timeSeries*ToGrid sibling of RangeWindowGridNative (#2748) — no axis-5 anchor-grid risk at all (the degenerate grid is always exactly one point by construction, never sliced); its per-series scan is bounded by maybePushRangeScanTimeBound's WHERE predicate, made unconditional by the emitter's own fail-closed Anchor-must-be-pinned guard rather than a runtime InstantScanBounded flag check — this is the memory win the feature exists for: the old fan-out's per-series groupArray had no such bound on this axis at all"},
	"RangeWindowStaleResample":     {boundGated, "axis 5: native-staleness resample variant; same anchor-grid bound"},
	"StepGrid":                     {boundStructural, "axis 5: query_range step grid capped at format.MaxResolutionPoints in the head handler"},
	"RangeBucketFanout":            {boundGated, "axis 5: own grid (Start/End/Step) gated by requireSubquerySampleBudget via RangeBucketFanout.NumAnchors, #2408; axis 4/7 sample-side fan-out row count into the collapse GROUP BY is now ALSO LIMIT+throwIf bounded at its own maxRangeBucketFanoutRows threshold (chsql.lwrFanoutBoundedSourceFrag, #2447/#2470) — a real, measured OOM risk for the groupArray-based classic-histogram aggregated path; the remaining runtime-net residue is the collapse's own GROUP BY cardinality, spill + max_memory_usage"},
	"RangeBucketGridNative":        {boundGated, "axis 5: own grid gated by requireSubquerySampleBudget via RangeBucketGridNative.NumAnchors, #2408; axis 4/7 the per-(series, `le` rung) native aggregate GROUP BY (each group materialising an anchor-wide array — not a #2447-style raw-sample fan-out) is now ALSO throwIf bounded, on a CHEAP group-count probe (no aggregate function) times the plan-time-known NumAnchors, gating entry into the GROUP BY itself rather than bounding its output after the fact (chsql.bucketGridGroupCountBoundedSourceFrag — see range_bucket_grid_native_bound.go, #2486 — a real testcontainers calibration found the obvious lwrFanoutBoundedSourceFrag-reuse shape paid for the expensive aggregate TWICE before its own guard could fire, letting a genuinely oversized query still reach a real ClickHouse OOM); `groups x anchors` alone was found NOT to be a complete cost model (#2523: a high-raw-row-density query can OOM real ClickHouse well under that threshold) — a SECOND, independent throwIf now ALSO combines that same cheap group-count probe with a raw-row-count + max-bound-width probe over the Input source (chsql.bucketGridDensityBoundedSourceFrag), gating on the SAME calibrated additive formula before Level 1 runs; the remaining runtime-net residue is Level 1's own native-aggregate spill + max_memory_usage for an in-budget query"},
	"RangeLWR":                     {boundGated, "axis 5: own grid gated by requireSubquerySampleBudget via RangeLWR.NumAnchors, #2408; axis 4/7 sample-side fan-out row count into the collapse GROUP BY is now ALSO LIMIT+throwIf bounded (chsql.lwrFanoutBoundedSourceFrag, #2447), sharing the fan-out SOURCE (lwrAnchorFanoutFrag) with RangeBucketFanout but NOT its threshold (#2470 gave RangeLWR its own maxRangeLWRFanoutRows, separate from RangeBucketFanout's maxRangeBucketFanoutRows, once a real nightly query showed the two collapse shapes do not share a real memory-risk profile); the remaining runtime-net residue is the collapse's own GROUP BY cardinality, spill + max_memory_usage"},
	"AbsentOverTime":               {boundStructural, "one synthesized row per absent series over the bounded window"},

	// Recursive / structural-join walks — axis 3, depth caps.
	"StructuralJoin":    {boundGated, "axis 3: recursive CTE depth-capped at defaultStructuralRecursionDepth"},
	"NestedSetAnnotate": {boundGated, "axis 3: recursive numbering walk depth-capped; row source bounded by BoundedTraceScope"},

	// Aggregations + joins — axis 4 cardinality, runtime net.
	"Aggregate":                {boundRuntimeNet, "axis 4: GROUP BY cardinality; external-aggregation spill + max_memory_usage; output bounded by the result drain SampleBudget"},
	"MetricsAggregate":         {boundRuntimeNet, "axis 4: TraceQL metrics GROUP BY; spill + max_memory_usage"},
	"MetricsSecondStage":       {boundRuntimeNet, "axis 4/7: second-stage metrics reduction over the bounded first stage"},
	"MetricsCompare":           {boundGated, "axis 4/7: compare() arithmetic over the scan-bounded input, PLUS the zero-filled series x anchor grid it synthesises after the drain — gated by requireCompareGridBudget, the only place that product can be charged (the result-drain SampleBudget never sees a synthesised sample)"},
	"MetricsHistogramOverTime": {boundRuntimeNet, "axis 4/7: per-bucket histogram over the bounded window"},
	"CrossJoin":                {boundRuntimeNet, "axis 4: row product; current callers (absent lowering) feed it a single-row side; max_memory_usage"},
	"InfoJoin":                 {boundRuntimeNet, "axis 4: info-metric enrichment join; spill + max_memory_usage"},
	"SetOperation":             {boundStructural, "≤ sum of the two bounded inputs"},
	"NaryVectorSetOp":          {boundStructural, "≤ sum of the bounded operand vectors"},

	// Per-row compute — axis 7, runtime net.
	"HistogramQuantile":       {boundRuntimeNet, "axis 7: per-row quantile over the bounded bucket set; when its Input is the classic-histogram classicBucketMergeShaping cross-series merge (sum by(le)(...) aggregated paths, both instant and range mode), that merge's own groupArray Aggregate is itself gated by promql's wrapClassicBucketMergeBudgetGuard (bucket-volume-times-widest-row throwIf, #2408) before this node ever reads a row"},
	"HistogramQuantileNative": {boundRuntimeNet, "axis 7: native per-row histogram quantile over the bounded input; its Input's across-series exponential-histogram merge is itself gated by promql's wrapExpHistogramMergeBudgetGuard (series-per-group x merged-bucket-width throwIf, #2385) before this node ever reads a row"},
	"HistogramProjection":     {boundStructural, "1:1 row map — publishes the bounded input's native-histogram structural columns verbatim (no fan-out, no aggregation, no per-row quantile compute); same shape as Project"},

	// Binary / set vector ops over two already-bounded operand vectors.
	"VectorJoin":               {boundRuntimeNet, "axis 4: on()/ignoring() match join over two bounded vectors; max_memory_usage"},
	"HistogramVectorJoin":      {boundRuntimeNet, "axis 4: group_left()/group_right() match join between two bounded histogram-valued operand vectors (VectorJoin's many-to-one counterpart for native histograms); max_memory_usage"},
	"HistogramFloatVectorJoin": {boundRuntimeNet, "axis 4: MUL/DIV histogram-scaling match join between a bounded histogram-valued operand and a bounded float-valued operand vector (VectorJoin's per-row-scale-factor counterpart for native histograms); max_memory_usage"},
	"MixedVectorJoin":          {boundRuntimeNet, "axis 4: vector-vector arithmetic/comparison match join between two bounded mixed float/histogram operand vectors (VectorJoin's fourteen-column counterpart for a mixed `or` on both sides, issue #2449); max_memory_usage"},
	"VectorSetOp":              {boundStructural, "and/or/unless — ≤ the union of the two bounded operand vectors"},
	"UnionAll":                 {boundStructural, "≤ the sum of its bounded inputs"},
}

// TestResourceBoundClassification_Exhaustive discovers every chplan plan-node
// type (the planNode() marker) from source and asserts each is classified
// above. A new node type fails this until its resource bound is declared.
func TestResourceBoundClassification_Exhaustive(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../chplan/*.go")
	if err != nil {
		t.Fatalf("glob chplan: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no chplan source files found — adjust the relative path")
	}
	// Matches `func (*Foo) planNode() {` and `func (f *Foo) planNode() {`.
	re := regexp.MustCompile(`func \([a-z]* ?\*?(\w+)\) planNode\(\)`)
	found := map[string]bool{}
	for _, f := range files {
		if filepath.Base(f) == "node.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = true
		}
	}
	if len(found) < 20 {
		t.Fatalf("discovered only %d plan-node types — the marker regex likely drifted", len(found))
	}
	for name := range found {
		if _, ok := resourceBoundClassification[name]; !ok {
			t.Errorf("chplan.%s is a plan node with no resource-bound classification — "+
				"add it to resourceBoundClassification (declare how its worst-case resource "+
				"use is bounded: structural / gated / runtime-net) per the Step-5 audit", name)
		}
	}
	// Also flag stale entries (a classification for a removed node).
	for name := range resourceBoundClassification {
		if !found[name] {
			t.Errorf("resourceBoundClassification has %q but no chplan plan node by that "+
				"name exists — remove the stale entry", name)
		}
	}
}
