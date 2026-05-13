// Package local provides an in-process PromQL evaluator intended to serve as
// the differential-testing oracle for cerberus shadow-mode (RC3 R3.9).
//
// The evaluator wraps Prometheus's own promql.Engine and runs queries against
// an in-memory SampleStore. It performs no ClickHouse I/O; callers seed
// synthetic series, then issue Instant or Range queries, and the resulting
// samples are diffed against cerberus's ClickHouse-emitted results.
//
// Design lineage: this package is inspired by the BadLiveware/promshim-clickhouse
// project's local evaluator, but is implemented from scratch on top of the
// Prometheus engine API (no code is ported verbatim, so no attribution is
// required).
//
// Status: scaffold (RC3 R3.10). No callers yet. RC3 R3.9 will wire this into
// the shadow-mode harness.
package local
