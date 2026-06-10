package tempo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
//
// Also pins the spec-compliant traceID hex shape (issue #209): every
// hex-shaped traceID on /api/search must be exactly 32 lowercase-hex
// chars (the canonical hex encoding of OTel's 16-byte TraceId).
// Cerberus historically stripped leading zeros to mirror reference
// Tempo's wire-format defect; this property-style assertion catches
// any regression that re-introduces stripping on the OUTPUT side
// regardless of which sample shape drives the projection.
func TestConformance_TempoSearchWire(t *testing.T) {
	t.Parallel()

	// Spec: TraceId is fixed 16 bytes → 32 lowercase-hex chars on
	// the wire. Anything shorter is a spec violation.
	traceIDHex := regexp.MustCompile(`^[0-9a-f]{32}$`)
	// Hex-shape sentinel — used to skip synthetic-key fallback
	// rows (e.g. `MetricName|TimestampNs`) that don't carry a real
	// hex traceID, so we only apply the canonical-shape rule where
	// the projection actually surfaced a hex value.
	hexLike := regexp.MustCompile(`^[0-9a-f]+$`)

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
		{
			// Issue #209: the projection now surfaces the OTel-CH
			// column verbatim, so a leading-zero traceID must
			// arrive as the canonical 32-char form, NOT the legacy
			// stripped shape. Seeds two traces whose canonical hex
			// starts with `00…` so any future reintroduction of
			// zero-stripping surfaces here as a < 32-char wire
			// value and trips the regex below.
			name: "leading_zero_trace_ids_padded",
			samples: []chclient.Sample{
				{
					MetricName: "GET /api/users",
					Labels: map[string]string{
						"service.name":            "frontend",
						"__cerberus_traceID":      "00af843259b0a78f5cbe59e11cbaf66b",
						"__cerberus_parentSpanID": "0000000000000000",
					},
					Timestamp: ts,
					Value:     100_000_000,
				},
				{
					MetricName: "POST /api/orders",
					Labels: map[string]string{
						"service.name":            "backend",
						"__cerberus_traceID":      "00000000000000000000000000000001",
						"__cerberus_parentSpanID": "0000000000000000",
					},
					Timestamp: ts,
					Value:     200_000_000,
				},
			},
			path: "/api/search?q=%7B%7D",
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
			// Property: every hex-shaped traceID on the wire must
			// match the OTel canonical 32-char lowercase-hex shape
			// (issue #209). Skip synthetic-key fallback rows whose
			// IDs aren't hex-only (e.g. `MetricName|Timestamp`).
			for _, tr := range sr.Traces {
				if tr.TraceID == "" || !hexLike.MatchString(tr.TraceID) {
					continue
				}
				if !traceIDHex.MatchString(tr.TraceID) {
					t.Errorf("traceID %q (len=%d) violates OTel canonical "+
						"32-char hex shape; cerberus must NOT strip leading "+
						"zeros on output (issue #209)",
						tr.TraceID, len(tr.TraceID))
				}
			}
		})
	}
}

// TestConformance_TempoTraceByIDWire_HexShape pins the wire-format
// invariant on /api/traces/{id}: every per-span TraceID / SpanID
// surfacing in the JSON response must match the OTel canonical hex
// shape (32 chars for trace IDs, 16 chars for span IDs, lowercase
// hex). Issue #209.
//
// Seeds a span row whose IDs start with leading zeros (the worst-case
// shape — reference Tempo's legacy wire layer would strip these) and
// asserts both fields round-trip as the canonical fixed-width form.
func TestConformance_TempoTraceByIDWire_HexShape(t *testing.T) {
	t.Parallel()

	const traceIDHex = "0000000000000000af843259b0a78f5c"
	const spanIDHex = "00000000000000ab"
	const parentSpanIDHex = "0000000000000000"

	q := &stubQuerier{
		samples: []chclient.Sample{{
			MetricName: "GET /api/users",
			Labels: map[string]string{
				"service.name":             "frontend",
				"__cerberus_traceID":       traceIDHex,
				"__cerberus_spanID":        spanIDHex,
				"__cerberus_parentSpanID":  parentSpanIDHex,
				"__cerberus_spanKind":      "SPAN_KIND_SERVER",
				"__cerberus_statusCode":    "STATUS_CODE_OK",
				"__cerberus_spanAttrsJSON": "{}",
			},
			Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			Value:     100_000_000,
		}},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/" + traceIDHex)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var tr tempo.TraceByIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}

	trace32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	span16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	sawSpan := false
	for _, b := range tr.Batches {
		for _, sp := range b.Spans {
			sawSpan = true
			if !trace32.MatchString(sp.TraceID) {
				t.Errorf("SpanEntry.TraceID = %q (len=%d), want canonical "+
					"32-char hex (issue #209)", sp.TraceID, len(sp.TraceID))
			}
			if !span16.MatchString(sp.SpanID) {
				t.Errorf("SpanEntry.SpanID = %q (len=%d), want canonical "+
					"16-char hex (issue #209)", sp.SpanID, len(sp.SpanID))
			}
			// ParentSpanId may legitimately be empty on the wire
			// (root span via the legacy fixture shape); when
			// present it must match the canonical 16-char form.
			if sp.ParentSpanID != "" && !span16.MatchString(sp.ParentSpanID) {
				t.Errorf("SpanEntry.ParentSpanID = %q (len=%d), want canonical "+
					"16-char hex (issue #209)", sp.ParentSpanID, len(sp.ParentSpanID))
			}
		}
	}
	if !sawSpan {
		t.Fatalf("response carried no spans; cannot exercise hex-shape invariant")
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

// TestConformance_TempoTraceByIDWire — 200 when found, 404 with error
// envelope when not. The error envelope is Tempo's distinct shape:
// {traceID, spanID, error, message}.
//
// Drives BOTH the v1 (`/api/traces/<id>`) and the v2
// (`/api/v2/traces/<id>`) URLs and pins their DISTINCT body shapes.
// Reference Tempo's two endpoints differ in envelope, not just path:
//
//   - v1 returns the bare trace — flattened `{"batches":[…]}` JSON /
//     proto-encoded *tempopb.Trace (upstream
//     modules/frontend/combiner/trace_by_id.go).
//   - v2 wraps it in a tempopb.TraceByIDResponse —
//     `{"trace":{…},"metrics":{}}` JSON via jsonpb / proto-encoded
//     envelope (upstream modules/frontend/combiner/trace_by_id_v2.go).
//
// Grafana 12.x unmarshals the v2 body as TraceByIDResponse before its
// OTLP conversion; an earlier cerberus revision aliased v2 to the v1
// handler and pinned byte parity, which broke every Grafana 12 trace
// drill-down with `proto: KeyValue: wiretype end group for non-group`.
// The inner trace must still be deterministic and identical across the
// two endpoints — that cross-check lives in
// TestTraceByIDV2_ProtoEnvelope (handler_trace_v2_test.go).
//
// Also pins the Content-Type negotiation under Accept: the bare /
// `application/json` paths keep the documented JSON envelope; the
// `application/protobuf` / `application/x-protobuf` / `application/grpc`
// paths flip to Content-Type: application/protobuf (Grafana's Tempo
// plugin requires this; a JSON body surfaces as
// `proto: illegal wireType …` on the Grafana side).
func TestConformance_TempoTraceByIDWire(t *testing.T) {
	t.Parallel()

	pathVariants := []struct {
		name string
		path string
	}{
		{name: "v1", path: "/api/traces/0123456789abcdef"},
		{name: "v2", path: "/api/v2/traces/0123456789abcdef"},
	}

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		// A fixed Timestamp keeps both bodies deterministic.
		fixedTime := time.Unix(1700000000, 0).UTC()
		sample := chclient.Sample{
			MetricName: "x",
			Labels:     map[string]string{"service.name": "frontend"},
			Timestamp:  fixedTime,
			Value:      1,
		}
		fetch := func(t *testing.T, path string) []byte {
			t.Helper()
			q := &stubQuerier{samples: []chclient.Sample{sample}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read body: %v", readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%q", resp.StatusCode, raw)
			}
			return raw
		}

		// v1: flattened batches shape, no envelope.
		v1Raw := fetch(t, pathVariants[0].path)
		var v1 tempo.TraceByIDResponse
		if err := json.Unmarshal(v1Raw, &v1); err != nil {
			t.Fatalf("v1 decode: %v", err)
		}
		if v1.Batches == nil {
			t.Errorf("v1: Batches nil; expected non-nil")
		}

		// v2: jsonpb-marshaled TraceByIDResponse envelope. Pin the
		// top-level keys structurally so an aliased-to-v1 regression
		// (top-level "batches", no "trace") fails loudly.
		v2Raw := fetch(t, pathVariants[1].path)
		var v2 map[string]json.RawMessage
		if err := json.Unmarshal(v2Raw, &v2); err != nil {
			t.Fatalf("v2 decode: %v", err)
		}
		if _, ok := v2["trace"]; !ok {
			t.Fatalf("v2: missing top-level \"trace\" envelope key; body=%s", v2Raw)
		}
		if _, ok := v2["metrics"]; !ok {
			t.Errorf("v2: missing top-level \"metrics\" key (reference Tempo always emits a non-nil metrics block); body=%s", v2Raw)
		}
		if _, ok := v2["batches"]; ok {
			t.Errorf("v2: stray top-level \"batches\" key — v2 must be the TraceByIDResponse envelope, not the bare v1 shape; body=%s", v2Raw)
		}
		var inner struct {
			ResourceSpans []json.RawMessage `json:"resourceSpans"`
		}
		if err := json.Unmarshal(v2["trace"], &inner); err != nil {
			t.Fatalf("v2: decode trace field: %v", err)
		}
		if len(inner.ResourceSpans) == 0 {
			t.Errorf("v2: trace.resourceSpans empty; body=%s", v2Raw)
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
				if er.TraceID != "0123456789abcdef" || !er.Error {
					t.Errorf("envelope: got %+v, want traceID=0123456789abcdef, error=true", er)
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
			path: "/api/traces/0123456789abcdef", wantCode: http.StatusBadGateway,
		},
		{
			name: "404_trace_not_found",
			stub: &stubQuerier{samples: nil},
			path: "/api/traces/0123456789abcdef", wantCode: http.StatusNotFound,
		},
		// v2 URL mirrors the v1 route — Grafana 11.x's Tempo plugin
		// defaults to `tempoApiVersion >= v2`, so error envelopes must
		// hold on both paths.
		{
			name: "502_trace_by_id_v2_ch_failure",
			stub: &stubQuerier{err: errors.New("ch failure")},
			path: "/api/v2/traces/0123456789abcdef", wantCode: http.StatusBadGateway,
		},
		{
			name: "404_trace_not_found_v2",
			stub: &stubQuerier{samples: nil},
			path: "/api/v2/traces/0123456789abcdef", wantCode: http.StatusNotFound,
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
// path segment are rejected up-front by the 16-/32-hex grammar gate
// with 400 ("invalid trace id"), matching reference Tempo. The gate
// runs before any CH lookup, so SQL injection / panic / crash paths
// are unreachable from these inputs — covered by inspection of the
// handler, this test pins the wire-format response.
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
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want 400; body=%s", resp.StatusCode, body)
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
