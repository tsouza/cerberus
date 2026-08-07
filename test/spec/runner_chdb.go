//go:build chdb

// Package spec — chDB-backed round-trip executor.
//
// This file is compiled only when the `chdb` build tag is set, which
// also implies the chdb-go driver and libchdb.so are present (see
// `just chdb-install`). The default `just test` lane stays
// CGO_ENABLED=0 and never sees this code.
//
// The executor opens an ephemeral in-process chDB session, runs the
// fixture's `seed:` DDL+INSERT statements, executes the emitted `sql:`
// (with `args:` bound), and asserts the resulting rows match the
// `expected_rows:` JSON. Map columns are wrapped server-side in
// `toJSONString(...)` to dodge the native parquet Map scan panic
// documented by the chDB driver probe.
package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/testsql"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// chdbEngineMu serializes ALL access to the chDB engine across the whole
// test process.
//
// chDB / libchdb is an EMBEDDED ClickHouse: a single native engine is
// shared by every `sql.Open("chdb", "")` in the process (the empty-DSN
// sessions are NOT isolated — see ApplySeed's "chdb-go shares one engine
// across a process" note). That engine is NOT thread-safe for concurrent
// in-process execution. The round-trip suite runs fixtures under
// t.Parallel() (see roundtrip_chdb_test.go) and spec.Walk fans each
// fixture into a t.Run subtest, so without serialization two goroutines
// can drive queries — and, worse, free native result state — at the same
// time. The observed failure is an intermittent
//
//	SIGABRT: abort / signal arrived during cgo execution
//
// inside chdb-purego.(*result).Free, which fires when a *sql.Rows is
// closed and when a *sql.DB is closed (driver teardown). To make this
// safe, every chDB engine call — Open/Ping/Exec(SET), seed Exec, Query,
// row iteration, rows.Close, AND the per-case db.Close — runs under this
// mutex. Only the parse / lower / SQL-build work of OTHER cases stays
// parallel; the engine span itself is single-threaded.
var chdbEngineMu sync.Mutex

// nowAnchorLiteral, substituteNow64, the seed-statement splitter, the
// idempotency promotion, and the ResourceAttributes backfill now live in the
// build-tag-free roundtrip_prep.go so the `integration`-tagged strict-scan
// differential shares the exact same seed + SQL prep pipeline. defaultNowAnchor
// stays here because its only consumers (the eval-instant sweep and
// TestNowAnchorLiteralMatchesDefault) are themselves chdb-tagged; the
// integration lane anchors via the exported SubstituteNow64 / nowAnchorLiteral
// seam and never needs the time.Time value.

// defaultNowAnchor is the deterministic eval instant every fixed-anchor
// round-trip fixture is seeded against. It mirrors the instant-eval
// anchor `internal/promql/lower_test.go` feeds into `LowerAt`
// (`time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)`), so each round-trip
// fixture sees the same wall-clock the lowering pass used to compute
// filter bounds. [nowAnchorLiteral] is `CHNow64Literal(defaultNowAnchor)`
// by construction (asserted in TestNowAnchorLiteralMatchesDefault), so
// the fixed-anchor and per-eval substitution paths share one source of
// truth for the default instant.
var defaultNowAnchor = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// CHNow64Literal renders a `time.Time` as the CH DateTime64(9) literal
// shape `substituteNow64` splices in — `toDateTime64('YYYY-MM-DD
// HH:MM:SS.fffffffff', 9, 'UTC')`. The nanosecond field is always nine
// digits so the parser sees a fractional-second literal (matching
// [nowAnchorLiteral]'s shape). Used by the eval-instant sweep to anchor
// the residual outer-projection `now64(?)` to the swept eval time T.
func CHNow64Literal(at time.Time) string {
	u := at.UTC()
	return fmt.Sprintf(
		"toDateTime64('%04d-%02d-%02d %02d:%02d:%02d.%09d', 9, 'UTC')",
		u.Year(), int(u.Month()), u.Day(),
		u.Hour(), u.Minute(), u.Second(), u.Nanosecond(),
	)
}

// OpenChDB returns a fresh ephemeral chDB session. The empty DSN
// triggers a temp-dir-backed session that's torn down with the
// connection — there is no `:memory:` literal in chdb-go.
func OpenChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	// db.Close tears down native engine state (chdb-purego result Free),
	// so it must not race another case's engine call. The cleanup runs
	// AFTER RunRoundTrip returns and has released chdbEngineMu, so it
	// takes the lock itself here. (OpenChDB itself is always called with
	// chdbEngineMu already held by RunRoundTrip, hence no lock around the
	// Open/Ping/Exec below.)
	t.Cleanup(func() {
		chdbEngineMu.Lock()
		defer chdbEngineMu.Unlock()
		_ = db.Close()
	})
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	// Enable the experimental timeSeries*ToGrid aggregate family at the
	// session level so the native-rate fixtures (RangeWindowNative →
	// timeSeriesRateToGrid) run. The setting is harmless for every other
	// fixture — it gates only those aggregates, which no other fixture
	// emits — and chDB does not enforce the gate, so this is belt-and-
	// braces for forward-compatibility if a future chDB starts to. The
	// production chclient sends the same setting per-query (only on the
	// native path); see internal/chclient.WithTSGridSetting. The spelling
	// is the CANONICAL `allow_experimental_time_series_aggregate_functions`
	// (ClickHouse PR #80590 renamed the gate before the v25.6 release; the
	// old `..._ts_to_grid_aggregate_function` survives only as an alias —
	// see chclient.SettingExperimentalTSGridAggregate). A chDB build older
	// than the family's introduction would reject the SET — the linked
	// libchdb answers `SELECT version()` with 26.5.1.1, well past the
	// v25.6 floor.
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid aggregate: %v", err)
	}
	return db
}

// ApplySeed splits a multi-statement script on top-level semicolons
// and exec's each piece. Statements wrapped in single-quoted strings
// keep their semicolons literal (handled by a tiny state machine).
//
// Cross-fixture isolation: chdb-go shares one engine across a process,
// so bare `CREATE TABLE foo` from a prior fixture survives to clash
// with the next. The applier promotes bare `CREATE TABLE` to
// `CREATE OR REPLACE TABLE` so re-running a fixture in the same
// process is idempotent. Fixture authors who want strict CH semantics
// can opt out by writing `CREATE OR REPLACE TABLE` /
// `CREATE TABLE IF NOT EXISTS` themselves.
func ApplySeed(t *testing.T, db *sql.DB, seed string) {
	t.Helper()
	stmts := testsql.BackfillMetricsColumns(testsql.SplitStatements(seed))
	// Also backfill the otel_traces columns wrapWithSampleProjection reads
	// unconditionally (TraceId / SpanId / ParentSpanId / SpanName / Duration /
	// Timestamp / ResourceAttributes) — a no-op for promql/logql seeds, which
	// never declare a table named otel_traces. Needed so
	// the traceql lane's [SQLReconstructor] wrap-projected SQL doesn't
	// fail with an unknown-column error against a fixture seed that
	// (deliberately) only declares the columns its OWN pre-wrap `-- sql --`
	// touches. Mirrors applyStrictScanSeed's identical backfill in
	// strictscan_integration_test.go.
	stmts = testsql.BackfillTracesColumns(stmts)
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = testsql.PromoteCreateTable(stmt)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// RunRoundTrip executes a fixture's seed + rewritten SQL against an
// ephemeral chDB session and asserts the resulting rows match
// `expected_rows:`. Caller passes the loaded fixture; if it's not a
// round-trip fixture, the call is a no-op.
//
// Determinism contract: cerberus's emitted instant-query SQL does not
// carry a top-level ORDER BY (PromQL's instant-query result is a set
// of (labels, value) pairs — no order is promised by the wire shape),
// so RunRoundTrip canonicalises both sides through [sortRows] before
// `reflect.DeepEqual`. Fixtures that rely on a stable order must
// emit one explicitly in the seed/SQL — none do today.
// Map column comparison uses reflect.DeepEqual on map[string]any so
// JSON key ordering is irrelevant.
func RunRoundTrip(t *testing.T, c *Case, opts ...RoundTripOption) {
	t.Helper()
	cfg := newRoundTripConfig(opts)
	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		return
	}
	if strings.TrimSpace(rt.SQL) == "" {
		t.Fatalf("fixture %s has seed/expected_rows but missing sql section", c.Name)
	}

	sql, args := rt.SQL, rt.Args

	// A lane whose fixtures record SQL from an earlier pipeline stage than
	// the one production sends supplies a reconstruction (see
	// [WithSQLReconstructor]); it replaces the recorded SQL/args before the
	// chDB prep pipeline runs, so `expected_rows:` is pinned against the SQL
	// production actually executes. Lanes whose fixtures already record the
	// production stage pass no option and execute what they recorded.
	if cfg.reconstruct != nil {
		if reSQL, reArgs, ok, err := cfg.reconstruct(c); err != nil {
			t.Fatalf("fixture %s: sql reconstruction: %v", c.Name, err)
		} else if ok {
			sql, args = reSQL, reArgs
		}
	}

	// expected_rows is ground truth for the pre-optimizer SQL above, so a
	// mismatch here is safe to fold into GOLDEN_UPDATE.
	runRoundTripSQL(t, c, rt, sql, args, true)
}

// RunRoundTripSQL is [RunRoundTrip]'s post-optimizer twin: it executes a
// CALLER-SUPPLIED sql/args pair — typically chsql.Emit of the OPTIMIZED
// plan (see spec.AssertScanTimeBoundAccepts's return value) — against the
// same fixture's `seed:` and asserts the resulting rows still match
// `expected_rows:`.
//
// This exists because production always runs the optimizer before
// chsql.Emit (internal/engine/engine.go's Parse → ProjectSamples →
// Optimize → Emit order), but every *.txtar fixture's own `sql:` /
// `chplan:` sections — and, until now, the `expected_rows:` round-trip
// itself — only ever validated the PRE-optimizer plan (issue #1700). The
// optimizer is meant to be semantics-preserving, so the optimized SQL's
// result set should be byte-identical to the pre-optimizer SQL's; a
// mismatch here is a real optimizer bug, not a golden to regenerate — so,
// unlike RunRoundTrip, GOLDEN_UPDATE=1 never rewrites `expected_rows` from
// this path. `expected_rows` stays anchored to the pre-optimizer
// execution that RunRoundTrip captures; this function only ever compares
// against it.
//
// A no-op for fixtures that never opted into round-trip execution, same
// as RunRoundTrip.
func RunRoundTripSQL(t *testing.T, c *Case, sql string, args []any) {
	t.Helper()
	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		return
	}
	if strings.TrimSpace(sql) == "" {
		t.Fatalf("fixture %s: optimized SQL is empty", c.Name)
	}
	runRoundTripSQL(t, c, rt, sql, args, false)
}

// runRoundTripSQL is the shared chDB-execution core both RunRoundTrip and
// RunRoundTripSQL delegate to: it seeds an ephemeral chDB session, executes
// sql/args, and asserts the resulting rows match rt.ExpectedRows.
// allowGoldenUpdate gates whether a mismatch under GOLDEN_UPDATE=1 rewrites
// `expected_rows` in place (RunRoundTrip, the pre-optimizer ground truth)
// or fails outright (RunRoundTripSQL, the post-optimizer check — a
// mismatch there must never silently become the new "expected", since
// that would launder a real optimizer bug into a passing golden).
func runRoundTripSQL(t *testing.T, c *Case, rt *RoundTripSections, sql string, args []any, allowGoldenUpdate bool) {
	t.Helper()

	// Acquire the process-global chDB engine lock for the FULL engine
	// span of this case: open/ping/seed, query, row iteration, and
	// rows.Close (whose driver teardown calls native result Free — the
	// site of the SIGABRT under parallel cases). The unlock is deferred
	// FIRST so it runs LAST (after the deferred rows.Close below, defers
	// being LIFO), keeping the lock held across the entire result
	// lifecycle. The interleaved SQL-build helpers below are pure string
	// work; holding the lock across them costs microseconds and keeps the
	// scope coarse and deadlock-free. db.Close runs later, in t.Cleanup,
	// which re-acquires this lock itself.
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)
	ApplySeed(t, db, rt.Seed)

	// substituteNow64 must run BEFORE rewriteMapProjections so the
	// `now64(?)`-consumed args are dropped before the Map rewrite
	// inspects the SQL. The two passes are independent textually but
	// the args side is global, and ordering them this way keeps the
	// argIdx accounting in substituteNow64 simple.
	query, queryArgs := substituteNow64(sql, args)
	query = testsql.ExpandStarProjection(query, testsql.SeedTableColumns(rt.Seed))
	query = testsql.RewriteMapProjections(query)
	query = testsql.NestMapOrderBy(query)
	colCount := testsql.ProjectionCount(query)

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		t.Fatalf("query failed:\n--- query ---\n%s\n--- args ---\n%#v\n--- err ---\n%v",
			query, queryArgs, err)
	}
	defer func() { _ = rows.Close() }()

	if colCount == 0 {
		// Wildcard outer projection (`SELECT R.* FROM ...`): the
		// fixture seed determines the actual column count. `rows.
		// Columns()` returns names without instantiating the
		// driver's column-type table, so it sidesteps the Map
		// `rows.ColumnTypes()` panic.
		cols, cerr := rows.Columns()
		if cerr != nil {
			t.Fatalf("rows.Columns: %v", cerr)
		}
		colCount = len(cols)
		if colCount == 0 {
			t.Fatalf("fixture %s: cannot determine SELECT projection count from sql", c.Name)
		}
	}

	got := make([][]any, 0, len(rt.ExpectedRows))
	for rows.Next() {
		// Scan into *interface{} so we receive the chdb-go driver's
		// native Go value (string, int64, float64, time.Time, []byte)
		// per chdb/driver/parquet.go's switch table. This sidesteps
		// rows.ColumnTypes() (which panics on Map columns per the
		// chDB driver probe).
		cells := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := make([]any, colCount)
		for i, v := range cells {
			row[i] = DecodeCell(v, rt.RawStrings)
		}
		got = append(got, row)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Coerce expected rows in-place: JSON numbers always decode as
	// float64; the runner normalizes the actual scan output through
	// the same lens so DeepEqual is symmetric. Same for Map columns
	// (already decoded as map[string]any on the got side).
	want := normalizeExpected(rt.ExpectedRows)
	gotNorm := normalizeExpected(got)

	// PromQL instant-query results are sets — the chDB engine is
	// free to return groups in any order when the emitted SQL lacks
	// a top-level ORDER BY (which it does). Sort both sides through
	// the same canonical form so DeepEqual reflects set-equality.
	sortRows(want)
	sortRows(gotNorm)

	if !reflect.DeepEqual(gotNorm, want) {
		// GOLDEN_UPDATE=1: rewrite `expected_rows` in-place rather
		// than failing — same flow as the text-equality goldens in
		// internal/promql/lower_test.go. Lets dev/CI regenerate the
		// round-trip cells after a semantically-correct query
		// change (e.g., PromQL `__name__`-drop fix in #355) without
		// hand-editing 70+ fixtures. Never taken from the
		// post-optimizer path (see RunRoundTripSQL's doc comment).
		if allowGoldenUpdate && os.Getenv(envGoldenUpdate) == "1" {
			Match(t, c, map[string]string{
				"expected_rows": formatExpectedRows(gotNorm),
			})
			return
		}
		t.Fatalf("round-trip mismatch (fixture %s)\n got = %s\nwant = %s",
			c.Name, mustJSON(gotNorm), mustJSON(want))
	}
}

// formatExpectedRows renders a row set in the canonical TXTAR
// `expected_rows:` shape: outer `[`/`]` on their own lines, each row
// on its own line indented with two spaces, cells rendered compact-JSON
// with `, ` separators (matching the byte shape every hand-authored
// fixture in test/spec/{promql,logql,traceql}/ pins).
//
// We don't lean on `json.Marshal` for the row itself because the
// stdlib produces no-space separators (`","`), so manual fixtures and
// regenerated ones would drift by whitespace alone. Each cell flows
// through json.Marshal individually, then we join with `, `.
func formatExpectedRows(rows [][]any) string {
	if len(rows) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[\n")
	for i, row := range rows {
		b.WriteString("  [")
		for j, cell := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatExpectedCell(cell))
		}
		b.WriteByte(']')
		if i < len(rows)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]")
	return b.String()
}

// formatExpectedCell renders one cell of an `expected_rows:` row.
// Maps render with spaces between key/value pairs (`{"k": "v", ...}`)
// to match the hand-authored fixture shape; everything else delegates
// to json.Marshal of the infSafe-wrapped value.
func formatExpectedCell(v any) string {
	if m, ok := v.(map[string]any); ok {
		return formatExpectedMap(m)
	}
	raw, err := json.Marshal(infSafe(v))
	if err != nil {
		return fmt.Sprintf(`"<json err: %v>"`, err)
	}
	return string(raw)
}

// formatExpectedMap renders a JSON object with deterministic key order
// (`sort.Strings`) and `, ` separators between pairs and `: ` between
// keys/values, matching the hand-authored fixture style. Used only by
// the GOLDEN_UPDATE regeneration path; the read path goes through
// `json.Unmarshal` which is whitespace-insensitive.
func formatExpectedMap(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		raw, err := json.Marshal(k)
		if err != nil {
			raw = []byte(fmt.Sprintf(`"<json err: %v>"`, err))
		}
		b.Write(raw)
		b.WriteString(": ")
		b.WriteString(formatExpectedCell(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// sortRows canonicalises a result set by sorting rows in-place on the
// JSON encoding of their cells. The encoding is deterministic for the
// types the runner emits (string, float64, bool, nil, []any,
// map[string]any), so any two row sets that compare set-equal end up
// with identical post-sort orderings.
func sortRows(rows [][]any) {
	sort.Slice(rows, func(i, j int) bool {
		return mustJSON(rows[i]) < mustJSON(rows[j])
	})
}

// DecodeCell turns a chdb-go driver-native value into the Go value
// used for comparison. The driver hands back time.Time, int64,
// float64, bool, string, []byte — see chdb/driver/parquet.go's
// switch table.
//
// For Map columns we wrapped server-side in toJSONString(...), the
// driver returns a string; we try JSON-decode and fall back to the
// raw string. time.Time values are normalized to RFC3339Nano so
// fixture authors can write them as quoted strings.
//
// When rawStrings is true the JSON-decode pass on String/[]byte cells
// is skipped — the runner returns the raw string. Fixtures opt in
// via the `raw_strings:` section when they need to assert literal
// brace-prefixed payloads against the SQL output.
func DecodeCell(v any, rawStrings bool) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		if rawStrings {
			return string(x)
		}
		return decodeBytes(x)
	case string:
		if rawStrings {
			return x
		}
		return decodeString(x)
	default:
		return v
	}
}

func decodeBytes(b []byte) any {
	return decodeString(string(b))
}

func decodeString(s string) any {
	trim := strings.TrimSpace(s)
	if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') {
		var v any
		if err := json.Unmarshal([]byte(trim), &v); err == nil {
			return v
		}
	}
	return s
}

// normalizeExpected walks a [][]any and coerces numeric cells to
// float64 so JSON-decoded numbers compare equal to scanned values.
func normalizeExpected(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		nr := make([]any, len(row))
		for j, v := range row {
			nr[j] = normalizeValue(v)
		}
		out[i] = nr
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case float64:
		// Non-finite floats normalize to the same string sentinels the
		// expected side uses (below). Comparing through strings — not
		// through math.NaN() — is load-bearing for NaN cells:
		// reflect.DeepEqual(math.NaN(), math.NaN()) is FALSE (IEEE NaN
		// inequality), so a NaN-valued fixture cell could never match
		// if both sides normalized to the float.
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, +1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		}
		return x
	case string:
		// JSON cannot represent ±Inf / NaN natively (json.Unmarshal
		// would reject the bare tokens). Fixture authors encode them
		// as string sentinels in `expected_rows:`; canonicalise the
		// spelling so "Inf" and "+Inf" compare equal and the actual
		// side's float64 specials (normalized above) line up.
		switch x {
		case "Inf", "+Inf":
			return "+Inf"
		case "-Inf":
			return "-Inf"
		case "NaN":
			return "NaN"
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalizeValue(vv)
		}
		return out
	default:
		return v
	}
}

func mustJSON(v any) string {
	// json.Marshal rejects non-finite float64 values; route them
	// through the same sentinel strings normalizeValue accepts on
	// the read path so sortRows can keep delegating to JSON
	// without per-call error handling for ±Inf / NaN cells.
	b, err := json.MarshalIndent(infSafe(v), "", "  ")
	if err != nil {
		return fmt.Sprintf("<json err: %v>", err)
	}
	return string(b)
}

// infSafe walks v and substitutes non-finite float64 values with the
// JSON-friendly string sentinels normalizeValue understands ("+Inf",
// "-Inf", "NaN"). Other types pass through unchanged. Used only by
// the mustJSON sort key — the cell values themselves are still
// compared via reflect.DeepEqual at full float64 precision.
func infSafe(v any) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, +1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = infSafe(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = infSafe(vv)
		}
		return out
	}
	return v
}
