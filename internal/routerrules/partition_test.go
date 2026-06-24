package routerrules

import (
	"context"
	"testing"
)

// fakePartitionSource is a programmable CorpusSource for exercising the
// per-partition sub-evaluation path (expandPartitioned + restrict) in
// isolation. Aggregate returns a fixed partition-keyed Value; EvalRule returns
// the same set of candidate GroupResults for EVERY sub-eval (one group per
// partition value), exactly as a real backend would if the rule's scalar-bound
// condition matched rows in both partitions. The restrict step in the evaluator
// is then the ONLY thing that keeps each sub-eval anchored to its own
// partition. If restrict is removed or made a pass-through, every sub-eval's
// groups leak into every partition and the per-class support double-counts.
type fakePartitionSource struct {
	agg      Value
	groups   []GroupResult
	lastEnvs []Env
}

func (s *fakePartitionSource) Aggregate(_ context.Context, _ AggSpec) (Value, error) {
	return s.agg, nil
}

func (s *fakePartitionSource) EvalRule(_ context.Context, q RuleQuery) ([]GroupResult, error) {
	s.lastEnvs = append(s.lastEnvs, q.Env)
	// Return a copy so the evaluator's in-place restrict can't corrupt the
	// canned fixture between sub-evals.
	out := make([]GroupResult, len(s.groups))
	copy(out, s.groups)
	return out, nil
}

// TestRestrictAnchorsPartitionedRuleToOwnPartition is the cross-partition
// leakage guard. The rule references a param partitioned by `language` across
// two partitions (promql, logql). The backend reports one candidate group per
// language. With a correct restrict(), each language's sub-eval keeps only its
// own group, so exactly two findings fire (one per language). With a broken /
// pass-through restrict(), every sub-eval keeps BOTH groups, so the same group
// is emitted once per partition value — doubling the finding count. The exact
// total-count and per-class assertions below go red the moment restrict stops
// filtering.
func TestRestrictAnchorsPartitionedRuleToOwnPartition(t *testing.T) {
	src := &fakePartitionSource{
		// A partitioned watermark keyed by `language` with two partitions.
		agg: Value{
			Partition:    map[string]float64{"promql": 100, "logql": 200},
			PartitionCol: "language",
		},
		// One candidate group per partition, both above min_support. A correct
		// restrict keeps promql's group only in the promql sub-eval and logql's
		// only in the logql sub-eval.
		groups: []GroupResult{
			{GroupKey: []string{"11", "promql"}, Support: 3},
			{GroupKey: []string{"22", "logql"}, Support: 5},
		},
	}

	cat := &Catalog{
		Params: []ParamSpec{
			{Name: "wmark_pctile", Kind: ParamConfig, Key: "p"},
			{
				Name: "slow_watermark", Kind: ParamCorpusPercentile,
				Column: "query_duration_ms", PartitionBy: []string{"language"},
				Percentile: &ParamRef{Ref: "wmark_pctile"},
			},
			{Name: "min_class_support", Kind: ParamConfig, Key: "m"},
		},
		Rules: []Rule{{
			ID: "slow_hot_shape", Severity: "medium", Status: StatusActive,
			GroupBy:    []string{"normalized_query_hash", "language"},
			MinSupport: &ParamRef{Ref: "min_class_support"},
			Condition: Predicate{All: []Predicate{
				{Col: "route", Op: "eq", Enum: "A"},
				{Col: "query_duration_ms", Op: "gte", Param: "slow_watermark"},
			}},
		}},
	}
	cfg := staticConfig(map[string]string{"p": "0.5", "m": "1"})

	report, err := NewEvaluator(cat, cfg, src).Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Sanity: the rule expanded into one sub-eval per partition value.
	if len(src.lastEnvs) != 2 {
		t.Fatalf("expected 2 per-partition sub-evals, got %d", len(src.lastEnvs))
	}
	// Each sub-eval must see a scalar-bound watermark, never the partition map.
	for i, e := range src.lastEnvs {
		v, ok := e["slow_watermark"]
		if !ok || v.IsPartitioned() {
			t.Fatalf("sub-eval %d env did not scalar-bind slow_watermark: %+v", i, e)
		}
	}

	// EXACT total: two partitions, one group each, restrict keeps one per
	// sub-eval => exactly 2 findings. A broken restrict yields 4 (each group
	// surviving both sub-evals).
	if len(report.Findings) != 2 {
		t.Fatalf("expected exactly 2 findings (restrict anchored each partition), got %d: %+v",
			len(report.Findings), report.Findings)
	}

	// Per-class support must be the single-partition value, not a doubled sum.
	byClass := map[string]int64{}
	for _, f := range report.Findings {
		byClass[classOf(f.GroupKey)] += f.Support
	}
	want := map[string]int64{
		"language=promql,normalized_query_hash=11": 3,
		"language=logql,normalized_query_hash=22":  5,
	}
	for cls, w := range want {
		if byClass[cls] != w {
			t.Errorf("class %q support = %d, want %d (restrict leakage?)", cls, byClass[cls], w)
		}
	}
	if len(byClass) != len(want) {
		t.Errorf("unexpected classes emitted: %+v", byClass)
	}
}

// TestRestrictDropsNonMatchingPartitionKey is a focused unit test of restrict()
// itself: a sub-eval anchored to one partition value must drop every
// GroupResult whose partition-column key differs, and keep the matching ones.
func TestRestrictDropsNonMatchingPartitionKey(t *testing.T) {
	sub := subEval{
		restrictCol: "language",
		restrictVal: "promql",
		groupBy:     []string{"normalized_query_hash", "language"},
	}
	in := []GroupResult{
		{GroupKey: []string{"11", "promql"}, Support: 3},
		{GroupKey: []string{"22", "logql"}, Support: 5},
		{GroupKey: []string{"33", "promql"}, Support: 2},
	}
	out := sub.restrict(in)
	if len(out) != 2 {
		t.Fatalf("restrict kept %d groups, want 2 (only promql)", len(out))
	}
	for _, g := range out {
		if g.GroupKey[1] != "promql" {
			t.Errorf("restrict leaked non-promql group: %+v", g)
		}
	}

	// An unrestricted sub-eval (no partitioned param) is a pass-through.
	open := subEval{groupBy: []string{"language"}}
	if got := open.restrict(in); len(got) != len(in) {
		t.Errorf("unrestricted restrict dropped groups: got %d, want %d", len(got), len(in))
	}
}

// TestSharedPartitionDifferingColumnsErrors exercises sharedPartition's
// differing-columns error path: two partitioned params keyed by different
// partition columns in one rule is unsplittable and must be a hard error.
func TestSharedPartitionDifferingColumnsErrors(t *testing.T) {
	env := Env{
		"by_lang":  {Partition: map[string]float64{"promql": 1}, PartitionCol: "language"},
		"by_shape": {Partition: map[string]float64{"cerb:sum": 1}, PartitionCol: "shape_id"},
	}
	_, _, err := sharedPartition([]string{"by_lang", "by_shape"}, env, []string{"language", "shape_id"})
	if err == nil {
		t.Fatal("expected an error for partitioned params on differing columns")
	}
	if !containsToken(err.Error(), "differing columns") {
		t.Fatalf("error should name the differing-columns case, got: %v", err)
	}
}

// TestSharedPartitionIntersectsKeySets exercises sharedPartition's
// key-intersection path: two params on the same column with overlapping but
// non-identical key sets must yield only the keys present in BOTH, so a value
// missing from one param is never scalar-bound for the other.
func TestSharedPartitionIntersectsKeySets(t *testing.T) {
	env := Env{
		"a": {Partition: map[string]float64{"promql": 1, "logql": 2}, PartitionCol: "language"},
		"b": {Partition: map[string]float64{"promql": 3, "traceql": 4}, PartitionCol: "language"},
	}
	col, values, err := sharedPartition([]string{"a", "b"}, env, []string{"language"})
	if err != nil {
		t.Fatalf("sharedPartition: %v", err)
	}
	if col != "language" {
		t.Fatalf("partition column = %q, want language", col)
	}
	if len(values) != 1 || values[0] != "promql" {
		t.Fatalf("intersection = %v, want [promql] only", values)
	}
}
