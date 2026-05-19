package tempo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

type stubQuerier struct {
	samples  []chclient.Sample
	strings  []string
	err      error
	lastSQL  string
	lastArgs []any
	// stringsBySQL lets tests stub multiple back-to-back QueryStrings
	// calls (e.g. /search/tags issues one query per attribute map);
	// when set, the longest substring match against the incoming SQL
	// picks the row set, with the bare `strings` field acting as the
	// default fallback.
	stringsBySQL map[string][]string
	// samplesBySQL lets tests stub multiple back-to-back Query calls
	// against different SQL shapes (e.g. /api/metrics/query_range
	// issues one matrix-shape query and one exemplars-shape query); the
	// first substring match against the incoming SQL picks the row set,
	// with the bare `samples` field acting as the default fallback.
	samplesBySQL map[string][]chclient.Sample
	// queriedSQLs records every SQL string Query was invoked with, in
	// arrival order. Lets tests assert that BOTH the matrix and the
	// exemplars queries actually fired.
	queriedSQLs []string
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.lastSQL = sql
	s.lastArgs = args
	s.queriedSQLs = append(s.queriedSQLs, sql)
	if s.err != nil {
		return nil, s.err
	}
	for needle, rows := range s.samplesBySQL {
		if strings.Contains(sql, needle) {
			return rows, nil
		}
	}
	return s.samples, nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	for needle, rows := range s.stringsBySQL {
		if strings.Contains(sql, needle) {
			return rows, nil
		}
	}
	return s.strings, nil
}

func newServer(q tempo.Querier, version string) *httptest.Server {
	h := tempo.New(q, schema.DefaultOTelTraces(), version, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestEcho(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || body != "echo" {
		t.Fatalf("status=%d body=%q want 200 \"echo\"", resp.StatusCode, body)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/status/version")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var v tempo.VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version != "v1.0.0-test" || v.GoVersion == "" {
		t.Fatalf("unexpected version body: %+v", v)
	}
}

func TestSearch_Empty(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// Grafana datasource health-check sometimes hits /api/search with no q.
	resp, err := http.Get(srv.URL + "/api/search")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 0 {
		t.Fatalf("expected empty traces, got %d", len(sr.Traces))
	}
}

func TestSearch_Query(t *testing.T) {
	t.Parallel()
	// hex-TraceID smuggled through the wrap projection via the reserved
	// __cerberus_traceID label key — toTraceSummaries reads it out so
	// the response surfaces real per-trace identity (32 hex chars)
	// rather than the legacy SpanName|Timestamp synthetic key.
	const wantTraceID = "0123456789abcdef0123456789abcdef"
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels: map[string]string{
					"service.name":       "frontend",
					"__cerberus_traceID": wantTraceID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     150_000_000, // 150ms in nanoseconds
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	if sr.Traces[0].TraceID != wantTraceID {
		t.Errorf("TraceID: got %q, want %q (32 hex chars from the search projection)",
			sr.Traces[0].TraceID, wantTraceID)
	}
	if len(sr.Traces[0].TraceID) != 32 {
		t.Errorf("TraceID length: got %d, want 32 (hex-encoded 16-byte trace id)",
			len(sr.Traces[0].TraceID))
	}
	if !isHexLower(sr.Traces[0].TraceID) {
		t.Errorf("TraceID format: got %q, want lowercase hex", sr.Traces[0].TraceID)
	}
	if sr.Traces[0].RootServiceName != "frontend" {
		t.Errorf("expected frontend service, got %q", sr.Traces[0].RootServiceName)
	}
	if sr.Traces[0].DurationMs != 150 {
		t.Errorf("expected 150ms, got %d", sr.Traces[0].DurationMs)
	}
}

// TestSearch_GroupsByTraceID asserts that multiple span rows sharing a
// real TraceID collapse into a single per-trace summary; this is the
// core behaviour change behind switching from the synthetic
// (SpanName | Timestamp) key to the real hex(TraceId).
func TestSearch_GroupsByTraceID(t *testing.T) {
	t.Parallel()
	const traceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const traceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Two spans on trace A — should collapse into one summary;
			// DurationMs reflects the max span duration; StartTimeUnixNano
			// the earliest span timestamp.
			{
				MetricName: "POST /api/orders",
				Labels: map[string]string{
					"service.name":       "checkout",
					"__cerberus_traceID": traceA,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     200_000_000,
			},
			{
				MetricName: "db.query",
				Labels: map[string]string{
					"service.name":       "checkout",
					"__cerberus_traceID": traceA,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 1, 0, time.UTC),
				Value:     50_000_000,
			},
			// One span on trace B — separate summary.
			{
				MetricName: "GET /healthz",
				Labels: map[string]string{
					"service.name":       "frontend",
					"__cerberus_traceID": traceB,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 5, 0, time.UTC),
				Value:     5_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 2 {
		t.Fatalf("expected 2 distinct traces (grouped by real TraceID), got %d", len(sr.Traces))
	}
	// Results sort ascending by TraceID.
	if sr.Traces[0].TraceID != traceA {
		t.Errorf("[0] TraceID: got %q, want %q", sr.Traces[0].TraceID, traceA)
	}
	if sr.Traces[1].TraceID != traceB {
		t.Errorf("[1] TraceID: got %q, want %q", sr.Traces[1].TraceID, traceB)
	}
	// DurationMs is the max of {200, 50}ms = 200ms across trace A's two spans.
	if sr.Traces[0].DurationMs != 200 {
		t.Errorf("[0] DurationMs: got %d, want 200 (max across grouped spans)",
			sr.Traces[0].DurationMs)
	}
}

// TestSearch_RootSpanResolution asserts that when a trace contains
// multiple spans (one root with ParentSpanId="" and several children
// pointing at the root), the response shaper anchors RootServiceName
// and RootTraceName on the root span — not on whichever child the
// underlying engine happens to return first. This is the Tempo wire
// spec: rootTraceName is the name of the span at the top of the trace
// tree.
//
// Pins the bug behind ~4 Tempo compat cases (status_eq_error /
// set_or_two_kinds / set_and_checkout_and_status_error /
// descendant_op_payments_to_consumer / direct_parent_op_checkout_to_child)
// — see PR description for the before/after wire shape.
func TestSearch_RootSpanResolution(t *testing.T) {
	t.Parallel()
	const traceID = "cccccccccccccccccccccccccccccccc"
	const rootSpanID = "0000000000000001"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// A child span (returned first by CH; this is exactly the
			// scenario that produced rootTraceName="checkout.child.2"
			// in the Tempo compat diff).
			{
				MetricName: "checkout.child.2",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 30_000_000, time.UTC),
				Value:     40_000_000,
			},
			// Another child.
			{
				MetricName: "checkout.child.0",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 10_000_000, time.UTC),
				Value:     20_000_000,
			},
			// The actual root span — ParentSpanId is empty. The shaper
			// must anchor RootServiceName + RootTraceName here.
			{
				MetricName: "GET /api/checkout/17",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": "",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     150_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20status%20%3D%20error%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	got := sr.Traces[0]
	if got.RootTraceName != "GET /api/checkout/17" {
		t.Errorf("RootTraceName: got %q, want %q (the span where ParentSpanId='', not a child)",
			got.RootTraceName, "GET /api/checkout/17")
	}
	if got.RootServiceName != "checkout" {
		t.Errorf("RootServiceName: got %q, want %q",
			got.RootServiceName, "checkout")
	}
}

// TestSearch_RootSpanResolution_MultipleRoots covers the broken-trace
// fallback: when two spans both have ParentSpanId="" (the trace was
// chopped during collection so multiple roots are present), Tempo
// anchors on the earliest by start time. The shaper mirrors that.
func TestSearch_RootSpanResolution_MultipleRoots(t *testing.T) {
	t.Parallel()
	const traceID = "dddddddddddddddddddddddddddddddd"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Later "root" (broken trace).
			{
				MetricName: "later-root",
				Labels: map[string]string{
					"service.name":            "svcB",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": "",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 5, 0, time.UTC),
				Value:     100_000_000,
			},
			// Earlier "root" — should win.
			{
				MetricName: "earliest-root",
				Labels: map[string]string{
					"service.name":            "svcA",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": "",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     200_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	if sr.Traces[0].RootTraceName != "earliest-root" {
		t.Errorf("RootTraceName: got %q, want %q (earliest of multiple roots)",
			sr.Traces[0].RootTraceName, "earliest-root")
	}
	if sr.Traces[0].RootServiceName != "svcA" {
		t.Errorf("RootServiceName: got %q, want %q",
			sr.Traces[0].RootServiceName, "svcA")
	}
}

// TestSearch_RootSpanResolution_TruncatedTrace covers the truncated-set
// fallback: when the matcher only matches child spans AND the
// follow-up root-lookup against otel_traces returns no row for that
// trace (the trace's root was never collected — true truncation), the
// shaper falls back to the earliest-by-timestamp span's metadata so
// the response surfaces *something* identifying the trace rather than
// dropping the row.
//
// The structural-join / status-filter / set-op compat cases hit the
// non-truncated path: their result set lacks a root row but
// otel_traces does carry one, so resolveTraceRoots recovers it (see
// TestSearch_StructuralJoin_RootSurfaced). This test is the
// degradation envelope: when the follow-up also misses, we keep the
// earliest-child anchor rather than silently dropping the RootTraceName
// field.
func TestSearch_RootSpanResolution_TruncatedTrace(t *testing.T) {
	t.Parallel()
	const traceID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const rootSpanID = "0000000000000099"
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "child.late",
				Labels: map[string]string{
					"service.name":            "svcLate",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 5, 0, time.UTC),
				Value:     100_000_000,
			},
			{
				MetricName: "child.early",
				Labels: map[string]string{
					"service.name":            "svcEarly",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     50_000_000,
			},
		},
		// The follow-up root-lookup query carries the
		// `RootSpanName` alias in its outer Project. Stub it with an
		// empty row set so the truncated-trace fallback fires.
		samplesBySQL: map[string][]chclient.Sample{
			"RootSpanName": {},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	// Earliest child wins when no root is in either the result set
	// OR the follow-up root lookup.
	if sr.Traces[0].RootTraceName != "child.early" {
		t.Errorf("RootTraceName: got %q, want %q (earliest child when root absent)",
			sr.Traces[0].RootTraceName, "child.early")
	}
	if sr.Traces[0].RootServiceName != "svcEarly" {
		t.Errorf("RootServiceName: got %q, want %q",
			sr.Traces[0].RootServiceName, "svcEarly")
	}
}

// TestSearch_RootSpanResolution_StrippedZero pins the post-strip root
// classification: the OTel-CH exporter stores ParentSpanId as a 16-char
// lowercase-hex string, and the search projection routes it through
// stripLeadingHexZeros — the regex `^0+([0-9a-f])` always retains ≥ 1
// hex digit, so an all-zero ParentSpanId (the on-disk form for a true
// root span) renders as `"0"` rather than `""`. The shaper must treat
// `"0"` as a root marker; without this, structural-join queries (`>>`,
// `<<`, `>`, `<`) report a child span's name as rootTraceName because
// the search projection's per-trace root row is mis-classified.
//
// Pins the failure-mode behind descendant_op_payments_to_consumer /
// direct_parent_op_checkout_to_child / set_and_checkout_and_status_error
// / status_eq_error in the Tempo compat report — see PR description.
func TestSearch_RootSpanResolution_StrippedZero(t *testing.T) {
	t.Parallel()
	const traceID = "ffffffffffffffffffffffffffffffff"
	const rootSpanID = "1"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Child span — parent points at the root, which after
			// stripLeadingHexZeros renders as "1".
			{
				MetricName: "payments.child.3",
				Labels: map[string]string{
					"service.name":            "payments",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 25_000_000, time.UTC),
				Value:     30_000_000,
			},
			// The actual root span — on-disk ParentSpanId is
			// "0000000000000000", which stripLeadingHexZeros collapses
			// to "0" (single hex digit, never empty). The shaper must
			// accept this as a root marker.
			{
				MetricName: "GET /api/payments/1",
				Labels: map[string]string{
					"service.name":            "payments",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": "0",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     120_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20status%20%3D%20error%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	got := sr.Traces[0]
	if got.RootTraceName != "GET /api/payments/1" {
		t.Errorf("RootTraceName: got %q, want %q (stripped-zero parent must classify as root)",
			got.RootTraceName, "GET /api/payments/1")
	}
	if got.RootServiceName != "payments" {
		t.Errorf("RootServiceName: got %q, want %q",
			got.RootServiceName, "payments")
	}
}

// TestSearch_StructuralJoin_RootSurfaced pins the fix for the four
// remaining tempo-compat regressions (status_eq_error,
// descendant_op_payments_to_consumer, direct_parent_op_checkout_to_child,
// set_and_checkout_and_status_error). Each query's result set carries
// only **child** spans — the structural join projects R-side rows,
// `{ status = error }` matches only the children that report error
// status, set ops like `&&` only return rows satisfying every leg —
// so no row in the original result is a root span (every
// __cerberus_parentSpanID is a non-empty / non-zero hex value pointing
// at the actual root).
//
// The handler must detect the missing-root case, issue a follow-up
// query against otel_traces filtered to
// `ParentSpanId IN (”, '0000000000000000') AND TraceId IN (...)`, and
// patch RootServiceName / RootTraceName on the affected summaries
// before responding. This test stubs both stages: the first query
// returns child rows; the second returns the recovered root for one
// of the two traces and nothing for the other (modelling a true
// truncation), letting us assert both code paths.
func TestSearch_StructuralJoin_RootSurfaced(t *testing.T) {
	t.Parallel()
	const (
		traceWithRoot = "17" // the stripped form (rootSpanID for child rows is the same)
		traceNoRoot   = "abc"
		rootSpanID    = "1"
	)
	q := &stubQuerier{
		// First (search) query returns two child rows on traceWithRoot
		// and one child row on traceNoRoot — neither trace's result
		// set contains its true root span (per structural-join /
		// status-filter semantics).
		samples: []chclient.Sample{
			{
				MetricName: "checkout.child.2",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceWithRoot,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 30_000_000, time.UTC),
				Value:     40_000_000,
			},
			{
				MetricName: "checkout.child.0",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceWithRoot,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 10_000_000, time.UTC),
				Value:     20_000_000,
			},
			{
				MetricName: "payments.child.4",
				Labels: map[string]string{
					"service.name":            "payments",
					"__cerberus_traceID":      traceNoRoot,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 50_000_000, time.UTC),
				Value:     60_000_000,
			},
		},
		// Second (root-lookup) query returns the recovered root for
		// traceWithRoot only — traceNoRoot is truncated. The follow-up
		// emits the canonical Sample envelope so chclient decodes it
		// positionally; the Attributes carry the stripped TraceID and
		// the recovered service.name.
		samplesBySQL: map[string][]chclient.Sample{
			"RootSpanName": {
				{
					MetricName: "GET /api/checkout/17",
					Labels: map[string]string{
						"service.name":       "checkout",
						"__cerberus_traceID": traceWithRoot,
					},
					Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
					Value:     0,
				},
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20status%20%3D%20error%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 2 {
		t.Fatalf("expected 2 traces, got %d (%+v)", len(sr.Traces), sr.Traces)
	}
	// Locate traces by ID — the slice is sorted by TraceID.
	var withRoot, noRoot tempo.TraceSummary
	for _, tr := range sr.Traces {
		switch tr.TraceID {
		case traceWithRoot:
			withRoot = tr
		case traceNoRoot:
			noRoot = tr
		}
	}
	if withRoot.RootTraceName != "GET /api/checkout/17" {
		t.Errorf("withRoot.RootTraceName: got %q, want %q (recovered from follow-up lookup, not the child)",
			withRoot.RootTraceName, "GET /api/checkout/17")
	}
	if withRoot.RootServiceName != "checkout" {
		t.Errorf("withRoot.RootServiceName: got %q, want %q",
			withRoot.RootServiceName, "checkout")
	}
	// Truncated trace: follow-up returned nothing; the earliest-span
	// fallback ("payments.child.4") stays in place so the summary
	// still identifies the trace.
	if noRoot.RootTraceName != "payments.child.4" {
		t.Errorf("noRoot.RootTraceName: got %q, want %q (earliest-span fallback when follow-up returns no root)",
			noRoot.RootTraceName, "payments.child.4")
	}

	// At least two SQL queries should have been issued (search + lookup).
	if len(q.queriedSQLs) < 2 {
		t.Errorf("expected ≥2 CH queries (search + root lookup), got %d: %v",
			len(q.queriedSQLs), q.queriedSQLs)
	}
	// The lookup SQL must filter on (TraceId, ParentSpanId).
	var lookupSQL string
	for _, sql := range q.queriedSQLs {
		if strings.Contains(sql, "RootSpanName") {
			lookupSQL = sql
			break
		}
	}
	if lookupSQL == "" {
		t.Fatalf("no follow-up root-lookup query was issued; queries=%v", q.queriedSQLs)
	}
	if !strings.Contains(lookupSQL, "argMin") {
		t.Errorf("lookup SQL must use argMin to pick the per-trace root span; got %s", lookupSQL)
	}
	if !strings.Contains(lookupSQL, "ParentSpanId") {
		t.Errorf("lookup SQL must filter on ParentSpanId; got %s", lookupSQL)
	}
	if !strings.Contains(lookupSQL, "TraceId") {
		t.Errorf("lookup SQL must filter on TraceId; got %s", lookupSQL)
	}
}

// TestSearch_StructuralJoin_DurationMsRecovered pins the duration
// arm of the root-lookup follow-up: the initial /api/search result
// set carries only matched child spans whose per-row Sample.Value
// captures the *child's* duration (here 20-60ms), but Tempo's wire
// spec reports `durationMs` as the **trace-wide** wall-clock span
// (here 150ms). The follow-up CH query's Aggregate computes
// (max(Timestamp + Duration) - min(Timestamp)) across every span of
// each affected trace and surfaces the result via the canonical
// Sample.Value slot; applyRootMetadata reads it back as
// rootMetadata.TraceDurationNs and rewrites summary.DurationMs.
//
// Mirrors the 4 Tempo-compat cases pre-fix reported durationMs=20
// (per-child) instead of 150 (trace-wide):
// descendant_op_payments_to_consumer, direct_parent_op_checkout_to_child,
// set_and_checkout_and_status_error, status_eq_error.
func TestSearch_StructuralJoin_DurationMsRecovered(t *testing.T) {
	t.Parallel()
	const (
		traceID    = "17"
		rootSpanID = "1"
	)
	q := &stubQuerier{
		// First (search) query returns two child rows for traceID —
		// neither is a root span, and each per-row Sample.Value
		// reports the child's own Duration (20ms / 40ms). Without
		// the follow-up duration patch, toTraceSummaries reports
		// DurationMs = max(40ms, 20ms) = 40ms — but Tempo says 150ms.
		samples: []chclient.Sample{
			{
				MetricName: "checkout.child.2",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 30_000_000, time.UTC),
				Value:     40_000_000,
			},
			{
				MetricName: "checkout.child.0",
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      traceID,
					"__cerberus_parentSpanID": rootSpanID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 10_000_000, time.UTC),
				Value:     20_000_000,
			},
		},
		// Second (root-lookup) query returns the trace-wide
		// envelope: Value carries 150_000_000 ns (= 150 ms), the
		// derived (TraceEndNs - TraceStartNs).
		samplesBySQL: map[string][]chclient.Sample{
			"RootSpanName": {
				{
					MetricName: "POST /api/checkout/17",
					Labels: map[string]string{
						"service.name":       "checkout",
						"__cerberus_traceID": traceID,
					},
					Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
					Value:     150_000_000,
				},
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20status%20%3D%20error%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d (%+v)", len(sr.Traces), sr.Traces)
	}
	if got := sr.Traces[0].DurationMs; got != 150 {
		t.Errorf("DurationMs: got %d, want 150 (trace-wide ns / 1e6, recovered via root-lookup follow-up; child-only result set under-reported pre-fix)", got)
	}
	// Confirm the root metadata also flowed through, so we know the
	// duration patch shares the same lookup row.
	if sr.Traces[0].RootTraceName != "POST /api/checkout/17" {
		t.Errorf("RootTraceName: got %q, want %q",
			sr.Traces[0].RootTraceName, "POST /api/checkout/17")
	}
}

// TestSearch_SQLProjectsParentSpanId pins the SQL emitted by the search
// path against an OTel-CH default schema. The ParentSpanId column must
// appear in the projection so toTraceSummaries can resolve the root
// span — without this column the shaper has no way to identify which
// row is the root.
func TestSearch_SQLProjectsParentSpanId(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	// The wrap-projection's reserved map must include
	// '__cerberus_parentSpanID' → ParentSpanId so the shaper can
	// classify each row's root status. Substring search keeps the
	// pin robust against unrelated SQL whitespace / arg-positional
	// shifts.
	if !strings.Contains(q.lastSQL, "ParentSpanId") {
		t.Errorf("emitted SQL must project ParentSpanId; got %s", q.lastSQL)
	}
	// The reserved key string is parameterised — find it in the args.
	var sawParentSpanIDKey bool
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "__cerberus_parentSpanID" {
			sawParentSpanIDKey = true
			break
		}
	}
	if !sawParentSpanIDKey {
		t.Errorf("emitted SQL args must include reserved __cerberus_parentSpanID slot; got args=%v", q.lastArgs)
	}
}

// TestSearch_SpansetAggregate_PerTrace asserts that
// `{ ... } | count() > 0` returns one summary per matching trace
// with real rootServiceName / rootTraceName fields, NOT a single
// corpus-wide row with empty envelope. Mirrors the
// count_spans_per_trace tempo-compat case.
func TestSearch_SpansetAggregate_PerTrace(t *testing.T) {
	t.Parallel()
	const (
		traceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		traceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		traceC = "cccccccccccccccccccccccccccccccc"
	)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "POST /api/orders",
				Labels:     map[string]string{"service.name": "checkout", "__cerberus_traceID": traceA},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:      3,
			},
			{
				MetricName: "GET /healthz",
				Labels:     map[string]string{"service.name": "frontend", "__cerberus_traceID": traceB},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 5, 0, time.UTC),
				Value:      2,
			},
			{
				MetricName: "db.query",
				Labels:     map[string]string{"service.name": "db", "__cerberus_traceID": traceC},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 10, 0, time.UTC),
				Value:      1,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D~%20%22.%2B%22%20%7D%20%7C%20count%28%29%20%3E%200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 3 {
		t.Fatalf("expected 3 traces (one per matching TraceID), got %d", len(sr.Traces))
	}
	if sr.Traces[0].TraceID != traceA || sr.Traces[0].RootServiceName != "checkout" || sr.Traces[0].RootTraceName != "POST /api/orders" {
		t.Errorf("trace A summary mismatch: %+v", sr.Traces[0])
	}
	if sr.Traces[1].TraceID != traceB || sr.Traces[1].RootServiceName != "frontend" || sr.Traces[1].RootTraceName != "GET /healthz" {
		t.Errorf("trace B summary mismatch: %+v", sr.Traces[1])
	}
	if sr.Traces[2].TraceID != traceC || sr.Traces[2].RootServiceName != "db" || sr.Traces[2].RootTraceName != "db.query" {
		t.Errorf("trace C summary mismatch: %+v", sr.Traces[2])
	}
}

// TestSearch_SpansetAggregate_DurationMsReflectsWholeTrace pins the
// Tempo wire spec: /api/search's `durationMs` is the **whole-trace**
// wall-clock span (`max(span.end) - min(span.start)` across every
// span in the trace), not the matched span's per-row Duration. The
// spanset-aggregate wrap-projection threads the derived value via
// the reserved `__cerberus_traceDurationNs` label slot so
// toTraceSummaries reports it verbatim — matching Tempo for the
// count_spans_per_trace + avg_duration_per_trace_status_ok compat
// cases (each ~100 rows of per-trace samples).
//
// Pre-fix shape: durationMs = max(per-row Duration) which on a
// multi-span trace under-reports vs Tempo's actual root-span span;
// for `| count()` the per-row Value is the count integer and the
// shaper effectively reported 0ms.
func TestSearch_SpansetAggregate_DurationMsReflectsWholeTrace(t *testing.T) {
	t.Parallel()
	const traceID = "1131bf7acf51ccb10aef0ec7e31bf71f"
	// One row per trace (the spanset-aggregate inner SELECT collapses
	// all spans of a trace into one row), carrying the derived
	// __cerberus_traceDurationNs = 150_000_000 ns (= 150 ms).
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "POST /api/orders",
				Labels: map[string]string{
					"service.name":               "checkout",
					"__cerberus_traceID":         traceID,
					"__cerberus_traceDurationNs": "150000000",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				// Value carries the aggregate (count=3) — must NOT bleed
				// into durationMs once the reserved-key slot is present.
				Value: 3,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D%20%7C%20count%28%29%20%3E%200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	if got := sr.Traces[0].DurationMs; got != 150 {
		t.Errorf("DurationMs: got %d, want 150 (trace-wide ns / 1e6, not per-row Sample.Value=3)", got)
	}
	if got := sr.Traces[0].TraceID; got != traceID {
		t.Errorf("TraceID: got %q, want %q", got, traceID)
	}
}

// TestSearch_SpansetAggregate_DurationMsMultiSpanReplaysAggregate
// stress-tests the multi-row shape: when the stub returns multiple
// rows for the same trace (spanset-aggregate today emits one row
// per trace but the shaper must be resilient if a future projection
// duplicates rows), the trace-duration reserved slot overrides the
// per-row Sample.Value fallback regardless of arrival order.
func TestSearch_SpansetAggregate_DurationMsMultiSpanReplaysAggregate(t *testing.T) {
	t.Parallel()
	const traceID = "118b9e55fa97da56152b463462b61607"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// First row — no reserved slot (older fixture / partial
			// projection). The shaper picks up Sample.Value (50 ns).
			{
				MetricName: "GET /a",
				Labels: map[string]string{
					"service.name":       "checkout",
					"__cerberus_traceID": traceID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     50_000_000,
			},
			// Second row — carries the derived trace-wide duration.
			// Must overwrite the Sample.Value-based pick from row 1.
			{
				MetricName: "GET /a",
				Labels: map[string]string{
					"service.name":               "checkout",
					"__cerberus_traceID":         traceID,
					"__cerberus_traceDurationNs": "150000000",
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 1, 0, time.UTC),
				Value:     2,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	if got := sr.Traces[0].DurationMs; got != 150 {
		t.Errorf("DurationMs: got %d, want 150 (reserved-slot wins over Sample.Value fallback)", got)
	}
}

// TestSearch_SpansetAggregate_SQLProjectsTraceDurationNs pins the
// SQL emitted by the spanset-aggregate path: the inner Aggregate
// must surface `TraceStartNs` + `TraceEndNs` aliases and the outer
// wrap-projection must merge their difference into Attributes via
// the `__cerberus_traceDurationNs` reserved key. Without this
// substring pin a regression in either the aggregate lowering or
// the wrap projection would silently drop the trace-wide duration.
func TestSearch_SpansetAggregate_SQLProjectsTraceDurationNs(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// `{ resource.service.name = "frontend" } | count() > 0`
	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D%20%7C%20count%28%29%20%3E%200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Inner aggregate must surface the two new aliases.
	if !strings.Contains(q.lastSQL, "TraceStartNs") {
		t.Errorf("emitted SQL must project TraceStartNs aggregate; got %s", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "TraceEndNs") {
		t.Errorf("emitted SQL must project TraceEndNs aggregate; got %s", q.lastSQL)
	}
	// Outer wrap must thread the derived duration through the
	// reserved-key slot.
	var sawDurationKey bool
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "__cerberus_traceDurationNs" {
			sawDurationKey = true
			break
		}
	}
	if !sawDurationKey {
		t.Errorf("emitted SQL args must include reserved __cerberus_traceDurationNs slot; got args=%v", q.lastArgs)
	}
}

// isHexLower reports whether s is a non-empty lowercase hex string.
// The OTel CH exporter writes TraceId via hex.EncodeToString, which
// produces lowercase a-f digits.
func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func TestTraceByID_NotFound(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/abc123")
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
	if !er.Error || er.TraceID != "abc123" {
		t.Fatalf("unexpected error body: %+v", er)
	}
}

func TestTraceByID_Found(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels:     map[string]string{"service.name": "frontend"},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:      150_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var tr tempo.TraceByIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tr.Batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(tr.Batches))
	}
	if len(tr.Batches[0].Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tr.Batches[0].Spans))
	}
}

// TestResponseHeaders_EngineInstrumentation covers the Tempo head's
// response-header contract: /api/search returns Strategy=native;
// /api/traces/{id} returns Strategy=trace-by-id (engine.Meta.IsTraceByID
// short-circuits the optimizer and tags the response).
func TestResponseHeaders_EngineInstrumentation(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels:     map[string]string{"service.name": "frontend"},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:      150_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	t.Run("search_native", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Cerberus-Strategy"); got != "native" {
			t.Errorf("X-Cerberus-Strategy: got %q, want native", got)
		}
		if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
			t.Errorf("X-Cerberus-Plan-Nodes: missing")
		}
		if got := resp.Header.Get("X-Cerberus-CH-Millis"); got == "" {
			t.Errorf("X-Cerberus-CH-Millis: missing")
		}
	})

	t.Run("traceByID_short_circuit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/traces/abc123")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Cerberus-Strategy"); got != "trace-by-id" {
			t.Errorf("X-Cerberus-Strategy: got %q, want trace-by-id", got)
		}
		if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
			t.Errorf("X-Cerberus-Plan-Nodes: missing")
		}
		if got := resp.Header.Get("X-Cerberus-CH-Millis"); got == "" {
			t.Errorf("X-Cerberus-CH-Millis: missing")
		}
	})
}
