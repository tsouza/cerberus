package routerrules

import (
	"context"
	"testing"
)

// countingCorpusSource wraps a CorpusSource and counts how many times Aggregate
// is invoked. Each Aggregate call is exactly one full corpus pass in both real
// backends (jsonlCorpusSource streams the file per call; source_ch issues one
// SELECT per call), so the count is the resolver's corpus-scan count.
type countingCorpusSource struct {
	inner      CorpusSource
	aggregates int
}

func (c *countingCorpusSource) Aggregate(ctx context.Context, spec AggSpec) (Value, error) {
	c.aggregates++
	return c.inner.Aggregate(ctx, spec)
}

func (c *countingCorpusSource) EvalRule(ctx context.Context, q RuleQuery) ([]GroupResult, error) {
	return c.inner.EvalRule(ctx, q)
}

// TestResolveScanCount pins the resolver's actual cost model: it performs one
// corpus scan per corpus-kind param (~O(corpus-params)), NOT one scan per
// distinct scope-group. It exists so the ParamResolver doc — which used to claim
// a batching optimization the code never had — cannot silently drift from the
// implementation again. If batching is ever added (the ponytail note on
// ParamResolver), this test is where the new, lower count gets re-pinned.
func TestResolveScanCount(t *testing.T) {
	cat := loadCatalogT(t)
	corpus := GenerateBenchCorpus(nominalBenchParams())
	counter := &countingCorpusSource{inner: NewMemCorpusSource(corpus.Rows)}

	r := NewParamResolver(staticConfigLookup(benchConfig()), counter)
	if _, err := r.Resolve(context.Background(), cat); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	wantCorpusParams := 0
	for _, p := range cat.Params {
		switch p.Kind {
		case ParamCorpusPercentile, ParamCorpusAgg, ParamCorpusCountRatio:
			wantCorpusParams++
		case ParamConfig:
			// config leaves resolve inline, no corpus scan.
		}
	}

	if wantCorpusParams == 0 {
		t.Fatal("catalog has no corpus-kind params; scan-count test would be vacuous")
	}
	if counter.aggregates != wantCorpusParams {
		t.Errorf("resolver did %d corpus scans for %d corpus-kind params; expected one scan per corpus param (no batching). "+
			"If the resolver gained AggSpec batching, update this expectation and the ParamResolver doc together.",
			counter.aggregates, wantCorpusParams)
	}
}
