// Package chsqltest provides the chDB harness internal/chsql's round-trip
// tests share: an isolated database per test, and one renderer for the
// metric-table seed shape those tests write into it.
//
// It exists as a package rather than as helpers inside internal/chsql's own
// test files because that suite straddles two test packages. The fused
// subquery differential must stay in the internal `package chsql` — it drives
// the unexported emitter entry points directly — while every other chDB test
// is `package chsql_test`. A shared definition therefore has to live outside
// both, the same way chclienttest sits outside the handler packages that
// consume it.
//
// Isolation is the reason the harness exists at all. chdb-go caches ONE
// session per process, so every sql.Open("chdb", "") in a test binary reaches
// the same engine; when those tests also shared one `default` database, table
// DDL leaked from each test into every later one. The metric read path is
// acutely sensitive to that, because a selector emits
// merge(currentDatabase(), '^(otel_metrics_gauge|...)$') and ClickHouse
// requires a referenced column to exist in EVERY table the regex matches. One
// test seeding an otel_metrics_* table with a different column set therefore
// broke an unrelated later test. OpenIsolatedChDB gives each test its own
// database, so currentDatabase() resolves to the caller's own and the regex
// can only ever match the caller's own tables.
//
// The default `just test` lane stays CGO_ENABLED=0 and never compiles the
// harness — everything but this file is gated behind the `chdb` build tag,
// the same tag the chDB driver probe and the TXTAR round-trip runner use.
// This file carries no tag so the package still has Go files in an untagged
// build.
package chsqltest
