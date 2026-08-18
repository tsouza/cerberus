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

func TestReadLivePatternsMetadata_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := readLivePatternsMetadata(filepath.Join(t.TempDir(), "missing.json"), time.Now())
	if err == nil {
		t.Fatal("missing file: want error, got nil")
	}
}

func TestReadLivePatternsMetadata_MalformedJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := readLivePatternsMetadata(path, time.Now()); err == nil || !strings.Contains(err.Error(), "decode:") {
		t.Fatalf("malformed JSON error=%v, want decode diagnostic", err)
	}
}

func TestValidateLivePatternsMetadata_RejectsEachInvalidField(t *testing.T) {
	t.Parallel()
	base := livePatternsTestMetadata()
	defaultNow := base.CreatedAt.Add(time.Minute)

	tests := []struct {
		name    string
		mutate  func(livePatternsMetadata) livePatternsMetadata
		now     time.Time
		wantErr string
	}{
		{
			name: "version mismatch",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.Version = livePatternsMetadataVersion + 1
				return m
			},
			wantErr: "version=",
		},
		{
			name: "empty selector",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.Selector = ""
				return m
			},
			wantErr: "selector is empty",
		},
		{
			name: "invalid window",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.Start = m.End
				return m
			},
			wantErr: "invalid window",
		},
		{
			name: "window span too wide",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.End = m.Start.Add(livePatternsWindowMaxSpan + time.Minute)
				m.CreatedAt = m.End
				return m
			},
			wantErr: "exceeds",
		},
		{
			name: "created_at precedes window end",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.CreatedAt = m.End.Add(-time.Second)
				return m
			},
			wantErr: "precedes window end",
		},
		{
			name: "created_at in the future",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				return m
			},
			now:     base.CreatedAt.Add(-time.Minute),
			wantErr: "is in the future",
		},
		{
			name: "entries_by_level empty",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.EntriesByLevel = map[string]int{}
				return m
			},
			wantErr: "entries_by_level is empty",
		},
		{
			name: "level key empty",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.EntriesByLevel = map[string]int{"": 10}
				return m
			},
			wantErr: "invalid level volume",
		},
		{
			name: "level key not lowercase",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.EntriesByLevel = map[string]int{"Error": 10}
				return m
			},
			wantErr: "invalid level volume",
		},
		{
			name: "level count not positive",
			mutate: func(m livePatternsMetadata) livePatternsMetadata {
				m.EntriesByLevel = map[string]int{"error": 0}
				return m
			},
			wantErr: "invalid level volume",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metadata := tc.mutate(base)
			now := tc.now
			if now.IsZero() {
				now = defaultNow
			}
			err := validateLivePatternsMetadata(metadata, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompareLivePatterns_ReportsFetchFailures(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	ok := livePatternsServer(t, metadata, `{"status":"success","data":[{"pattern":"p","level":"info","samples":[[%d,10]]}]}`)
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("reference unreachable", func(t *testing.T) {
		t.Parallel()
		results := compareLivePatterns(client, flags{addr1: "http://127.0.0.1:1", addr2: ok.URL}, metadata)
		for _, r := range results {
			if !strings.Contains(r.UnexpectedFailure, "reference (-addr-1) failed") {
				t.Fatalf("result=%+v, want reference failure", r)
			}
		}
	})

	t.Run("test endpoint unreachable", func(t *testing.T) {
		t.Parallel()
		results := compareLivePatterns(client, flags{addr1: ok.URL, addr2: "http://127.0.0.1:1"}, metadata)
		for _, r := range results {
			if !strings.Contains(r.UnexpectedFailure, "test endpoint (-addr-2) failed") {
				t.Fatalf("result=%+v, want test endpoint failure", r)
			}
		}
	})
}

func TestFetchLivePatterns_NonOKStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(server.Close)
	metadata := livePatternsTestMetadata()
	_, err := fetchLivePatterns(&http.Client{Timeout: 5 * time.Second}, server.URL, metadata)
	if err == nil || !strings.Contains(err.Error(), "status=500") {
		t.Fatalf("err=%v, want status=500 diagnostic", err)
	}
}

func TestDecodePatternsWire_HandlesMalformedEnvelopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want func(patternsWire) bool
	}{
		{
			name: "not JSON",
			body: `not json`,
			want: func(w patternsWire) bool { return strings.HasPrefix(w.Status, "decode error:") },
		},
		{
			name: "missing status",
			body: `{"data":[]}`,
			want: func(w patternsWire) bool { return w.Status == "missing" },
		},
		{
			name: "non-string status",
			body: `{"status":42,"data":[]}`,
			want: func(w patternsWire) bool { return strings.HasPrefix(w.Status, "invalid:") },
		},
		{
			name: "data not an array",
			body: `{"status":"success","data":{"oops":true}}`,
			want: func(w patternsWire) bool {
				return len(w.Data) == 1 && strings.HasPrefix(w.Data[0].Level, "__data_decode_error__")
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodePatternsWire([]byte(tc.body))
			if !tc.want(got) {
				t.Fatalf("decodePatternsWire(%q) = %+v", tc.body, got)
			}
		})
	}
}

func TestObserveLivePatterns_SetsEnvelopeError(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()

	t.Run("status not success", func(t *testing.T) {
		t.Parallel()
		obs := observeLivePatterns(patternsWire{Status: "error"}, metadata)
		if !strings.Contains(obs.envelopeErr, `status field="error"`) {
			t.Fatalf("envelopeErr=%q", obs.envelopeErr)
		}
	})

	t.Run("empty data", func(t *testing.T) {
		t.Parallel()
		obs := observeLivePatterns(patternsWire{Status: "success"}, metadata)
		if obs.envelopeErr != "data is missing, null, or empty" {
			t.Fatalf("envelopeErr=%q", obs.envelopeErr)
		}
	})
}

func TestObserveLivePatterns_RejectsMalformedSamples(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	inWindow := metadata.Start.Add(time.Minute).Unix()

	tests := []struct {
		name    string
		samples string
		want    string
	}{
		{
			name:    "wrong tuple length",
			samples: fmt.Sprintf(`[[%d,10,20]]`, inWindow),
			want:    "want 2",
		},
		{
			name:    "timestamp outside window",
			samples: fmt.Sprintf(`[[%d,10]]`, metadata.End.Add(time.Hour).Unix()),
			want:    "outside",
		},
		{
			name:    "non-positive count",
			samples: fmt.Sprintf(`[[%d,0]]`, inWindow),
			want:    "want positive",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"status":"success","data":[{"level":"info","samples":%s}]}`, tc.samples)
			wire := decodePatternsWire([]byte(body))
			obs := observeLivePatterns(wire, metadata)
			if !strings.Contains(obs.encodingErr, tc.want) {
				t.Fatalf("encodingErr=%q, want substring %q", obs.encodingErr, tc.want)
			}
		})
	}
}

func TestSideErrors_RendersEachCombination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		reference string
		test      string
		want      string
	}{
		{name: "both empty", reference: "", test: "", want: ""},
		{name: "reference only", reference: "ref boom", test: "", want: "axis: reference=ref boom"},
		{name: "test only", reference: "", test: "test boom", want: "axis: test endpoint=test boom"},
		{name: "both", reference: "ref boom", test: "test boom", want: "axis: reference=ref boom; test endpoint=test boom"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sideErrors("axis", tc.reference, tc.test); got != tc.want {
				t.Fatalf("sideErrors=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestSideSliceDiff_ReportsMismatch(t *testing.T) {
	t.Parallel()
	if got := sideSliceDiff("levels", []string{"error", "info"}, []string{"error"}, []string{"error", "info"}); got == "" {
		t.Fatal("mismatched reference: want non-empty diff")
	}
	if got := sideSliceDiff("levels", []string{"error", "info"}, []string{"error", "info"}, []string{"info"}); got == "" {
		t.Fatal("mismatched test endpoint: want non-empty diff")
	}
}

func TestCompareLivePatternsAxis_UnknownKindReportsDiagnostic(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	got := compareLivePatternsAxis("bogus_axis", patternsObservation{}, patternsObservation{}, metadata)
	if !strings.Contains(got, "unknown live patterns axis bogus_axis") {
		t.Fatalf("got=%q", got)
	}
}

func TestCompareLivePatternsAxis_LevelsMismatch(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	reference := patternsObservation{levels: []string{"error", "info", "warn"}}
	test := patternsObservation{levels: []string{"info", "warn"}}
	if got := compareLivePatternsAxis("patterns_levels", reference, test, metadata); got == "" {
		t.Fatal("mismatched levels: want non-empty diff")
	}
}

func TestCompareLivePatternsAxis_VolumeUndercountReported(t *testing.T) {
	t.Parallel()
	metadata := livePatternsTestMetadata()
	reference := patternsObservation{volume: map[string]int64{"error": 40, "info": 40, "warn": 40}}
	test := patternsObservation{volume: map[string]int64{"error": 0, "info": 40, "warn": 40}}
	got := compareLivePatternsAxis("patterns_volume", reference, test, metadata)
	if !strings.Contains(got, `test endpoint level="error" volume=0`) {
		t.Fatalf("got=%q", got)
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
