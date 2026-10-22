// Package chserver holds the real-ClickHouse evidence for server-side
// mechanisms cerberus relies on but cannot implement itself: the query
// condition cache's result equivalence, the server's cancellation of
// CPU-bound expressions cerberus emits, and the read-back of an aggregation
// that spills its GROUP BY over cerberus's canonical series key. Every test boots pinned
// clickhouse/clickhouse-server builds — the supported floor, builds a known
// upstream defect affects, and builds carrying its fix — under container CPU
// and memory limits, drives cerberus-emitted shapes through the production
// handlers and client, and checks the observed server behaviour against the
// version policy internal/chopt records for that build.
//
// The real-server tests carry the `integration` build tag (Docker required)
// and run in the ch-server-safety job of strict-scan.yml through
// `just ch-server-safety-integration`. The cancellation probes' judging logic
// — where a request deadline lands and the separation verdict — is untagged,
// and its tests run in the unit lane against a modelled query.
package chserver
