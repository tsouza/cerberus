package tempo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// --- Section A: wire-format conformance ----------------------------------
//
// Tempo's wire formats differ across endpoints:
//   - /api/search                  → {traces, metrics}
//   - /api/search/recent           → same shape as /api/search
//   - /api/search/tags             → {tagNames}
//   - /api/v2/search/tags          → {scopes:[{name, tags}, …]}
//   - /api/search/tag/<n>/values   → {tagValues:[string]}
//   - /api/v2/search/tag/<n>/values→ {tagValues:[{type, value}]}
//   - /api/traces/<id>             → {batches}
//   - /api/echo                    → text/plain "echo"
//   - /api/status/version          → {version, goVersion}
//
// Each test below pins one or more representative payloads against the
// documented JSON shape. We assert struct decoding succeeds and the
// per-endpoint required fields are present.

// TestConformance_TempoEchoWire — /api/echo returns text/plain "echo".
func TestConformance_TempoEchoWire(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if body != "echo" {
		t.Errorf("body: got %q, want \"echo\"", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: got %q, want text/plain prefix", ct)
	}
}

// TestConformance_TempoVersionWire — VersionResponse round-trip on both
// /api/status/version (Tempo's documented endpoint) and
// /api/status/buildinfo (Grafana's per-page probe). cerberus serves the
// same VersionResponse shape from both routes so Grafana's "loadBuildInfo"
// poll stops 404'ing.
func TestConformance_TempoVersionWire(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/status/version", "/api/status/buildinfo"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{}, "v1.2.3-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d on %s — Grafana's Tempo probe lands "+
					"on /api/status/buildinfo per page load; a 404 here "+
					"surfaces as 'Failure in retrieving build information' "+
					"in every user's console", resp.StatusCode, path)
			}
			var v tempo.VersionResponse
			if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if v.Version != "v1.2.3-test" {
				t.Errorf("version: got %q, want v1.2.3-test", v.Version)
			}
			if !strings.HasPrefix(v.GoVersion, "go") {
				t.Errorf("goVersion: got %q, want a string starting with go", v.GoVersion)
			}
		})
	}
}

// TestConformance_TempoSearchWire — empty + happy + multi-trace payloads.
func TestConformance_TempoSearchWire(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		samples []chclient.Sample
		path    string
	}{
		{
			name:    "empty_no_query",
			samples: nil,
			path:    "/api/search",
		},
		{
			name: "one_trace",
			samples: []chclient.Sample{
				{MetricName: "GET /api/users", Labels: map[string]string{"service.name": "frontend"}, Timestamp: ts, Value: 100_000_000},
			},
			path: "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: c.samples}, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var sr tempo.SearchResponse
			if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if sr.Traces == nil {
				t.Errorf("Traces is nil; should be empty slice, not nil")
			}
			// Cardinality: empty sample input must produce an
			// empty Traces array; non-empty input must produce at
			// least one trace. A regression that silently swaps
			// shapes would slip past the nil-check alone.
			switch {
			case len(c.samples) == 0 && len(sr.Traces) != 0:
				t.Errorf("Traces length: got %d, want 0 with no samples", len(sr.Traces))
			case len(c.samples) > 0 && len(sr.Traces) == 0:
				t.Errorf("Traces length: got 0, want non-empty (%d sample(s) seeded)", len(c.samples))
			}
		})
	}
}

// TestConformance_TempoSearchRecentWire — /api/search/recent returns
// SearchResponse shape; default limit applied when absent.
func TestConformance_TempoSearchRecentWire(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "x", Labels: map[string]string{"service.name": "frontend"}, Timestamp: time.Now(), Value: 1},
		},
	}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.Traces == nil {
		t.Errorf("Traces is nil; should be slice")
	}
	// Cardinality: stubQuerier returned 1 sample, so /api/search/recent
	// must surface at least one trace. A regression that drops the
	// projection on this endpoint would still pass the nil-check alone.
	if len(sr.Traces) == 0 {
		t.Errorf("Traces length: got 0, want non-empty (1 sample seeded)")
	}
}

// TestConformance_TempoSearchTagsWire — V1 returns {tagNames}; V2 returns
// {scopes:[{name, tags}]}.
func TestConformance_TempoSearchTagsWire(t *testing.T) {
	t.Parallel()

	t.Run("v1", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{strings: []string{"service.name", "host.name"}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/search/tags")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		var r tempo.SearchTagsResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Default V1 envelope mirrors upstream Tempo: dynamic attribute
		// keys only — intrinsics are reserved for the explicit
		// `scope=intrinsic` carve-out tested below.
		if len(r.TagNames) == 0 {
			t.Errorf("TagNames empty; expected dynamic attribute keys")
		}
	})
	t.Run("v2", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{strings: []string{"service.name"}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/v2/search/tags")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var r tempo.SearchTagsResponseV2
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// V2: three scopes — resource, span, intrinsic.
		seen := map[string]bool{}
		for _, s := range r.Scopes {
			seen[s.Name] = true
		}
		for _, want := range []string{"resource", "span", "intrinsic"} {
			if !seen[want] {
				t.Errorf("missing scope %q in %+v", want, r.Scopes)
			}
		}
	})
}

// TestConformance_TempoSearchTagValuesWire — V1 returns {tagValues}; V2
// wraps each value with type.
func TestConformance_TempoSearchTagValuesWire(t *testing.T) {
	t.Parallel()

	t.Run("v1_dynamic", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{strings: []string{"frontend", "backend"}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/search/tag/service.name/values")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var r tempo.SearchTagValuesResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.TagValues == nil {
			t.Errorf("TagValues nil; should be non-nil slice")
		}
	})
	t.Run("v1_intrinsic", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{strings: []string{"GET /api/users", "POST /api/orders"}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/search/tag/name/values")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var r tempo.SearchTagValuesResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("v2", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{strings: []string{"frontend"}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)
		resp, err := http.Get(srv.URL + "/api/v2/search/tag/service.name/values")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var r tempo.SearchTagValuesResponseV2
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, tv := range r.TagValues {
			if tv.Type == "" {
				t.Errorf("V2 entry missing type: %+v", tv)
			}
		}
	})
}

// TestConformance_TempoTraceByIDWire — 200 with batches when found, 404
// with error envelope when not. The error envelope is Tempo's distinct
// shape: {traceID, spanID, error, message}.
//
// Drives BOTH the v1 (`/api/traces/<id>`) and the v2
// (`/api/v2/traces/<id>`) URLs and asserts byte-identical bodies for the
// same input. Grafana 11.x's Tempo datasource defaults to
// `tempoApiVersion >= v2` for newly-provisioned datasources, so the v2
// URL is what the modern UI actually hits when drilling into a trace.
// Upstream Tempo's QueryTrace + QueryTraceV2 (see
// compatibility/tempo/upstream/pkg/httpclient/client.go) differ only in
// path — the response body is the same trace shape — so cerberus must
// alias the route to keep both datasource versions working. Before the
// alias landed, the v2 URL 404'd unconditionally and every modern
// Grafana trace drill-down broke (cerberus task #208).
//
// Also pins the Content-Type negotiation under Accept: the bare /
// `application/json` paths keep the documented JSON envelope; the
// `application/protobuf` / `application/x-protobuf` / `application/grpc`
// paths flip to Content-Type: application/protobuf (Grafana 11.x's
// Tempo plugin requires this; a JSON body surfaces as
// `proto: illegal wireType …` on the Grafana side).
func TestConformance_TempoTraceByIDWire(t *testing.T) {
	t.Parallel()

	// Both URLs must resolve to the same handler and produce
	// byte-identical bodies. The v2 entries are the new gate added by
	// PR fix/tempo-v2-traces-alias; the v1 entries remain the
	// historical contract.
	pathVariants := []struct {
		name string
		path string
	}{
		{name: "v1", path: "/api/traces/abc123"},
		{name: "v2", path: "/api/v2/traces/abc123"},
	}

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		// Capture each variant's body to a shared map (test runs are
		// sequential within this sub-test so the map write is safe).
		// A fixed Timestamp keeps the response deterministic for the
		// byte-level cross-check below.
		fixedTime := time.Unix(1700000000, 0).UTC()
		sample := chclient.Sample{
			MetricName: "x",
			Labels:     map[string]string{"service.name": "frontend"},
			Timestamp:  fixedTime,
			Value:      1,
		}
		bodies := map[string][]byte{}
		for _, pv := range pathVariants {
			q := &stubQuerier{samples: []chclient.Sample{sample}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + pv.path)
			if err != nil {
				t.Fatalf("%s: GET: %v", pv.name, err)
			}
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatalf("%s: read body: %v", pv.name, readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status=%d body=%q", pv.name, resp.StatusCode, raw)
			}
			var r tempo.TraceByIDResponse
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("%s: decode: %v", pv.name, err)
			}
			if r.Batches == nil {
				t.Errorf("%s: Batches nil; expected non-nil", pv.name)
			}
			bodies[pv.name] = raw
		}
		// v1 vs v2 must be byte-identical — same handler, same input.
		// If v2 ever diverges (different status, different shape, an
		// added/removed field), this fails loudly.
		if v1, v2 := bodies["v1"], bodies["v2"]; string(v1) != string(v2) {
			t.Errorf("v1 vs v2 body diverged:\n v1=%s\n v2=%s", v1, v2)
		}
	})
	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		for _, pv := range pathVariants {
			pv := pv
			t.Run(pv.name, func(t *testing.T) {
				t.Parallel()
				srv := newServer(&stubQuerier{samples: nil}, "v1.0.0-test")
				t.Cleanup(srv.Close)
				resp, err := http.Get(srv.URL + pv.path)
				if err != nil {
					t.Fatalf("GET: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("status=%d, want 404", resp.StatusCode)
				}
				var er tempo.ErrorResponse
				if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if er.TraceID != "abc123" || !er.Error {
					t.Errorf("envelope: got %+v, want traceID=abc123, error=true", er)
				}
			})
		}
	})

	// Accept-header negotiation — both the JSON and proto branches
	// stamp the documented Content-Type. The proto branch is what
	// Grafana 11.x's Tempo datasource plugin actually sends; the JSON
	// branch keeps existing callers (curl, conformance harnesses,
	// dashboards using the /api/ds/query proxy) working.
	t.Run("content_type_negotiation", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{samples: []chclient.Sample{{
			MetricName: "x", Labels: map[string]string{"service.name": "frontend"},
			Timestamp: time.Now(), Value: 1,
		}}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)

		// Each row: Accept header value → wantContentType substring.
		// Empty contains check ("") means "not application/protobuf";
		// the JSON branch routes through httperr.WriteJSON whose
		// Content-Type is "application/json".
		cases := []struct {
			accept string
			wantCT string // substring assertion
		}{
			{"", "application/json"},
			{"application/json", "application/json"},
			{"application/protobuf", "application/protobuf"},
			{"application/x-protobuf", "application/protobuf"},
			{"application/grpc", "application/protobuf"},
			// Multi-value Accept (Grafana's plugin sometimes sends
			// `application/protobuf, application/json;q=0.9`): the
			// proto entry wins.
			{"application/protobuf, application/json;q=0.9", "application/protobuf"},
		}
		// The same negotiation must hold for both the v1 and v2 URLs —
		// Grafana 11.x's plugin hits v2 by default and expects the same
		// Content-Type switching behavior the v1 path has shipped with.
		for _, pv := range pathVariants {
			for _, tc := range cases {
				req, err := http.NewRequest("GET", srv.URL+pv.path, nil)
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				if tc.accept != "" {
					req.Header.Set("Accept", tc.accept)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s Accept=%q: GET: %v", pv.name, tc.accept, err)
				}
				resp.Body.Close()
				if !strings.Contains(resp.Header.Get("Content-Type"), tc.wantCT) {
					t.Errorf("%s Accept=%q: Content-Type=%q, want substring %q",
						pv.name, tc.accept, resp.Header.Get("Content-Type"), tc.wantCT)
				}
			}
		}
	})
}

// --- Section B: error envelope per head ----------------------------------
//
// Tempo's envelope is {traceID, spanID, error, message} — distinct from
// Prom/Loki's {status, errorType, error}. Some endpoints (echo / version)
// don't surface errors per upstream contract.

// TestConformance_TempoErrorEnvelope — drives the handler through each
// error class and asserts the Tempo envelope shape.
func TestConformance_TempoErrorEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		path     string
		stub     *stubQuerier
		wantCode int
	}
	cases := []tc{
		{
			name: "400_invalid_traceql_search",
			stub: &stubQuerier{}, path: "/api/search?q=wharblgarbl",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "502_search_ch_failure",
			stub:     &stubQuerier{err: errors.New("clickhouse: connection refused")},
			path:     "/api/search?q=%7B%20resource.service.name%20%3D%20%22api%22%20%7D",
			wantCode: http.StatusBadGateway,
		},
		{
			name: "502_search_recent_ch_failure",
			stub: &stubQuerier{err: errors.New("clickhouse: timeout")},
			path: "/api/search/recent", wantCode: http.StatusBadGateway,
		},
		{
			name: "502_search_tags_ch_failure",
			stub: &stubQuerier{err: errors.New("ch failure")},
			path: "/api/search/tags", wantCode: http.StatusBadGateway,
		},
		{
			name: "502_search_tag_values_ch_failure",
			stub: &stubQuerier{err: errors.New("ch failure")},
			path: "/api/search/tag/service.name/values", wantCode: http.StatusBadGateway,
		},
		{
			name: "502_trace_by_id_ch_failure",
			stub: &stubQuerier{err: errors.New("ch failure")},
			path: "/api/traces/abc123", wantCode: http.StatusBadGateway,
		},
		{
			name: "404_trace_not_found",
			stub: &stubQuerier{samples: nil},
			path: "/api/traces/abc123", wantCode: http.StatusNotFound,
		},
		// v2 URL mirrors the v1 route — Grafana 11.x's Tempo plugin
		// defaults to `tempoApiVersion >= v2`, so error envelopes must
		// hold on both paths.
		{
			name: "502_trace_by_id_v2_ch_failure",
			stub: &stubQuerier{err: errors.New("ch failure")},
			path: "/api/v2/traces/abc123", wantCode: http.StatusBadGateway,
		},
		{
			name: "404_trace_not_found_v2",
			stub: &stubQuerier{samples: nil},
			path: "/api/v2/traces/abc123", wantCode: http.StatusNotFound,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(c.stub, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				t.Fatalf("status: got %d, want %d body=%s", resp.StatusCode, c.wantCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want json", ct)
			}
			// Verify envelope shape: Tempo's distinct {error, message} block.
			var er tempo.ErrorResponse
			if err := json.Unmarshal(body, &er); err != nil {
				t.Fatalf("decode envelope: %v body=%s", err, body)
			}
			if !er.Error {
				t.Errorf("error: got %v, want true; body=%s", er.Error, body)
			}
			if er.Message == "" {
				t.Errorf("message: empty (Grafana renders this)")
			}
		})
	}
}

// --- Section C: header pins ---------------------------------------------

// TestConformance_TempoHeaders — Content-Type + cerberus instrumentation
// headers present on /api/search.
func TestConformance_TempoHeaders(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{{
		MetricName: "x", Labels: map[string]string{"service.name": "frontend"},
		Timestamp: time.Now(), Value: 1,
	}}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Cerberus-Strategy"); got == "" {
		t.Errorf("X-Cerberus-Strategy: missing")
	}
	if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
		t.Errorf("X-Cerberus-Plan-Nodes: missing")
	}
	chMillis := resp.Header.Get("X-Cerberus-CH-Millis")
	if chMillis == "" {
		t.Errorf("X-Cerberus-CH-Millis: missing")
	} else if _, err := strconv.Atoi(chMillis); err != nil {
		t.Errorf("X-Cerberus-CH-Millis: got %q, want numeric", chMillis)
	}
}

// --- Section D: time parameter parsing matrix ---------------------------

// TestConformance_TempoStartEndMatrix — search-tags accepts start/end
// as unix-seconds int or nanoseconds (heuristic > 1e12), RFC3339 forms;
// invalid garbage 400s.
func TestConformance_TempoStartEndMatrix(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		query    string
		wantCode int
	}
	cases := []tc{
		{"unix_seconds", "start=1717995600&end=1717999200", http.StatusOK},
		{"unix_nanos", "start=1717995600000000000&end=1717999200000000000", http.StatusOK},
		{"rfc3339", "start=2024-01-01T00:00:00Z&end=2024-01-01T01:00:00Z", http.StatusOK},
		{"empty_optional", "", http.StatusOK},
		{"garbage_start", "start=banana&end=1717999200", http.StatusBadRequest},
		{"end_before_start", "start=1717999200&end=1717995600", http.StatusBadRequest},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{strings: []string{"x"}}, "v1.0.0-test")
			t.Cleanup(srv.Close)
			path := "/api/search/tags"
			if c.query != "" {
				path += "?" + c.query
			}
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
		})
	}
}

// --- Section E: trace-id edge cases -------------------------------------

// TestConformance_TempoTraceIDEdge — special characters in the trace
// path segment are passed to CH safely (parameterised) — no panic, no
// SQL injection. We expect 404 (no rows match) but no crash.
func TestConformance_TempoTraceIDEdge(t *testing.T) {
	t.Parallel()

	cases := []string{
		"DROP-TABLE-spans",
		"quote-test",
		"abc-def-123",
	}
	for _, id := range cases {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: nil}, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/api/traces/" + id)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want 404; body=%s", resp.StatusCode, body)
			}
		})
	}
}

// --- Section G: admission control / concurrency cap ---------------------

// TestConformance_TempoAdmitRejectsAtCap — saturated limiter returns 503
// + Retry-After on Tempo handler.
func TestConformance_TempoAdmitRejectsAtCap(t *testing.T) {
	t.Parallel()
	limiter := admit.New("tempo", 1)
	rel, ok := limiter.Acquire(context.Background())
	if !ok {
		t.Fatalf("setup acquire")
	}
	defer rel()

	h := tempo.New(&stubQuerier{}, schema.DefaultOTelTraces(), "v1.0.0-test", nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22api%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("Retry-After: missing on 503")
	}
}

// TestConformance_TempoAdmitMultipleEndpointsSamePool — the limiter is
// shared across every /api/* endpoint on the tempo handler. Saturate via
// /search, then verify /api/echo and /api/status/version also hit 503.
func TestConformance_TempoAdmitMultipleEndpointsSamePool(t *testing.T) {
	t.Parallel()
	limiter := admit.New("tempo", 1)
	rel, _ := limiter.Acquire(context.Background())
	defer rel()

	h := tempo.New(&stubQuerier{}, schema.DefaultOTelTraces(), "v1.0.0-test", nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/api/echo", "/api/status/version", "/api/search?q=%7B%7D"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status=%d, want 503", path, resp.StatusCode)
		}
	}
}

// TestConformance_TempoAdmitNilPassesThrough — nil limiter (the
// admit-disabled deployment) admits everything in parallel.
func TestConformance_TempoAdmitNilPassesThrough(t *testing.T) {
	t.Parallel()
	h := tempo.New(&stubQuerier{samples: []chclient.Sample{{
		MetricName: "x", Labels: map[string]string{"service.name": "x"}, Timestamp: time.Now(), Value: 1,
	}}}, schema.DefaultOTelTraces(), "v1.0.0-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	var hits atomic.Int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/api/echo")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				hits.Add(1)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 25 {
		t.Errorf("nil limiter must admit every request: got %d/25", hits.Load())
	}
}

// TestConformance_TempoGrafanaMsTimestamps_ResourcesProxy is the
// request-level pin for #194 on the Tempo side. Grafana 11.x's Tempo
// datasource sends 13-digit ms `start` / `end` timestamps over
// `/api/datasources/uid/<ds>/resources/...`; cerberus must decode them
// as milliseconds (not nanoseconds → year-58353 → ClickHouse
// `toDateTime64` overflow → HTTP 500 → empty Grafana panels).
//
// Drives ms-shaped bounds through every Tempo endpoint that consumes
// parseTempoStartEnd:
//   - /api/search
//   - /api/search/tags
//   - /api/search/tag/{name}/values
//   - /api/metrics/query_range
//   - /api/metrics/query   (instant)
//
// Each call must return HTTP 200; a 500 here means the heuristic
// regressed and a real ms timestamp was misrouted into the ns branch.
func TestConformance_TempoGrafanaMsTimestamps_ResourcesProxy(t *testing.T) {
	t.Parallel()

	// 2025-01-26 ≈ 1_737_864_000_000 ms; 1 hour later.
	const startMs = "1737000000000"
	const endMs = "1737003600000"

	cases := []struct {
		name string
		path string
	}{
		{"search", "/api/search?start=" + startMs + "&end=" + endMs},
		{"search-tags", "/api/search/tags?start=" + startMs + "&end=" + endMs},
		{"search-tag-values", "/api/search/tag/service.name/values?start=" + startMs + "&end=" + endMs},
		{
			"metrics-query-range",
			"/api/metrics/query_range?q=" + url.QueryEscape("{} | rate()") +
				"&start=" + startMs + "&end=" + endMs + "&step=60s",
		},
		{
			"metrics-query-instant",
			"/api/metrics/query?q=" + url.QueryEscape("{} | rate()") +
				"&start=" + startMs + "&end=" + endMs,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{}, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s (ms→ns misroute regression)", resp.StatusCode, body)
			}
		})
	}
}
