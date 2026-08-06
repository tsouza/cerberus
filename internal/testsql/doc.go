// Package testsql owns the textual SQL and DDL rewrites cerberus's test
// substrates apply around an emitted query.
//
// Two things live here, and both are facts about a SUBSTRATE rather than
// about a query language, which is why neither belongs to any one head:
//
//   - The chDB Map-column workaround. chdb-go's parquet driver cannot
//     decode a `Map` cell into a Go destination, so the outer projection
//     of an already-emitted statement is textually wrapped in
//     `toJSONString(...)` ([RewriteMapProjections]), the star projections
//     that would hide such a column from that wrap are expanded first
//     ([ExpandStarProjection]), and an `ORDER BY <Map>[k]` that the wrap
//     would re-bind to the wrapped String alias is nested one level down
//     ([NestMapOrderBy]).
//   - The fixture-seed DDL backfills. A test's simplified
//     `CREATE TABLE otel_metrics_*` / `otel_traces` declares only the
//     columns its own query names, while the read path projects several
//     unconditionally; [BackfillMetricsColumns] and
//     [BackfillTracesColumns] inject the missing ones with production's
//     DEFAULTs so a seed fails on real bugs, not on UNKNOWN_IDENTIFIER.
//
// The package exists because two independent consumers need byte-identical
// transforms and previously carried their own copies, which drifted:
//
//   - internal/chclienttest — the handler-level chDB-backed fake, driving
//     the `probe` CI job;
//   - test/spec — the TXTAR round-trip runner, the perf profiler's
//     prepare seam, and the strict-scan differential.
//
// Everything here is a TEXTUAL transform over SQL that internal/chsql has
// ALREADY emitted through its typed Frag API. That is deliberate and is
// the reason it lives outside internal/chsql: the transforms exist to
// paper over a test driver's decoding limits, they must never influence
// what production emits, and expressing them through the typed builder
// would put substrate workarounds inside the emitter. Nothing in this
// package composes a query from scratch.
package testsql
