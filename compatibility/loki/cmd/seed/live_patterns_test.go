package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBuildLivePatternsFixture_UsesOneInjectedClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 12, 34, 56, 789, time.UTC)
	fixture := buildLivePatternsFixture(now)

	if fixture.stream.config.ServiceName != livePatternsService {
		t.Fatalf("service_name=%q, want %q", fixture.stream.config.ServiceName, livePatternsService)
	}
	for _, config := range serviceConfigs {
		if config.ServiceName == livePatternsService {
			t.Fatalf("live service leaked into static serviceConfigs: %+v", config)
		}
	}
	wantEntries := len(livePatternsLevels) * livePatternsEntriesPerLevel
	if got := len(fixture.stream.entries); got != wantEntries {
		t.Fatalf("entries=%d, want %d", got, wantEntries)
	}
	wantCreatedAt := now.Truncate(time.Second)
	if !fixture.metadata.CreatedAt.Equal(wantCreatedAt) || !fixture.metadata.End.Equal(wantCreatedAt) {
		t.Fatalf("metadata clock=(created=%s end=%s), want both %s", fixture.metadata.CreatedAt, fixture.metadata.End, wantCreatedAt)
	}
	if !fixture.stream.entries[0].ts.Equal(fixture.metadata.Start) {
		t.Fatalf("first timestamp=%s, metadata start=%s", fixture.stream.entries[0].ts, fixture.metadata.Start)
	}
	last := fixture.stream.entries[len(fixture.stream.entries)-1].ts
	if !last.Before(fixture.metadata.End) {
		t.Fatalf("last timestamp=%s must be before window end=%s", last, fixture.metadata.End)
	}
	wantCounts := map[string]int{"error": 40, "info": 40, "warn": 40}
	if !reflect.DeepEqual(fixture.metadata.EntriesByLevel, wantCounts) {
		t.Fatalf("entries_by_level=%v, want %v", fixture.metadata.EntriesByLevel, wantCounts)
	}
}

func TestWriteLivePatternsMetadata_ReplacesCompleteJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "live-patterns.json")
	metadata := buildLivePatternsFixture(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)).metadata
	if err := writeLivePatternsMetadata(path, metadata); err != nil {
		t.Fatalf("writeLivePatternsMetadata: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got livePatternsMetadata
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("metadata is not complete JSON: %v\n%s", err, payload)
	}
	if !reflect.DeepEqual(got, metadata) {
		t.Fatalf("metadata=%+v, want %+v", got, metadata)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".live-patterns-*.json"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary metadata files remain after rename: %v", matches)
	}
}
