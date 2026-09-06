package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// mutationTablePlaceholder marks the table name in a mutation-family SQL
// template declared in stale.go/showcase_traceql.go — substituted for the
// resolved table name via mutationTableSQL below. A dedicated token rather
// than a fmt.Sprintf %s verb: every one of those templates' TraceId/
// predicate clauses already contains literal '%' LIKE wildcards, which a
// %s verb would force into confusing (and regression-test-breaking — see
// test/regression/seed_test.go's TestShowcaseTraceStaleDeleteIsDataAnchored,
// which scans the raw source text for a single '%') '%%' escaping. Chosen
// to be a string no seeded table name, SQL keyword, or LIKE pattern in this
// package could ever collide with.
//
// Each template — the step-A cutoff SELECT and the step-B literal-cutoff
// ALTER/DELETE alike (see this file's package doc comment for the two-step
// design, issue #3124) — carries exactly ONE occurrence of this token: a
// step-A template substitutes it for the PUBLIC table name, a step-B
// template substitutes it for the resolved mutation target (the "_local"
// table under the datashard lane). The two statements are substituted
// independently by two separate mutationTableSQL calls; nothing here ties
// one template's substitution to the other's the way the old single-
// statement subquery design once did.
const mutationTablePlaceholder = "@@MUTATION_TABLE@@"

// mutationOnClusterPlaceholder marks the optional " ON CLUSTER '{cluster}'"
// clause on a step-B ALTER TABLE/DELETE FROM template — substituted by
// resolveMutationTarget's onCluster return value via mutationTableSQL. Empty
// on every lane except the datashard one; see this file's package doc
// comment for why ON CLUSTER is what the local-table redirect alone does not
// fix. Never appears in a step-A cutoff SELECT template: that statement is a
// plain per-node SELECT (mutationTableSQL still erases the token if a
// template doesn't carry it, so passing onCluster="" for a step-A
// substitution is always safe).
const mutationOnClusterPlaceholder = "@@MUTATION_ON_CLUSTER@@"

// mutationTableSQL fills in template's mutationTablePlaceholder occurrence
// with table and its mutationOnClusterPlaceholder occurrence (if any) with
// onCluster. Used for BOTH halves of the two-step literal-cutoff design
// (issue #3124): once per family for the step-A cutoff SELECT (table =
// the PUBLIC name, onCluster = "" — a SELECT needs no cluster broadcast),
// and once per family for the step-B ALTER/DELETE (table = the resolved
// mutation target, onCluster = resolveMutationTarget's own return value).
// Earlier revisions of this function filled the (then two-occurrence)
// placeholder differently for the outer statement vs. the inner max(...)
// subquery it now replaces; that split is gone along with the subquery.
func mutationTableSQL(template, table, onCluster string) string {
	s := strings.ReplaceAll(template, mutationTablePlaceholder, table)
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
//
// The redirect alone is not the whole fix. Every stale-row cutoff — "how
// fresh is the data that's actually here" — is computed by a
// max(<time column>) SELECT (resolveStaleCutoff below), and that SELECT
// must run against the PUBLIC name, never a shard's own "_local" table: a
// SELECT against a Distributed table (unlike a mutation) is exactly what
// the engine is FOR, and ClickHouse fans it out and aggregates the true
// cluster-wide max() regardless of which shard node happens to run it.
// Anchoring the read at a shard's own "_local" table instead breaks on a
// LOW-row-count family (base-traces' fixed 7-row fixture): a Distributed
// table's INSERT spreads rows across shards essentially at random
// (Config.DataShardingKey, default rand()), so a tick can land NONE of its
// fresh rows on a given shard, and that shard's own max() would reflect
// only OLD rows (or nothing newer than the sentinel itself) — confirmed
// live: TestReSeedRowCountStability/base-traces alone (the lowest-row-count
// family; every higher-volume family's fresh rows are near-certain to land
// on every shard every tick, masking the same bug) failed on dispatch run
// 34016653322 after the local-table + ON CLUSTER fix alone. resolveStaleCutoff
// therefore always reads the PUBLIC name.
//
// Reading the cutoff is now (issue #3124) a SEPARATE statement from the
// DELETE that consumes it, not a subquery nested inside the mutation.
// Issue #3105's original design — a live `max(...)` subquery embedded
// directly in the `ALTER ... DELETE`/`DELETE FROM` — worked against a
// single, non-replicated "_local" table per shard, but the
// `datashard-replica-affinity` lane (issue #3086) runs MULTIPLE REPLICAS per
// data shard, whose "_local" tables are ReplicatedMergeTree-family; ClickHouse
// rejects a subquery-bearing mutation against a replicated table outright
// ("ALTER UPDATE/ALTER DELETE statement with subquery may be nondeterministic",
// code 36) because an `ON CLUSTER` broadcast has every replica of every shard
// independently re-evaluate the subquery, and two replicas evaluating
// `max(...)` at slightly different wall-clock moments could disagree on which
// rows to delete and silently diverge their data. resolveStaleCutoff
// (this file) runs the `max(...)` SELECT once, client-side, and its caller
// binds the resulting time.Time as a literal `{cutoff:DateTime64(9)}`
// parameter on the DELETE — every replica's own copy of the mutation then
// compares against the exact same fixed value, so no subquery (and no
// per-replica re-evaluation) survives inside the mutation at all. This
// removes the nondeterminism ClickHouse's guard rejects by construction,
// rather than by disabling the guard via allow_nondeterministic_mutations —
// the guard's own reasoning (real, not a false positive) is left intact for
// any future mutation that still needs it.

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

// resolveStaleCutoff runs selectTemplate — a step-A "SELECT max(<time
// column>) - INTERVAL ... FROM @@MUTATION_TABLE@@ WHERE ..." query, with
// exactly one mutationTablePlaceholder occurrence and no
// mutationOnClusterPlaceholder — against publicTable and scans the single
// resulting value into a time.Time. That value is the literal cutoff every
// replica of the caller's own step-B ALTER/DELETE compares against (see
// this file's package doc comment for why resolving it client-side, once,
// before the mutation is issued removes issue #3124's nondeterminism
// hazard by construction). Always queries the PUBLIC Distributed name,
// never the resolved mutation target — see the package doc comment for why
// a Distributed SELECT is what correctly aggregates the cluster-wide
// max() regardless of which shard node runs it. selectArgs are forwarded
// to conn.QueryRow verbatim (most callers bind a {margin:UInt64} parameter
// alongside the query text).
func resolveStaleCutoff(ctx context.Context, conn driver.Conn, selectTemplate, publicTable string, selectArgs ...any) (time.Time, error) {
	sql := mutationTableSQL(selectTemplate, publicTable, "")
	var cutoff time.Time
	if err := conn.QueryRow(ctx, sql, selectArgs...).Scan(&cutoff); err != nil {
		return time.Time{}, fmt.Errorf("resolve stale cutoff: %w", err)
	}
	return cutoff, nil
}
