// Package property is the oracle-based property-testing framework for
// cerberus. It is the third tier in the cerberus test pyramid:
//
//  1. test/spec/ — hand-curated TXTAR golden fixtures: input QL → emitted
//     SQL (text equality) and, on the chdb-tagged lane, round-tripped row
//     sets. The author writes both sides; the framework checks the SQL
//     text and (optionally) the row equality. Catches regressions to the
//     known-good shapes.
//  2. harness/prometheus-compliance/ — reference-impl-driven differential testing:
//     a fixed Prometheus + cerberus pair both ingest the same OTel
//     fixture and the same canned queries are diffed against the
//     reference Prometheus instance over HTTP. Catches drift against
//     a known-good upstream implementation.
//  3. test/property/ (this package) — randomises BOTH data and queries
//     and uses an INDEPENDENT spec (oracle) to compute the expected
//     output. With shrinking via pgregory.net/rapid, a discovered
//     failure is minimised to the smallest reproducer before the test
//     reports — so the developer reading the failure sees a one-series,
//     one-point dataset and a two-token query rather than a 4kB blob.
//
// # Phase 1 PR 1 (this drop) — framework scaffolding
//
// This PR lands the framework infrastructure. The oracle is a TEMPORARY
// bridge to Prometheus's own promql.Engine (via internal/promshim/local),
// which means the "differential" property — "cerberus matches the oracle
// for every random query" — degenerates here to "cerberus matches
// Prometheus's engine for the MVP query set". That's not a from-scratch
// independent specification; it's a sanity check that the framework
// plumbing (rapid → dataset → chDB → cerberus → oracle → comparator)
// works end-to-end.
//
// The reason this is still useful as a landing PR:
//
//   - The framework infrastructure (generators, chDB session, comparator,
//     shrinking driver) is the load-bearing part. Replacing the oracle
//     is a follow-up of bounded scope (Phase 1 PR 2).
//   - Even with a bridge oracle, the MVP test still exercises every
//     generator path and every comparator branch on each run. A bug in
//     the generator (e.g. producing a label that no series carries) or
//     in the comparator (e.g. losing a sample on the cerberus side) is
//     surfaced loud and early.
//   - Cerberus's own end-to-end pipeline (HTTP → parse → lower → optimize
//     → emit → execute → response shape) is exercised on every iteration
//     against a real chDB session, catching pipeline-level regressions
//     the spec/ fixtures don't reach (they pin a SQL string rather than
//     a result shape).
//
// # Phase 1 PR 2 (follow-up) — from-scratch oracle
//
// PR 2 replaces oracle/bridge.go with an in-tree, from-scratch PromQL
// evaluator that reads the same MetricsModel. At that point the test
// stops being a "cerberus vs. Prometheus" sanity check and becomes a
// true differential property: cerberus must match an INDEPENDENT
// specification of PromQL semantics, written against the same model
// the generators populate.
//
// # File layout
//
//   - doc.go — this file.
//   - framework.go — runner glue: rapid.Check loop, types
//     (Dataset / Query / Outcome / LogsModel), per-iteration seed →
//     generate → run → diff → fail. Exports Run (PromQL) and
//     RunLogs (LogQL) entry points; both share the comparator and
//     dataset-dump path.
//   - chdb.go — chdb-tagged session helpers (open, apply DDL, map-
//     projection rewrite, decode cell). Replicated from
//     test/spec/runner_chdb.go; kept in-package so test/property has
//     no dependency on test/spec internals.
//   - chdb_stub.go — non-chdb stub that emits t.Skip so the default
//     CGO-free `just test` lane stays green.
//   - gen/metrics.go — random Dataset generator (DDL + in-memory
//     mirror; gauge-only for the MVP).
//   - gen/promql.go — random PromQL query generator targeted at the
//     from-scratch oracle's accept-set.
//   - gen/logql.go — random LogQL dataset + query generator (logs
//     mirror; log-stream selectors with `|=` / `!=` / `| label_format`).
//   - oracle/bridge.go — temporary bridge to promshim/local; retained
//     as a secondary sanity check for PromQL.
//   - oracle/promql/ — from-scratch PromQL evaluator.
//   - oracle/logql/  — from-scratch LogQL evaluator (log-stream
//     subset; metric-form queries deferred).
//   - promql_test.go — TestPromQL_Property_FromScratch, the chdb-
//     tagged PromQL property test.
//   - logql_test.go  — TestLogQL_Property, the chdb-tagged LogQL
//     property test.
//
// # Running locally
//
//	go test -tags chdb ./test/property/...
//
// The default lane (`just test`, CGO-free) skips this package because
// every test in it is chdb-tagged. CI's chdb lane (the same one that
// runs test/spec/promql/roundtrip_chdb_test.go) is the canonical
// scheduled runner.
package property
