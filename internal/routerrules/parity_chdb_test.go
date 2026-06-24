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

// sinceAfterAllRows is an event_time floor past every seed row, so a --since
// scan over the seeded corpus selects zero rows — the empty/TTL'd-corpus case.
const sinceAfterAllRows = 100_000

// TestCrossBackendParityEmptyCorpus is the empty-corpus parity guard. Both
// backends scan the SAME seeded corpus but with a --since floor past every row,
// so each sees an empty population. The contract is: empty corpus = no signal =
// no findings, and every corpus-derived param resolves to 0 on BOTH backends.
// Before the ifNull(...,0) Frag wrap + NaN/Inf guard in source_ch.go, the CH
// backend resolved its watermarks to NaN (a non-grouped aggregate over zero
// rows), while the JSONL backend resolved them to 0 — diverging the resolved
// Env and silently suppressing every finding on CH (x > NaN is always false).
// This test asserts the two backends agree, scalar-for-scalar, and that both
// produce an empty report.
func TestCrossBackendParityEmptyCorpus(t *testing.T) {
	db := openParityChDB(t)
	seedParityTable(t, db)

	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := evalConfig()

	chSrc := NewCHCorpusSource(&sqlDBConn{db: db}, sinceAfterAllRows)
	jsonlSrc := NewJSONLCorpusSource("testdata/seed.jsonl", sinceAfterAllRows)

	// 1) Resolved Env parity: every corpus-derived param must resolve to the
	//    identical value (0) on both backends.
	chEnv, err := NewParamResolver(cfg, chSrc).Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("ch resolve: %v", err)
	}
	jsonlEnv, err := NewParamResolver(cfg, jsonlSrc).Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("jsonl resolve: %v", err)
	}
	assertEnvParity(t, chEnv, jsonlEnv)

	// 2) Findings parity: both backends must produce an empty report (no
	//    signal => no findings).
	chReport, err := NewEvaluator(cat, cfg, chSrc).Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("ch evaluate: %v", err)
	}
	jsonlReport, err := NewEvaluator(cat, cfg, jsonlSrc).Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("jsonl evaluate: %v", err)
	}
	if len(chReport.Findings) != 0 {
		t.Errorf("empty corpus must yield no CH findings, got %d: %+v", len(chReport.Findings), chReport.Findings)
	}
	if len(jsonlReport.Findings) != 0 {
		t.Errorf("empty corpus must yield no JSONL findings, got %d: %+v", len(jsonlReport.Findings), jsonlReport.Findings)
	}
}

// assertEnvParity fails if the two resolved Envs differ in key set, scalar
// values (NaN/Inf is never tolerated), or partition contents.
func assertEnvParity(t *testing.T, a, b Env) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("env key count differs: ch=%d jsonl=%d", len(a), len(b))
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok {
			t.Errorf("param %q present in ch env, absent from jsonl", name)
			continue
		}
		if av.IsPartitioned() != bv.IsPartitioned() {
			t.Errorf("param %q partitioned-ness differs: ch=%v jsonl=%v", name, av.IsPartitioned(), bv.IsPartitioned())
			continue
		}
		if !av.IsPartitioned() {
			if av.Scalar != bv.Scalar {
				t.Errorf("param %q scalar differs: ch=%v jsonl=%v", name, av.Scalar, bv.Scalar)
			}
			continue
		}
		if len(av.Partition) != len(bv.Partition) {
			t.Errorf("param %q partition size differs: ch=%d jsonl=%d", name, len(av.Partition), len(bv.Partition))
		}
		for k, ascal := range av.Partition {
			if bscal, ok := bv.Partition[k]; !ok || ascal != bscal {
				t.Errorf("param %q partition[%q] differs: ch=%v jsonl=%v", name, k, ascal, bscal)
			}
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
