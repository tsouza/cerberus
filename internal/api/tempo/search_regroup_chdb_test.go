//go:build chdb

// chDB-backed pins for /api/search over a regrouping pipeline stage —
// `by(...)` and `coalesce()` — through the full pipeline (parse → lower →
// wrap → optimize → emit → ClickHouse → response shaping).
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// regroupSeed: three traces with the same shape and service mix, so a
// grouping that ignored the trace would merge them. In each, the root
// (frontend) starts first and a backend child ends 50ms after the root does,
// so the trace's extent (root start to child end) is wider than any single
// group's when the spans are grouped by service.
//
//	trace ...1  root 'GET /a'  0ms +100ms   child backend 10ms +140ms   child frontend 20ms +10ms
//	trace ...2  root 'GET /b'  same offsets, 1s later
//	trace ...3  root 'GET /c'  same offsets, 2s later
const regroupSeed = `INSERT INTO otel_traces VALUES
    ('c0000000000000000000000000000001', '3000000000000011', '',                 'GET /a', 'Server',   100000000, toDateTime64('2026-05-01 11:00:00.000', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('c0000000000000000000000000000001', '3000000000000012', '3000000000000011', 'db',     'Client',   140000000, toDateTime64('2026-05-01 11:00:00.010', 9), 'Unset', '', '', '', map(), map('service.name', 'backend'), 'backend'),
    ('c0000000000000000000000000000001', '3000000000000013', '3000000000000011', 'render', 'Internal', 10000000,  toDateTime64('2026-05-01 11:00:00.020', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('c0000000000000000000000000000002', '3000000000000021', '',                 'GET /b', 'Server',   100000000, toDateTime64('2026-05-01 11:00:01.000', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('c0000000000000000000000000000002', '3000000000000022', '3000000000000021', 'db',     'Client',   140000000, toDateTime64('2026-05-01 11:00:01.010', 9), 'Unset', '', '', '', map(), map('service.name', 'backend'), 'backend'),
    ('c0000000000000000000000000000002', '3000000000000023', '3000000000000021', 'render', 'Internal', 10000000,  toDateTime64('2026-05-01 11:00:01.020', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('c0000000000000000000000000000003', '3000000000000031', '',                 'GET /c', 'Server',   100000000, toDateTime64('2026-05-01 11:00:02.000', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('c0000000000000000000000000000003', '3000000000000032', '3000000000000031', 'db',     'Client',   140000000, toDateTime64('2026-05-01 11:00:02.010', 9), 'Unset', '', '', '', map(), map('service.name', 'backend'), 'backend'),
    ('c0000000000000000000000000000003', '3000000000000033', '3000000000000031', 'render', 'Internal', 10000000,  toDateTime64('2026-05-01 11:00:02.020', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend');`

// regroupDDL is tracesDDL plus the ServiceName column the rootServiceName
// intrinsic reads.
const regroupDDL = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String),
    ServiceName LowCardinality(String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// regroupTraceExtentMs is each seeded trace's extent: root start to the
// backend child's end (10ms + 140ms).
const regroupTraceExtentMs = 150

func newRegroupChDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, regroupDDL)
	c.Seed(t, regroupSeed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runRegroupSearch issues /api/search for query with the given trace limit
// and returns the summaries, newest first.
func runRegroupSearch(t *testing.T, srv *httptest.Server, query string, limit int) []tempo.TraceSummary {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(query) + "&start=1777593600&end=1777680000&limit=" + strconv.Itoa(limit))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	return sr.Traces
}

// wantRegroupTraces asserts the summaries are exactly the seeded traces
// named by rootNames (newest first), each with its own root and extent.
func wantRegroupTraces(t *testing.T, got []tempo.TraceSummary, rootNames ...string) {
	t.Helper()
	if len(got) != len(rootNames) {
		t.Fatalf("got %d traces, want %d: %+v", len(got), len(rootNames), got)
	}
	for i, want := range rootNames {
		tr := got[i]
		if tr.RootTraceName != want || tr.RootServiceName != "frontend" || tr.DurationMs != regroupTraceExtentMs {
			t.Errorf("trace %d = {rootTraceName %q, rootServiceName %q, durationMs %d}, want {%q, \"frontend\", %d}",
				i, tr.RootTraceName, tr.RootServiceName, tr.DurationMs, want, regroupTraceExtentMs)
		}
	}
}

// TestSearch_GroupBy_PerTrace_ChDB pins that by() groups each trace's spans
// on their own: three traces sharing every service value come back as three
// summaries, each reporting its own root and its whole extent.
func TestSearch_GroupBy_PerTrace_ChDB(t *testing.T) {
	srv := newRegroupChDBServer(t)
	wantRegroupTraces(t, runRegroupSearch(t, srv, `{ } | by(resource.service.name)`, 20), "GET /c", "GET /b", "GET /a")
}

// TestSearch_Regroup_LimitCountsTraces_ChDB pins that the search limit
// bounds traces, not groups: each trace yields two service groups (or three
// spans under coalesce()), and limit=2 still answers the two newest traces.
func TestSearch_Regroup_LimitCountsTraces_ChDB(t *testing.T) {
	srv := newRegroupChDBServer(t)
	for _, q := range []string{`{ } | by(resource.service.name)`, `{ } | coalesce()`} {
		t.Run(q, func(t *testing.T) {
			wantRegroupTraces(t, runRegroupSearch(t, srv, q, 2), "GET /c", "GET /b")
		})
	}
}

// TestSearch_Regroup_UnsetTraceFields_ChDB pins the trace-level fields
// reference Tempo leaves unset for a regrouping search that reads their
// intrinsic for every span (ast.RootExpr.RegroupLeavesTraceFieldUnset), and
// that the others keep their values.
func TestSearch_Regroup_UnsetTraceFields_ChDB(t *testing.T) {
	srv := newRegroupChDBServer(t)
	cases := []struct {
		query               string
		rootTraceNameSet    bool
		durationSet         bool
		wantRootServiceName string
	}{
		{`{ } | by(rootName)`, false, true, "frontend"},
		{`{ } | by(traceDuration * 2)`, true, false, "frontend"},
		{`{ rootServiceName = "frontend" } | coalesce()`, true, true, "<root span not yet received>"},
		{`{ rootName =~ "GET.*" } | by(name)`, true, true, "frontend"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := runRegroupSearch(t, srv, tc.query, 20)
			if len(got) != 3 {
				t.Fatalf("got %d traces, want 3: %+v", len(got), got)
			}
			for _, tr := range got {
				if (tr.RootTraceName != "") != tc.rootTraceNameSet {
					t.Errorf("%s: rootTraceName = %q, want set=%v", tr.TraceID, tr.RootTraceName, tc.rootTraceNameSet)
				}
				if tr.RootServiceName != tc.wantRootServiceName {
					t.Errorf("%s: rootServiceName = %q, want %q", tr.TraceID, tr.RootServiceName, tc.wantRootServiceName)
				}
				wantDur := 0
				if tc.durationSet {
					wantDur = regroupTraceExtentMs
				}
				if tr.DurationMs != wantDur {
					t.Errorf("%s: durationMs = %d, want %d", tr.TraceID, tr.DurationMs, wantDur)
				}
			}
		})
	}
}
