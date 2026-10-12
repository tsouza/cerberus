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
	"reflect"
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

// Timestamp envelopes must include roots older than the search window and the
// final fractional-second span. An incomplete cohort must use full retention.
func TestResolveMissingRoots_TimestampEnvelope_ChDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lookup       string
		createLookup bool
	}{
		{name: "complete", createLookup: true, lookup: `('a0000000000000000000000000000001', '2026-04-01 10:00:00', '2026-04-01 10:00:00'),
   ('a0000000000000000000000000000001', '2026-05-01 10:00:00', '2026-05-01 10:00:01'),
   ('b0000000000000000000000000000002', '2026-03-01 10:00:00', '2026-03-01 10:00:00')`},
		{name: "partial", createLookup: true, lookup: `('a0000000000000000000000000000001', '2026-04-01 10:00:00', '2026-05-01 10:00:01')`},
		{name: "empty", createLookup: true},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chclienttest.NewChDB(t)
			c.Seed(t, `CREATE TABLE otel_traces (
   TraceId String, SpanId String, ParentSpanId String, SpanName String,
   Duration Int64, Timestamp DateTime64(9), ResourceAttributes Map(String,String)
  ) ENGINE=MergeTree() PARTITION BY toDate(Timestamp) ORDER BY Timestamp;`)
			c.Seed(t, `INSERT INTO otel_traces VALUES
   ('a0000000000000000000000000000001','1000000000000001','','old root',1000000000,toDateTime64('2026-04-01 10:00:00',9),map('service.name','frontend')),
   ('a0000000000000000000000000000001','1000000000000002','1000000000000001','child',3000000000,toDateTime64('2026-05-01 10:00:01.999999999',9),map('service.name','worker')),
   ('b0000000000000000000000000000002','2000000000000001','','older root',1000000000,toDateTime64('2026-03-01 10:00:00',9),map('service.name','backend'));`)
			if tc.createLookup {
				c.Seed(t, `CREATE TABLE otel_traces_trace_id_ts (TraceId String,Start DateTime,End DateTime) ENGINE=MergeTree() ORDER BY (TraceId,Start);`)
				if tc.lookup != "" {
					c.Seed(t, "INSERT INTO otel_traces_trace_id_ts VALUES "+tc.lookup)
				}
			}
			ids := []string{"a0000000000000000000000000000001", "b0000000000000000000000000000002", "a0000000000000000000000000000001"}
			run := func(enabled bool) []tempo.TraceSummary {
				s := schema.DefaultOTelTraces()
				s.TraceIDTsEnabled = enabled
				h := tempo.New(c, s, "test", nil)
				summaries := []tempo.TraceSummary{{TraceID: ids[0]}, {TraceID: ids[1]}}
				if err := h.ResolveMissingRoots(context.Background(), summaries, ids); err != nil {
					t.Fatal(err)
				}
				return summaries
			}
			want, got := run(false), run(true)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("bounded summaries differ: got %+v, want %+v", got, want)
			}
			if got[0].RootTraceName != "old root" || got[1].RootTraceName != "older root" {
				t.Fatalf("old roots missing: %+v", got)
			}
			if got[0].DurationMs <= 0 || got[1].DurationMs != 1000 {
				t.Fatalf("duration missing: %+v", got)
			}
		})
	}
}
