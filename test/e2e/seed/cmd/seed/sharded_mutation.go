package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// mutationTablePlaceholder marks the table name in the DELETE templates
// declared in stale.go and showcase_traceql.go — substituted for the
// resolved mutation target (resolveMutationTarget) via mutationTableSQL
// below. A dedicated token rather than a fmt.Sprintf %s verb: every one of
// those templates' TraceId/predicate clauses already contains literal '%'
// LIKE wildcards, which a %s verb would force into confusing (and
// regression-test-breaking — see test/regression/seed_test.go's
// TestShowcaseTraceStaleDeleteIsDataAnchored, which scans the raw source
// text for a single '%') '%%' escaping. Chosen to be a string no seeded
// table name, SQL keyword, or LIKE pattern in this package could ever
// collide with.
const mutationTablePlaceholder = "@@MUTATION_TABLE@@"

// mutationOnClusterPlaceholder marks the optional " ON CLUSTER '{cluster}'"
// clause on the OUTER `ALTER TABLE` line only (never the inner max(...)
// subquery, which is a plain per-node SELECT) — substituted by
// resolveMutationTarget's onCluster return value via mutationTableSQL.
// Empty on every lane except the datashard one; see this file's package
// doc comment for why ON CLUSTER is what the local-table redirect alone
// does not fix.
const mutationOnClusterPlaceholder = "@@MUTATION_ON_CLUSTER@@"

// mutationTableSQL substitutes every occurrence of mutationTablePlaceholder
// in template with target, and mutationOnClusterPlaceholder with onCluster.
// Each DELETE template carries mutationTablePlaceholder twice (the outer
// ALTER TABLE/DELETE FROM and the inner max(...) subquery's FROM), and both
// must resolve to the same table — see this file's package doc comment for
// why the resolved name can differ from the table's public name.
func mutationTableSQL(template, target, onCluster string) string {
	s := strings.ReplaceAll(template, mutationTablePlaceholder, target)
	return strings.ReplaceAll(s, mutationOnClusterPlaceholder, onCluster)
}

// Mutation-target resolution for the DATA-shard topology (issue #3077).
//
// Under Config.DataShardCount > 1, internal/schema/ddl wraps every base
// metrics/logs/traces table in a Distributed-engine table under the
// table's PUBLIC name (otel_metrics_gauge, otel_logs, otel_traces, ...)
// and moves the real MergeTree-family storage to a LOCAL table suffixed
// ddl.DataShardLocalSuffix (otel_metrics_gauge_local, ...; see ddl.go's
// renderDistributedWrapper / renderDataShardedSignal / dataShardLocalConfig).
// Under DataShardCount == 0 (every other lane this seeder runs against —
// compose, bwc, single-shard k3d) there is no wrapper at all: the public
// name IS the storage table.
//
// ClickHouse's Distributed engine does not support ALTER TABLE ... DELETE
// or lightweight DELETE FROM mutations at all, confirmed live on the
// datashard e2e lane (issue #3105, dispatch run 34014775573):
//
//	seed: insert metrics: gauge-shaped metrics stale delete: code: 48,
//	message: Table engine Distributed doesn't support mutations
//
// stale.go's and showcase_traceql.go's stale-row DELETEs are written
// against the literal public table names, which is correct everywhere
// except the datashard lane — the one place where the public name doesn't
// resolve to real storage. resolveMutationTarget below fixes that by
// asking ClickHouse's own system.tables what engine the public name
// actually is, at the moment the seeder needs to mutate it, and
// redirecting to the "_local" table when it turns out to be a Distributed
// wrapper. This needs no seeder-side knowledge of DataShardCount and no
// new Justfile/env-var plumbing: the answer is read straight from the
// schema the seeder is already connected to, the same way waitForTables
// (main.go) already reads system.tables to learn whether the external
// schema writer has created a table yet.

// distributedEngine is the exact system.tables.engine value ClickHouse
// reports for a Distributed-engine table — the one engine that rejects
// every mutation the DELETEs in this package issue.
const distributedEngine = "Distributed"

// onClusterMacro is ClickHouse's own `{cluster}` macro, expanded
// server-side from system.macros — the same self-discovery mechanism
// internal/schema/ddl's own ON CLUSTER DDL and the chart's macros ConfigMap
// (deploy/helm/cerberus/templates/clickhouse/configmap-config.yaml,
// `<macros><cluster>bwc_cluster</cluster></macros>`) already rely on. Using
// the macro rather than a literal cluster name means this file needs no new
// knowledge of what the datashard lane's cluster is actually called, and
// stays correct if that name ever changes.
//
// A "_local" table is created ON CLUSTER (ddl.go's dataShardLocalConfig /
// renderDataShardedSignal) so it exists, identically named, on every shard
// node — an ALTER ... DELETE issued only against the ONE node the seeder is
// connected to would silently mutate just that node's own fraction of the
// sharded data, leaving stale rows on every OTHER shard (confirmed live:
// TestReSeedRowCountStability failed for exactly the tables whose insert
// path spreads rows across shards, on dispatch run 34015811450 — the local-
// table redirect alone got the mutation to succeed, but only partially).
// ON CLUSTER broadcasts the ALTER to every node via ClickHouse's own
// distributed-DDL queue, each executing it against its own local data.
const onClusterMutationClause = " ON CLUSTER '{cluster}'"

// shardMutationTarget is what resolveMutationTarget resolves a public table
// name to: the table to actually mutate, plus the ON CLUSTER clause (empty
// outside the datashard lane) needed to reach every shard's copy of it.
type shardMutationTarget struct {
	table     string
	onCluster string
}

// shardMutationTargets memoizes resolveMutationTarget's system.tables
// lookups for the life of one seeder process. Whether a given public table
// name is a Distributed wrapper is fixed by Config.DataShardCount at
// cluster-provisioning time and never changes while the seeder runs, but
// the rolling re-seeder calls resolveMutationTarget once per stale-DELETE
// on every tick (`--re-seed-interval`, default 30s per main.go's flag doc)
// — memoizing avoids re-querying system.tables for the same static fact on
// every tick, for the life of the run.
//
// Package-level and unsynchronized: both the one-shot path (run()) and the
// rolling re-seed loop (main.go's ticker loop) call seedAll synchronously
// from a single goroutine, never concurrently, so a bare map needs no
// lock. A future caller that drives seedAll from multiple goroutines would
// need to add one.
var shardMutationTargets = make(map[string]shardMutationTarget)

// mutationTargetForEngine returns the table (and ON CLUSTER clause, if any)
// a DELETE against table should actually target, given engine — table's
// system.tables.engine value. A Distributed wrapper redirects to its
// ddl.DataShardLocalSuffix companion and needs ON CLUSTER to reach every
// shard's copy of it; every other engine (MergeTree, ReplicatedMergeTree,
// ReplacingMergeTree, ...) is already the real, single-node storage table
// and is returned unchanged with no ON CLUSTER clause. Pure and independent
// of any live connection, so it is unit-testable directly against
// fabricated engine strings (see sharded_mutation_test.go).
func mutationTargetForEngine(table, engine string) shardMutationTarget {
	if engine == distributedEngine {
		return shardMutationTarget{table: table + ddl.DataShardLocalSuffix, onCluster: onClusterMutationClause}
	}
	return shardMutationTarget{table: table}
}

// tableEngine looks up table's engine in system.tables. Scoped to
// currentDatabase() rather than a bound database parameter because the
// seeder already relies on Auth.Database to resolve every unqualified
// table name it INSERTs into server-side (main.go's package doc comment)
// — currentDatabase() applies that same resolution to this lookup.
func tableEngine(ctx context.Context, conn driver.Conn, table string) (string, error) {
	const engineSQL = `SELECT engine FROM system.tables WHERE database = currentDatabase() AND name = {table:String}`
	var engine string
	row := conn.QueryRow(ctx, engineSQL, clickhouse.Named("table", table))
	if err := row.Scan(&engine); err != nil {
		return "", fmt.Errorf("resolve engine for table %s: %w", table, err)
	}
	return engine, nil
}

// resolveMutationTarget returns the table (and ON CLUSTER clause, if any) a
// stale-row DELETE against the public table name `table` should actually
// target — see this file's package doc comment for the Distributed/local/
// ON CLUSTER mechanism. Memoized in shardMutationTargets so only the first
// call per table name on a given process ever queries system.tables.
func resolveMutationTarget(ctx context.Context, conn driver.Conn, table string) (shardMutationTarget, error) {
	if target, ok := shardMutationTargets[table]; ok {
		return target, nil
	}
	engine, err := tableEngine(ctx, conn, table)
	if err != nil {
		return shardMutationTarget{}, err
	}
	target := mutationTargetForEngine(table, engine)
	shardMutationTargets[table] = target
	return target, nil
}
