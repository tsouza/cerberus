package routerrules

import (
	"context"
	"testing"
)

func TestResolveConfigKinds(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{
		{Name: "p", Kind: ParamConfig, Key: "k"},
	}}
	r := NewParamResolver(staticConfig(map[string]string{"k": "0.95"}), nil)
	env, err := r.Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env["p"].Scalar != 0.95 {
		t.Fatalf("config param = %v, want 0.95", env["p"].Scalar)
	}
}

func TestResolveMissingConfigKeyErrors(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{{Name: "p", Kind: ParamConfig, Key: "absent"}}}
	r := NewParamResolver(staticConfig(map[string]string{}), nil)
	_, err := r.Resolve(context.Background(), cat)
	if err == nil {
		t.Fatalf("expected error for missing config key")
	}
	if stringIndex(err.Error(), "absent") < 0 {
		t.Fatalf("error should name the key, got: %v", err)
	}
}

func TestResolvePercentileFractionIsAParamRef(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{
		{Name: "frac", Kind: ParamConfig, Key: "f"},
		{Name: "wm", Kind: ParamCorpusPercentile, Column: "memory_usage", Percentile: &ParamRef{Ref: "frac"}},
	}}
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	r := NewParamResolver(staticConfig(map[string]string{"f": "0.5"}), src)
	env, err := r.Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Median memory_usage across the whole seed; just assert it resolved to a
	// concrete scalar (the exact value is checked in the parity test).
	if env["wm"].IsPartitioned() {
		t.Fatalf("expected scalar watermark")
	}
	if env["wm"].Scalar <= 0 {
		t.Fatalf("watermark = %v, want > 0", env["wm"].Scalar)
	}
}

func TestResolvePartitionedPercentile(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{
		{Name: "frac", Kind: ParamConfig, Key: "f"},
		{Name: "wm", Kind: ParamCorpusPercentile, Column: "memory_usage", Percentile: &ParamRef{Ref: "frac"}, PartitionBy: []string{"language"}},
	}}
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	r := NewParamResolver(staticConfig(map[string]string{"f": "0.5"}), src)
	env, err := r.Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	v := env["wm"]
	if !v.IsPartitioned() || v.PartitionCol != "language" {
		t.Fatalf("expected partitioned-by-language value, got %+v", v)
	}
	if _, ok := v.Partition["promql"]; !ok {
		t.Fatalf("expected a promql partition entry, got %+v", v.Partition)
	}
}

func TestResolveCountRatio(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{{
		Name: "oom_frac", Kind: ParamCorpusCountRatio,
		NumeratorScope:   Scope{"exit_status": "oom"},
		DenominatorScope: Scope{"route": "A"},
	}}}
	src := NewJSONLCorpusSource("testdata/seed.jsonl", 0)
	r := NewParamResolver(staticConfig(map[string]string{}), src)
	env, err := r.Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// 2 oom rows over 7 route-A rows in the seed.
	got := env["oom_frac"].Scalar
	if got <= 0 || got >= 1 {
		t.Fatalf("count ratio = %v, want a fraction in (0,1)", got)
	}
}

func TestResolveCycleFails(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{
		{Name: "a", Kind: ParamCorpusPercentile, Column: "memory_usage", Percentile: &ParamRef{Ref: "b"}},
		{Name: "b", Kind: ParamCorpusPercentile, Column: "memory_usage", Percentile: &ParamRef{Ref: "a"}},
	}}
	r := NewParamResolver(staticConfig(map[string]string{}), NewJSONLCorpusSource("testdata/seed.jsonl", 0))
	_, err := r.Resolve(context.Background(), cat)
	if err == nil || stringIndex(err.Error(), "cycle") < 0 {
		t.Fatalf("expected a cycle error, got: %v", err)
	}
}

func TestResolveCorpusKindWithoutSourceFails(t *testing.T) {
	cat := &Catalog{Params: []ParamSpec{
		{Name: "frac", Kind: ParamConfig, Key: "f"},
		{Name: "wm", Kind: ParamCorpusPercentile, Column: "memory_usage", Percentile: &ParamRef{Ref: "frac"}},
	}}
	r := NewParamResolver(staticConfig(map[string]string{"f": "0.5"}), nil)
	_, err := r.Resolve(context.Background(), cat)
	if err == nil || stringIndex(err.Error(), "no corpus source") < 0 {
		t.Fatalf("expected a missing-source error, got: %v", err)
	}
}

// TestQuantileExactMatchesNearestRank pins the quantile formula so the JSONL and
// CH paths stay in lockstep.
func TestQuantileExactMatchesNearestRank(t *testing.T) {
	vs := []float64{10, 20, 30, 40, 50}
	cases := []struct {
		p    float64
		want float64
	}{
		{0.0, 10},
		{0.5, 30}, // idx = floor(0.5*5)=2 -> sorted[2]=30
		{0.9, 50}, // idx = floor(0.9*5)=4 -> sorted[4]=50
		{1.0, 50},
	}
	for _, c := range cases {
		if got := quantileExact(vs, c.p); got != c.want {
			t.Errorf("quantileExact(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}
