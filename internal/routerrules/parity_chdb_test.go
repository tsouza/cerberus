//go:build chdb

package routerrules

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// TestCrossBackendParity seeds one fixture as a chdb table AND as JSONL, runs
// the embedded catalog through both backends, and asserts the findings are
// identical. This is the end-to-end proof that the shared condition AST and the
// matched quantile formula keep the CH (SQL) path and the JSONL (in-Go) path in
// lockstep. It runs only under -tags chdb (the CGO-free default lane is covered
// by the SQL-shape + quantile-formula tests).
func TestCrossBackendParity(t *testing.T) {
	db := openParityChDB(t)
	seedParityTable(t, db)

	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := evalConfig()

	chReport, err := NewEvaluator(cat, cfg, NewCHCorpusSource(&sqlDBConn{db: db}, 0)).
		Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("ch evaluate: %v", err)
	}
	jsonlReport, err := NewEvaluator(cat, cfg, NewJSONLCorpusSource("testdata/seed.jsonl", 0)).
		Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("jsonl evaluate: %v", err)
	}

	chKeys := findingKeys(chReport)
	jsonlKeys := findingKeys(jsonlReport)
	if len(chKeys) != len(jsonlKeys) {
		t.Fatalf("finding count differs: ch=%d jsonl=%d", len(chKeys), len(jsonlKeys))
	}
	for k, chSup := range chKeys {
		jSup, ok := jsonlKeys[k]
		if !ok {
			t.Errorf("ch finding %q absent from jsonl", k)
			continue
		}
		if chSup != jSup {
			t.Errorf("support differs for %q: ch=%d jsonl=%d", k, chSup, jSup)
		}
	}
}

func findingKeys(r *Report) map[string]int64 {
	out := map[string]int64{}
	for _, f := range r.Findings {
		out[f.RuleID+"|"+classOf(f.GroupKey)] = f.Support
	}
	return out
}

func openParityChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	return db
}

// seedParityTable creates the corpus table in chdb and loads the seed rows. The
// column types mirror the optcorpus MergeTree DDL so the CH source's queries
// behave as they would against the real table.
func seedParityTable(t *testing.T, db *sql.DB) {
	t.Helper()
	ddl := "CREATE OR REPLACE TABLE " + CorpusTableName + ` (
		event_time DateTime,
		shape_id LowCardinality(String),
		language LowCardinality(String),
		normalized_query_hash UInt64,
		n_anchors UInt32, fanout UInt32, cumulative_d UInt32,
		outer_range UInt32, step UInt32,
		route Enum8('A'=0,'B'=1),
		k_shards UInt8,
		decision_reason LowCardinality(String),
		read_rows UInt64, read_bytes UInt64,
		query_duration_ms UInt64, memory_usage UInt64,
		exit_status Enum8('ok'=0,'oom'=1,'timeout'=2,'sample_budget'=3,'breaker'=4,'rejected'=5)
	) ENGINE = MergeTree ORDER BY (shape_id, event_time)`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create table: %v", err)
	}

	rows := readSeedRows(t)
	var vals []string
	for _, r := range rows {
		vals = append(vals, fmt.Sprintf(
			"(toDateTime(%d),'%s','%s',%d,%d,%d,%d,%d,%d,'%s',%d,'%s',%d,%d,%d,%d,'%s')",
			int64(r.EventTime), r.ShapeID, r.Language, r.NormalizedQueryHash,
			int(r.NAnchors), int(r.Fanout), int(r.CumulativeD), int(r.OuterRange), int(r.Step),
			r.Route, int(r.KShards), r.DecisionReason,
			int(r.ReadRows), int(r.ReadBytes), int(r.QueryDurationMS), int(r.MemoryUsage), r.ExitStatus,
		))
	}
	stmt := "INSERT INTO " + CorpusTableName + " VALUES " + strings.Join(vals, ",")
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("insert seed: %v", err)
	}
}

// readSeedRows decodes the JSONL fixture into jsonlRow values for re-insertion
// into chdb, so both backends read byte-identical data.
func readSeedRows(t *testing.T) []jsonlRow {
	t.Helper()
	data, err := os.ReadFile("testdata/seed.jsonl")
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	var out []jsonlRow
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var jr jsonlRow
		if err := json.Unmarshal([]byte(line), &jr); err != nil {
			t.Fatalf("decode seed line: %v", err)
		}
		out = append(out, jr)
	}
	return out
}

// sqlDBConn adapts a database/sql chdb handle to the narrow CHConn the CH source
// consumes, wrapping *sql.Rows in a driver.Rows shim.
type sqlDBConn struct{ db *sql.DB }

func (c *sqlDBConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	r, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsAdapter{rows: r}, nil
}

// sqlRowsAdapter implements the narrow rows surface (Next/Scan/Err/Close) over
// *sql.Rows; it embeds driver.Rows for the unused methods of that wide
// interface.
type sqlRowsAdapter struct {
	driver.Rows
	rows *sql.Rows
}

func (a *sqlRowsAdapter) Next() bool          { return a.rows.Next() }
func (a *sqlRowsAdapter) Scan(d ...any) error { return a.rows.Scan(d...) }
func (a *sqlRowsAdapter) Err() error          { return tolerantParityErr(a.rows.Err()) }
func (a *sqlRowsAdapter) Close() error        { return a.rows.Close() }

// tolerantParityErr swallows the chdb parquet driver's spurious empty-row
// sentinel, mirroring the spec/property harness.
func tolerantParityErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "empty row") {
		return nil
	}
	return err
}
