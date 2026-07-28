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
// Only a real server distinguishes "the column already has this member" from
// "the ALTER widened it". This lane creates the narrow table BY HAND, then
// asserts sink construction widens it and a row carrying a new member lands
// and reads back under its own name.
//
// Gated by the `integration` build tag (Docker required); run by
// `just router-corpus-integration`.
package optcorpus

import (
	"context"
	"strings"
	"testing"
)

// legacyCorpusDDL creates the corpus table with an exit_status Enum8 holding
// only the members a binary without the ClickHouse abort/error classes could
// produce. It is the deployed-table-lags-binary starting state.
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
// a deployed exit_status column with the member set the binary can write, and
// that a row carrying a member the deployed table lacked lands correctly once
// it has.
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

	var deployed string
	if err := conn.QueryRow(
		ctx,
		"SELECT type FROM system.columns WHERE database = currentDatabase() AND table = ? AND name = 'exit_status'",
		CorpusTableName,
	).Scan(&deployed); err != nil {
		t.Fatalf("read the deployed exit_status type: %v", err)
	}
	for _, member := range ExitStatusTokens() {
		if !strings.Contains(deployed, "'"+member+"'") {
			t.Errorf("deployed exit_status type %q does not hold member %q", deployed, member)
		}
	}

	if err := sink.Write([]Row{{
		ShapeID:    "cerb:vector_selector",
		Language:   "promql",
		Route:      "A",
		ExitStatus: ExitAborted.String(),
	}}); err != nil {
		t.Fatalf("write a row carrying the %q exit status: %v", ExitAborted, err)
	}

	var landed string
	if err := conn.QueryRow(
		ctx,
		"SELECT toString(exit_status) FROM "+CorpusTableName,
	).Scan(&landed); err != nil {
		t.Fatalf("read the written exit_status back: %v", err)
	}
	if landed != ExitAborted.String() {
		t.Errorf("exit_status read back as %q; want %q", landed, ExitAborted)
	}
}
