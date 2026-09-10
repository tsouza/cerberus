package optcorpus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
)

// clusterName is the ClickHouse cluster the ON CLUSTER tests below configure —
// the bundled chart's own `<cluster>` name, which is what a real
// CERBERUS_CH_DATA_SHARDS > 1 deployment carries in CERBERUS_SCHEMA_CLUSTER.
const clusterName = "bwc_cluster"

// onClusterClause is the fragment every statement must carry when a cluster is
// configured — and must NOT carry when one is not.
const onClusterClause = "ON CLUSTER `" + clusterName + "`"

// classicReplicatedTableEngine is the CERBERUS_SCHEMA_TABLE_ENGINE value a
// classic ON CLUSTER deployment carries — verbatim the expression the bundled
// chart renders for the SIGNAL tables under dataShards.count > 1, and the
// shape internal/config's own knob doc names. Cerberus reads it only as a
// declaration that this deployment replicates; the tests below pin that none of
// it reaches the corpus DDL.
const classicReplicatedTableEngine = "ReplicatedMergeTree('/clickhouse/tables/{shard}/{database}/{table}', '{replica}')"

// nonReplicatingTableEngine is the counterpart an operator pins on a classic
// cluster with nothing to replicate to (multi-shard, single-replica): a cluster
// name is set, but the tables do not replicate and the corpus must not either.
const nonReplicatingTableEngine = "MergeTree()"

// TestCorpusDDL_OnCluster pins the whole reconciliation's cluster-awareness on
// BOTH sides of the switch: with a cluster configured EVERY statement
// construction issues carries `ON CLUSTER`, and with none configured NOT ONE of
// them does.
//
// Both halves matter, and neither is redundant. Without the first, the DDL is
// executed only on whichever node served the connection: under
// CERBERUS_CH_DATA_SHARDS > 1 the `otel` database is necessarily Atomic (a
// Replicated database engine and an ON CLUSTER cluster are mutually exclusive),
// so nothing propagates and every INSERT that later lands on one of the other
// nodes fails with "Table otel.cerberus_router_corpus does not exist" —
// cerberus issue #3225. Without the second, a fix could satisfy the first by
// stamping ON CLUSTER unconditionally, which would break every single-node and
// Replicated-database deployment (there is no cluster to name).
//
// It asserts over the statements the SINK ACTUALLY EXECUTES rather than over
// the three renderers one by one, because the failure this pins is precisely a
// statement that was left un-threaded: a renderer-by-renderer test passes while
// the caller still hands one of them the wrong argument.
func TestCorpusDDL_OnCluster(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		cluster string
		want    bool
	}{
		{name: "sharded", cluster: clusterName, want: true},
		{name: "single-node", cluster: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fe := &fakeExecer{}
			if _, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{Cluster: tc.cluster}); err != nil {
				t.Fatalf("NewCHTableSink(cluster=%q): %v", tc.cluster, err)
			}
			// One CREATE, one ADD COLUMN per corpus column, one MODIFY
			// COLUMN per reconciled enum column: anything less means a
			// statement was dropped rather than merely un-threaded.
			if want := 1 + len(CorpusColumns()) + len(reconciledEnumColumns); len(fe.execSQL) != want {
				t.Fatalf("construction ran %d statements, want %d: %q", len(fe.execSQL), want, fe.execSQL)
			}
			for _, sql := range fe.execSQL {
				if got := strings.Contains(sql, onClusterClause); got != tc.want {
					t.Errorf("statement carries %s = %v, want %v:\n%s", onClusterClause, got, tc.want, sql)
				}
			}
		})
	}
}

// TestCorpusCreateTableSQL_OnClusterPosition pins WHERE the clause lands.
// ClickHouse accepts `ON CLUSTER` only between the table name and the column
// list; a clause rendered anywhere else is a syntax error the fake executer
// above would never notice, since it parses nothing.
func TestCorpusCreateTableSQL_OnClusterPosition(t *testing.T) {
	t.Parallel()

	sql := corpusCreateTableSQL(CorpusTableTopology{Cluster: clusterName})
	if want := "CREATE TABLE IF NOT EXISTS " + CorpusTableName + " " + onClusterClause + " ("; !strings.Contains(sql, want) {
		t.Errorf("DDL missing %q\nfull SQL:\n%s", want, sql)
	}
}

// TestCorpusCreateTableSQL_Shape pins the rendered MergeTree DDL against the
// dossier schema. The typed chsql builder produces it; this test is the golden
// that catches a column / type / engine / order-by / TTL drift.
func TestCorpusCreateTableSQL_Shape(t *testing.T) {
	t.Parallel()
	sql := corpusCreateTableSQL(CorpusTableTopology{})

	wantFragments := []string{
		"CREATE TABLE IF NOT EXISTS cerberus_router_corpus (",
		"`event_time` DateTime",
		"`shape_id` LowCardinality(String)",
		"`language` LowCardinality(String)",
		"`normalized_query_hash` UInt64",
		"`n_anchors` UInt32",
		"`fanout` UInt32",
		"`cumulative_d` UInt32",
		"`outer_range` UInt32",
		"`step` UInt32",
		"`route` Enum8('A' = 0, 'B' = 1, '' = 2)",
		"`k_shards` UInt8",
		"`decision_reason` LowCardinality(String)",
		"`read_rows` UInt64",
		"`read_bytes` UInt64",
		"`query_duration_ms` UInt64",
		"`memory_usage` UInt64",
		"`exit_status` Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
			"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5, " +
			"'aborted' = 6, 'error' = 7, 'byte_budget' = 8)",
		"`shards_observed` UInt8",
		"`parallelism` UInt8",
		"ENGINE = MergeTree",
		"ORDER BY (`shape_id`, `n_anchors`, `fanout`)",
		"TTL toDateTime(event_time) + toIntervalDay(30)",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(sql, frag) {
			t.Errorf("DDL missing %q\nfull SQL:\n%s", frag, sql)
		}
	}
}

// TestCorpusCreateTableSQL_EngineFollowsDeploymentReplication pins the engine
// on EVERY side of the switch cerberus issues #3241 and #3250 are about.
//
// ON CLUSTER (pinned above) decides where the TABLE exists; the engine decides
// where the ROWS live, and only a Replicated* engine replicates them. Nothing
// converts a plain MergeTree — neither a Replicated DATABASE nor a cluster
// definition — so a plain-MergeTree corpus table on a replicating deployment
// leaves every replica holding only what was written through it, and
// internal/routerrules' ordinary single-node SELECT then mines one replica's
// slice as if it were the whole corpus.
//
// The four cases are the four real topologies, and each rejects the mistake it
// invites:
//
//   - A Replicated database (#3241) and a classic ON CLUSTER cluster whose
//     operator pinned a replicating engine (#3250) BOTH need a replicating
//     corpus table. Both get the BARE ReplicatedMergeTree, and the classic case
//     rejects the operator's own expression appearing in the DDL — the corpus
//     must not inherit another table's Keeper coordinates or engine family.
//   - A single-node deployment and a classic cluster with a NON-replicating
//     engine (a multi-shard, single-replica deployment, which has nothing to
//     replicate to and need run no Keeper) must NOT get one: emitting
//     ReplicatedMergeTree there fails at CREATE.
func TestCorpusCreateTableSQL_EngineFollowsDeploymentReplication(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		topology CorpusTableTopology
		want     string
		reject   string
	}{
		{
			name:     "replicated-database",
			topology: CorpusTableTopology{DatabaseReplicated: true},
			want:     "ENGINE = ReplicatedMergeTree",
			// The bare form is what a Replicated database requires: it
			// supplies the Keeper coordinates itself and rejects explicit
			// engine arguments with code 36.
			reject: "ENGINE = ReplicatedMergeTree(",
		},
		{
			name: "classic-cluster-with-replicating-engine",
			topology: CorpusTableTopology{
				Cluster:     clusterName,
				TableEngine: classicReplicatedTableEngine,
			},
			want: "ENGINE = ReplicatedMergeTree",
			// The operator's expression is a DECLARATION, never DDL: its
			// Keeper path belongs to the SIGNAL tables, and its family was
			// chosen for them. The corpus emits its own bare engine, whose
			// path the server derives per table from default_replica_path.
			reject: "ENGINE = ReplicatedMergeTree(",
		},
		{
			name:     "single-node",
			topology: CorpusTableTopology{},
			want:     "ENGINE = MergeTree",
			reject:   "ENGINE = ReplicatedMergeTree",
		},
		{
			name: "classic-cluster-without-replication",
			topology: CorpusTableTopology{
				Cluster:     clusterName,
				TableEngine: nonReplicatingTableEngine,
			},
			want:   "ENGINE = MergeTree",
			reject: "ENGINE = ReplicatedMergeTree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql := corpusCreateTableSQL(tc.topology)
			if !strings.Contains(sql, tc.want) {
				t.Errorf("DDL missing %q\nfull SQL:\n%s", tc.want, sql)
			}
			if strings.Contains(sql, tc.reject) {
				t.Errorf("DDL carries %q, which this topology must not emit\nfull SQL:\n%s", tc.reject, sql)
			}
		})
	}
}

// TestEngineReplicates pins the predicate that reads CERBERUS_SCHEMA_TABLE_ENGINE
// as a DECLARATION — the whole of what cerberus asks of an operator-supplied
// engine expression (cerberus issue #3250).
//
// Two properties are load-bearing and neither is obvious:
//
//   - It is case-SENSITIVE. ClickHouse engine names are, so a
//     `replicatedMergeTree` accepted here would make cerberus emit a
//     ReplicatedMergeTree corpus table on a deployment whose own signal-table
//     CREATE the server rejects — reading a typo as a topology.
//   - It matches the FAMILY, not one engine. A ReplicatedReplacingMergeTree
//     pinned for the signal tables still says "this deployment replicates", and
//     the corpus still needs a replicating engine; what it must not do is
//     inherit that engine, which corpusTableEngine's own test pins.
func TestEngineReplicates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		engine string
		want   bool
	}{
		{name: "empty-is-the-plain-mergetree-default", engine: ""},
		{name: "plain-mergetree", engine: nonReplicatingTableEngine},
		{name: "replicated-mergetree-with-coordinates", engine: classicReplicatedTableEngine, want: true},
		{name: "bare-replicated-mergetree", engine: "ReplicatedMergeTree", want: true},
		{name: "another-replicated-family-member", engine: "ReplicatedReplacingMergeTree('/p', '{replica}')", want: true},
		{name: "leading-whitespace-from-yaml", engine: "  ReplicatedMergeTree", want: true},
		{name: "wrong-case-is-not-an-engine-clickhouse-accepts", engine: "replicatedMergeTree('/p', '{replica}')"},
		{name: "replacing-is-not-replicating", engine: "ReplacingMergeTree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := engineReplicates(tc.engine); got != tc.want {
				t.Errorf("engineReplicates(%q) = %v; want %v", tc.engine, got, tc.want)
			}
		})
	}
}

// TestNewCHTableSink_EngineReadFailureIsFatal pins that an unreadable
// deployed-engine read fails construction rather than being read as "no
// engine".
//
// All three are real driver outcomes and all three are silent by default: a
// query the server refuses, a Scan that fails mid-row, and an error the driver
// only reports after iteration. If any were swallowed, readDeployedEngine would
// hand back the zero string,
// verifyTableEngine would read that as a non-replicating engine, and the sink
// would be refused on a deployment whose table is perfectly fine — or, on a
// non-replicated deployment, the unread failure would simply vanish. Neither is
// an answer about the server; only an error is.
func TestNewCHTableSink_EngineReadFailureIsFatal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fe   *fakeExecer
	}{
		{name: "query-fails", fe: &fakeExecer{engineQueryErr: errors.New("query boom")}},
		{name: "scan-fails", fe: &fakeExecer{engineScanErr: errors.New("scan boom")}},
		{name: "iteration-reports-an-error", fe: &fakeExecer{engineRowsErr: errors.New("rows boom")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewCHTableSink(context.Background(), tc.fe, CorpusTableTopology{})
			if err == nil {
				t.Fatal("NewCHTableSink over an unreadable deployed engine: want an error, got nil")
			}
			if !strings.Contains(err.Error(), "engine") {
				t.Errorf("error %q does not say the ENGINE read is what failed", err)
			}
		})
	}
}

// TestNewCHTableSink_RejectsNonReplicatingDeployedEngine pins the migration half
// of cerberus issues #3241 and #3250: emitting the right engine only fixes a
// table this binary CREATES, and `CREATE TABLE IF NOT EXISTS` is a no-op against
// a table an older binary already left behind as a plain MergeTree. No ALTER
// converts one, so the only thing that can distinguish "replicating" from "not"
// is the engine the SERVER reports — and if construction accepted it anyway, the
// corpus would go on being mined one replica at a time with nothing saying so.
//
// Both replicating topologies are covered on both sides — a Replicated database
// (#3241) and a classic ON CLUSTER cluster whose operator declared a replicating
// engine (#3250) each FAIL over a deployed plain MergeTree and are BUILT over a
// deployed replicating one. The two non-replicating topologies — single-node,
// and a classic cluster with nothing to replicate to — are untouched by the
// check, where a plain MergeTree is correct and there may be no Keeper at all.
//
// The one cell deliberately left unchecked is a REPLICATING deployed engine on a
// non-replicating deployment: replicating more than the deployment asked for
// costs correctness nothing, and refusing it would brick a deployment that
// turned replication off after the table was made.
//
// The message is asserted per topology, not generically. An operator can only
// act on the knob they actually set, and the DROP they are handed has to reach
// every node it must: on a classic cluster that means carrying ON CLUSTER, or
// the remedy repairs one node out of N and the next start fails the same way.
func TestNewCHTableSink_RejectsNonReplicatingDeployedEngine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		topology  CorpusTableTopology
		deployed  string
		wantErr   bool
		wantInErr []string
	}{
		{
			name:     "replicated-database-over-plain-mergetree",
			topology: CorpusTableTopology{DatabaseReplicated: true},
			deployed: "MergeTree",
			wantErr:  true,
			wantInErr: []string{
				"CERBERUS_SCHEMA_DATABASE_REPLICATED",
				"DROP TABLE " + CorpusTableName,
			},
		},
		{
			name:     "replicated-database-over-replicated-mergetree",
			topology: CorpusTableTopology{DatabaseReplicated: true},
			deployed: "ReplicatedMergeTree",
		},
		{
			name: "classic-cluster-over-plain-mergetree",
			topology: CorpusTableTopology{
				Cluster:     clusterName,
				TableEngine: classicReplicatedTableEngine,
			},
			deployed: "MergeTree",
			wantErr:  true,
			wantInErr: []string{
				"CERBERUS_SCHEMA_TABLE_ENGINE",
				"DROP TABLE " + CorpusTableName + " " + onClusterClause,
			},
		},
		{
			name: "classic-cluster-over-replicated-mergetree",
			topology: CorpusTableTopology{
				Cluster:     clusterName,
				TableEngine: classicReplicatedTableEngine,
			},
			deployed: "ReplicatedMergeTree",
		},
		{
			name:     "single-node-deployment-over-plain-mergetree",
			topology: CorpusTableTopology{},
			deployed: "MergeTree",
		},
		{
			name: "classic-cluster-without-replication-over-plain-mergetree",
			topology: CorpusTableTopology{
				Cluster:     clusterName,
				TableEngine: nonReplicatingTableEngine,
			},
			deployed: "MergeTree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fe := &fakeExecer{deployedEngine: tc.deployed}
			sink, err := NewCHTableSink(context.Background(), fe, tc.topology)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("NewCHTableSink over a deployed %s engine: %v", tc.deployed, err)
				}
				if sink == nil {
					t.Fatal("NewCHTableSink returned no sink and no error")
				}
				return
			}
			if err == nil {
				t.Fatalf("NewCHTableSink over a deployed %s engine on a replicating deployment: "+
					"want an error, got nil", tc.deployed)
			}
			// The operator has to act on this, so the message has to name the
			// table, the engine the server reported, the knob that made it
			// wrong, and the exact statement that repairs it — not merely that
			// something did not line up.
			for _, want := range append([]string{CorpusTableName, tc.deployed}, tc.wantInErr...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// fakeExecer records every statement executed and the batch rows appended, and
// answers the deployed-schema read from deployedType / absentColumn — keyed by
// column name, defaulting to the full column list with the types this binary
// writes, so construction succeeds unless a test says otherwise.
// failStatement / failErr make ONE statement fail while the rest succeed, which
// is how a least-privilege deployment presents (a CH user may hold CREATE but
// not ALTER); execErr fails every statement. The two reads are failed
// SEPARATELY — engineQueryErr for the system.tables engine read, queryErr for
// the system.columns schema read — because they run one after the other, so a
// single shared error would only ever pin whichever runs first and would leave
// the other read's fatal path with no coverage at all.
type fakeExecer struct {
	execSQL        []string
	execErr        error
	failStatement  string
	failErr        error
	batchErr       error
	batch          *fakeBatch
	deployedType   map[string]string
	absentColumn   map[string]bool
	deployedEngine string
	engineScanErr  error
	engineRowsErr  error
	engineQueryErr error
	queryErr       error
}

// systemTablesRelation is the relation the ENGINE read names, rendered the same
// way corpusEngineQuery renders it, so the fake dispatches on the production
// statement rather than on a hand-typed copy of it.
var systemTablesRelation = chsql.RenderDDL(chsql.Qual("system", "tables"))

// isEngineQuery reports whether query is the deployed-ENGINE read rather than
// the deployed-SCHEMA read. Construction issues both against the same table
// name, so the bound argument cannot tell them apart — the relation can.
func isEngineQuery(query string) bool { return strings.Contains(query, systemTablesRelation) }

func (f *fakeExecer) Exec(_ context.Context, query string, _ ...any) error {
	f.execSQL = append(f.execSQL, query)
	if f.failStatement != "" && strings.Contains(query, f.failStatement) {
		return f.failErr
	}
	return f.execErr
}

// Query answers the deployed-schema read (see corpusSchemaQuery) with one
// (name, type) row per corpus column. Each type defaults to what this binary
// writes, so construction succeeds unless a test overrides one via deployedType
// (which is what lets a test narrow exit_status while leaving route wide) or
// removes one via absentColumn (the shape of a table an older binary created).
//
// The table name is the query's last bound argument; checking it keeps the fake
// honest about WHICH table it is answering for.
func (f *fakeExecer) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	if isEngineQuery(query) {
		if f.engineQueryErr != nil {
			return nil, f.engineQueryErr
		}
	} else if f.queryErr != nil {
		return nil, f.queryErr
	}
	if len(args) == 0 {
		return nil, errors.New("fakeExecer: schema query bound no arguments")
	}
	table, ok := args[len(args)-1].(string)
	if !ok {
		return nil, errors.New("fakeExecer: schema query's last argument is not a table name")
	}
	if table != CorpusTableName {
		return nil, errors.New("fakeExecer: schema query asked about table " + table)
	}
	if isEngineQuery(query) {
		engine := f.deployedEngine
		if engine == "" {
			engine = chsql.RenderDDL(corpusTableEngine(CorpusTableTopology{}))
		}
		return &fakeRows{
			cols:     []string{"engine"},
			rows:     [][]string{{engine}},
			scanErr:  f.engineScanErr,
			finalErr: f.engineRowsErr,
		}, nil
	}
	rows := &fakeRows{cols: []string{"name", "type"}}
	for _, c := range CorpusColumns() {
		if f.absentColumn[c.Name] {
			continue
		}
		deployed, ok := f.deployedType[c.Name]
		if !ok {
			deployed = chsql.RenderDDL(c.Type)
		}
		rows.rows = append(rows.rows, []string{c.Name, deployed})
	}
	return rows, nil
}

// executed reports whether any recorded statement contains want.
func (f *fakeExecer) executed(want string) bool {
	for _, sql := range f.execSQL {
		if strings.Contains(sql, want) {
			return true
		}
	}
	return false
}

// fakeRows is an all-String driver.Rows over a fixed column list: exactly the
// shape both construction reads consume — the (name, type) pairs of the
// deployed-schema read and the single engine name of the engine read.
type fakeRows struct {
	cols []string
	rows [][]string
	next int
	// scanErr / finalErr make the row set fail mid-iteration and after it —
	// the two ways a real driver reports a read that started fine and did not
	// finish. Both are silent unless a test sets them.
	scanErr  error
	finalErr error
}

func (r *fakeRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != len(r.cols) {
		return errors.New("fakeRows: want exactly one scan destination per column")
	}
	if r.next == 0 || r.next > len(r.rows) {
		return errors.New("fakeRows: Scan called outside a row")
	}
	row := r.rows[r.next-1]
	for i, d := range dest {
		p, ok := d.(*string)
		if !ok {
			return errors.New("fakeRows: want *string scan destinations")
		}
		*p = row[i]
	}
	return nil
}

func (r *fakeRows) HasData() bool                    { return r.next < len(r.rows) }
func (r *fakeRows) ScanStruct(any) error             { return nil }
func (r *fakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeRows) Totals(...any) error              { return nil }
func (r *fakeRows) Columns() []string                { return r.cols }
func (r *fakeRows) Close() error                     { return nil }
func (r *fakeRows) Err() error                       { return r.finalErr }

func (f *fakeExecer) PrepareBatch(_ context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	f.batch = &fakeBatch{}
	return f.batch, nil
}

// fakeBatch records appended rows; it implements driver.Batch with the methods
// the sink uses (Append / Send) and no-ops the rest.
type fakeBatch struct {
	rows [][]any
	sent bool
}

func (b *fakeBatch) Append(v ...any) error         { b.rows = append(b.rows, v); return nil }
func (b *fakeBatch) AppendStruct(any) error        { return nil }
func (b *fakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeBatch) Flush() error                  { return nil }
func (b *fakeBatch) Send() error                   { b.sent = true; return nil }
func (b *fakeBatch) Abort() error                  { return nil }
func (b *fakeBatch) IsSent() bool                  { return b.sent }
func (b *fakeBatch) Rows() int                     { return len(b.rows) }
func (b *fakeBatch) Columns() []column.Interface   { return nil }
func (b *fakeBatch) Close() error                  { return nil }

// TestCHTableSink_CreatesTableAndWrites pins the CH-table sink end to end on a
// fake conn: construction runs the CREATE TABLE DDL, and Write streams the Row
// through the columnar batch in the corpus column order.
func TestCHTableSink_CreatesTableAndWrites(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{}
	sink, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err != nil {
		t.Fatalf("NewCHTableSink: %v", err)
	}
	if !fe.executed("CREATE TABLE IF NOT EXISTS cerberus_router_corpus") {
		t.Fatalf("construction did not run the corpus DDL; got %q", fe.execSQL)
	}
	// Every reconciled column is widened, not just the first: a column left out
	// of the loop keeps whatever narrow type a pre-existing deployment declared,
	// and the member this binary added is rejected on every batch forever.
	for _, col := range reconciledEnumColumns {
		if !fe.executed(alterMarker(col.name)) {
			t.Fatalf("construction did not reconcile the %s column; got %q", col.name, fe.execSQL)
		}
	}
	// EVERY column is also offered to the deployed table, not just the ones added
	// most recently: CREATE IF NOT EXISTS is a no-op on an existing table, so a
	// column omitted from this loop never reaches a deployment that predates it,
	// and the positional batch then binds every later value to the wrong column.
	for _, col := range CorpusColumns() {
		if !fe.executed(addMarker(col.Name)) {
			t.Fatalf("construction did not offer the %s column; got %q", col.Name, fe.execSQL)
		}
	}

	row := Row{
		ShapeID:        "cerb:agg",
		Language:       "promql",
		NAnchors:       241,
		Fanout:         20,
		CumulativeD:    300,
		OuterRange:     3600,
		Step:           15,
		Route:          "B",
		KShards:        8,
		DecisionReason: "routed",
		ReadRows:       1000,
		MemoryUsage:    2048,
		ExitStatus:     "oom",
		ShardsObserved: 3,
		Parallelism:    3,
	}
	if err := sink.Write([]Row{row}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fe.batch == nil || !fe.batch.sent {
		t.Fatal("batch not sent")
	}
	if len(fe.batch.rows) != 1 {
		t.Fatalf("appended %d rows; want 1", len(fe.batch.rows))
	}
	got := fe.batch.rows[0]
	// Append is POSITIONAL against the deployed column order, so the batch must
	// carry one value per corpus column, no more and no fewer.
	if len(got) != len(CorpusColumns()) {
		t.Fatalf("appended %d columns; want %d", len(got), len(CorpusColumns()))
	}
	// The values are checked BY COLUMN NAME rather than by a hardcoded index, so
	// appending a column to CorpusColumns() cannot silently re-point an
	// assertion at its neighbour.
	wantValues := map[string]any{
		"language":        "promql",
		"route":           int8(1), // B
		"k_shards":        uint8(8),
		"exit_status":     int8(1), // oom
		"shards_observed": uint8(3),
		"parallelism":     uint8(3),
	}
	for name, want := range wantValues {
		i := corpusColumnIndex(t, name)
		if got[i] != want {
			t.Errorf("col[%d] %s = %#v, want %#v", i, name, got[i], want)
		}
	}
}

// corpusColumnIndex resolves a corpus column's position in the batch's
// positional Append order.
func corpusColumnIndex(t *testing.T, name string) int {
	t.Helper()
	for i, c := range CorpusColumns() {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("corpus column %q does not exist", name)
	return -1
}

// TestRouteEnumValue / TestExitEnumValue pin the string→Enum8 mappings.
//
// The unclassified cases are the load-bearing ones. An unclassified row must NOT
// land on 'A': the rule engine reads `route = 'A'` as "the solver classified this
// query and the cost thresholds declined to shard it", and every LogQL / TraceQL
// row is unclassified, so folding them into 'A' enrols both non-PromQL heads in
// every route-A population and disagrees with the JSONL sink, which writes the
// token verbatim.
func TestRouteEnumValue(t *testing.T) {
	t.Parallel()
	if routeEnumValue("A") != 0 || routeEnumValue("B") != 1 {
		t.Error("classified route enum mapping wrong")
	}
	if got := routeEnumValue(RouteUnclassified); got != routeUnclassifiedValue {
		t.Errorf("routeEnumValue(unclassified) = %d, want %d", got, routeUnclassifiedValue)
	}
	// A token outside the member set is an unrecognised classifier read-out, not
	// evidence the query took route A.
	if got := routeEnumValue("Z"); got != routeUnclassifiedValue {
		t.Errorf("routeEnumValue(unknown token) = %d, want %d (unclassified)", got, routeUnclassifiedValue)
	}
	// The mapping must agree with the column type for EVERY member, so a member
	// added to routeEnumMembers without a mapping cannot slip through.
	for _, m := range routeEnumMembers() {
		if got := routeEnumValue(m.Name); got != m.Value {
			t.Errorf("routeEnumValue(%q) = %d, but the column declares it as %d", m.Name, got, m.Value)
		}
	}
}

func TestExitEnumValue(t *testing.T) {
	t.Parallel()
	if exitEnumValue("ok") != 0 || exitEnumValue("oom") != 1 || exitEnumValue("timeout") != 2 || exitEnumValue("") != 0 {
		t.Error("exit enum mapping wrong")
	}
	// Cerberus-side outcomes must map to their DDL Enum8 values, in lockstep
	// with the ExitStatus iota and the corpusCreateTableSQL Enum8.
	if exitEnumValue("sample_budget") != 3 || exitEnumValue("breaker") != 4 || exitEnumValue("rejected") != 5 {
		t.Error("cerberus-side exit enum mapping wrong")
	}
	// The ClickHouse-side abort / error classes take the next two values.
	if exitEnumValue("aborted") != 6 || exitEnumValue("error") != 7 {
		t.Error("clickhouse-side exit enum mapping wrong")
	}
	// byte_budget was APPENDED to the iota rather than slotted beside its
	// sample_budget sibling, so it takes the next free value and every status
	// above keeps the value already deployed in the column. Pinning the literal
	// here is what makes an accidental re-slotting — which would silently
	// relabel every historical row from sample_budget upwards — a test failure.
	if exitEnumValue("byte_budget") != 8 {
		t.Error("byte_budget must append to the enum, not renumber its siblings")
	}
	// The enum value must round-trip from ExitStatus.String() through
	// exitEnumValue for EVERY status, so a member added to the iota without a
	// mapping cannot slip through.
	for _, status := range exitStatuses {
		if got := exitEnumValue(status.String()); got != int8(status) {
			t.Errorf("exitEnumValue(%q) = %d, want %d", status.String(), got, int8(status))
		}
	}
	if len(exitStatuses) != 9 {
		t.Fatalf("exitStatuses covers %d statuses; the round-trip above must cover all 9", len(exitStatuses))
	}
}

// TestEnum8Members_ParsesDeployedType pins the deployed-type parser against the
// exact rendering ClickHouse returns from system.columns (spaces around `=`),
// including an escaped quote inside a member name.
func TestEnum8Members_ParsesDeployedType(t *testing.T) {
	t.Parallel()
	got := enum8Members(`Enum8('ok' = 0, 'o\'q' = 1, 'error' = 7)`)
	want := []string{"ok", "o'q", "error"}
	if len(got) != len(want) {
		t.Fatalf("parsed %d members (%v); want %d", len(got), got, len(want))
	}
	for _, member := range want {
		if _, ok := got[member]; !ok {
			t.Errorf("member %q missing from %v", member, got)
		}
	}
}

// narrowExitStatusType / narrowRouteType are deployed column types that predate a
// member this binary emits: exit_status without the two ClickHouse-side outcome
// classes, and route without the unclassified member. They are what a corpus
// table created by an older binary actually looks like.
const (
	narrowExitStatusType = "Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
		"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5)"
	narrowRouteType = "Enum8('A' = 0, 'B' = 1)"
)

// TestNewCHTableSink_RejectsNarrowDeployedColumn pins that construction FAILS
// when a deployed enum column cannot hold a member this binary emits, naming it.
// Without this the sink would return healthy and every batch carrying that
// member would be rejected on every reconcile interval.
//
// Both reconciled columns are covered, because the verify loop is only as good
// as its narrowest link: a route column still declared Enum8('A','B') rejects
// every unclassified row — which is every LogQL and TraceQL query — so a sink
// that reports healthy over one would silently drop the majority of the corpus.
func TestNewCHTableSink_RejectsNarrowDeployedColumn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		column   string
		deployed string
		missing  []string
	}{
		{
			column:   exitStatusColumn,
			deployed: narrowExitStatusType,
			missing:  []string{ExitAborted.String(), ExitError.String()},
		},
		{
			column:   routeColumn,
			deployed: narrowRouteType,
			missing:  []string{RouteUnclassified},
		},
	} {
		t.Run(tc.column, func(t *testing.T) {
			t.Parallel()

			fe := &fakeExecer{deployedType: map[string]string{tc.column: tc.deployed}}
			_, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
			if err == nil {
				t.Fatalf("NewCHTableSink over a narrow %s column: want an error, got nil", tc.column)
			}
			if !strings.Contains(err.Error(), tc.column) {
				t.Errorf("error %q does not name the narrow column %q", err, tc.column)
			}
			for _, member := range tc.missing {
				if !strings.Contains(err.Error(), strconv.Quote(member)) {
					t.Errorf("error %q does not name the missing member %q", err, member)
				}
			}
		})
	}
}

// alterMarker is the fragment that identifies one column's widening among the
// statements construction runs.
func alterMarker(column string) string {
	return "MODIFY COLUMN IF EXISTS `" + column + "`"
}

// addMarker is the fragment that identifies one column's addition among the
// statements construction runs.
func addMarker(column string) string {
	return "ADD COLUMN IF NOT EXISTS `" + column + "`"
}

// TestNewCHTableSink_WideningIsBestEffort pins that a REFUSED widening does not
// by itself disable the sink.
//
// A CH user holding INSERT and CREATE but not ALTER is a routine least-
// privilege grant. On such a deployment the widening is refused on every start
// while the deployed column already holds every member — nothing is wrong, and
// failing construction there would turn the whole corpus off for a statement
// whose work was already done.
func TestNewCHTableSink_WideningIsBestEffort(t *testing.T) {
	t.Parallel()

	for _, col := range reconciledEnumColumns {
		t.Run(col.name, func(t *testing.T) {
			t.Parallel()

			fe := &fakeExecer{
				failStatement: alterMarker(col.name),
				failErr:       errors.New("not enough privileges"),
			}
			sink, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
			if err != nil {
				t.Fatalf("NewCHTableSink over an already-wide %s with no ALTER grant: %v", col.name, err)
			}
			if sink == nil {
				t.Fatal("NewCHTableSink returned no sink and no error")
			}
			if !fe.executed(alterMarker(col.name)) {
				t.Errorf("construction never attempted the widening; got %q", fe.execSQL)
			}
		})
	}
}

// TestNewCHTableSink_NarrowColumnReportsWideningFailure pins the other half:
// when the column IS too narrow AND the widening was refused, the refusal is
// the operator's actionable cause and must appear in the error. The verify is
// still the authority on whether construction fails.
func TestNewCHTableSink_NarrowColumnReportsWideningFailure(t *testing.T) {
	t.Parallel()

	const grantErr = "not enough privileges"
	fe := &fakeExecer{
		deployedType:  map[string]string{exitStatusColumn: narrowExitStatusType},
		failStatement: alterMarker(exitStatusColumn),
		failErr:       errors.New(grantErr),
	}
	_, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err == nil {
		t.Fatal("NewCHTableSink over a narrow exit_status column: want an error, got nil")
	}
	if !strings.Contains(err.Error(), grantErr) {
		t.Errorf("error %q does not carry the widening failure %q", err, grantErr)
	}
	for _, member := range []string{ExitAborted.String(), ExitError.String()} {
		if !strings.Contains(err.Error(), member) {
			t.Errorf("error %q does not name the missing member %q", err, member)
		}
	}
}

// TestNewCHTableSink_SchemaReadFailureIsFatal pins that an unreadable deployed
// schema fails construction. The read is the whole reconciliation's evidence —
// without it nothing is known about what the table carries, and proceeding would
// mean writing batches into a schema the sink never checked.
func TestNewCHTableSink_SchemaReadFailureIsFatal(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{queryErr: errors.New("system.columns unavailable")}
	if _, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{}); err == nil {
		t.Fatal("NewCHTableSink with an unreadable schema: want an error, got nil")
	}
}

// TestNewCHTableSink_AddColumnIsBestEffort pins that a REFUSED column addition
// does not by itself disable the sink, for the same reason a refused widening
// does not: on a deployment whose CH user holds INSERT and CREATE but not ALTER,
// the ADD is refused on every start while the table already carries the column.
// The verify below — not the ALTER's exit status — is the authority.
func TestNewCHTableSink_AddColumnIsBestEffort(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{
		failStatement: addMarker(parallelismColumn),
		failErr:       errors.New("not enough privileges"),
	}
	sink, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err != nil {
		t.Fatalf("NewCHTableSink over an already-complete table with no ALTER grant: %v", err)
	}
	if sink == nil {
		t.Fatal("NewCHTableSink returned no sink and no error")
	}
	if !fe.executed(addMarker(parallelismColumn)) {
		t.Errorf("construction never attempted the addition; got %q", fe.execSQL)
	}
}

// TestNewCHTableSink_RejectsMissingColumn pins the other half: a table that
// really is missing a column this binary writes fails construction, naming the
// column and — when the ADD was refused — the refusal that left it missing.
//
// This is the shape a pre-existing deployment presents after the corpus gains a
// column: CREATE IF NOT EXISTS is a no-op against the old table, so without the
// ADD the column never arrives. Write appends POSITIONALLY, so the consequence
// is not one absent field — it is every later value bound to the wrong column,
// or a batch the table rejects, on every reconcile interval, forever.
func TestNewCHTableSink_RejectsMissingColumn(t *testing.T) {
	t.Parallel()

	const grantErr = "not enough privileges"
	fe := &fakeExecer{
		absentColumn:  map[string]bool{shardsObservedColumn: true},
		failStatement: addMarker(shardsObservedColumn),
		failErr:       errors.New(grantErr),
	}
	_, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err == nil {
		t.Fatalf("NewCHTableSink over a table missing %s: want an error, got nil", shardsObservedColumn)
	}
	if !strings.Contains(err.Error(), shardsObservedColumn) {
		t.Errorf("error %q does not name the missing column %q", err, shardsObservedColumn)
	}
	if !strings.Contains(err.Error(), grantErr) {
		t.Errorf("error %q does not carry the addition failure %q", err, grantErr)
	}
}

// TestNewCHTableSink_MissingColumnFailsWithoutAnAlterError pins that an absent
// column fails construction even when every ALTER reported success — the server
// is the authority on what the table carries, not the statements just run.
func TestNewCHTableSink_MissingColumnFailsWithoutAnAlterError(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{absentColumn: map[string]bool{parallelismColumn: true}}
	_, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err == nil {
		t.Fatalf("NewCHTableSink over a table missing %s: want an error, got nil", parallelismColumn)
	}
	if !strings.Contains(err.Error(), parallelismColumn) {
		t.Errorf("error %q does not name the missing column %q", err, parallelismColumn)
	}
}

// TestNewCHTableSink_CreateFailureIsFatal pins that the CREATE remains a hard
// failure: without a table there is nothing to verify and nowhere to write.
func TestNewCHTableSink_CreateFailureIsFatal(t *testing.T) {
	t.Parallel()

	fe := &fakeExecer{
		failStatement: "CREATE TABLE IF NOT EXISTS " + CorpusTableName,
		failErr:       errors.New("not enough privileges"),
	}
	if _, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{}); err == nil {
		t.Fatal("NewCHTableSink with a refused CREATE: want an error, got nil")
	}
}

// TestNewCHTableSink_RejectsRenumberedDeployedColumn pins that construction
// FAILS when the deployed exit_status column carries every expected member NAME
// but at different integers.
//
// Write appends this column as an int8, and clickhouse-go's Enum8.AppendRow
// keys the int path on the INTEGER — it never resolves a name. The transposition
// used here (aborted/error swapped) leaves both integers defined in the deployed
// type, so the append is ACCEPTED and every aborted row is stored as 'error' and
// vice versa: silent, permanent mislabelling of the two outcomes the corpus
// exists to distinguish. Renumber a member onto an integer the deployed type
// does not define instead and the same column rejects every batch with "unknown
// element N". A name-only membership check reports both shapes healthy.
//
// The shape is reachable whenever the table is operator-owned (the documented
// case) or was created by a binary that ordered the ExitStatus iota differently;
// the widening ALTER cannot repair it either, since ClickHouse refuses a MODIFY
// that changes an existing member's value.
func TestNewCHTableSink_RejectsRenumberedDeployedColumn(t *testing.T) {
	t.Parallel()

	// Same members as exitStatusEnumType, with aborted/error transposed.
	fe := &fakeExecer{deployedType: map[string]string{
		exitStatusColumn: "Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, " +
			"'sample_budget' = 3, 'breaker' = 4, 'rejected' = 5, 'error' = 6, 'aborted' = 7)",
	}}
	_, err := NewCHTableSink(context.Background(), fe, CorpusTableTopology{})
	if err == nil {
		t.Fatal("NewCHTableSink over a renumbered exit_status column: want an error, got nil")
	}
	for _, member := range []string{ExitAborted.String(), ExitError.String()} {
		if !strings.Contains(err.Error(), member) {
			t.Errorf("error %q does not name the misnumbered member %q", err, member)
		}
	}
}

// TestEnum8Members_ParsesValues pins that the deployed-type parse carries the
// integer each member is stored as, not just its name — the half the append
// path actually keys on.
func TestEnum8Members_ParsesValues(t *testing.T) {
	t.Parallel()

	got := enum8Members("Enum8('ok' = 0, 'oom' = 1, 'timeout' = -3)")
	want := map[string]int64{"ok": 0, "oom": 1, "timeout": -3}
	if len(got) != len(want) {
		t.Fatalf("parsed %v; want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("member %q parsed as %d; want %d", name, got[name], value)
		}
	}
}

// TestEnum8AppendKeysOnTheInteger pins the upstream driver behaviour the whole
// exit_status value check rests on: clickhouse-go's Enum8 column, given an int,
// validates and stores that INTEGER against the deployed type and resolves no
// name. Write appends this column as an int8, so the deployed integers are the
// contract — not the deployed names.
//
// If a clickhouse-go bump ever made the int path resolve through the name
// mapping instead, verifyEnumColumn's value comparison would be
// over-strict rather than load-bearing, and this test is what says so.
func TestEnum8AppendKeysOnTheInteger(t *testing.T) {
	t.Parallel()

	// The deployed type carries every expected NAME, with aborted/error
	// transposed relative to this binary's ExitStatus iota.
	deployed := column.Type("Enum8('ok' = 0, 'oom' = 1, 'timeout' = 2, 'sample_budget' = 3, " +
		"'breaker' = 4, 'rejected' = 5, 'error' = 6, 'aborted' = 7)")
	col, err := deployed.Column(exitStatusColumn, nil)
	if err != nil {
		t.Fatalf("Column(%s): %v", exitStatusColumn, err)
	}

	// What Write would append for an aborted row.
	if err := col.AppendRow(exitEnumValue(ExitAborted.String())); err != nil {
		t.Fatalf("AppendRow(%d) rejected by a type that defines that integer: %v — "+
			"the int path is no longer keyed on the integer", int8(ExitAborted), err)
	}
	if got := col.Row(0, false); got != ExitError.String() {
		t.Fatalf("appending the aborted integer stored %q; want %q — a by-name append "+
			"would have stored %q and made the value check unnecessary",
			got, ExitError.String(), ExitAborted.String())
	}

	// An integer the deployed type does not define is the rejecting shape.
	undefinedEnumValue := int8(len(exitStatuses) + 1)
	if err := col.AppendRow(undefinedEnumValue); err == nil {
		t.Fatalf("AppendRow(%d) accepted by a type that does not define it; want rejection",
			undefinedEnumValue)
	}
}
