//go:build integration

// enummigrate_realch_integration_test.go — the corpus table's exit_status
// Enum8 reconciled against a REAL ClickHouse when the DEPLOYED table predates
// the running binary's member set.
//
// # WHY THIS LANE EXISTS
//
// The corpus table is created with `CREATE TABLE IF NOT EXISTS`, which is a
// no-op against an existing table no matter how its columns are declared. A
// binary that learns a new exit_status member therefore starts writing an
// Enum8 value the deployed column cannot hold. clickhouse-go rejects the
// Append with `unknown element`, the whole batch is invalidated, and the
// reconciler retries the identical batch every interval — a permanently wedged
// corpus that no unit test sees, because a fake batch accepts any int8 and a
// freshly-created table always carries the newest members.
//
// A binary that learns a new COLUMN hits the same wall from the other side:
// the deployed table simply does not have it, and because clickhouse-go's
// Append is positional against the deployed column list, the batch is not
// missing one field — every value after the gap is bound to the wrong column,
// or the batch is rejected outright.
//
// Only a real server distinguishes "the column already has this member" from
// "the ALTER widened it", or "the table already had this column" from "the
// ALTER added it". This lane creates the narrow, short table BY HAND, then
// asserts sink construction widens and extends it, and that a row carrying a
// new enum member and values in the added columns lands and reads back under
// its own name.
//
// Gated by the `integration` build tag (Docker required); run by
// `just router-corpus-integration`.
package optcorpus

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// legacyCorpusDDL creates the corpus table with BOTH reconciled Enum8 columns
// narrower than this binary writes: exit_status holds only the members a binary
// without the ClickHouse abort/error classes could produce, and route holds only
// 'A' and 'B' — no unclassified member. It is the deployed-table-lags-binary
// starting state, and it is what every corpus table deployed before those members
// existed actually looks like.
const legacyCorpusDDL = "CREATE TABLE " + CorpusTableName + ` (
  event_time DateTime,
  shape_id LowCardinality(String),
  language LowCardinality(String),
  normalized_query_hash UInt64,
  n_anchors UInt32,
  fanout UInt32,
  cumulative_d UInt32,
  outer_range UInt32,
  step UInt32,
  route Enum8('A' = 0, 'B' = 1),
  k_shards UInt8,
  decision_reason LowCardinality(String),
  read_rows UInt64,
  read_bytes UInt64,
  query_duration_ms UInt64,
  memory_usage UInt64,
  exit_status Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, 'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5)
) ENGINE = MergeTree ORDER BY (shape_id, n_anchors, fanout)`

// TestCorpusEnumMigrationRealClickHouse pins that sink construction reconciles
// EVERY reconciledEnumColumns entry against the member set this binary writes, and
// that a row carrying members the deployed table lacked lands correctly once it
// has.
//
// The route half is the one with production consequences. A deployment whose table
// still declares route Enum8('A','B') has nowhere to put an unclassified row, and
// the failure is silent in the worst way: without the widening the sink folds every
// LogQL and TraceQL row into 'A', so `route = 'A'` stops meaning "classified" and
// the route-scoped rules recommend sharding queries the router never even looked
// at. A fresh table always carries the newest members, so only a legacy table on a
// real server can tell "the ALTER widened it" from "it was already wide".
func TestCorpusEnumMigrationRealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), queryExitCHStartTimeout)
	defer cancel()

	conn := startQueryExitCH(ctx, t)

	if err := conn.Exec(ctx, legacyCorpusDDL); err != nil {
		t.Fatalf("create the narrow corpus table: %v", err)
	}

	sink, err := NewCHTableSink(ctx, conn)
	if err != nil {
		t.Fatalf("build CH table sink over the narrow table: %v", err)
	}

	// Every reconciled column, not just the two names spelled out below: a third
	// entry added to reconciledEnumColumns is covered here the moment it is added.
	for _, col := range reconciledEnumColumns {
		assertDeployedEnumHoldsMembers(ctx, t, conn, col)
	}
	// The legacy DDL above also predates the route-B fan-out columns, so the same
	// construction had to ADD them. That half decides whether the positional
	// batch below lands at all: clickhouse-go appends by position against the
	// deployed column list, so a column the ALTER never added does not degrade
	// one field, it invalidates every batch.
	assertDeployedColumnsPresent(ctx, t, conn)

	// One row carrying the new member of BOTH enum columns — the unclassified
	// route and the exit status a pre-widening table could not represent — plus
	// non-zero values in the two added columns, so the row exercises both halves
	// of the reconciliation at once.
	const wantShardsObserved, wantParallelism = 5, 3
	if err := sink.Write([]Row{{
		ShapeID:        "cerb:vector_selector",
		Language:       "logql",
		Route:          RouteUnclassified,
		ExitStatus:     ExitAborted.String(),
		KShards:        8,
		ShardsObserved: wantShardsObserved,
		Parallelism:    wantParallelism,
	}}); err != nil {
		t.Fatalf("write a row carrying route %q and exit status %q: %v", RouteUnclassified, ExitAborted, err)
	}

	var landedRoute, landedExit string
	var landedShards, landedParallelism uint8
	if err := conn.QueryRow(
		ctx,
		"SELECT toString(route), toString(exit_status), shards_observed, parallelism FROM "+CorpusTableName,
	).Scan(&landedRoute, &landedExit, &landedShards, &landedParallelism); err != nil {
		t.Fatalf("read the written row back: %v", err)
	}
	if landedRoute != RouteUnclassified {
		t.Errorf("route read back as %q; want the unclassified member %q — a fold into 'A' is the bug this guards",
			landedRoute, RouteUnclassified)
	}
	if landedExit != ExitAborted.String() {
		t.Errorf("exit_status read back as %q; want %q", landedExit, ExitAborted)
	}
	// Reading the ADDED columns back by name is what proves the positional
	// append bound each value to the column it was meant for: a batch that
	// silently shifted by one would still succeed, and only the values give it
	// away.
	if landedShards != wantShardsObserved {
		t.Errorf("shards_observed read back as %d; want %d", landedShards, wantShardsObserved)
	}
	if landedParallelism != wantParallelism {
		t.Errorf("parallelism read back as %d; want %d", landedParallelism, wantParallelism)
	}
}

// assertDeployedColumnsPresent reads the table's column list off the server and
// checks it carries every column this binary writes.
func assertDeployedColumnsPresent(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()

	rows, err := conn.Query(
		ctx,
		"SELECT name FROM system.columns WHERE database = currentDatabase() AND table = ?",
		CorpusTableName,
	)
	if err != nil {
		t.Fatalf("read the deployed column list: %v", err)
	}
	defer func() { _ = rows.Close() }()

	deployed := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan the deployed column list: %v", err)
		}
		deployed[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the deployed column list: %v", err)
	}
	for _, c := range CorpusColumns() {
		if !deployed[c.Name] {
			t.Errorf("deployed table does not carry column %q", c.Name)
		}
	}
}

// assertDeployedEnumHoldsMembers reads the column's type off the server — not off
// the ALTER's return — and checks it holds every member this binary can write.
func assertDeployedEnumHoldsMembers(ctx context.Context, t *testing.T, conn driver.Conn, col reconciledEnumColumn) {
	t.Helper()

	var deployed string
	if err := conn.QueryRow(
		ctx,
		"SELECT type FROM system.columns WHERE database = currentDatabase() AND table = ? AND name = ?",
		CorpusTableName, col.name,
	).Scan(&deployed); err != nil {
		t.Fatalf("read the deployed %s type: %v", col.name, err)
	}
	for _, m := range col.members() {
		if !strings.Contains(deployed, "'"+m.Name+"'") {
			t.Errorf("deployed %s type %q does not hold member %q", col.name, deployed, m.Name)
		}
	}
}
