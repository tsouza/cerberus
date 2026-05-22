//go:build chdb

// Property tests for the optimizer.
//
// The contract we want is "semantic equivalence": for any well-formed plan
// p, the optimized plan q := Default().Run(ctx, p) returns the same rows
// (as a set) when executed against the same data. The golden-text TXTAR
// suite only catches changes in the *emitted SQL*; if a future rule emits
// different SQL that still parses but flips the result set, only a
// round-trip assertion will catch it.
//
// This file generates random plan trees and round-trips both the
// unoptimized and optimized forms against an ephemeral chDB session,
// asserting their row sets are equal. The grammar is intentionally narrow
// (Scan / Filter / Project + a tiny predicate language) so that:
//
//   - Every generated plan emits valid ClickHouse SQL.
//   - Every predicate references columns that exist in the seed.
//   - The optimizer's baseline rules (FilterFusion, ConstantFold,
//     ProjectionPushdown) and the FilterProjectTranspose /
//     FilterAggregateTranspose pair all have shots at firing without
//     needing aggregates or windows.
//
// Wider node coverage (Aggregate, RangeWindow, joins) is future work —
// the property test catches the high-traffic shapes today, and the
// existing TXTAR fixtures already exercise the wide shapes in
// text-equality mode.
//
// chDB quirks the comparison code papers over:
//
//   - Float64 precision: every value in the seed is exact in IEEE-754
//     (no irrational-arithmetic surprises). Comparison is on raw
//     float64 bits.
//   - Map columns: the test never projects Attributes directly; the
//     row-set comparison normalises everything through JSON so the
//     toJSONString shim's output round-trips to the same Go value.
//   - ORDER BY determinism: the comparison sorts both row sets before
//     reflect.DeepEqual so the optimizer is free to reorder reads
//     (today no rule does, but a future TopK pushdown might).
package optimizer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// propertyTable is the single test table the generator targets. Schema
// mirrors the OTel-CH gauge layout (the same shape the abs_metric
// fixture uses) so the predicate grammar can reach for the same column
// names the optimizer's hand-rolled fixtures do.
const propertyTable = "otel_metrics_gauge"

// propertyDDL seeds an idempotent table-create + 10 deterministic rows.
// The promoteCreateTable shim in test/spec/runner_chdb.go would rewrite
// `CREATE TABLE` to `CREATE OR REPLACE TABLE` for us, but the optimizer
// property test owns its own chDB session so we write the OR-REPLACE
// form directly.
//
// Engine is MergeTree (not Memory) because the chsql emitter promotes
// Filter(Scan) predicates from WHERE → PREWHERE, and ClickHouse's
// Memory engine rejects PREWHERE with `ILLEGAL_PREWHERE`. The sort key
// `(MetricName, TimeUnix)` mirrors the production OTel-CH layout, so
// the optimized plans the property test round-trips exercise the same
// PREWHERE shapes a production deployment would hit.
const propertyDDL = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('up',          map('job', 'api',     'host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('up',          map('job', 'api',     'host', 'b'), toDateTime64('2026-01-01 00:00:01', 9), 1.0),
    ('up',          map('job', 'web',     'host', 'a'), toDateTime64('2026-01-01 00:00:02', 9), 0.0),
    ('down',        map('job', 'api',     'host', 'a'), toDateTime64('2026-01-01 00:00:03', 9), 2.5),
    ('down',        map('job', 'web',     'host', 'b'), toDateTime64('2026-01-01 00:00:04', 9), -1.5),
    ('temperature', map('job', 'sensors', 'host', 'a'), toDateTime64('2026-01-01 00:00:05', 9), 23.0),
    ('temperature', map('job', 'sensors', 'host', 'b'), toDateTime64('2026-01-01 00:00:06', 9), 19.5),
    ('humidity',    map('job', 'sensors', 'host', 'a'), toDateTime64('2026-01-01 00:00:07', 9), 60.0),
    ('humidity',    map('job', 'sensors', 'host', 'b'), toDateTime64('2026-01-01 00:00:08', 9), 55.5),
    ('latency',     map('job', 'api',     'host', 'c'), toDateTime64('2026-01-01 00:00:09', 9), 3.14);
`

// generatorAlphabet bundles the column / literal vocabulary the
// generator draws from. Keeping these tight makes most generated plans
// match at least *some* rows so the test catches optimizer bugs that
// would otherwise hide behind empty result sets.
var (
	propertyMetricNames = []string{"up", "down", "temperature", "humidity", "latency", "nope"}
	propertyFloats      = []float64{0.0, 1.0, 2.5, 19.5, 23.0, 60.0, -1.5}
	propertyColumns     = []string{"MetricName", "Value", "TimeUnix"}
)

// TestPropertyOptimizerSemanticEquivalence is the property test: for
// N randomly generated plans, assert that the optimizer preserves the
// row set when both plans are executed against the same chDB session.
//
// N defaults to 100; -short halves it to 10 so a `go test -short -tags
// chdb ./...` run stays under a minute. Each iteration logs the plan
// shape on failure so reproducing locally is just a matter of rerunning
// with the same -seed value.
func TestPropertyOptimizerSemanticEquivalence(t *testing.T) {
	const defaultN = 100
	const shortN = 10
	n := defaultN
	if testing.Short() {
		n = shortN
	}

	// Use a deterministic seed so failures are reproducible. The seed
	// can be flipped with -seed if a future test discovers an
	// adversarial plan worth pinning.
	seed := int64(20260514)
	rng := rand.New(rand.NewSource(seed))

	db := openPropertyChDB(t)
	if _, err := db.Exec(propertyDDL); err != nil {
		// The DDL is a multi-statement script. chdb-go's driver runs
		// one statement per Exec, so split on top-level semicolons.
		for _, stmt := range splitDDLStatements(propertyDDL) {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("seed exec failed: stmt=%q err=%v", stmt, err)
			}
		}
	}

	ctx := context.Background()
	opt := optimizer.Default()

	tried := 0
	dropped := 0
	for tried < n {
		plan := generatePlan(rng, 0)
		if plan == nil {
			dropped++
			continue
		}

		gotPre, errPre := runPlan(ctx, db, plan)
		if errPre != nil {
			// Generator produced something the emitter can't render
			// (e.g. a degenerate Projection list). Skip — this is
			// not what the property checks.
			dropped++
			continue
		}

		optimized := opt.Run(ctx, plan)
		gotPost, errPost := runPlan(ctx, db, optimized)
		if errPost != nil {
			t.Fatalf("optimized plan failed to execute (pre ran fine)\n--- pre ---\n%s\n--- post ---\n%s\n--- err ---\n%v",
				dumpPlan(plan), dumpPlan(optimized), errPost)
		}

		if !rowsetEqual(gotPre, gotPost) {
			t.Fatalf("semantic equivalence violated (seed=%d iter=%d)\n--- pre ---\n%s\n--- post ---\n%s\n--- pre rows ---\n%s\n--- post rows ---\n%s",
				seed, tried, dumpPlan(plan), dumpPlan(optimized),
				dumpRows(gotPre), dumpRows(gotPost))
		}

		tried++
	}
	if testing.Verbose() {
		t.Logf("property check: %d plans verified (%d dropped) against seed %d", tried, dropped, seed)
	}
}

// generatePlan builds a random plan tree with bounded depth. Returns
// nil to signal "skip this iteration" when the generator picks a shape
// that's known-ill (empty projection list, etc.).
//
// Depth budget: at depth 0 the generator picks any node type; once
// depth ≥ 3 it bottoms out into a Scan to keep trees small. This
// covers the three plan shapes the optimizer is expected to handle:
//
//	Scan(table)
//	Filter(<expr>, Scan(table))
//	Filter(<expr>, Filter(<expr>, Scan(table)))   ← fusion target
//	Project(<projs>, Scan(table))                 ← pushdown target
//	Filter(<expr>, Project(<projs>, Scan(table))) ← transpose target
func generatePlan(rng *rand.Rand, depth int) chplan.Node {
	if depth >= 3 {
		return makeScan()
	}
	switch rng.Intn(4) {
	case 0:
		return makeScan()
	case 1:
		return &chplan.Filter{
			Input:     generatePlan(rng, depth+1),
			Predicate: generatePredicate(rng, 0),
		}
	case 2:
		inner := generatePlan(rng, depth+1)
		projs := generateProjections(rng)
		if len(projs) == 0 {
			return inner
		}
		return &chplan.Project{
			Input:       inner,
			Projections: projs,
		}
	case 3:
		// Two-deep filter for fusion coverage.
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     generatePlan(rng, depth+2),
				Predicate: generatePredicate(rng, 0),
			},
			Predicate: generatePredicate(rng, 0),
		}
	}
	return makeScan()
}

func makeScan() chplan.Node {
	// Default columns: empty list -> emitter renders `SELECT *`. The
	// emitter then projects every base column, which the runner can
	// scan. Map columns get the toJSONString shim via rewriteMapProjections.
	return &chplan.Scan{Table: propertyTable}
}

// generateProjections picks 1–3 columns from the alphabet, optionally
// with an alias. The Attributes Map column is excluded — the property
// runner doesn't currently scan Map columns in arbitrary projection
// positions, only the top-level SELECT (and the toJSONString shim
// only rewrites SELECT-list projections by alias).
func generateProjections(rng *rand.Rand) []chplan.Projection {
	count := 1 + rng.Intn(3)
	used := map[string]bool{}
	out := make([]chplan.Projection, 0, count)
	for i := 0; i < count; i++ {
		col := propertyColumns[rng.Intn(len(propertyColumns))]
		if used[col] {
			continue
		}
		used[col] = true
		out = append(out, chplan.Projection{
			Expr: &chplan.ColumnRef{Name: col},
			// No alias: keeps the projection list scannable by the
			// runner without alias bookkeeping. Tests for alias paths
			// live in TXTAR fixtures.
		})
	}
	return out
}

// generatePredicate builds a random predicate. The depth budget caps
// boolean nesting at 2 so trees stay scannable.
//
// The literal-constant arm produces shapes like `LitBool(true) AND P`
// that the ConstantFold rule rewrites — those are the most important
// shapes to round-trip because the rule changes the SQL byte-for-byte.
func generatePredicate(rng *rand.Rand, depth int) chplan.Expr {
	if depth >= 2 {
		return generateLeafPredicate(rng)
	}
	switch rng.Intn(6) {
	case 0, 1, 2:
		return generateLeafPredicate(rng)
	case 3:
		return &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  generatePredicate(rng, depth+1),
			Right: generatePredicate(rng, depth+1),
		}
	case 4:
		return &chplan.Binary{
			Op:    chplan.OpOr,
			Left:  generatePredicate(rng, depth+1),
			Right: generatePredicate(rng, depth+1),
		}
	case 5:
		// `true AND P` — fodder for ConstantFold.
		return &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: generatePredicate(rng, depth+1),
		}
	}
	return generateLeafPredicate(rng)
}

// generateLeafPredicate emits a single comparison against a known
// column. Choices:
//
//	MetricName <eq/ne> <literal-name>
//	Value <gt/lt/ge/le/eq> <literal-float>
func generateLeafPredicate(rng *rand.Rand) chplan.Expr {
	switch rng.Intn(2) {
	case 0:
		op := chplan.OpEq
		if rng.Intn(2) == 0 {
			op = chplan.OpNe
		}
		return &chplan.Binary{
			Op:    op,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: propertyMetricNames[rng.Intn(len(propertyMetricNames))]},
		}
	default:
		ops := []chplan.BinaryOp{chplan.OpGt, chplan.OpLt, chplan.OpGe, chplan.OpLe, chplan.OpEq}
		return &chplan.Binary{
			Op:    ops[rng.Intn(len(ops))],
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.LitFloat{V: propertyFloats[rng.Intn(len(propertyFloats))]},
		}
	}
}

// runPlan emits the plan, applies the Map-column rewrite, and returns
// the result row set as a [][]any. Map columns surface as
// map[string]any (decoded from toJSONString output); time columns
// surface as RFC3339Nano strings. Numeric cells stay as int64/float64
// per chdb-go's parquet driver.
func runPlan(ctx context.Context, db *sql.DB, plan chplan.Node) ([][]any, error) {
	sqlStr, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}
	rewritten := rewriteMapProjectionsLocal(sqlStr)

	rows, err := db.QueryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	colCount := projectionCount(rewritten)
	if colCount == 0 {
		return nil, fmt.Errorf("cannot determine projection count for %q", rewritten)
	}

	var out [][]any
	for rows.Next() {
		cells := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		row := make([]any, colCount)
		for i, v := range cells {
			row[i] = decodeCellLocal(v)
		}
		out = append(out, row)
	}
	if err := tolerantRowsErrLocal(rows.Err()); err != nil {
		return nil, fmt.Errorf("rows.Err: %w", err)
	}
	return out, nil
}

// rowsetEqual reports whether two row sets are equal as multisets.
// Both sides are sorted by their JSON serialization before
// reflect.DeepEqual so the optimizer is free to reorder reads.
func rowsetEqual(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	sortRows(a)
	sortRows(b)
	return reflect.DeepEqual(a, b)
}

func sortRows(rs [][]any) {
	sort.SliceStable(rs, func(i, j int) bool {
		return rowKey(rs[i]) < rowKey(rs[j])
	})
}

func rowKey(r []any) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func dumpRows(rs [][]any) string {
	b, _ := json.MarshalIndent(rs, "", "  ")
	return string(b)
}

func dumpPlan(n chplan.Node) string {
	sqlStr, args, err := chsql.Emit(context.Background(), n)
	if err != nil {
		return fmt.Sprintf("<emit error: %v>", err)
	}
	return fmt.Sprintf("sql: %s\nargs: %#v", sqlStr, args)
}

// openPropertyChDB opens a fresh ephemeral chDB session owned by t.
// Mirrors the openChDB helper in test/spec/runner_chdb.go but kept
// local so the property test doesn't import a `_test` package.
func openPropertyChDB(t *testing.T) *sql.DB {
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

// chdb-go quirks (see test/spec/runner_chdb.go for the canonical
// versions). These are re-declared here so the optimizer property
// test stays self-contained — internal/optimizer/ doesn't depend on
// test/spec/ and we don't want to flip that for a 30-line helper.

const chdbEOFSentinelLocal = "empty row"

func tolerantRowsErrLocal(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinelLocal) {
		return nil
	}
	return err
}

// rewriteMapProjectionsLocal wraps every top-level SELECT projection
// whose alias is "Attributes" in toJSONString(...). The transform is
// keyed off the alias only because the generator never picks the
// Attributes column for an explicit Project — the only path it can
// reach the outer SELECT is via the implicit `SELECT *` Scan, where
// the column carries its base name as its alias.
func rewriteMapProjectionsLocal(query string) string {
	const sel = "SELECT "
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, sel) {
		return query
	}
	// Find the first depth-0 " FROM " — that bounds the projection list.
	rest := query[len(sel):]
	depth := 0
	fromAt := -1
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+6 <= len(rest) && strings.EqualFold(rest[i:i+6], " FROM ") {
			fromAt = i
			break
		}
	}
	if fromAt < 0 {
		return query
	}
	head := rest[:fromAt]
	tail := rest[fromAt:]
	projs := splitTopLevelCommas(head)
	for i, p := range projs {
		expr, alias := splitOuterAlias(p)
		bare := alias
		if bare == "" {
			bare = strings.Trim(strings.TrimSpace(expr), "`")
		}
		if bare != "Attributes" {
			continue
		}
		projs[i] = "toJSONString(" + expr + ") AS `Attributes`"
	}
	return sel + strings.Join(projs, ", ") + tail
}

func splitTopLevelCommas(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		depth int
		inStr byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
			buf.WriteByte(c)
		case c == '\'' || c == '`':
			inStr = c
			buf.WriteByte(c)
		case c == '(':
			depth++
			buf.WriteByte(c)
		case c == ')':
			depth--
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

func splitOuterAlias(s string) (expr, alias string) {
	lower := strings.ToLower(s)
	depth := 0
	inStr := byte(0)
	for i := 0; i+4 <= len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && inStr == 0 && lower[i:i+4] == " as " {
			a := strings.TrimSpace(s[i+4:])
			a = strings.Trim(a, "`")
			return strings.TrimSpace(s[:i]), a
		}
	}
	return s, ""
}

func projectionCount(query string) int {
	const sel = "SELECT "
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, sel) {
		return 0
	}
	rest := query[len(sel):]
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+6 <= len(rest) && strings.EqualFold(rest[i:i+6], " FROM ") {
			head := rest[:i]
			return len(splitTopLevelCommas(head))
		}
	}
	// `SELECT *` with no FROM in the literal text (rare): one slot.
	if strings.TrimSpace(rest) == "*" {
		return 1
	}
	// `SELECT *` with FROM at the outermost level should have been
	// caught above; an unrecognised shape falls through with the
	// count of `*` columns in our fixed schema.
	if strings.HasPrefix(rest, "*") {
		return 4 // MetricName, Attributes, TimeUnix, Value
	}
	return 0
}

// decodeCellLocal mirrors decodeCell in test/spec/runner_chdb.go.
// time.Time → RFC3339Nano, JSON-shaped strings → decoded any tree,
// everything else passes through.
func decodeCellLocal(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return decodeStringLocal(string(x))
	case string:
		return decodeStringLocal(x)
	default:
		return v
	}
}

func decodeStringLocal(s string) any {
	trim := strings.TrimSpace(s)
	if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') {
		var v any
		if err := json.Unmarshal([]byte(trim), &v); err == nil {
			return v
		}
	}
	return s
}

// splitDDLStatements splits propertyDDL on top-level semicolons so
// chdb-go's single-statement Exec gets one piece at a time. Strings
// are shielded.
func splitDDLStatements(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		inStr bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case c == ';' && !inStr:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}
