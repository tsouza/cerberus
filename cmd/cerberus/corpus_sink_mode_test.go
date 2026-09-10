package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/optcorpus"
)

// errCorpusSinkDDL is the failure a CH-table sink construction hits on a
// deployment whose CH user cannot run the corpus DDL.
var errCorpusSinkDDL = errors.New("not enough privileges")

// failingCorpusConn is an optcorpus.CHTableConn whose every statement fails,
// standing in for the least-privilege deployment: the sink cannot be built.
type failingCorpusConn struct{}

func (failingCorpusConn) Exec(context.Context, string, ...any) error { return errCorpusSinkDDL }

func (failingCorpusConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, errCorpusSinkDDL
}

func (failingCorpusConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, errCorpusSinkDDL
}

// TestBuildCorpusSink_CHTableFailureDisablesReconciler pins the sink-selection
// contract the CH-table reconciliation depends on: the CONFIGURED mode is
// honoured or nothing is.
//
// The temptation is to treat a chtable failure as a reason to write the JSONL
// file instead, since a sink path may well be configured. That would tell an
// operator who asked for the CH table that the corpus is healthy while every
// consumer of cerberus_router_corpus reads an empty table. The reconciler stays
// off instead, and the startup log says so. This test pins that decision with a
// SinkPath deliberately set, because the tempting-but-wrong behaviour is
// distinguishable only in exactly that configuration.
func TestBuildCorpusSink_CHTableFailureDisablesReconciler(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.CHOptCorpus.SinkMode = corpusSinkModeCHTable
	cfg.CHOptCorpus.SinkPath = filepath.Join(t.TempDir(), "corpus.jsonl")

	sink, desc, ok := buildCorpusSink(context.Background(), discardLogger(), failingCorpusConn{}, cfg)
	if ok {
		t.Fatalf("chtable sink construction failed but buildCorpusSink reported ok (desc %q)", desc)
	}
	if sink != nil {
		t.Errorf("sink = %#v; a failed construction must return no sink", sink)
	}
	if desc != "" {
		t.Errorf("sink description = %q; want empty on failure", desc)
	}
}

// TestBuildCorpusSink_JSONLModeIgnoresCHFailure is the other half of the
// contract: in jsonl mode the CH connection is never consulted, so a CH that
// rejects every statement does not disable the reconciler.
func TestBuildCorpusSink_JSONLModeIgnoresCHFailure(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.CHOptCorpus.SinkPath = filepath.Join(t.TempDir(), "corpus.jsonl")

	sink, desc, ok := buildCorpusSink(context.Background(), discardLogger(), failingCorpusConn{}, cfg)
	if !ok {
		t.Fatalf("jsonl sink must build without touching ClickHouse")
	}
	t.Cleanup(func() { _ = sink.Close() })
	if want := "jsonl:" + cfg.CHOptCorpus.SinkPath; desc != want {
		t.Errorf("sink description = %q; want %q", desc, want)
	}
}

// recordingCorpusConn is an optcorpus.CHTableConn that records every statement
// and answers both construction reads the way a server that HONOURED the DDL
// would: the engine it reports back is the one the recorded CREATE asked for,
// and the column list is the one this binary writes.
//
// Deriving the engine from the CREATE rather than hardcoding it is what makes
// the wiring test below double-discriminating. Drop the config plumbing and the
// CREATE emits a plain MergeTree, so a `DatabaseReplicated` deployment both
// fails the DDL assertion AND fails sink construction outright — the server's
// answer no longer matches what the deployment needs.
type recordingCorpusConn struct {
	stmts []string
}

func (c *recordingCorpusConn) Exec(_ context.Context, query string, _ ...any) error {
	c.stmts = append(c.stmts, query)
	return nil
}

// createSQL returns the recorded CREATE TABLE statement, or "" if none ran.
func (c *recordingCorpusConn) createSQL() string {
	for _, s := range c.stmts {
		if strings.HasPrefix(s, "CREATE TABLE") {
			return s
		}
	}
	return ""
}

func (c *recordingCorpusConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	if strings.Contains(query, "`system`.`tables`") {
		engine := "MergeTree"
		if strings.Contains(c.createSQL(), "ENGINE = ReplicatedMergeTree") {
			engine = "ReplicatedMergeTree"
		}
		return &corpusStringRows{cols: 1, rows: [][]string{{engine}}}, nil
	}
	rows := &corpusStringRows{cols: 2}
	for _, col := range optcorpus.CorpusColumns() {
		rows.rows = append(rows.rows, []string{col.Name, chsql.RenderDDL(col.Type)})
	}
	return rows, nil
}

func (c *recordingCorpusConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, errCorpusSinkDDL
}

// corpusStringRows is an all-String driver.Rows: the shape both construction
// reads consume.
type corpusStringRows struct {
	cols int
	rows [][]string
	next int
}

func (r *corpusStringRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func (r *corpusStringRows) Scan(dest ...any) error {
	if len(dest) != r.cols {
		return errors.New("corpusStringRows: wrong scan arity")
	}
	for i, d := range dest {
		p, ok := d.(*string)
		if !ok {
			return errors.New("corpusStringRows: want *string scan destinations")
		}
		*p = r.rows[r.next-1][i]
	}
	return nil
}

func (r *corpusStringRows) HasData() bool                    { return r.next < len(r.rows) }
func (r *corpusStringRows) ScanStruct(any) error             { return nil }
func (r *corpusStringRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *corpusStringRows) Totals(...any) error              { return nil }
func (r *corpusStringRows) Columns() []string                { return nil }
func (r *corpusStringRows) Close() error                     { return nil }
func (r *corpusStringRows) Err() error                       { return nil }

// TestBuildCorpusSink_ThreadsTheDeploymentTopology pins the WIRING between
// config and the corpus DDL — the seam cerberus issues #3225, #3241 and #3250
// were all lost in, and the one no test in internal/optcorpus can reach.
//
// Every optcorpus test constructs a CorpusTableTopology by hand, so deleting
// any field from buildCorpusSink's literal leaves that whole package green
// while a real deployment silently goes back to a table that exists on one node
// (#3225) or holds rows on one replica (#3241 on the Replicated-database path,
// #3250 on the classic ON CLUSTER one). Only a test that starts from a
// config.Config and reads the emitted statement closes that.
//
// The four cases are the four topologies an operator can actually configure:
// the single-node default; single-shard `replicas > 1`, where the chart sets
// CERBERUS_SCHEMA_DATABASE_REPLICATED; a classic ON CLUSTER cluster whose
// operator pinned a replicating CERBERUS_SCHEMA_TABLE_ENGINE, which the chart
// renders under dataShards.count > 1 and an operator can configure by hand
// against their own cluster; and a classic cluster with nothing to replicate
// to, which must NOT be handed a Replicated engine.
//
// The replicating classic case also denies the operator's own expression: what
// is threaded is the DECLARATION that this deployment replicates, never DDL, so
// no part of the signal tables' Keeper path or engine family may appear in the
// corpus statement.
func TestBuildCorpusSink_ThreadsTheDeploymentTopology(t *testing.T) {
	t.Parallel()

	const (
		clusterName = "bwc_cluster"
		// classicReplicatedTableEngine is verbatim what the bundled chart
		// renders into schema.TABLE_ENGINE for the SIGNAL tables on the classic
		// ON CLUSTER path.
		classicReplicatedTableEngine = "ReplicatedMergeTree('/clickhouse/tables/{shard}/{database}/{table}', '{replica}')"
	)
	for _, tc := range []struct {
		name        string
		cluster     string
		replicated  bool
		tableEngine string
		want        []string
		deny        []string
	}{
		{
			name: "single-node",
			want: []string{"ENGINE = MergeTree"},
			deny: []string{"ON CLUSTER", "ENGINE = ReplicatedMergeTree"},
		},
		{
			name:       "replicated-database",
			replicated: true,
			want:       []string{"ENGINE = ReplicatedMergeTree"},
			deny:       []string{"ON CLUSTER"},
		},
		{
			name:        "classic-cluster-with-replicating-engine",
			cluster:     clusterName,
			tableEngine: classicReplicatedTableEngine,
			want:        []string{"ON CLUSTER `" + clusterName + "`", "ENGINE = ReplicatedMergeTree"},
			deny: []string{
				"ENGINE = ReplicatedMergeTree(",
				"/clickhouse/tables/",
				"{replica}",
			},
		},
		{
			name:    "classic-cluster-without-replication",
			cluster: clusterName,
			want:    []string{"ON CLUSTER `" + clusterName + "`", "ENGINE = MergeTree"},
			deny:    []string{"ENGINE = ReplicatedMergeTree"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Config{}
			cfg.CHOptCorpus.SinkMode = corpusSinkModeCHTable
			cfg.SchemaProvisioning.Cluster = tc.cluster
			cfg.SchemaProvisioning.DatabaseReplicated = tc.replicated
			cfg.SchemaProvisioning.TableEngine = tc.tableEngine

			conn := &recordingCorpusConn{}
			sink, _, ok := buildCorpusSink(context.Background(), discardLogger(), conn, cfg)
			if !ok {
				t.Fatalf("chtable sink construction failed against a server that honoured the DDL; "+
					"statements: %q", conn.stmts)
			}
			t.Cleanup(func() { _ = sink.Close() })

			create := conn.createSQL()
			if create == "" {
				t.Fatalf("construction ran no CREATE TABLE; statements: %q", conn.stmts)
			}
			for _, w := range tc.want {
				if !strings.Contains(create, w) {
					t.Errorf("CREATE does not carry %q:\n%s", w, create)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(create, d) {
					t.Errorf("CREATE wrongly carries %q:\n%s", d, create)
				}
			}
		})
	}
}
