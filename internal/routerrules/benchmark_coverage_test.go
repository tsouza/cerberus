package routerrules

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// This file closes the high-value coverage gaps the benchmark exposed:
//   - resolver-kind coverage: corpus_agg (max/avg/min/stddevPop) is a live
//     resolver kind with no catalog param exercising it on a realistic corpus,
//     and the in-memory source is a second backend that must agree with the
//     JSONL one;
//   - multi-rule interaction: a single class that is the textbook positive for
//     several rules must fire EXACTLY those rules and no others;
//   - adversarial corpora: monochrome, all-failure, and shifted distributions
//     that try to drive the rules into false positives or false negatives.

// TestResolverKindCorpusAggOnBenchmark exercises every corpus_agg function over
// the realistic benchmark corpus, partitioned and scalar, through the in-memory
// source. The catalog ships no corpus_agg param today, so without this the
// resolver kind is only unit-tested on tiny fixtures; here it must resolve to a
// non-degenerate value on a real distribution.
func TestResolverKindCorpusAggOnBenchmark(t *testing.T) {
	corpus := GenerateBenchCorpus(nominalBenchParams())
	src := corpus.AsCorpusSource()
	ctx := context.Background()

	// Scalar aggregates over the whole corpus.
	for _, fn := range []AggFunc{AggMax, AggAvg, AggMin, AggStdDev} {
		v, err := src.Aggregate(ctx, AggSpec{Agg: fn, Column: "memory_usage"})
		if err != nil {
			t.Fatalf("agg %s: %v", fn, err)
		}
		if v.IsPartitioned() || v.NoSignal {
			t.Errorf("agg %s should be a scalar with signal, got partitioned=%v nosignal=%v", fn, v.IsPartitioned(), v.NoSignal)
		}
		if fn != AggMin && v.Scalar <= 0 {
			t.Errorf("agg %s memory_usage = %v, want positive", fn, v.Scalar)
		}
	}

	// Ordering invariant on a real distribution: min <= avg <= max.
	mn := mustScalar(t, src, AggSpec{Agg: AggMin, Column: "memory_usage"})
	av := mustScalar(t, src, AggSpec{Agg: AggAvg, Column: "memory_usage"})
	mx := mustScalar(t, src, AggSpec{Agg: AggMax, Column: "memory_usage"})
	if !(mn <= av && av <= mx) {
		t.Errorf("agg ordering violated: min=%v avg=%v max=%v", mn, av, mx)
	}

	// Partitioned corpus_agg: one max per language, every bucket positive.
	part, err := src.Aggregate(ctx, AggSpec{Agg: AggMax, Column: "query_duration_ms", PartitionBy: []string{"language"}})
	if err != nil {
		t.Fatalf("partitioned agg: %v", err)
	}
	if !part.IsPartitioned() || len(part.Partition) == 0 {
		t.Fatalf("partitioned agg should yield per-language buckets, got %+v", part)
	}
	for lang, v := range part.Partition {
		if v <= 0 {
			t.Errorf("partitioned agg query_duration_ms[%q] = %v, want positive", lang, v)
		}
	}
}

func mustScalar(t *testing.T, src CorpusSource, spec AggSpec) float64 {
	t.Helper()
	v, err := src.Aggregate(context.Background(), spec)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	return v.Scalar
}

// TestMemSourceMatchesJSONLOnAgg pins the in-memory benchmark source against the
// JSONL source: written to the same rows, every corpus_agg resolves identically.
// This is the parity guarantee that the benchmark measures the production
// matcher, not a divergent re-implementation.
func TestMemSourceMatchesJSONLOnAgg(t *testing.T) {
	corpus := GenerateBenchCorpus(nominalBenchParams())
	mem := corpus.AsCorpusSource()
	jsonl := jsonlFromBenchRows(t, corpus.Rows)
	ctx := context.Background()

	specs := []AggSpec{
		{Agg: AggMax, Column: "memory_usage"},
		{Agg: AggStdDev, Column: "read_rows"},
		{Agg: AggAvg, Column: "fanout", PartitionBy: []string{"language"}},
		{CountRatio: true, NumScope: Scope{"exit_status": "oom"}, DenScope: Scope{"route": "A"}},
	}
	for _, spec := range specs {
		mv, err := mem.Aggregate(ctx, spec)
		if err != nil {
			t.Fatalf("mem aggregate: %v", err)
		}
		jv, err := jsonl.Aggregate(ctx, spec)
		if err != nil {
			t.Fatalf("jsonl aggregate: %v", err)
		}
		if mv.NoSignal != jv.NoSignal || mv.Scalar != jv.Scalar {
			t.Errorf("scalar parity broke for %+v: mem={%v,%v} jsonl={%v,%v}", spec, mv.Scalar, mv.NoSignal, jv.Scalar, jv.NoSignal)
		}
		if len(mv.Partition) != len(jv.Partition) {
			t.Errorf("partition parity broke for %+v: mem=%d jsonl=%d buckets", spec, len(mv.Partition), len(jv.Partition))
		}
		for k, mscal := range mv.Partition {
			if jscal, ok := jv.Partition[k]; !ok || jscal != mscal {
				t.Errorf("partition[%q] parity broke for %+v: mem=%v jsonl=%v", k, spec, mscal, jscal)
			}
		}
	}
}

// TestMultiRuleInteraction asserts that a class which is the textbook positive
// for several rules fires EXACTLY its labeled rule set — no rule bleeds onto a
// class outside its territory, and no labeled rule is silently dropped. The
// route-A heavy OOM class is the canonical multi-rule class (oom_on_route_a +
// failure_cluster_by_reason + heavy_shape_geometry_failing).
func TestMultiRuleInteraction(t *testing.T) {
	cat := loadCatalogT(t)
	corpus := GenerateBenchCorpus(nominalBenchParams())
	rep, err := NewEvaluator(cat, staticConfigLookup(benchConfig()), corpus.AsCorpusSource()).
		Evaluate(context.Background(), EvalOptions{IncludeExperimental: true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// firedByClass[classID] = set of rule ids fired on it.
	firedByClass := map[string]map[string]struct{}{}
	for _, f := range rep.Findings {
		id := matchClassID(f, corpus)
		if id == "" {
			t.Errorf("finding for rule %q matched no labeled class: %s", f.RuleID, classOfFinding(f))
			continue
		}
		set := firedByClass[id]
		if set == nil {
			set = map[string]struct{}{}
			firedByClass[id] = set
		}
		set[f.RuleID] = struct{}{}
	}

	for i := range corpus.Classes {
		c := &corpus.Classes[i]
		want := map[string]struct{}{}
		for _, r := range c.Expect {
			want[r] = struct{}{}
		}
		got := firedByClass[classID(*c)]
		if !setsEqual(want, got) {
			t.Errorf("class %s/%s fired %v, expected exactly %v",
				c.Language, c.ShapeID, sortedKeys(got), sortedKeys(want))
		}
	}
}

// TestAdversarialMonochrome is the zero-false-positive proof on a HEALTHY-ONLY
// corpus: a deployment whose every query is route-A and ok (the real-world
// monochrome-healthy shape) must produce no findings at all — the failure rules
// have nothing to gate on, and the self-relative tail detectors must not invent
// a pathology out of a uniform healthy body.
func TestAdversarialMonochrome(t *testing.T) {
	cat := loadCatalogT(t)
	corpus := GenerateBenchCorpus(BenchParams{Seed: 7, MinSupport: 5})
	// Strip every planted pathology row + class, keeping only the healthy bulk.
	healthyOnly := &BenchCorpus{}
	keep := map[string]struct{}{}
	for _, c := range corpus.Classes {
		if c.Severity == SevHealthy {
			healthyOnly.Classes = append(healthyOnly.Classes, c)
			keep[c.ShapeID] = struct{}{}
		}
	}
	for _, r := range corpus.Rows {
		if _, ok := keep[r.ShapeID]; ok {
			healthyOnly.Rows = append(healthyOnly.Rows, r)
		}
	}

	rep, err := NewEvaluator(cat, staticConfigLookup(benchConfig()), healthyOnly.AsCorpusSource()).
		Evaluate(context.Background(), EvalOptions{IncludeExperimental: true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("monochrome-healthy corpus must yield zero findings, got %d:\n%s",
			len(rep.Findings), dumpFindings(rep.Findings))
	}
}

// TestAdversarialDistributionShift stress-tests robustness to a non-stationary
// corpus: the same labeled pathologies but with the healthy body shifted to a
// different seed (a different deployment's cost surface). Detection must remain
// effective — the self-relative watermarks adapt to the new body, so recall
// stays at the floor and precision does not collapse.
func TestAdversarialDistributionShift(t *testing.T) {
	cat := loadCatalogT(t)
	for _, seed := range []int64{2, 13, 99, 12345} {
		corpus := GenerateBenchCorpus(BenchParams{Seed: seed, MinSupport: 5})
		m, err := ScoreCatalog(context.Background(), cat, benchConfig(), corpus)
		if err != nil {
			t.Fatalf("seed %d: score: %v", seed, err)
		}
		if m.Overall.Recall < saneRangeRecallFloor {
			t.Errorf("seed %d: recall %.3f below floor %.3f (self-relative watermarks should adapt)\n%s",
				seed, m.Overall.Recall, saneRangeRecallFloor, FormatMetricsTable(m))
		}
		if m.Overall.Precision < saneRangePrecisionFloor {
			t.Errorf("seed %d: precision %.3f below floor %.3f\n%s",
				seed, m.Overall.Precision, saneRangePrecisionFloor, FormatMetricsTable(m))
		}
	}
}

// jsonlFromBenchRows writes bench rows to a temp JSONL file and returns a JSONL
// source over them, so the in-memory and JSONL backends can be compared on
// byte-identical data.
func jsonlFromBenchRows(t *testing.T, rows []BenchRow) CorpusSource {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/bench.jsonl"
	writeBenchJSONL(t, path, rows)
	return NewJSONLCorpusSource(path, 0)
}

// writeBenchJSONL serialises bench rows as JSONL matching jsonlRow's tags, so a
// JSONL source reads them byte-for-byte as the in-memory source folds them.
func writeBenchJSONL(t *testing.T, path string, rows []BenchRow) {
	t.Helper()
	var sb strings.Builder
	for _, r := range rows {
		jr := jsonlRow{
			ShapeID: r.ShapeID, Language: r.Language, NormalizedQueryHash: r.NormalizedQueryHash,
			NAnchors: r.NAnchors, Fanout: r.Fanout, CumulativeD: r.CumulativeD,
			OuterRange: r.OuterRange, Step: r.Step, Route: r.Route, KShards: r.KShards,
			DecisionReason: r.DecisionReason, ReadRows: r.ReadRows, ReadBytes: r.ReadBytes,
			QueryDurationMS: r.QueryDurationMS, MemoryUsage: r.MemoryUsage, ExitStatus: r.ExitStatus,
		}
		b, err := json.Marshal(jr)
		if err != nil {
			t.Fatalf("marshal bench row: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write bench jsonl: %v", err)
	}
}

func setsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
