package main

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Showcase-TraceQL seed shapes (PR feat/showcase-traceql). The
// showcase-traceql dashboard's feature panels need span topologies the
// dogfood self-telemetry can't guarantee deterministically:
//
//   - structural depth — a 4-deep parent→child chain plus a sibling
//     pair, so every structural operator (> >> < << ~ and the negated
//     / union-prefixed variants) selects real rows;
//   - status variety (Ok / Error / Unset) + non-empty StatusMessage;
//   - kind variety (Server / Client / Internal / Producer / Consumer);
//   - span Events (name 'exception' + exception.message attribute) for
//     event:name / event.attr panels;
//   - span Links (TraceId / SpanId across the two showcase traces +
//     opentracing.ref_type attribute) for link:traceID / link:spanID /
//     link.attr panels;
//   - ScopeName / ScopeVersion ('showcase-instrumentation' / '1.2.3')
//     for the instrumentation:* intrinsics;
//   - numeric / float / bool span attributes (payload_bytes /
//     checkout.amount / cache.hit) for arithmetic, spanset-aggregate
//     and *_over_time(attr) panels.
//
// All rows re-anchor on now64(9) so the rolling re-seeder (30 s ticks)
// keeps the spans inside every panel's query window. Trace/span IDs are
// stable across ticks (the b0... range; the base fixture owns a0...) —
// re-inserting the same span with a slightly newer Timestamp just adds
// a near-duplicate row, which trace search tolerates (results collapse
// per TraceId).
//
// Span topology:
//
//	Trace b...001 (checkout, root Ok):
//	  b101 gateway  GET /api/checkout   Server   Ok                250ms
//	  └─ b102 shop  checkout            Internal Ok                200ms
//	     ├─ b103 payments charge        Client   Error (declined)  150ms  [event: exception]
//	     │  └─ b105 payments charge-retry Internal Unset            20ms
//	     └─ b104 db  orders.insert      Client   Ok                 50ms
//
//	Trace b...002 (orders, root Error):
//	  b201 gateway  POST /api/orders    Server   Error (deadline)  600ms
//	  ├─ b202 queue orders.publish      Producer Ok                 30ms
//	  └─ b203 shop  orders.process      Consumer Ok                 80ms   [link → b...001/b103]
//	     └─ b204 db orders.update       Client   Ok                 40ms
const insertShowcaseTracesSQL = `INSERT INTO otel_traces
  (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
   ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes, Duration,
   StatusCode, StatusMessage,
   Events.Timestamp, Events.Name, Events.Attributes,
   Links.TraceId, Links.SpanId, Links.TraceState, Links.Attributes)
VALUES
  (now64(9) - INTERVAL 40 SECOND, 'b0000000000000000000000000000001', 'b000000000000101', '',
   'GET /api/checkout', 'Server', 'gateway', map('service.name', 'gateway'),
   'showcase-instrumentation', '1.2.3',
   map('http.method', 'GET', 'http.status_code', '200', 'payload_bytes', '2048', 'cache.hit', 'true', 'checkout.amount', '12.5'),
   250000000, 'Ok', '', [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 39 SECOND, 'b0000000000000000000000000000001', 'b000000000000102', 'b000000000000101',
   'checkout', 'Internal', 'shop', map('service.name', 'shop'),
   'showcase-instrumentation', '1.2.3',
   map('payload_bytes', '512', 'cache.hit', 'false'),
   200000000, 'Ok', '', [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 38 SECOND, 'b0000000000000000000000000000001', 'b000000000000103', 'b000000000000102',
   'charge', 'Client', 'payments', map('service.name', 'payments'),
   'showcase-instrumentation', '1.2.3',
   map('http.method', 'POST', 'http.status_code', '502', 'payload_bytes', '128'),
   150000000, 'Error', 'card declined',
   [now64(9) - INTERVAL 38 SECOND], ['exception'],
   [map('exception.message', 'card declined: timeout talking to processor')],
   [], [], [], []),
  (now64(9) - INTERVAL 38 SECOND, 'b0000000000000000000000000000001', 'b000000000000104', 'b000000000000102',
   'orders.insert', 'Client', 'db', map('service.name', 'db'),
   'showcase-instrumentation', '1.2.3',
   map('db.system', 'postgres', 'db.system.name', 'postgres', 'payload_bytes', '64'),
   50000000, 'Ok', '', [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 37 SECOND, 'b0000000000000000000000000000001', 'b000000000000105', 'b000000000000103',
   'charge-retry', 'Internal', 'payments', map('service.name', 'payments'),
   'showcase-instrumentation', '1.2.3',
   map('payload_bytes', '32'),
   20000000, 'Unset', '', [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 25 SECOND, 'b0000000000000000000000000000002', 'b000000000000201', '',
   'POST /api/orders', 'Server', 'gateway', map('service.name', 'gateway'),
   'showcase-instrumentation', '1.2.3',
   map('http.method', 'POST', 'http.status_code', '500', 'payload_bytes', '4096'),
   600000000, 'Error', 'deadline exceeded while processing order',
   [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 24 SECOND, 'b0000000000000000000000000000002', 'b000000000000202', 'b000000000000201',
   'orders.publish', 'Producer', 'queue', map('service.name', 'queue'),
   'showcase-instrumentation', '1.2.3',
   map('messaging.system', 'kafka', 'payload_bytes', '256'),
   30000000, 'Ok', '', [], [], [], [], [], [], []),
  (now64(9) - INTERVAL 23 SECOND, 'b0000000000000000000000000000002', 'b000000000000203', 'b000000000000201',
   'orders.process', 'Consumer', 'shop', map('service.name', 'shop'),
   'showcase-instrumentation', '1.2.3',
   map('messaging.system', 'kafka', 'payload_bytes', '256'),
   80000000, 'Ok', '',
   [], [], [],
   ['b0000000000000000000000000000001'], ['b000000000000103'], [''],
   [map('opentracing.ref_type', 'child_of')]),
  (now64(9) - INTERVAL 22 SECOND, 'b0000000000000000000000000000002', 'b000000000000204', 'b000000000000203',
   'orders.update', 'Client', 'db', map('service.name', 'db'),
   'showcase-instrumentation', '1.2.3',
   map('db.system', 'postgres', 'db.system.name', 'postgres', 'payload_bytes', '96'),
   40000000, 'Ok', '', [], [], [], [], [], [], [])`

// selectStaleShowcaseTracesCutoffSQLTemplate resolves the stale-row cutoff
// for the b0... showcase TraceId range (issue #3124's step A) — run once
// against the PUBLIC otel_traces name (see sharded_mutation.go's package
// doc comment for why a Distributed SELECT, unlike a mutation, correctly
// aggregates cluster-wide regardless of which node runs it) and scanned
// into a time.Time by resolveStaleCutoff. deleteStaleShowcaseTracesSQLTemplate
// (step B) then binds that literal back as {cutoff:DateTime64(9)} — see its
// own doc comment for the full margin/ordering analysis this split
// preserves unchanged, and for why the cutoff must live in a separate
// statement from the DELETE at all.
//
// Carries exactly ONE occurrence of the literal text "@@MUTATION_TABLE@@"
// (mutationTablePlaceholder's value, sharded_mutation.go), substituted for
// the PUBLIC table name via mutationTableSQL — never the resolved mutation
// target, which under the datashard lane (issue #3105) is the "_local"
// table a plain SELECT never needs to redirect to. Typed directly into this
// single backtick string (never built by concatenating separate
// backtick-quoted segments around mutationTablePlaceholder) — see
// deleteStaleShowcaseTracesSQLTemplate's own doc comment for why: it keeps
// this const a single unbroken backtick literal for
// test/regression/seed_test.go's extractBacktickConst, and it keeps the
// LIKE 'b00...%' wildcard byte-identical rather than forcing fmt.Sprintf
// %%-escaping.
const selectStaleShowcaseTracesCutoffSQLTemplate = `SELECT max(Timestamp) - INTERVAL 20 SECOND
FROM @@MUTATION_TABLE@@
WHERE TraceId LIKE 'b00000000000000000000000000000%'`

// deleteStaleShowcaseTracesSQL drops the *previous* ticks' showcase
// spans (the b0... TraceId range is exclusively this seeder's) AFTER
// the re-anchored INSERT has landed. Without any delete every 30 s
// tick stacked another full copy of each span; structural-join
// closures then multiplied the duplicates per recursion level, and
// trace-detail views showed N copies of every span. Lightweight
// DELETE keeps the tick cheap (mask-based, no part rewrite).
//
// Ordering + predicate are both load-bearing (compose-smoke flake on
// PR #769): lightweight DELETEs are asynchronous mutations, so the
// original DELETE-then-INSERT left a visible window where the whole
// showcase range was empty — any TraceQL query landing inside it saw
// zero spans, and failOnFlakyTests turned the retry-pass into a CI
// failure. The fix inverts the order (INSERT first, so the range is
// never empty) and anchors the cutoff on the DATA rather than the
// clock: rows strictly older than `max(showcase Timestamp) - 20 s`
// are stale by construction.
//
// Why 20 s: each tick's INSERT spans offsets `now-40s .. now-22s`
// (an 18 s spread), so the freshest tick's own rows sit within 18 s
// of its max and are always spared, while the previous tick (30 s
// older; its newest row is 52 s behind the new max) always falls
// past the cutoff. Any margin in (18 s, 30 s) exclusive works; 20 s
// leaves slack on the sparing side. Because the cutoff follows the
// data, the scheme is immune to statement-gap timing, client/server
// clock skew, and late mutation application: whenever the predicate
// is evaluated, the newest tick's rows are inside the margin, and
// rows inserted after the mutation was created are untouched by it
// by ClickHouse's mutation-versioning guarantee. Worst case readers
// briefly see two copies of a span — the recursive-closure per-level
// DISTINCT (#762) collapses dupes, and showcase contracts assert
// nonempty/error classes, never exact values (#757).
//
// The cutoff (`max(showcase Timestamp) - 20s`) is resolved to a literal
// time.Time BEFORE this statement runs — selectStaleShowcaseTracesCutoffSQLTemplate
// above, via resolveStaleCutoff — rather than living as a subquery nested
// inside this DELETE. issue #3105's original design nested it directly, and
// that worked against a single non-replicated "_local" table per shard, but
// the datashard-replica-affinity lane (issue #3086) runs multiple REPLICAS
// per shard, and ClickHouse rejects a subquery-bearing mutation against a
// replicated table outright ("...statement with subquery may be
// nondeterministic", code 36): every replica of every shard would otherwise
// re-evaluate the subquery independently, and two replicas evaluating
// max(...) at slightly different wall-clock moments could disagree on which
// rows to delete and silently diverge their data. Binding the literal
// {cutoff:DateTime64(9)} instead means every replica's own copy of this
// DELETE compares against the exact same fixed value — no subquery, and no
// per-replica re-evaluation, survives inside the mutation at all.
//
// Carries exactly ONE occurrence of the literal text "@@MUTATION_TABLE@@"
// (mutationTablePlaceholder's value, sharded_mutation.go) marking the
// table to target — substituted at call time via
// mutationTableSQL/resolveMutationTarget rather than baked in as a
// literal. Under the datashard lane (issue #3105) otel_traces is a
// Distributed wrapper that rejects DELETE FROM outright ("Table engine
// Distributed doesn't support mutations", code 48); the resolved name
// redirects to the underlying "_local" table instead. One occurrence of
// "@@MUTATION_ON_CLUSTER@@" follows it — empty outside the datashard lane,
// " ON CLUSTER '{cluster}'" within it, needed because a "_local" table's
// mutation must reach every shard's own copy, not just the node the
// seeder is connected to.
//
// The placeholder is typed directly into this single backtick string
// (never built by concatenating separate backtick-quoted segments around
// mutationTablePlaceholder) — see stale.go's matching doc comment for why:
// it keeps this const a single unbroken backtick literal for
// test/regression/seed_test.go's extractBacktickConst, and it keeps the
// LIKE 'b00...%' wildcard below byte-identical to what it was before this
// const learned to target a resolved table name (no fmt.Sprintf
// %%-escaping).
const deleteStaleShowcaseTracesSQLTemplate = `DELETE FROM @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@
WHERE TraceId LIKE 'b00000000000000000000000000000%'
  AND Timestamp < {cutoff:DateTime64(9)}`

// insertShowcaseTraces re-seeds the two showcase trace topologies. Runs
// inside seedAll so each rolling re-seed tick re-anchors the spans on
// the current wall clock. INSERT strictly precedes the stale-row
// DELETE so readers never observe an empty (or partially-deleted)
// showcase range — see deleteStaleShowcaseTracesSQLTemplate for the full
// race analysis.
func insertShowcaseTraces(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(ctx, insertShowcaseTracesSQL); err != nil {
		return fmt.Errorf("showcase traces: %w", err)
	}
	target, err := resolveMutationTarget(ctx, conn, tracesTable)
	if err != nil {
		return fmt.Errorf("showcase traces stale delete: %w", err)
	}
	cutoff, err := resolveStaleCutoff(ctx, conn, selectStaleShowcaseTracesCutoffSQLTemplate, tracesTable)
	if err != nil {
		return fmt.Errorf("showcase traces stale delete: %w", err)
	}
	sql := mutationTableSQL(deleteStaleShowcaseTracesSQLTemplate, target.table, target.onCluster)
	if err := conn.Exec(ctx, sql, clickhouse.Named("cutoff", cutoff)); err != nil {
		return fmt.Errorf("showcase traces stale delete: %w", err)
	}
	return nil
}
