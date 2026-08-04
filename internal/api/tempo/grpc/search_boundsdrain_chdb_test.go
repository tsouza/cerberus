//go:build chdb

// chDB-backed bounds-drain coverage for the Tempo gRPC Search RPC — the
// streaming-transport row #1515 flagged missing. The shared
// chclienttest.RunBoundsDrain harness (internal/chclienttest/boundsdrain.go)
// was wired into exactly two entrypoints before this file: PromQL
// query_range (internal/api/prom) and the Tempo HTTP /api/search
// (search_trace_limit_chdb_test.go in package tempo_test). This file adds
// the gRPC row using the SAME trace-limit-pushdown seed shape the HTTP row
// uses, driven through the real streaming RPC against a real chDB session
// instead of the bufconn + stubCursorQuerier fake search_test.go uses for
// its wire-format tests.
//
// Search reports its drain via the SAME tempo.HeaderInspectedSpans trailer
// key the HTTP handler stamps as a response header (see search.go's
// stream.SetTrailer call) — the wire mechanism differs (gRPC trailer vs HTTP
// header) but the drain accounting is byte-identical between the two
// transports, since both route through tempo.Handler.SearchResult.
package grpc_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// grpcBoundsTracesDDL mirrors package tempo_test's tracesDDL (unexported
// there, so redeclared here) — the OTel-CH traces shape the plain-search
// trace-limit pushdown reads.
const grpcBoundsTracesDDL = `CREATE TABLE otel_traces (
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
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// Bounds-drain seed scale for the Tempo gRPC Search RPC. Mirrors
// search_trace_limit_chdb_test.go's manyTracesSeed shape: each trace carries
// grpcBoundsSpansPerTrace spans (one root + two children), traces spread
// across strictly increasing minutes so the newest-by-min-start ranking
// never needs a tie-break, and grpcBoundsSearchLimit < grpcBoundsTraceCount
// so the pushdown must genuinely reduce the drain below the full seed.
const (
	grpcBoundsTraceCount    = 60
	grpcBoundsSpansPerTrace = 3
	grpcBoundsSearchLimit   = 5
)

// grpcBoundsTracesSeed builds grpcBoundsTraceCount traces (root + two
// children each), trace i's root starting at base + i minutes so start
// times strictly increase with i.
func grpcBoundsTracesSeed(base time.Time) (seed string, fullSeed int64) {
	const tsFmt = "2006-01-02 15:04:05.000000000"
	rows := make([]string, 0, grpcBoundsTraceCount*grpcBoundsSpansPerTrace)
	for i := 1; i <= grpcBoundsTraceCount; i++ {
		traceID := fmt.Sprintf("b%031x", i)
		rootTS := base.Add(time.Duration(i) * time.Minute)
		c1TS := rootTS.Add(1 * time.Nanosecond)
		c2TS := rootTS.Add(2 * time.Nanosecond)
		root := fmt.Sprintf("%016x", i*10+1)
		child1 := fmt.Sprintf("%016x", i*10+2)
		child2 := fmt.Sprintf("%016x", i*10+3)
		rows = append(
			rows,
			fmt.Sprintf("('%s', '%s', '', 'GET /root', 'Server', 1000, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'))",
				traceID, root, rootTS.Format(tsFmt)),
			fmt.Sprintf("('%s', '%s', '%s', 'child-a', 'Internal', 500, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'svc-a'))",
				traceID, child1, root, c1TS.Format(tsFmt)),
			fmt.Sprintf("('%s', '%s', '%s', 'child-b', 'Client', 300, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'svc-b'))",
				traceID, child2, root, c2TS.Format(tsFmt)),
		)
	}
	seed = "INSERT INTO otel_traces VALUES\n    " + joinRows(rows) + ";"
	return seed, int64(len(rows))
}

func joinRows(rows []string) string {
	out := rows[0]
	for _, r := range rows[1:] {
		out += ",\n    " + r
	}
	return out
}

// dialChDBSearchServer wires a real chDB-backed tempo.Handler into a gRPC
// Service over a bufconn listener — the chDB-driven sibling of
// search_test.go's dialServer (which takes the in-memory stubCursorQuerier
// fake instead).
func dialChDBSearchServer(t *testing.T, c *chclienttest.Client) (tempopb.StreamingQuerierClient, func()) {
	t.Helper()
	handler := tempo.New(c, schema.DefaultOTelTraces(), "test", nil)
	svc := tempogrpc.NewService(handler, nil, nil)
	srv := tempogrpc.NewServer(svc)
	lis := bufconn.Listen(1 << 20)
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc Serve returned: %v", err)
		}
	}()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	return tempopb.NewStreamingQuerierClient(conn), func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
}

// grpcSearchBoundsDrainCase builds the Tempo gRPC Search bounds-drain row.
// It seeds many traces via a real chDB session, drives a plain `{}` search
// with a small Limit over the streaming RPC, and reads the drain off the
// tempo.HeaderInspectedSpans trailer — the gRPC counterpart of the HTTP
// row's X-Cerberus-Inspected-Spans response header.
func grpcSearchBoundsDrainCase() chclienttest.BoundsDrainCase {
	return chclienttest.BoundsDrainCase{
		Name:        "tempo/grpc_search/trace_limit_pushdown",
		OutputBound: int64(grpcBoundsSearchLimit * grpcBoundsSpansPerTrace),
		Run: func(t *testing.T) (drain, fullSeed int64) {
			base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			seed, full := grpcBoundsTracesSeed(base)

			c := chclienttest.NewChDB(t)
			c.Seed(t, grpcBoundsTracesDDL)
			c.Seed(t, seed)

			client, cleanup := dialChDBSearchServer(t, c)
			t.Cleanup(cleanup)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)

			start := base
			end := base.Add(time.Duration(grpcBoundsTraceCount+1) * time.Minute)
			stream, err := client.Search(ctx, &tempopb.SearchRequest{
				Query: "{}",
				Limit: grpcBoundsSearchLimit,
				//nolint:gosec // seed window is bounded well under 2^32 seconds.
				Start: uint32(start.Unix()),
				//nolint:gosec // seed window is bounded well under 2^32 seconds.
				End: uint32(end.Unix()),
			})
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}

			var traceCount int
			for {
				f, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("recv: %v", err)
				}
				traceCount += len(f.Traces)
			}
			if traceCount != grpcBoundsSearchLimit {
				t.Fatalf("got %d traces, want %d (search limit)", traceCount, grpcBoundsSearchLimit)
			}

			trailer := stream.Trailer()
			vals := trailer.Get(tempo.HeaderInspectedSpans)
			if len(vals) != 1 {
				t.Fatalf("trailer %s: got %v, want exactly one value", tempo.HeaderInspectedSpans, vals)
			}
			spans, err := strconv.Atoi(vals[0])
			if err != nil {
				t.Fatalf("trailer %s = %q, want an integer: %v", tempo.HeaderInspectedSpans, vals[0], err)
			}

			return int64(spans), full
		},
	}
}

// TestBoundsDrain_TempoGRPCSearch_ChDB is the Tempo gRPC row for the shared
// bounds-drain gate: the same trace-limit pushdown
// TestBoundsDrain_TempoSearch_ChDB (package tempo_test) proves over HTTP,
// driven here over the streaming gRPC transport Grafana's Tempo datasource
// actually uses. Falsifiability: reverting stampSearchTraceLimit's wrap (so
// the plan stays a bare Scan/Filter) makes InspectedSpans jump to the full
// seed on this transport exactly as it does on HTTP, failing the bound
// assertion.
func TestBoundsDrain_TempoGRPCSearch_ChDB(t *testing.T) {
	chclienttest.RunBoundsDrain(t, []chclienttest.BoundsDrainCase{
		grpcSearchBoundsDrainCase(),
	})
}
