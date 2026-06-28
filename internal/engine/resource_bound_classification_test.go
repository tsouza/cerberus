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
	"RangeWindow":         {boundGated, "axis 5: subquery anchor grid (incl. nested product) gated by requireSubquerySampleBudget; axis 1 instant leaf by requireInstantScanBound"},
	"RangeWindowNative":   {boundGated, "axis 5: native timeSeries*ToGrid variant of RangeWindow; same anchor-grid bound"},
	"RangeWindowResample": {boundGated, "axis 5: native-staleness resample variant; same anchor-grid bound"},
	"StepGrid":            {boundStructural, "axis 5: query_range step grid capped at format.MaxResolutionPoints in the head handler"},
	"RangeBucketFanout":   {boundRuntimeNet, "axis 4/7: histogram bucket fan-out over the scan-bounded window; spill + max_memory_usage"},
	"RangeLWR":            {boundRuntimeNet, "axis 7: linear-regression-window compute over the scan-bounded window"},
	"AbsentOverTime":      {boundStructural, "one synthesized row per absent series over the bounded window"},

	// Recursive / structural-join walks — axis 3, depth caps.
	"StructuralJoin":    {boundGated, "axis 3: recursive CTE depth-capped at defaultStructuralRecursionDepth"},
	"NestedSetAnnotate": {boundGated, "axis 3: recursive numbering walk depth-capped; row source bounded by BoundedTraceScope"},

	// Aggregations + joins — axis 4 cardinality, runtime net.
	"Aggregate":                {boundRuntimeNet, "axis 4: GROUP BY cardinality; external-aggregation spill + max_memory_usage; output bounded by the result drain SampleBudget"},
	"MetricsAggregate":         {boundRuntimeNet, "axis 4: TraceQL metrics GROUP BY; spill + max_memory_usage"},
	"MetricsSecondStage":       {boundRuntimeNet, "axis 4/7: second-stage metrics reduction over the bounded first stage"},
	"MetricsCompare":           {boundRuntimeNet, "axis 7: compare() arithmetic over the scan-bounded input"},
	"MetricsHistogramOverTime": {boundRuntimeNet, "axis 4/7: per-bucket histogram over the bounded window"},
	"CrossJoin":                {boundRuntimeNet, "axis 4: row product; current callers (absent lowering) feed it a single-row side; max_memory_usage"},
	"InfoJoin":                 {boundRuntimeNet, "axis 4: info-metric enrichment join; spill + max_memory_usage"},
	"SetOperation":             {boundStructural, "≤ sum of the two bounded inputs"},
	"NaryVectorSetOp":          {boundStructural, "≤ sum of the bounded operand vectors"},

	// Per-row compute — axis 7, runtime net.
	"HistogramQuantile":       {boundRuntimeNet, "axis 7: per-row quantile over the bounded bucket set"},
	"HistogramQuantileNative": {boundRuntimeNet, "axis 7: native per-row histogram quantile over the bounded input"},

	// Binary / set vector ops over two already-bounded operand vectors.
	"VectorJoin":  {boundRuntimeNet, "axis 4: on()/ignoring() match join over two bounded vectors; max_memory_usage"},
	"VectorSetOp": {boundStructural, "and/or/unless — ≤ the union of the two bounded operand vectors"},
	"UnionAll":    {boundStructural, "≤ the sum of its bounded inputs"},
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
