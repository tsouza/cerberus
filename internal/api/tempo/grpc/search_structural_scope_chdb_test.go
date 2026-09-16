//go:build chdb

// chDB-backed gRPC-transport counterparts of two TraceQL semantic detectors
// that already exist over HTTP (test/property/traceql_test.go's
// TestTraceQLBranchingTreeStructuralRelations and the
// traceql.selector.scope-collision-conjunction shape exercised by
// TestTraceQL_PropertyShapeRoster). Both prove real production
// transformations in internal/traceql/lower.go are reached and produce a
// wrong answer when mutated — see test/semantic/mutants/
// MUTANT-TRACEQL-DESCENDANT-AS-CHILD.json and
// MUTANT-TRACEQL-SCOPE-SWAP.json (issue #3451).
//
// This file exists specifically so those two mutants carry INDEPENDENT
// evidence on the gRPC transport, not just HTTP: dialChDBSearchServer (this
// package's own search_boundsdrain_chdb_test.go) wires a real *grpc.Server
// in front of the same tempo.Handler the HTTP tests exercise, over a
// bufconn listener — no Docker, no external Tempo reference, just cerberus's
// own real streaming Search RPC against a real chDB session. The two
// transports read wire-distinct response shapes (JSON SearchResponse vs
// tempopb's binary TraceSearchMetadata frames), so a bug that corrupted only
// one transport's own reshaping logic could not hide behind the other's
// result — that is what "independently" means here, not that the two
// detectors are expected to ever disagree on a real cerberus build.
package grpc_test

import (
	"context"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/chclienttest"
)

// grpcSearchTraceQL drives one TraceQL query over the streaming Search RPC
// and returns the matched TraceIDs, one entry per matched SPAN (a trace
// whose SpanSet reports Matched=2 contributes its TraceID twice) — the
// gRPC-wire equivalent of the HTTP side's traceIdentityRows (see
// traceql_test.go's doc), and deliberately NOT a bare count: two mutations
// can agree on cardinality while matching the WRONG trace (see
// TestTraceQLScopeCollisionConjunction_GRPC below), which only an identity
// comparison — not a count — can catch. Sorted so callers can assert an
// exact multiset with reflect.DeepEqual / slices.Equal without depending on
// wire order. A trace with no SpanSet at all (the aggregate-pipeline shape,
// not used by either case in this file) contributes nothing.
func grpcSearchTraceQL(ctx context.Context, t *testing.T, client tempopb.StreamingQuerierClient, query string, start, end time.Time) []string {
	t.Helper()
	stream, err := client.Search(ctx, &tempopb.SearchRequest{
		Query: query,
		Limit: 100,
		//nolint:gosec // fixture windows in this file are bounded well under 2^32 seconds.
		Start: uint32(start.Unix()),
		//nolint:gosec // fixture windows in this file are bounded well under 2^32 seconds.
		End: uint32(end.Unix()),
	})
	if err != nil {
		t.Fatalf("open stream for query %q: %v", query, err)
	}

	var traceIDs []string
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv for query %q: %v", query, err)
		}
		for _, tr := range f.Traces {
			for _, ss := range tr.SpanSets {
				for i := uint32(0); i < ss.Matched; i++ {
					traceIDs = append(traceIDs, tr.TraceID)
				}
			}
		}
	}
	sort.Strings(traceIDs)
	return traceIDs
}

// grpcMatchedCount is grpcSearchTraceQL's row count — used by cases in this
// file where cardinality alone is the discriminator (the branching-tree
// structural relations below never place two candidate traces at the same
// count, so a count check is already a real assertion there).
func grpcMatchedCount(ctx context.Context, t *testing.T, client tempopb.StreamingQuerierClient, query string, start, end time.Time) int {
	t.Helper()
	return len(grpcSearchTraceQL(ctx, t, client, query, start, end))
}

// TestTraceQLBranchingTreeStructuralRelations_GRPC is the gRPC-transport
// sibling of traceql_test.go's TestTraceQLBranchingTreeStructuralRelations,
// against the identical branching-tree fixture (root -> childA -> grandchild,
// root -> childB) gen/traceql.go's drawTraceQLBranchingTree draws for the
// property lane. Proves the SAME depth-two discriminator — direct child (`>`)
// must not match a grandchild, descendant (`>>`) must — over cerberus's real
// streaming Search RPC, independent of the HTTP-transport evidence.
func TestTraceQLBranchingTreeStructuralRelations_GRPC(t *testing.T) {
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
    (toDateTime64('2026-05-13 12:00:00', 9), '0102030405060708090a0b0c0d0e0f10', '0102030405060708', '', 'root', 'Internal', 'root-svc', map('service.name', 'root-svc'), map(), 1, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:01', 9), '0102030405060708090a0b0c0d0e0f10', '1112131415161718', '0102030405060708', 'childA', 'Internal', 'childa-svc', map('service.name', 'childa-svc'), map(), 1, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:02', 9), '0102030405060708090a0b0c0d0e0f10', '2122232425262728', '0102030405060708', 'childB', 'Internal', 'childb-svc', map('service.name', 'childb-svc'), map(), 1, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:03', 9), '0102030405060708090a0b0c0d0e0f10', '3132333435363738', '1112131415161718', 'grandchild', 'Internal', 'grandchild-svc', map('service.name', 'grandchild-svc'), map(), 1, 'Unset', '', '', '');`)

	client, cleanup := dialChDBSearchServer(t, c)
	t.Cleanup(cleanup)

	start := time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 13, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		query       string
		wantMatched int
	}{
		{"root_direct_child_childA_matches", `{ resource.service.name = "root-svc" } > { resource.service.name = "childa-svc" }`, 1},
		{"direct_child_mutant_root_grandchild_does_not_match", `{ resource.service.name = "root-svc" } > { resource.service.name = "grandchild-svc" }`, 0},
		{"descendant_root_grandchild_matches", `{ resource.service.name = "root-svc" } >> { resource.service.name = "grandchild-svc" }`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			got := grpcMatchedCount(ctx, t, client, tc.query, start, end)
			if got != tc.wantMatched {
				t.Fatalf("query %q: matched spans = %d, want %d", tc.query, got, tc.wantMatched)
			}
		})
	}
}

// TestTraceQLScopeCollisionConjunction_GRPC is the gRPC-transport sibling of
// the traceql.selector.scope-collision-conjunction shape
// TestTraceQL_PropertyShapeRoster exercises over HTTP. Two DIFFERENT traces
// carry the SAME attribute key (`environment`) at different scopes with
// deliberately MIRRORED values (gen/traceql.go's
// TraceQLScopeCollisionAttributeKey doc): trace1's span has
// resource.environment="prod" + span.environment="shadow"; trace2's span
// (the control) has the exact opposite pairing, resource.environment=
// "shadow" + span.environment="prod".
//
// The query only trace1 should satisfy is asserted by TRACE IDENTITY, not a
// bare count: a resource/span carrier swap makes `resource.environment`
// read the SpanAttributes map and `span.environment` read the
// ResourceAttributes map, and because the two traces' values are exact
// mirrors of each other, the swapped query matches trace2 instead of
// trace1 — cardinality stays 1 either way, so a count-only assertion is
// blind to this exact misattribution. This is the "equal cardinality, wrong
// identity" shape the acceptance criteria call for; grpcSearchTraceQL's
// per-span TraceID list (not grpcMatchedCount) is what catches it.
func TestTraceQLScopeCollisionConjunction_GRPC(t *testing.T) {
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
    (toDateTime64('2026-05-13 12:00:00', 9), '0102030405060708090a0b0c0d0e0f10', '0102030405060708', '', 'span1', 'Internal', 'scope-svc', map('service.name', 'scope-svc', 'environment', 'prod'), map('environment', 'shadow'), 1, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:01', 9), '1112131415161718191a1b1c1d1e1f20', '1112131415161718', '', 'span2', 'Internal', 'scope-svc', map('service.name', 'scope-svc', 'environment', 'shadow'), map('environment', 'prod'), 1, 'Unset', '', '', '');`)

	client, cleanup := dialChDBSearchServer(t, c)
	t.Cleanup(cleanup)

	start := time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 13, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	query := `{ resource.environment = "prod" && span.environment = "shadow" }`
	wantTraceID := "0102030405060708090a0b0c0d0e0f10" // trace1 — the only correctly-scoped match
	got := grpcSearchTraceQL(ctx, t, client, query, start, end)
	if len(got) != 1 || got[0] != wantTraceID {
		t.Fatalf("query %q: matched trace IDs = %v, want exactly [%s] (trace1, not trace2's mirrored pairing)",
			query, got, wantTraceID)
	}
}
