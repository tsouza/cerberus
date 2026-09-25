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
//     ProjectionPushdown) and the FilterAggregateTranspose rule all
//     have shots at firing without needing aggregates or windows.
//
// The stage nodes (Aggregate, the RangeWindow family, set-op chains) are
// generated in property_stage_gen_test.go, the two joins in
// property_join_gen_test.go and TopK in property_topk_gen_test.go, each
// over a seed built for the shape; property_coverage_test.go is the
// ledger that fails when a kind the optimizer can act on stops being
// round-tripped.
//
// chDB quirks the comparison code papers over:
//
//   - Float64 precision: every value in the seed is exact in IEEE-754
//     (no irrational-arithmetic surprises). Comparison is on raw
//     float64 bits.
//   - Map columns: runPlan routes the emitted SQL through the same
//     testsql rewrite pipeline test/spec's runner uses, so every Map
//     column that reaches the outermost SELECT is wrapped in
//     toJSONString and decoded back into a Go map before comparison.
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
	"github.com/tsouza/cerberus/internal/testsql"
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
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
INSERT INTO otel_metrics_histogram VALUES
    ('latency_bucket', map('job', 'api',     'host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 6,  12.0, [1, 2, 3], [1.0, 2.0, 3.0]),
    ('latency_bucket', map('job', 'api',     'host', 'b'), toDateTime64('2026-01-01 00:00:01', 9), 10, 30.0, [4, 3, 3], [1.0, 2.0, 3.0]),
    ('size_bucket',    map('job', 'sensors', 'host', 'a'), toDateTime64('2026-01-01 00:00:02', 9), 4,  8.0,  [2, 1, 1], [0.5, 1.5, 2.5]);
CREATE OR REPLACE TABLE otel_metrics_gauge_join (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
INSERT INTO otel_metrics_gauge_join VALUES
    ('requests', map('job', 'api', 'host', 'a'),    toDateTime64('2026-01-01 00:01:00', 9), 1.0),
    ('requests', map('job', 'api', 'host', 'b'),    toDateTime64('2026-01-01 00:01:01', 9), 2.0),
    ('requests', map('job', 'web', 'host', 'a'),    toDateTime64('2026-01-01 00:01:02', 9), 3.0),
    ('requests', map('job', 'web', 'host', 'c'),    toDateTime64('2026-01-01 00:01:03', 9), 4.0),
    ('errors',   map('job', 'api', 'host', 'a'),    toDateTime64('2026-01-01 00:01:04', 9), 5.0),
    ('errors',   map('job', 'api', 'host', 'b'),    toDateTime64('2026-01-01 00:01:05', 9), 6.0),
    ('errors',   map('job', 'web', 'host', 'a'),    toDateTime64('2026-01-01 00:01:06', 9), 7.0),
    ('errors',   map('job', 'web', 'host', 'c'),    toDateTime64('2026-01-01 00:01:07', 9), 8.0),
    ('capacity', map('job', 'api', 'tier', 'gold'), toDateTime64('2026-01-01 00:01:08', 9), 9.0),
    ('capacity', map('job', 'web', 'tier', 'silver'), toDateTime64('2026-01-01 00:01:09', 9), 10.0),
    ('quota',    map('job', 'api', 'tier', 'gold'), toDateTime64('2026-01-01 00:01:10', 9), 11.0),
    ('quota',    map('job', 'web', 'tier', 'silver'), toDateTime64('2026-01-01 00:01:11', 9), 12.0);
CREATE OR REPLACE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind String,
    Duration UInt64,
    Timestamp DateTime64(9),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp, SpanId);
INSERT INTO otel_traces VALUES
    ('t1', 's1', '',        'GET /',     'Server', 100, toDateTime64('2026-01-01 00:00:00', 9), map('service.name', 'frontend')),
    ('t1', 's2', 's1',      'GET /api',  'Server', 80,  toDateTime64('2026-01-01 00:00:01', 9), map('service.name', 'api')),
    ('t1', 's3', 's2',      'SELECT',    'Client', 30,  toDateTime64('2026-01-01 00:00:02', 9), map('service.name', 'db')),
    ('t1', 's4', 's2',      'GET',       'Client', 20,  toDateTime64('2026-01-01 00:00:03', 9), map('service.name', 'cache')),
    ('t2', 'r1', '',        'POST /api', 'Server', 50,  toDateTime64('2026-01-02 00:00:00', 9), map('service.name', 'api')),
    ('t2', 'r2', 'r1',      'INSERT',    'Client', 40,  toDateTime64('2026-01-02 00:00:01', 9), map('service.name', 'db')),
    ('t3', 'o1', '',        'GET /',     'Server', 60,  toDateTime64('2026-01-03 00:00:00', 9), map('service.name', 'frontend')),
    ('t3', 'o2', 'missing', 'GET /api',  'Server', 45,  toDateTime64('2026-01-03 00:00:01', 9), map('service.name', 'api')),
    ('t3', 'o3', 'o2',      'SELECT',    'Client', 15,  toDateTime64('2026-01-03 00:00:02', 9), map('service.name', 'db'));
`

// propertyHistogramTable is the classic-histogram seed the
// HistogramQuantile stage shape reads. The gauge table carries no
// BucketCounts / ExplicitBounds arrays, so that shape cannot be
// generated over it.
const propertyHistogramTable = "otel_metrics_histogram"

// propertyJoinTable is the seed built for VectorJoin, and the one TopK
// ranks over. Its series come in two shapes so every matching the join
// generator draws is well-defined by construction rather than by accident:
//
//   - `requests` and `errors` carry the SAME four (job, host) series, so
//     a full-Attributes match pairs them one-to-one, and `job` repeats
//     across hosts so they are the "many" side of a group_left/right.
//   - `capacity` and `quota` carry ONE series per job (with a `tier`
//     label to carry through group_left(tier)), so they are the "one"
//     side under on(job) / ignoring(host, tier) and pair one-to-one with
//     each other under every matching the generator draws.
//
// Every Value is a distinct small integer: distinct so a TopK ranking
// over any partition the generator can draw has no tie for ClickHouse to
// break arbitrarily (the optimized plan is a different query, and a tie
// broken the other way would flake rather than report), small integers
// so every binary-op result is exact in IEEE-754. Every TimeUnix is
// distinct too, so the join's per-side argMax has no tie either.
const propertyJoinTable = "otel_metrics_gauge_join"

// propertySpansTable is the spans seed StructuralJoin reads. Three traces
// give every structural relation something to match and something to
// refuse: t1 is a rooted four-span tree (frontend → api → {db, cache}, so
// the leaves are siblings), t2 a rooted two-span chain, and t3 a root
// beside an ORPHAN chain (o2's parent is missing, so o2 and o3 are
// unrooted) — the rows the rootedness gate on the left side must never
// let through. Timestamps fall on three different days so a request
// window can exclude a whole trace.
const propertySpansTable = "otel_traces"

// propertySessionSettings are executed once against the session before
// any plan runs.
//
// The two ClickHouse-native grid aggregates behind RangeWindowGridNative
// and RangeWindowStaleResample — timeSeriesRateToGrid and
// timeSeriesResampleToGridWithStaleness — are gated behind this setting
// and answer UNKNOWN_AGGREGATE_FUNCTION without it. Production reaches
// them through the internal/chopt capability registry rather than a bare
// SET; here the session is the harness's own, so enabling it directly is
// what makes the correctness-critical native arm reachable at all.
var propertySessionSettings = []string{
	"SET allow_experimental_time_series_aggregate_functions = 1",
}

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
// N defaults to 100; -short cuts it to 10 so a `go test -short -tags
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
	for _, setting := range propertySessionSettings {
		if _, err := db.Exec(setting); err != nil {
			t.Fatalf("session setting %q failed: %v", setting, err)
		}
	}
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

	// verified accumulates the node kinds of plans that actually
	// completed a round-trip. Counting kinds at GENERATION time would
	// credit a shape that never executes — every iteration of it takes
	// the loop's `continue` — so coverage is measured on the plans the
	// property really checked.
	verified := map[string]bool{}

	dropBudget := n * maxDroppedPlansPerVerified
	tried := 0
	dropped := 0
	var lastDropErr error
	for tried < n {
		plan := generatePlan(rng, 0)

		gotPre, errPre := runPlan(ctx, db, executablePropertyBaseline(plan))
		if errPre != nil {
			// Generator produced something the emitter can't render
			// (e.g. a degenerate Projection list). Skip — this is
			// not what the property checks.
			dropped++
			lastDropErr = errPre
			if testing.Verbose() {
				t.Logf("dropped plan (pre-optimizer run failed): %v\n%s", errPre, dumpPlan(plan))
			}
			if dropped > dropBudget {
				t.Fatalf("drop budget exhausted: %d generated plans failed their PRE-optimizer "+
					"run after verifying only %d of %d (budget %d = %d x maxDroppedPlansPerVerified). "+
					"Emission is broken for the generated shapes, so the property was never checked; "+
					"last drop error: %v",
					dropped, tried, n, dropBudget, n, lastDropErr)
			}
			continue
		}

		optimized := opt.Run(ctx, plan)
		gotPost, errPost := runPlan(ctx, db, optimized)
		if errPost != nil {
			t.Fatalf("optimized plan failed to execute (pre ran fine)\n--- pre ---\n%s\n--- post ---\n%s\n--- err ---\n%v",
				dumpPlan(plan), dumpPlan(optimized), errPost)
		}

		recordVerifiedKinds(verified, plan)
		recordVerifiedKinds(verified, optimized)

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
	assertOptimizerKindsCovered(t, verified)
}

// maxDroppedPlansPerVerified bounds the generator's drop budget as a
// multiple of the plans the property actually verifies, so the bound
// scales with -short instead of being a second magic number beside n.
//
// A drop is a generated plan whose PRE-optimizer run failed to execute —
// a generator/emitter mismatch, not a property violation, so the
// iteration is discarded rather than failed. Under a healthy generator
// drops are rare (the `SELECT *` column-count bug that made every
// wildcard plan drop was fixed in runPlan), so any run that discards
// twice as many plans as it verifies is reporting a regression in
// emission, not sampling noise. Without the budget that regression made
// the loop spin forever — `tried` only advances on a successful pre-run —
// and a test that hangs reports nothing at all.
const maxDroppedPlansPerVerified = 2

// generatePlan builds a random plan tree with bounded depth. It always
// returns a plan: every arm bottoms out in a Scan, and the Project arm
// falls back to its own input when the projection list comes out empty,
// so callers never have to handle a nil node.
//
// Depth budget: at depth 0 the generator picks any node type; once
// depth ≥ 3 it bottoms out into a Scan to keep trees small. The leaf
// grammar covers the shapes the predicate-pushdown batch works on:
//
//	Scan(table)
//	Filter(<expr>, Scan(table))
//	Filter(<expr>, Filter(<expr>, Scan(table)))   ← fusion target
//	Project(<projs>, Scan(table))                 ← pushdown target
//	Filter(<expr>, Project(<projs>, Scan(table))) ← transpose target
//
// and the stage arm covers ProjectionPushdown's own shapes (3)–(9),
// each generated directly over Scan or Filter(Scan) because that is the
// adjacency applyStageScan matches. See generateStagePlan. The last arm
// draws one of the shapes that read a seed of their own — VectorJoin,
// StructuralJoin and TopK — with the same weight as the whole stage
// family, so that each of the three is drawn often enough per run for
// the coverage ledger to pin it rather than depend on the seed's luck.
func generatePlan(rng *rand.Rand, depth int) chplan.Node {
	if depth >= 3 {
		return makeScan()
	}
	switch rng.Intn(7) {
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
	case 4, 5:
		return generateStagePlan(rng)
	case 6:
		return generateSeededShape(rng)
	}
	return makeScan()
}

// generateSeededShape draws one of the shapes that cannot be generated
// over the leaf grammar's gauge seed and read a seed of their own.
func generateSeededShape(rng *rand.Rand) chplan.Node {
	switch rng.Intn(3) {
	case 0:
		return generateVectorJoin(rng)
	case 1:
		return generateStructuralJoin(rng, 0)
	default:
		return generateTopK(rng)
	}
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
	for range count {
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

// propertySeedColumns is the seed's table → column catalog, the fallback
// the star expansion below resolves a bare `SELECT * FROM <table>`
// against.
var propertySeedColumns = testsql.SeedTableColumns(propertyDDL)

// runPlan emits the plan, applies the Map-column rewrite, and returns
// the result row set as a [][]any. Map columns surface as
// map[string]any (decoded from toJSONString output); time columns
// surface as RFC3339Nano strings. Numeric cells stay as int64/float64
// per chdb-go's parquet driver.
//
// The rewrite is the same four-pass pipeline test/spec's runner applies
// (testsql), not a local copy: chdb-go cannot decode a Map cell, and
// hands back NULL for it AND for every column after it, so a Map that
// reaches the outermost SELECT raw silently blanks the rest of the row
// on both sides of the comparison. The star expansion is what makes the
// leaf grammar's `SELECT *` shapes compare their whole row.
func runPlan(ctx context.Context, db *sql.DB, plan chplan.Node) ([][]any, error) {
	sqlStr, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}
	rewritten := testsql.ExpandStarProjection(sqlStr, propertySeedColumns)
	rewritten = testsql.RewriteMapProjections(rewritten)
	rewritten = testsql.NestMapOrderBy(rewritten)
	rewritten = testsql.NestMapWhere(rewritten)

	rows, err := db.QueryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Ask the driver how many columns the result actually has. Deriving it
	// by parsing the SELECT list cannot see through `SELECT *`, which counts
	// as ONE projection and expands to the table's full width at scan time —
	// so every iteration of a `SELECT *`-shaped plan failed with "expected 4
	// destination arguments in Scan, not 1", took the loop's `continue`, and
	// the property was never checked.
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	colCount := len(cols)
	if colCount == 0 {
		return nil, fmt.Errorf("result set has no columns for %q", rewritten)
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
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
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
