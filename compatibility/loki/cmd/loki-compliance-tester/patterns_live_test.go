package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func livePatternsTestMetadata() livePatternsMetadata {
	end := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	return livePatternsMetadata{
		Version:        livePatternsMetadataVersion,
		Selector:       `{service_name="cerberus-patterns-live"}`,
		Start:          end.Add(-2 * time.Minute),
		End:            end,
		CreatedAt:      end,
		EntriesByLevel: map[string]int{"error": 40, "info": 40, "warn": 40},
	}
}

func TestReadLivePatternsMetadata_RejectsStaleAndMalformedHandshakes(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	write := func(t *testing.T, value any, suffix string) string {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		path := filepath.Join(t.TempDir(), "live.json")
		if err := os.WriteFile(path, append(payload, suffix...), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	if got, err := readLivePatternsMetadata(write(t, metadata, "\n"), metadata.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	} else if got.Selector != metadata.Selector {
		t.Fatalf("selector=%q, want %q", got.Selector, metadata.Selector)
	}

	staleNow := metadata.CreatedAt.Add(livePatternsMetadataMaxAge + time.Second)
	if _, err := readLivePatternsMetadata(write(t, metadata, ""), staleNow); err == nil || !strings.Contains(err.Error(), "metadata age") {
		t.Fatalf("stale metadata error=%v, want age diagnostic", err)
	}
	if _, err := readLivePatternsMetadata(write(t, metadata, " {}"), metadata.CreatedAt); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing JSON error=%v", err)
	}
	withUnknown := map[string]any{
		"version": metadata.Version, "selector": metadata.Selector, "start": metadata.Start,
		"end": metadata.End, "created_at": metadata.CreatedAt, "entries_by_level": metadata.EntriesByLevel,
		"unrecognised": true,
	}
	if _, err := readLivePatternsMetadata(write(t, withUnknown, ""), metadata.CreatedAt); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error=%v", err)
	}
}

func TestCompareLivePatterns_GradesOnlyStableAxes(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	reference := livePatternsServer(t, metadata, `{
  "status":"success",
  "data":[
    {"pattern":"upstream alpha <*> beta","level":"info","samples":[[%d,20],[%d,15]]},
    {"pattern":"upstream error","level":"error","samples":[[%d,31]]},
    {"pattern":"upstream warn","level":"warn","samples":[[%d,40]]}
  ]
}`)
	testEndpoint := livePatternsServer(t, metadata, `{
  "status":"success",
  "data":[
    {"pattern":"clean room cluster one","level":"warn","samples":[[%d,20],[%d,15]]},
    {"pattern":"clean room cluster two","level":"info","samples":[[%d,40]]},
    {"pattern":"clean room cluster three","level":"error","samples":[[%d,30]]}
  ]
}`)

	results := compareLivePatterns(&http.Client{Timeout: 5 * time.Second}, flags{addr1: reference.URL, addr2: testEndpoint.URL}, metadata)
	if len(results) != len(livePatternsAxes()) {
		t.Fatalf("results=%d, want %d", len(results), len(livePatternsAxes()))
	}
	for _, result := range results {
		if !result.success() {
			t.Fatalf("stable-axis comparison failed for %s: %+v", result.TestCase.Kind, result)
		}
		if result.TestCase.Source != livePatternsSource || result.TestCase.Query != metadata.Selector {
			t.Fatalf("case envelope=%+v", result.TestCase)
		}
	}
}

func TestCompareLivePatterns_ReportsPerLevelOvercount(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	tooLarge := `{"status":"success","data":[` +
		`{"pattern":"a","level":"error","samples":[[%d,41]]},` +
		`{"pattern":"b","level":"info","samples":[[%d,20],[%d,20]]},` +
		`{"pattern":"c","level":"warn","samples":[[%d,40]]}]}`
	reference := livePatternsServer(t, metadata, tooLarge)
	testEndpoint := livePatternsServer(t, metadata, tooLarge)
	results := compareLivePatterns(&http.Client{Timeout: 5 * time.Second}, flags{addr1: reference.URL, addr2: testEndpoint.URL}, metadata)

	for _, result := range results {
		if result.TestCase.Kind == "patterns_volume" {
			if !strings.Contains(result.Diff, `level="error" volume=41 outside (0,40]`) {
				t.Fatalf("volume diff=%q", result.Diff)
			}
			return
		}
	}
	t.Fatal("patterns_volume result missing")
}

func TestObserveLivePatterns_RejectsNonIntegerSampleEncoding(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	wire := decodePatternsWire([]byte(`{"status":"success","data":[{"level":"info","samples":[[1.5,2]]}]}`))
	observation := observeLivePatterns(wire, metadata)
	if !strings.Contains(observation.encodingErr, "not an integer tuple") {
		t.Fatalf("encoding error=%q", observation.encodingErr)
	}
}

func livePatternsServer(t *testing.T, metadata livePatternsMetadata, bodyFormat string) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/patterns" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("query"); got != metadata.Selector {
			t.Errorf("query=%q, want %q", got, metadata.Selector)
		}
		if got := r.URL.Query().Get("start"); got != fmt.Sprint(metadata.Start.UnixNano()) {
			t.Errorf("start=%q, want %d", got, metadata.Start.UnixNano())
		}
		if got := r.URL.Query().Get("end"); got != fmt.Sprint(metadata.End.UnixNano()) {
			t.Errorf("end=%q, want %d", got, metadata.End.UnixNano())
		}
		ts := metadata.Start.Add(time.Minute).Unix()
		_, _ = fmt.Fprintf(w, bodyFormat, ts, ts+10, ts+20, ts+30)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}
