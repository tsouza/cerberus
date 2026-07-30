//go:build chdb

package routerrules

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optcorpus"
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
// no findings. A scalar fire-gate watermark over an empty population resolves to
// Value{NoSignal:true} on BOTH backends (the CH backend detects emptiness via a
// count() companion; the JSONL backend via len(all)==0), so assertEnvParity
// checks NoSignal parity as well as scalar parity. A message-only count ratio
// still resolves to a finite 0 on both. The rules then produce no findings —
// either skipped for no signal (the fire-gate rules) or matching zero rows (the
// enum-only rules).
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

// TestCrossBackendParityEffectiveness runs the realistic effectiveness fixture
// through both backends and asserts the findings are identical, class-for-class
// and support-for-support. This is the end-to-end proof that the SQL (CH) path
// and the in-Go (JSONL) path agree on the full catalog over a corpus that
// actually exercises every rule's fire path — not just the minimal seed. It is
// the parity counterpart of the default-lane TestEffectivenessGolden.
func TestCrossBackendParityEffectiveness(t *testing.T) {
	db := openParityChDB(t)
	seedParityTableFrom(t, db, "testdata/effectiveness.jsonl")

	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := effConfig()
	opts := EvalOptions{IncludeExperimental: true}

	chReport, err := NewEvaluator(cat, cfg, NewCHCorpusSource(&sqlDBConn{db: db}, 0)).Evaluate(context.Background(), opts)
	if err != nil {
		t.Fatalf("ch evaluate: %v", err)
	}
	jsonlReport, err := NewEvaluator(cat, cfg, effSource()).Evaluate(context.Background(), opts)
	if err != nil {
		t.Fatalf("jsonl evaluate: %v", err)
	}

	chKeys := findingKeys(chReport)
	jsonlKeys := findingKeys(jsonlReport)
	if len(chKeys) != len(jsonlKeys) {
		t.Fatalf("finding count differs: ch=%d jsonl=%d\nch=%v\njsonl=%v", len(chKeys), len(jsonlKeys), chKeys, jsonlKeys)
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

// TestCrossBackendParityBenchmark runs the LABELED benchmark corpus through both
// backends and asserts identical findings. This proves the precision/recall/F1
// the benchmark reports is the same whether measured on the in-Go matcher (the
// default-lane benchmark harness) or the production CH SQL path — so the
// effectiveness numbers are not an artefact of the in-memory source.
func TestCrossBackendParityBenchmark(t *testing.T) {
	corpus := GenerateBenchCorpus(nominalBenchParams())

	db := openParityChDB(t)
	seedParityTableFromBench(t, db, corpus.Rows)

	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := staticConfigLookup(benchConfig())
	opts := EvalOptions{IncludeExperimental: true}

	chReport, err := NewEvaluator(cat, cfg, NewCHCorpusSource(&sqlDBConn{db: db}, 0)).Evaluate(context.Background(), opts)
	if err != nil {
		t.Fatalf("ch evaluate: %v", err)
	}
	memReport, err := NewEvaluator(cat, cfg, corpus.AsCorpusSource()).Evaluate(context.Background(), opts)
	if err != nil {
		t.Fatalf("mem evaluate: %v", err)
	}

	chKeys := findingKeys(chReport)
	memKeys := findingKeys(memReport)
	if len(chKeys) != len(memKeys) {
		t.Fatalf("finding count differs: ch=%d mem=%d\nch=%v\nmem=%v", len(chKeys), len(memKeys), chKeys, memKeys)
	}
	for k, chSup := range chKeys {
		mSup, ok := memKeys[k]
		if !ok {
			t.Errorf("ch finding %q absent from mem", k)
			continue
		}
		if chSup != mSup {
			t.Errorf("support differs for %q: ch=%d mem=%d", k, chSup, mSup)
		}
	}

	// And the scored metrics must be identical across backends.
	chMetrics := scoreReport(chReport, corpus)
	memMetrics := scoreReport(memReport, corpus)
	if chMetrics.Overall != memMetrics.Overall {
		t.Errorf("overall metrics differ across backends:\nch=%+v\nmem=%+v", chMetrics.Overall, memMetrics.Overall)
	}
}

// seedParityTableFromBench creates the corpus table in chdb and loads benchmark
// rows directly (no JSONL round-trip), mirroring seedParityTableFrom's DDL.
func seedParityTableFromBench(t *testing.T, db *sql.DB, rows []BenchRow) {
	t.Helper()
	jr := make([]jsonlRow, len(rows))
	for i, r := range rows {
		jr[i] = jsonlRow{
			ShapeID: r.ShapeID, Language: r.Language, NormalizedQueryHash: r.NormalizedQueryHash,
			NAnchors: r.NAnchors, Fanout: r.Fanout, CumulativeD: r.CumulativeD,
			OuterRange: r.OuterRange, Step: r.Step, Route: r.Route, KShards: r.KShards,
			DecisionReason: r.DecisionReason, ReadRows: r.ReadRows, ReadBytes: r.ReadBytes,
			QueryDurationMS: r.QueryDurationMS, MemoryUsage: r.MemoryUsage, ExitStatus: r.ExitStatus,
		}
	}
	seedParityTableFromRows(t, db, jr)
}

// assertEnvParity fails if the two resolved Envs differ in key set, NoSignal
// flags, scalar values (NaN/Inf is never tolerated), or partition contents.
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
		if av.NoSignal != bv.NoSignal {
			t.Errorf("param %q NoSignal differs: ch=%v jsonl=%v", name, av.NoSignal, bv.NoSignal)
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

// seedParityTable creates the corpus table in chdb and loads testdata/seed.jsonl.
func seedParityTable(t *testing.T, db *sql.DB) {
	t.Helper()
	seedParityTableFrom(t, db, "testdata/seed.jsonl")
}

// seedParityTableFrom creates the corpus table in chdb and loads the named JSONL
// fixture, so the CH source's queries run against the production table shape.
func seedParityTableFrom(t *testing.T, db *sql.DB, fixture string) {
	t.Helper()
	seedParityTableFromRows(t, db, readSeedRows(t, fixture))
}

// seedParityTableFromRows creates the corpus table in chdb and loads the given
// decoded rows.
//
// The shape is taken from optcorpus.CorpusColumns rather than restated here: this
// seeder used to hand-write the DDL, and its route column had silently drifted to
// Enum8('A'=0,'B'=1) — no unclassified member — so the parity lane could not even
// represent an unclassified row, let alone catch it being folded into 'A'.
//
// Two deliberate departures from the production DDL, both about substrate rather
// than shape: no TTL (fixture event_time values are small epoch offsets a
// retention clause would treat as long expired, so a background TTL merge could
// empty the table mid-test) and ORDER BY (shape_id, event_time), which is the
// access pattern the parity queries actually scan.
func seedParityTableFromRows(t *testing.T, db *sql.DB, rows []jsonlRow) {
	t.Helper()

	cols := optcorpus.CorpusColumns()
	if _, err := db.Exec("DROP TABLE IF EXISTS " + CorpusTableName); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	ddl := chsql.CreateTable(CorpusTableName).
		Columns(cols...).
		Engine(chsql.EngineMergeTree()).
		OrderBy("shape_id", "event_time").
		SQL()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create table: %v", err)
	}

	vals := make([]string, 0, len(rows))
	for _, r := range rows {
		vals = append(vals, parityRowTuple(t, cols, r))
	}
	stmt := "INSERT INTO " + CorpusTableName + " VALUES " + strings.Join(vals, ",")
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("insert seed: %v", err)
	}
}

// parityRowTuple renders one VALUES tuple in the table's own column order. The
// per-column literals are looked up by name, so adding a column to the corpus
// table fails here loudly instead of shifting every later value one position left.
func parityRowTuple(t *testing.T, cols []chsql.ColumnDef, r jsonlRow) string {
	t.Helper()

	byName := map[string]string{
		"event_time":            fmt.Sprintf("toDateTime(%d)", int64(r.EventTime)),
		"shape_id":              chLiteral(r.ShapeID),
		"language":              chLiteral(r.Language),
		"normalized_query_hash": strconv.FormatUint(r.NormalizedQueryHash, 10),
		"n_anchors":             strconv.Itoa(int(r.NAnchors)),
		"fanout":                strconv.Itoa(int(r.Fanout)),
		"cumulative_d":          strconv.Itoa(int(r.CumulativeD)),
		"outer_range":           strconv.Itoa(int(r.OuterRange)),
		"step":                  strconv.Itoa(int(r.Step)),
		"route":                 chLiteral(r.Route),
		"k_shards":              strconv.Itoa(int(r.KShards)),
		"decision_reason":       chLiteral(r.DecisionReason),
		"read_rows":             strconv.Itoa(int(r.ReadRows)),
		"read_bytes":            strconv.Itoa(int(r.ReadBytes)),
		"query_duration_ms":     strconv.Itoa(int(r.QueryDurationMS)),
		"memory_usage":          strconv.Itoa(int(r.MemoryUsage)),
		"exit_status":           chLiteral(r.ExitStatus),
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		v, ok := byName[c.Name]
		if !ok {
			t.Fatalf("corpus column %q has no parity fixture value — add one to parityRowTuple", c.Name)
		}
		out = append(out, v)
	}
	return "(" + strings.Join(out, ",") + ")"
}

// chLiteral single-quotes a fixture string for a VALUES tuple. The empty string
// is a real value here — it is the unclassified route and the absent decision
// reason — so it must render as ” rather than being elided.
func chLiteral(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// readSeedRows decodes the JSONL fixture into jsonlRow values for re-insertion
// into chdb, so both backends read byte-identical data.
func readSeedRows(t *testing.T, fixture string) []jsonlRow {
	t.Helper()
	data, err := os.ReadFile(fixture) //nolint:gosec // test fixture path under testdata/
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
