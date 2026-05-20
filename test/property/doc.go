// Package property is the oracle-based property-testing framework for
// cerberus. It is the third tier in the cerberus test pyramid:
//
//  1. test/spec/ — hand-curated TXTAR golden fixtures: input QL → emitted
//     SQL (text equality) and, on the chdb-tagged lane, round-tripped row
//     sets. The author writes both sides; the framework checks the SQL
//     text and (optionally) the row equality. Catches regressions to the
//     known-good shapes.
//  2. compatibility/prometheus/ — reference-impl-driven differential testing:
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
// The oracle is an in-tree, from-scratch PromQL evaluator that reads
// the same MetricsModel the generators populate. This makes the test a
// true differential property: cerberus must match an INDEPENDENT
// specification of PromQL semantics, not delegate back to Prometheus's
// own engine.
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
//   - oracle/promql/ — from-scratch PromQL evaluator.
//   - oracle/logql/  — from-scratch LogQL evaluator (log-stream
//     subset; metric-form queries not covered).
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
