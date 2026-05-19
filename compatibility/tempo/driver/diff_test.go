package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildURL_Search(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:     "x",
		Query:    `{ resource.service.name = "checkout" }`,
		Endpoint: "search",
	}
	start := time.Unix(1000, 0)
	end := time.Unix(2000, 0)
	u, err := buildURL("http://tempo:3200", tc, "tempo", start, end, 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/search?") {
		t.Fatalf("unexpected url: %s", u)
	}
	if !strings.Contains(u, "limit=200") {
		t.Fatalf("missing limit: %s", u)
	}
	if !strings.Contains(u, "start=1000") || !strings.Contains(u, "end=2000") {
		t.Fatalf("missing start/end: %s", u)
	}
	if !strings.Contains(u, "q=") {
		t.Fatalf("missing q= param: %s", u)
	}
}

func TestBuildURL_SearchRecent(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "search_recent"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(0, 0), time.Unix(0, 0), 20)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/search/recent?") {
		t.Fatalf("unexpected url: %s", u)
	}
}

func TestBuildURL_TracesByID(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:            "x",
		Endpoint:        "traces",
		TraceIDTemplate: "checkout/0",
	}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(0, 0), time.Unix(0, 0), 20)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/traces/") {
		t.Fatalf("unexpected url: %s", u)
	}
	// Hex-encoded 16-byte ID = 32 hex chars; verify the URL contains a
	// 32-hex-char suffix so the differ derived a real ID.
	parts := strings.Split(u, "/")
	id := parts[len(parts)-1]
	if len(id) != 32 {
		t.Fatalf("trace id length = %d, want 32 (hex of 16 bytes): %q", len(id), id)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("non-hex char in trace id: %q", id)
		}
	}
}

func TestDeriveTraceIDFromTemplate_DeterministicAcrossCalls(t *testing.T) {
	t.Parallel()
	a, err := deriveTraceIDFromTemplate("checkout/0")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	b, err := deriveTraceIDFromTemplate("checkout/0")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a != b {
		t.Fatalf("non-deterministic derivation: %s vs %s", a, b)
	}
	c, err := deriveTraceIDFromTemplate("checkout/1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a == c {
		t.Fatal("trace IDs for checkout/0 and checkout/1 collided")
	}
}

func TestDeriveTraceIDFromTemplate_MatchesSeederHash(t *testing.T) {
	t.Parallel()
	// The seeder's deriveTraceID (in seeder.go) is the canonical hash;
	// the differ's deriveTraceIDFromTemplate must produce byte-identical
	// output for the same (svc, idx) pair or /api/traces/<id> will 404
	// against the real Tempo backend.
	gotDiffer, err := deriveTraceIDFromTemplate("checkout/0")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// Compute the seeder's hash directly.
	seederID := deriveTraceID("checkout", 0)
	wantHex := ""
	for _, b := range seederID[:] {
		const hexDigits = "0123456789abcdef"
		wantHex += string(hexDigits[b>>4]) + string(hexDigits[b&0xf])
	}
	if gotDiffer != wantHex {
		t.Fatalf("differ trace ID = %s, seeder trace ID = %s — these MUST match", gotDiffer, wantHex)
	}
}

func TestDeriveTraceIDFromTemplate_BadFormatFails(t *testing.T) {
	t.Parallel()
	if _, err := deriveTraceIDFromTemplate("missing-slash"); err == nil {
		t.Fatal("expected error on missing slash")
	}
	if _, err := deriveTraceIDFromTemplate("svc/notanumber"); err == nil {
		t.Fatal("expected error on non-numeric idx")
	}
}

func TestBuildURL_TagsV1(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "tags_v1"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/search/tags?") {
		t.Fatalf("unexpected url: %s", u)
	}
	if !strings.Contains(u, "start=1000") || !strings.Contains(u, "end=2000") {
		t.Fatalf("missing start/end: %s", u)
	}
}

func TestBuildURL_TagsV2WithScope(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "tags_v2", Scope: "resource"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/v2/search/tags?") {
		t.Fatalf("unexpected url: %s", u)
	}
	if !strings.Contains(u, "scope=resource") {
		t.Fatalf("scope query missing: %s", u)
	}
}

func TestBuildURL_TagsV2WithoutScopeOmitsParam(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "tags_v2"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(0, 0), time.Unix(0, 0), 20)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if strings.Contains(u, "scope=") {
		t.Fatalf("scope= should be omitted when Scope is empty: %s", u)
	}
}

func TestBuildURL_TagValuesV1(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "tag_values_v1", TagName: "service.name"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/search/tag/service.name/values?") {
		t.Fatalf("unexpected url: %s", u)
	}
}

func TestBuildURL_TagValuesV2(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "x", Endpoint: "tag_values_v2", TagName: "deployment.env"}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/v2/search/tag/deployment.env/values?") {
		t.Fatalf("unexpected url: %s", u)
	}
}

func TestCompareForEndpoint_DispatchesTagShape(t *testing.T) {
	t.Parallel()
	// Both bodies are V1 tag-names envelopes; the trace-search Compare
	// would error on the shape, so a clean Diff confirms the dispatch
	// routed to CompareTagNames.
	a := []byte(`{"tagNames":["service.name","http.method"]}`)
	b := []byte(`{"tagNames":["service.name","http.method"]}`)
	tc := CorpusCase{Name: "tags_match", Endpoint: "tags_v1"}
	d, err := compareForEndpoint(tc, a, b)
	if err != nil {
		t.Fatalf("compareForEndpoint: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected equal on identical tag names, got %+v", d)
	}
}

func TestBuildURL_MetricsRange(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:     "m",
		Query:    `{ } | rate()`,
		Endpoint: "metrics_range",
		Step:     "60s",
	}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/metrics/query_range?") {
		t.Fatalf("unexpected url: %s", u)
	}
	if !strings.Contains(u, "step=60s") {
		t.Fatalf("missing step=60s: %s", u)
	}
	if !strings.Contains(u, "start=1000") || !strings.Contains(u, "end=2000") {
		t.Fatalf("missing start/end: %s", u)
	}
	if !strings.Contains(u, "q=") {
		t.Fatalf("missing q=: %s", u)
	}
}

func TestBuildURL_MetricsRangeMissingStepFails(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:     "m",
		Query:    `{ } | rate()`,
		Endpoint: "metrics_range",
	}
	if _, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200); err == nil {
		t.Fatal("expected error: metrics_range without Step")
	}
}

func TestBuildURL_MetricsInstant(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:     "m",
		Query:    `{ } | rate()`,
		Endpoint: "metrics_instant",
	}
	u, err := buildURL("http://tempo:3200", tc, "tempo", time.Unix(1000, 0), time.Unix(2000, 0), 200)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(u, "http://tempo:3200/api/metrics/query?") {
		t.Fatalf("unexpected url: %s", u)
	}
	if !strings.Contains(u, "start=1000") || !strings.Contains(u, "end=2000") {
		t.Fatalf("missing start/end: %s", u)
	}
	if !strings.Contains(u, "q=") {
		t.Fatalf("missing q=: %s", u)
	}
	if strings.Contains(u, "step=") {
		t.Fatalf("instant endpoint must not carry step: %s", u)
	}
}
