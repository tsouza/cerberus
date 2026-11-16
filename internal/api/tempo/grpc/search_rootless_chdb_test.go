//go:build chdb

// chDB-backed pin for the streaming Search RPC over a trace whose root span
// was never ingested: the gRPC counterpart of api/tempo's
// TestSearch_RootlessTrace_ChDB.
package grpc_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/thirdparty/tempopb"
)

// TestSearch_RootlessTrace_GRPC pins that the Search RPC reports a trace
// with no root span as reference Tempo does — no rootTraceName and the
// "<root span not yet received>" rootServiceName — while a rooted trace
// keeps its root.
func TestSearch_RootlessTrace_GRPC(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_traces (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(String, String),
    SpanAttributes Map(String, String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String
) ENGINE = MergeTree ORDER BY (Timestamp, TraceId);
INSERT INTO otel_traces VALUES
    (toDateTime64('2026-05-13 12:00:00', 9), 'd0000000000000000000000000000001', '4000000000000011', '',                 'GET /a',       'Server', 'frontend', map('service.name', 'frontend'), map(), 100000000, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:00.010', 9), 'd0000000000000000000000000000001', '4000000000000012', '4000000000000011', 'db',       'Client', 'backend',  map('service.name', 'backend'),  map(), 20000000,  'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:01', 9), 'd0000000000000000000000000000002', '4000000000000021', '4000000000000099', 'orphan.entry', 'Server', 'orphan',   map('service.name', 'orphan'),   map(), 200000000, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:01.050', 9), 'd0000000000000000000000000000002', '4000000000000022', '4000000000000021', 'orphan.child', 'Client', 'orphan', map('service.name', 'orphan'), map(), 50000000, 'Unset', '', '', '');`)

	client, cleanup := dialChDBSearchServer(t, c)
	t.Cleanup(cleanup)

	start := time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 13, 0, 0, 0, time.UTC)
	for _, q := range []string{`{ }`, `{ } | by(resource.service.name)`} {
		t.Run(q, func(t *testing.T) {
			stream, err := client.Search(context.Background(), &tempopb.SearchRequest{
				Query: q,
				Limit: 20,
				//nolint:gosec // the fixture window is bounded well under 2^32 seconds.
				Start: uint32(start.Unix()),
				//nolint:gosec // the fixture window is bounded well under 2^32 seconds.
				End: uint32(end.Unix()),
			})
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			byID := map[string]*tempopb.TraceSearchMetadata{}
			for {
				f, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("recv: %v", err)
				}
				for _, tr := range f.Traces {
					byID[tr.TraceID] = tr
				}
			}
			rootless, rooted := byID["d0000000000000000000000000000002"], byID["d0000000000000000000000000000001"]
			if rootless == nil || rooted == nil {
				t.Fatalf("want both seeded traces, got %v", byID)
			}
			if rootless.RootTraceName != "" || rootless.RootServiceName != "<root span not yet received>" {
				t.Errorf("rootless trace = {rootTraceName %q, rootServiceName %q}, want {\"\", \"<root span not yet received>\"}",
					rootless.RootTraceName, rootless.RootServiceName)
			}
			if rooted.RootTraceName != "GET /a" || rooted.RootServiceName != "frontend" {
				t.Errorf("rooted trace = {rootTraceName %q, rootServiceName %q}, want {\"GET /a\", \"frontend\"}",
					rooted.RootTraceName, rooted.RootServiceName)
			}
		})
	}
}
