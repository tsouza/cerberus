package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
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

	series := tempo.PostProcessCompareForTest(samples, 2, anchors)

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
