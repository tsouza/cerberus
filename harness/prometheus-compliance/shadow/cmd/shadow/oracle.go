package main

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/harness/prometheus-compliance/shadow"
	"github.com/tsouza/cerberus/internal/promshim/local"
)

// localOracle is the in-process PromQL oracle for shadow-mode. It wraps
// internal/promshim/local's Prometheus engine over a SampleStore seeded with
// the canonical "shadow corpus" dataset, evaluates each corpus query as an
// instant query at the harness's evaluation timestamp, and converts the
// promshim result into the shadow VectorResult shape the differ understands.
//
// The seeded dataset mirrors the shape exercised by the smoke corpus
// (`harness/prometheus-compliance/shadow/corpus/smoke.txt`):
//
//   - http_requests_total counters across job × method
//   - up gauges across job
//   - node_load1 gauges across job × instance
//   - histogram buckets for histogram_quantile
//
// Anchored at baseTS so timestamps line up with the harness's --at flag
// (defaults to "5 minutes after baseTS" if --at is unset; see main.go).
//
// Why an in-process oracle: the differential signal is "does cerberus's CH
// pipeline drift from the reference Prometheus engine on the same dataset?".
// Spinning up real Prometheus would gain us nothing — the engine API the
// promshim wraps is the exact upstream engine. Keeping the oracle in-process
// also means the shadow workflow runs without containers (the heavyweight
// reference lives in `harness/prometheus-compliance/`'s Docker Compose stack).
type localOracle struct {
	engine *local.Engine
	store  *local.SampleStore
	at     time.Time
}

// shadowBaseTS is the anchor for the seeded dataset. Choosing a stable epoch
// (rather than time.Now()) keeps oracle results deterministic across runs.
var shadowBaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// defaultEvalOffset is added to shadowBaseTS when the user does not pass --at.
// Five minutes of seeded counter samples leaves the rate()/histogram_quantile
// queries with enough data to produce non-trivial output.
const defaultEvalOffset = 5 * time.Minute

// newLocalOracle builds the oracle, seeds the in-memory dataset, and pins the
// evaluation timestamp. If at is the zero value, it defaults to
// shadowBaseTS + defaultEvalOffset so the smoke corpus produces real samples.
func newLocalOracle(at time.Time) *localOracle {
	store := local.NewSampleStore()
	seedShadowDataset(store)
	if at.IsZero() {
		at = shadowBaseTS.Add(defaultEvalOffset)
	}
	return &localOracle{
		engine: local.NewEngine(local.Options{}),
		store:  store,
		at:     at,
	}
}

// Evaluate runs query as an instant query against the seeded SampleStore and
// returns the result in the shadow VectorResult shape.
func (o *localOracle) Evaluate(ctx context.Context, query string) (shadow.VectorResult, error) {
	res, err := o.engine.Instant(ctx, o.store, query, o.at)
	if err != nil {
		return shadow.VectorResult{}, fmt.Errorf("oracle: instant %q: %w", query, err)
	}
	return shadow.ResultToVector(res), nil
}

// seedShadowDataset populates store with the canonical shadow-mode dataset.
//
// Layout (10 minutes inclusive at 15s; counter values are cumulative):
//
//	http_requests_total{job="api",   method="get"}    +1 every 15s
//	http_requests_total{job="api",   method="post"}   +2 every 15s
//	http_requests_total{job="batch", method="get"}    +5 every 15s
//	node_load1{job="api",   instance="0"}             gauge: 1,2,3,...
//	node_load1{job="api",   instance="1"}             gauge: 2,4,6,...
//	node_load1{job="batch", instance="0"}             gauge: 5,5,5,...
//	up{job="api"}                                     ≡ 1
//	up{job="batch"}                                   ≡ 1
//	up{job="web"}                                     ≡ 0
//	http_request_duration_seconds_bucket{le=…}        classic histogram buckets
//
// This mirrors the shape exercised by promql_shadow_test.go's promqlSeed but
// is intentionally maintained independently of the test file — the test
// dataset is the suite's contract; this dataset is the CLI's.
func seedShadowDataset(store *local.SampleStore) {
	const samples = 41 // 10 min @ 15s, inclusive of both endpoints
	step := 15 * time.Second

	counters := []struct {
		lset labels.Labels
		incr float64
	}{
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "get"), 1},
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "post"), 2},
		{labels.FromStrings("__name__", "http_requests_total", "job", "batch", "method", "get"), 5},
	}
	for _, c := range counters {
		v := 0.0
		for i := 0; i < samples; i++ {
			store.Append(c.lset, shadowBaseTS.Add(time.Duration(i)*step).UnixMilli(), v)
			v += c.incr
		}
	}

	gauges := []struct {
		lset labels.Labels
		fn   func(i int) float64
	}{
		{labels.FromStrings("__name__", "node_load1", "job", "api", "instance", "0"), func(i int) float64 { return float64(i + 1) }},
		{labels.FromStrings("__name__", "node_load1", "job", "api", "instance", "1"), func(i int) float64 { return float64((i + 1) * 2) }},
		{labels.FromStrings("__name__", "node_load1", "job", "batch", "instance", "0"), func(int) float64 { return 5 }},
	}
	for _, g := range gauges {
		for i := 0; i < samples; i++ {
			store.Append(g.lset, shadowBaseTS.Add(time.Duration(i)*step).UnixMilli(), g.fn(i))
		}
	}

	ups := []struct {
		lset labels.Labels
		v    float64
	}{
		{labels.FromStrings("__name__", "up", "job", "api"), 1},
		{labels.FromStrings("__name__", "up", "job", "batch"), 1},
		{labels.FromStrings("__name__", "up", "job", "web"), 0},
	}
	for _, u := range ups {
		for i := 0; i < samples; i++ {
			store.Append(u.lset, shadowBaseTS.Add(time.Duration(i)*step).UnixMilli(), u.v)
		}
	}

	// Classic histogram buckets for histogram_quantile(...) queries.
	bucketSpec := []struct {
		le   string
		incr float64
	}{
		{"0.5", 6},
		{"1", 9},
		{"2", 15},
		{"5", 22},
		{"+Inf", 22},
	}
	for _, b := range bucketSpec {
		lset := labels.FromStrings(
			"__name__", "http_request_duration_seconds_bucket",
			"job", "api",
			"le", b.le,
		)
		v := 0.0
		for i := 0; i < samples; i++ {
			store.Append(lset, shadowBaseTS.Add(time.Duration(i)*step).UnixMilli(), v)
			v += b.incr
		}
	}
}
