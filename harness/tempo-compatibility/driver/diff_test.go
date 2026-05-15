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
