package migrateverify

import (
	"reflect"
	"sort"
	"testing"
)

func TestLogqlMatcherKeys(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []string
	}{
		{"bare selector", `{job="api"}`, []string{"job"}},
		{"selector with pipeline", `{job="api", app="x"} |= "error"`, []string{"app", "job"}},
		{"metric query has no LogSelectorExpr", `sum(rate({job="api"}[5m]))`, nil},
		{"unparseable expr contributes nothing", `{job=`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logqlMatcherKeys(tc.expr)
			sort.Strings(got)
			if !keySlicesEqual(got, tc.want) {
				t.Errorf("logqlMatcherKeys(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// keySlicesEqual compares two key slices ignoring the nil-vs-empty distinction:
// sortedStringSet always returns a non-nil (possibly zero-length) slice, so a
// literal nil in a test's "want" column means simply "no keys".
func keySlicesEqual(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func TestTraceqlAttributeKeys(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []string
	}{
		{"single attribute", `{ span.http.status_code = 500 }`, []string{"http.status_code"}},
		{"two attributes, one filter", `{ span.a = "b" && resource.c = "d" }`, []string{"a", "c"}},
		{"nested spanset operation", `{ .a = 1 } && { .b = 2 }`, []string{"a", "b"}},
		{"intrinsic contributes nothing", `{ name = "GET" }`, nil},
		{"bare true filter contributes nothing", `{}`, nil},
		{"unparseable expr contributes nothing", `{ span.a = `, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := traceqlAttributeKeys(tc.expr)
			sort.Strings(got)
			if !keySlicesEqual(got, tc.want) {
				t.Errorf("traceqlAttributeKeys(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestDiscoveryProbesForCorpus(t *testing.T) {
	t.Run("prom-only corpus anchors nothing", func(t *testing.T) {
		got := discoveryProbesForCorpus([]Query{{Head: HeadProm, Kind: KindMetricMatrix, Expr: "up"}})
		if len(got) != 0 {
			t.Errorf("got %+v, want no discovery probes: a prom-only corpus must stay byte-identical", got)
		}
	})

	t.Run("loki entry anchors labels + one label-values probe per matcher key", func(t *testing.T) {
		got := discoveryProbesForCorpus([]Query{
			{Head: HeadLoki, Kind: KindLogStream, Expr: `{job="api"}`},
		})
		want := []Query{
			{Head: HeadLoki, Lang: "logql", Kind: KindTagDiscovery, Surface: SurfaceLokiLabels, Source: discoverySourceTag},
			{Head: HeadLoki, Lang: "logql", Kind: KindTagDiscovery, Surface: SurfaceLokiLabelValues, TagName: "job", Source: discoverySourceTag},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("tempo entry anchors both unfiltered tags probes and one tag-values pair per attribute key", func(t *testing.T) {
		got := discoveryProbesForCorpus([]Query{
			{Head: HeadTempo, Kind: KindTraceSearch, Expr: `{ span.a = "b" }`},
		})
		want := []Query{
			{Head: HeadTempo, Lang: "traceql", Kind: KindTagDiscovery, Surface: SurfaceTempoTagsV1, Source: discoverySourceTag},
			{Head: HeadTempo, Lang: "traceql", Kind: KindTagDiscovery, Surface: SurfaceTempoTagsV2, Source: discoverySourceTag},
			{Head: HeadTempo, Lang: "traceql", Kind: KindTagDiscovery, Surface: SurfaceTempoTagValuesV1, TagName: "a", Source: discoverySourceTag},
			{Head: HeadTempo, Lang: "traceql", Kind: KindTagDiscovery, Surface: SurfaceTempoTagValuesV2, TagName: "a", Source: discoverySourceTag},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("keys are deduplicated and sorted across the whole corpus", func(t *testing.T) {
		got := discoveryProbesForCorpus([]Query{
			{Head: HeadLoki, Kind: KindLogStream, Expr: `{job="api"}`},
			{Head: HeadLoki, Kind: KindLogStream, Expr: `{job="worker", app="x"}`},
		})
		var values []string
		for _, q := range got {
			if q.Surface == SurfaceLokiLabelValues {
				values = append(values, q.TagName)
			}
		}
		want := []string{"app", "job"}
		if !reflect.DeepEqual(values, want) {
			t.Errorf("label-values keys = %v, want %v (deduplicated, sorted)", values, want)
		}
	})
}
