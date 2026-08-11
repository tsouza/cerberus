package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// compareRow builds one raw compare-SQL row as the engine's Sample
// decoder surfaces it — the internal __is_sel / __attr / __val label
// scheme the Sample projection (wrapCompareForSample) emits.
func compareRow(isSel, attr, val string, ts time.Time, count float64) chclient.Sample {
	return chclient.Sample{
		Labels: map[string]string{
			"__is_sel": isSel,
			"__attr":   attr,
			"__val":    val,
		},
		Timestamp: ts,
		Value:     count,
	}
}

// TestPostProcessCompare_Semantics pins the BaselineAggregator-mirror
// fold: cohort split, per-(cohort, attr) top-N by total count,
// per-attribute totals counting EVERY occurrence (including values the
// top-N cap dropped), zero-filled anchor grids, and the __meta_type
// label scheme.
func TestPostProcessCompare_Semantics(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	anchors := []time.Time{t0, t1}

	samples := []chclient.Sample{
		// baseline "name": three values — "a" (3 hits), "b" (2), "c" (1).
		compareRow("0", "name", "a", t0, 2),
		compareRow("0", "name", "a", t1, 1),
		compareRow("0", "name", "b", t0, 2),
		compareRow("0", "name", "c", t1, 1),
		// selection "name": one value.
		compareRow("1", "name", "a", t1, 4),
	}

	// 0 disables the grid budget, so this case keeps asserting the fold
	// semantics alone (the budget has its own boundary test below).
	series, err := tempo.PostProcessCompareForTest(samples, 2, anchors, 0)
	if err != nil {
		t.Fatalf("postProcessCompare: %v", err)
	}

	type key struct{ meta, attr, val string }
	got := map[key][]float64{}
	for _, s := range series {
		if len(s.Labels) != 2 {
			t.Fatalf("series must carry exactly {__meta_type, <attr>}; got %+v", s.Labels)
		}
		if s.Labels[0].Key != "__meta_type" {
			t.Fatalf("first label must be __meta_type (Tempo label order); got %+v", s.Labels)
		}
		if s.Exemplars == nil {
			t.Errorf("series %+v: Exemplars must be the empty array, not null", s.Labels)
		}
		vals := make([]float64, len(s.Samples))
		for i, smp := range s.Samples {
			vals[i] = smp.Value
			wantTS := anchors[i].UnixMilli()
			if smp.TimestampMs != wantTS {
				t.Errorf("series %+v sample[%d] ts=%d want %d (zero-filled full grid)", s.Labels, i, smp.TimestampMs, wantTS)
			}
		}
		got[key{s.Labels[0].Value, s.Labels[1].Key, s.Labels[1].Value}] = vals
	}

	want := map[key][]float64{
		// topN=2 keeps "a" (3) and "b" (2); "c" (1) is dropped from the
		// value series but still counted in the totals.
		{"baseline", "name", "a"}:          {2, 1},
		{"baseline", "name", "b"}:          {2, 0},
		{"selection", "name", "a"}:         {0, 4},
		{"baseline_total", "name", "nil"}:  {4, 2},
		{"selection_total", "name", "nil"}: {0, 4},
	}
	if len(got) != len(want) {
		t.Fatalf("series count = %d, want %d; got %v", len(got), len(want), got)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing series %+v; got %v", k, got)
			continue
		}
		if len(g) != len(w) {
			t.Errorf("series %+v: %v want %v", k, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("series %+v sample[%d] = %g, want %g", k, i, g[i], w[i])
			}
		}
	}
}

// TestCompareAnchorGrid_MatchesMatrixGrid — the Go-side grid must equal
// the SQL emitters' end-inclusive [Start, End] anchor set.
func TestCompareAnchorGrid_MatchesMatrixGrid(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Minute)
	got := tempo.CompareAnchorGridForTest(start, end, time.Minute)
	if len(got) != 4 {
		t.Fatalf("anchor count = %d, want 4", len(got))
	}
	for i, a := range got {
		want := start.Add(time.Duration(i) * time.Minute)
		if !a.Equal(want) {
			t.Errorf("anchor[%d] = %v, want %v", i, a, want)
		}
	}
}

// TestMetricsQueryRange_CompareDrilldownVerbatim — consumer-grade: the
// exact query Grafana Traces Drilldown's Comparison tab issues returns
// 200 with the __meta_type-labelled series shape Grafana renders.
// (Crawl signature traceql-metrics-compare-unsupported-422 pinned the
// prior 422.)
func TestMetricsQueryRange_CompareDrilldownVerbatim(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		compareRow("0", "resource.service.name", "shop", ts, 3),
		compareRow("1", "resource.service.name", "shop", ts, 1),
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{nestedSetParent<0 && true} | compare({status = error}, 10)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 2 value series + 2 totals series.
	if len(body.Series) != 4 {
		t.Fatalf("series count = %d, want 4: %+v", len(body.Series), body.Series)
	}
	metas := map[string]int{}
	for _, s := range body.Series {
		if len(s.Labels) == 0 || s.Labels[0].Key != "__meta_type" {
			t.Fatalf("every compare() series must lead with __meta_type; got %+v", s.Labels)
		}
		metas[s.Labels[0].Value]++
		// The aligned [10:00, 10:03] grid at 60s steps has 4 anchors;
		// every series zero-fills across all of them.
		if len(s.Samples) != 4 {
			t.Errorf("series %+v has %d samples, want 4 (zero-filled grid)", s.Labels, len(s.Samples))
		}
		if s.Exemplars == nil {
			t.Errorf("series %+v: exemplars must be [], not null", s.Labels)
		}
	}
	for _, m := range []string{"baseline", "selection", "baseline_total", "selection_total"} {
		if metas[m] != 1 {
			t.Errorf("__meta_type=%s series count = %d, want 1 (got %v)", m, metas[m], metas)
		}
	}
}

// TestMetricsQueryInstant_Compare — the instant endpoint collapses each
// compare() series to a single value (translateQueryRangeToInstant).
func TestMetricsQueryInstant_Compare(t *testing.T) {
	t.Parallel()

	// Instant evaluation anchors at `end`.
	end := time.Unix(1778580180, 0).UTC()
	q := &stubQuerier{samples: []chclient.Sample{
		compareRow("0", "status", "ok", end, 5),
		compareRow("1", "status", "error", end, 2),
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	vals := url.Values{}
	vals.Set("q", `{} | compare({status = error})`)
	vals.Set("start", fixtureStartUnix)
	vals.Set("end", fixtureEndUnix)
	u := srv.URL + "/api/metrics/query?" + vals.Encode()

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 4 {
		t.Fatalf("series count = %d, want 4: %+v", len(body.Series), body.Series)
	}
	byMeta := map[string]float64{}
	for _, s := range body.Series {
		if len(s.Labels) == 0 || s.Labels[0].Key != "__meta_type" {
			t.Fatalf("instant compare() series must lead with __meta_type; got %+v", s.Labels)
		}
		byMeta[s.Labels[0].Value] = s.Value
	}
	if byMeta["baseline"] != 5 || byMeta["selection"] != 2 {
		t.Errorf("instant values = %v, want baseline=5 selection=2", byMeta)
	}
	if byMeta["baseline_total"] != 5 || byMeta["selection_total"] != 2 {
		t.Errorf("instant totals = %v, want baseline_total=5 selection_total=2", byMeta)
	}
}

// TestPostProcessCompare_GridBudget is the regression pin for the compare()
// sample-budget bypass.
//
// compare() is the only metrics shape that SYNTHESISES its samples instead of
// forwarding ClickHouse's rows, so the Go-side result drain — the thing that
// enforces MaxQuerySamples for every other shape — never sees them. Measured on
// a live compose stack before the fix: a 15 m window at Grafana's auto-step
// drained a handful of sparse rows and emitted 540,300 samples against a
// configured 5,000-sample budget (108x) as a 21.6 MB HTTP 200. The budget has to
// be charged on the emitted product, which is what these cases pin.
func TestPostProcessCompare_GridBudget(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)

	// One attribute with one value yields exactly two series: the value series
	// and its totals series. Emitted samples are therefore 2 x len(anchors).
	const seriesPerAttr = 2
	rows := []chclient.Sample{compareRow("0", "name", "a", t0, 1)}

	anchorsOf := func(n int) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = t0.Add(time.Duration(i) * time.Minute)
		}
		return out
	}

	t.Run("at the cap is served", func(t *testing.T) {
		t.Parallel()
		const anchors = 50
		series, err := tempo.PostProcessCompareForTest(
			rows, 0, anchorsOf(anchors), seriesPerAttr*anchors,
		)
		if err != nil {
			t.Fatalf("a grid exactly at the budget must be served, got: %v", err)
		}
		got := 0
		for _, s := range series {
			got += len(s.Samples)
		}
		if got != seriesPerAttr*anchors {
			t.Fatalf("emitted %d samples, want %d — the case no longer sits ON the cap, "+
				"so the boundary it claims to pin is untested", got, seriesPerAttr*anchors)
		}
	})

	t.Run("one sample over the cap is refused", func(t *testing.T) {
		t.Parallel()
		const anchors = 50
		_, err := tempo.PostProcessCompareForTest(
			rows, 0, anchorsOf(anchors), seriesPerAttr*anchors-1,
		)
		if err == nil {
			t.Fatal("a grid one sample over the budget was served")
		}
		if !errors.Is(err, chclient.ErrTooManySamples) {
			t.Fatalf("over-budget compare must raise ErrTooManySamples so ClassifyErr "+
				"answers 422 (not a 500); got %v", err)
		}
	})

	t.Run("a sparse drain cannot buy an unbounded grid", func(t *testing.T) {
		t.Parallel()
		// THE bug: one input row, a dense output grid. Every bound upstream of
		// this point sees a single cheap row — the CH memory cap, the drain
		// budget and format.MaxResolutionPoints all pass — so if the product is
		// not charged here it is never charged at all.
		const anchors = 10_000
		if seriesPerAttr*anchors <= len(rows) {
			t.Fatal("test setup is wrong: the emitted grid must dwarf the drained rows")
		}
		_, err := tempo.PostProcessCompareForTest(rows, 0, anchorsOf(anchors), 5_000)
		if err == nil {
			t.Fatalf("%d drained row(s) synthesised a %d-sample grid under a 5,000-sample "+
				"budget without rejection — the drain-side budget cannot see synthesised "+
				"samples, so this path is unbounded", len(rows), seriesPerAttr*anchors)
		}
		if !errors.Is(err, chclient.ErrTooManySamples) {
			t.Fatalf("want ErrTooManySamples, got %v", err)
		}
	})

	t.Run("many series over few anchors is charged on the product", func(t *testing.T) {
		t.Parallel()
		// The mirror of the sparse-drain case: the anchor axis is small (well
		// inside format.MaxResolutionPoints) and the CARDINALITY axis is what
		// runs away. Neither axis alone is over any threshold — only the
		// product is, which is the whole point of charging it here.
		manyVals := make([]chclient.Sample, 0, 4096)
		for i := range 4096 {
			manyVals = append(manyVals,
				compareRow("0", "name", "v"+strconv.Itoa(i), t0, 1))
		}
		const anchors = 100
		_, err := tempo.PostProcessCompareForTest(manyVals, 0, anchorsOf(anchors), 100_000)
		if err == nil {
			t.Fatalf("4096 values over %d anchors (~%d samples) was served under a "+
				"100,000-sample budget", anchors, 4097*anchors)
		}
		if !errors.Is(err, chclient.ErrTooManySamples) {
			t.Fatalf("want ErrTooManySamples, got %v", err)
		}
	})

	t.Run("a non-positive budget stays inert", func(t *testing.T) {
		t.Parallel()
		// Matches the cursor's and requireSubquerySampleBudget's semantics, so a
		// handler that never wired a budget keeps serving.
		if _, err := tempo.PostProcessCompareForTest(rows, 0, anchorsOf(10_000), 0); err != nil {
			t.Fatalf("maxSamples<=0 must disable the budget, got: %v", err)
		}
	})
}
