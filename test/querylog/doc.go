// Package querylog holds the real-ClickHouse evidence for the query-actuals
// query-log source (internal/chclient/query_log_actuals.go and
// internal/engine's QueryLogActualsReconciler): which observations the local
// system.query_log misses when a query ran on another server or before the
// log rotated, which of them the native packet path already covers, and that
// the system.all_query_log union finds the rest without accounting any
// physical query twice. It also measures that a routed request's shard
// statements enter another process's tracker as one observation of the
// request's total, and that a dispatch over the HTTP protocol, which streams
// no progress packets, is observed from the query log with its real totals.
//
// Every test boots pinned clickhouse/clickhouse-server builds — the supported
// floor and the first line carrying create_union_system_log_tables — under
// container CPU and memory limits: two data shards behind a Distributed
// table, and one server upgraded from the floor so its query log rotates.
// Queries are dispatched through cerberus's own client with actuals capture
// armed exactly as the engine arms it, and the reconciler reads through a
// client that fails over to the other shard.
//
// The tests carry the `integration` build tag (Docker required) and run in
// the query-log-union job of strict-scan.yml through
// `just query-log-integration`.
package querylog
