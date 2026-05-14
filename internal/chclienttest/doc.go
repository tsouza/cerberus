// Package chclienttest provides a chDB-backed Querier implementation
// used by handler-level unit tests. The handlers (api/prom, api/loki,
// api/tempo) each define their own Querier interface as the subset of
// chclient.Client they consume; this package's Client type satisfies
// all three of them so any handler test can drop the stub and exercise
// the full parse → lower → optimize → emit → execute pipeline against
// real ClickHouse semantics via the in-process chDB engine.
//
// The default `just test` lane stays CGO_ENABLED=0 and never compiles
// this package — every file is gated behind the `chdb` build tag, the
// same tag the chDB probe (RC8 R8.0) and the TXTAR round-trip runner
// (RC8 R8.1) use. The `chdb` CI job runs after `just chdb-install` has
// placed libchdb.so at /usr/local/lib.
//
// Map column scan: chdb-go v1.11.0's parquet driver panics on the
// Parquet MAP logical type, so every emitted SQL that projects a
// Map(String, String) column has to wrap it in toJSONString(...)
// server-side. The package rewrites the outer SELECT's projection
// list before issuing the query, mirroring the rewriter that ships
// with the spec runner (test/spec/runner_chdb.go). Callers don't see
// the rewrite — the Querier returns the same shapes the production
// chclient.Client returns.
//
// Lifetime: NewChDB binds the session to the test via t.Cleanup so
// each test gets an isolated chDB session and the temp directory the
// driver allocates is torn down with the test. Tests that want to
// pre-seed the session should call (*Client).Seed before exercising
// the handler — Seed runs the multi-statement DDL+INSERT script
// against the same session the handler will read from.
package chclienttest
