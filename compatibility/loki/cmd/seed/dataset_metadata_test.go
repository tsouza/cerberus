package main

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/compatibility/loki/corpus"
	"github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestDatasetMetadataNamesTheSeededStreams holds the hand-authored
// dataset metadata to the streams this seeder pushes: every selector in
// `all_selectors` matches exactly the label set of one seeded stream, and
// every seeded stream the generic `${SELECTOR}` resolver may target has a
// selector there. A selector naming a stream the seeder never pushes
// resolves to an empty answer on both backends, and a seeded stream with
// no selector is never drawn.
func TestDatasetMetadataNamesTheSeededStreams(t *testing.T) {
	t.Parallel()
	md, err := corpus.LoadMetadata(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		t.Fatal(err)
	}
	unindexed := []string{packedFormat, metadataProbeFormat}
	want := map[string]bool{}
	var seeded []map[string]string
	for _, s := range buildStreams(start) {
		seeded = append(seeded, s.labels)
		if !slices.Contains(unindexed, s.config.Format) {
			want[s.config.Name] = true
		}
	}

	covered := map[string]bool{}
	for _, sel := range md.AllSelectors {
		expr, err := lsyntax.ParseExpr(sel)
		if err != nil {
			t.Fatalf("parse %s: %v", sel, err)
		}
		m, ok := expr.(*lsyntax.MatchersExpr)
		if !ok {
			t.Fatalf("%s is not a bare stream selector", sel)
		}
		got := map[string]string{}
		for _, matcher := range m.Matchers() {
			got[matcher.Name] = matcher.Value
		}
		i := slices.IndexFunc(seeded, func(l map[string]string) bool { return maps.Equal(l, got) })
		if i < 0 {
			t.Errorf("all_selectors lists %s, which matches no stream the seeder pushes", sel)
			continue
		}
		covered[seeded[i]["service"]] = true
	}
	for name := range want {
		if !covered[name] {
			t.Errorf("seeded stream %q has no selector in all_selectors", name)
		}
	}
	if len(want) == 0 {
		t.Fatal("the seeder pushes no stream the generic resolver may target")
	}
}
