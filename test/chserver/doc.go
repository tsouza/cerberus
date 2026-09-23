// Package chserver holds the real-ClickHouse evidence for server-side
// mechanisms cerberus relies on but cannot implement itself: the query
// condition cache's result equivalence and the server's cancellation of
// CPU-bound expressions cerberus emits. Every test boots pinned
// clickhouse/clickhouse-server builds — the supported floor, builds a known
// upstream defect affects, and builds carrying its fix — under container CPU
// and memory limits, drives cerberus-emitted shapes through the production
// handlers and client, and checks the observed server behaviour against the
// version policy internal/chopt records for that build.
//
// The tests carry the `integration` build tag (Docker required) and run in the
// strict-scan lane through `just ch-server-safety-integration`.
package chserver
