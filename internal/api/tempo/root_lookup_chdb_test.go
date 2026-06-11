//go:build chdb

// chDB-backed regression pin for the /api/search root-span lookup's
// parse depth. The pre-fix lookup rendered the trace-ID filter as a
// nested OR-chain — one ClickHouse AST level per trace ID — and blew
// `max_parser_depth` (default 1000) with code 306 ("Maximum parse
// depth (1000) exceeded. Consider rising max_parser_depth") once a
// search returned >1000 traces needing root enrichment (compose-smoke
// run 27307036248, missing=1006). The failure WARN-degraded: search
// responses silently lost their root decoration. The fix renders a
// single flat `TraceId IN (...)` (chplan.InList), whose parse depth is
// constant in the ID count; this test executes the real lookup against
// an embedded ClickHouse at N=1500 — comfortably past the 1000-depth
// cliff — and asserts the lookup both parses and recovers the seeded
// trace's root metadata.
package tempo_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestResolveMissingRoots_ParseDepthFlatAt1500IDs_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    Duration Int64,
    Timestamp DateTime64(9),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`)
	// One real trace among the 1500 looked-up IDs: a root span (empty
	// ParentSpanId) plus a child, 1.5s wall-clock end to end. The other
	// 1499 IDs have no rows — the lookup legitimately returns nothing
	// for them and the caller's fallback stays in place.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'GET /home', 1000000000, toDateTime64('2026-05-01 10:00:00.000000000', 9), map('service.name', 'frontend')),
    ('a0000000000000000000000000000001', '1000000000000002', '1000000000000001', 'auth', 500000000, toDateTime64('2026-05-01 10:00:01.000000000', 9), map('service.name', 'auth'));`)

	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)

	const seededTraceID = "a0000000000000000000000000000001"
	missing := make([]string, 0, 1500)
	missing = append(missing, seededTraceID)
	for i := 1; i < 1500; i++ {
		missing = append(missing, fmt.Sprintf("b%031x", i))
	}
	summaries := []tempo.TraceSummary{{
		TraceID:         seededTraceID,
		RootServiceName: "fallback-svc",
		RootTraceName:   "fallback-name",
		DurationMs:      1,
	}}

	// Pre-fix this returned the CH parse error (code 306) once
	// len(missing) pushed the OR-chain past max_parser_depth.
	if err := h.ResolveMissingRoots(context.Background(), summaries, missing); err != nil {
		t.Fatalf("ResolveMissingRoots with %d IDs: %v", len(missing), err)
	}

	if summaries[0].RootTraceName != "GET /home" || summaries[0].RootServiceName != "frontend" {
		t.Errorf("root metadata = (%q, %q), want (frontend root recovered): (\"GET /home\", \"frontend\")",
			summaries[0].RootTraceName, summaries[0].RootServiceName)
	}
	// Trace-wide wall clock: child starts 1s after the root and runs
	// 0.5s — max(ts+dur) - min(ts) = 1.5s.
	if summaries[0].DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500 (trace-wide wall-clock span)", summaries[0].DurationMs)
	}
}
