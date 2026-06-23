//go:build integration

// strictscan_integration_test.go — the production strict-scan differential.
//
// # WHY THIS LANE EXISTS
//
// The chDB round-trip lane (runner_chdb.go) and the text goldens are both
// structurally BLIND to a documented production incident class: chDB (libchdb,
// an embedded ClickHouse fronted by chdb-go's Parquet driver) leniently
// COERCES result column types into the Go destinations a Scan supplies — a
// UInt8/UInt64 column happily lands in a *float64, a Map can be massaged, etc.
// Production cerberus does NOT use chDB: it talks to a real ClickHouse over the
// native protocol via clickhouse-go/v2, whose Scan is STRICT — a type that does
// not match the destination is a hard error (`code 47`, `cannot convert`,
// "converting ... to ..."), which the API layer surfaces to Grafana as a 502.
//
// The concrete shape (project memory "chDB vs prod-CH scan strictness"): an
// emitter that projects a `Value` column as a CH integer expression (UInt8 from
// a comparison, UInt64 from a count) instead of wrapping it in `toFloat64(...)`
// passes every chDB golden — chdb-go coerces it into the cursor's `*float64` —
// yet 502s in production the instant clickhouse-go strict-scans the same row.
// The chdb-tagged `expected_rows` cells only ever observe the COERCED value, so
// they can never catch an emit-type bug; only a real-CH strict scan does.
//
// # WHAT THIS TEST DOES
//
// It spins ONE real `clickhouse/clickhouse-server` via testcontainers (the
// exact pattern internal/chclient/{client,columnar}_integration_test.go and the
// schema-integration lane already use), then walks the round-trip-capable
// fixtures under test/spec/{promql,logql,traceql}. For each fixture it:
//
//  1. applies the fixture's own `-- seed --` DDL+INSERTs (split, OR-REPLACE
//     promoted for idempotency in the shared DB, ResourceAttributes-backfilled)
//     — reusing the SAME build-tag-free prep pipeline (roundtrip_prep.go) the
//     chDB runner uses, so the seed is byte-identical;
//  2. substitutes `now64(...)` with the deterministic anchor literal (consuming
//     the now64(?) arg slot) — again the shared pipeline;
//  3. executes the RAW emitted `-- sql --` (native Map / DateTime64 / Float64
//     columns — NOT the chDB `toJSONString` Map-wrap that masks the divergence)
//     through `chclient.Client.QueryCursor`, which scans positionally into the
//     EXACT destination types the production matrix decoder uses
//     (`MetricName string`, `map[string]string`, `time.Time`, `float64`; plus a
//     5th `Metadata` String for the Loki log-stream path);
//  4. drains the cursor and asserts NO scan-type error.
//
// Step 3 is the load-bearing one: QueryCursor's rowsCursor.Next() runs the very
// `rows.Scan(&s.MetricName, &labels, &s.Timestamp, &s.Value)` production runs,
// so a column whose CH type can't strict-convert into its Go destination fails
// here exactly as it would in prod — the failure the chDB lane swallows.
//
// SCOPE: the three query heads' round-trip fixtures (the metric/range/vector-
// join emitters where Value-typed columns live are all under promql; logql and
// traceql exercise the Map + log-stream + trace projections). The optimizer
// lane has no round-trip fixtures. Args are taken verbatim from each fixture's
// `-- args --` block, so no synthetic placeholder binding is needed.
//
// Gated by the `integration` build tag (Docker required); INFORMATIONAL — wired
// on pull_request + push but NOT a required status check (see
// .github/workflows/strict-scan.yml). Promote to required once it has a green
// track record, the same playbook the compatibility + schema-integration gates
// used.
package spec_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/test/spec"
)

// strictScanHeads are the per-head fixture directories the differential
// walks. The optimizer lane is excluded because it carries no round-trip
// (seed + sql + expected_rows) fixtures.
var strictScanHeads = []string{"promql", "logql", "traceql"}

// strictScanCHImage pins the same ClickHouse server image the existing
// chclient integration tests + schema-integration lane use, so the strict-scan
// behaviour observed here matches the version those lanes exercise.
const strictScanCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// strictScanDB is the database the fixtures' unqualified table names resolve
// against. The OTel exporter defaults to `otel`; cerberus's read path issues
// unqualified table references, so the connection's default database must hold
// the seeded tables.
const strictScanDB = "otel"

// TestStrictScanDifferential executes every round-trip-capable spec fixture's
// emitted SQL against a real ClickHouse through the production cursor and fails
// on any strict-scan type error — the prod-vs-chDB divergence the chDB goldens
// cannot see.
func TestStrictScanDifferential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := newStrictScanClient(ctx, t)

	repoRoot := repoRootFromTest(t)
	var ran, skippedNonRT, skippedNonMatrix int
	for _, head := range strictScanHeads {
		dir := filepath.Join(repoRoot, "test", "spec", head)
		spec.Walk(t, dir, func(t *testing.T, c *spec.Case) {
			rt, err := spec.LoadRoundTrip(c)
			if err != nil {
				t.Fatalf("LoadRoundTrip: %v", err)
			}
			if !rt.IsRoundTrip() || strings.TrimSpace(rt.SQL) == "" {
				skippedNonRT++
				return
			}
			switch runStrictScanCase(ctx, t, client, rt) {
			case caseRan:
				ran++
			case caseNonMatrix:
				skippedNonMatrix++
			}
		})
	}

	// A guard against a silently-empty corpus: if the walk matched no
	// matrix-shaped round-trip fixtures the test would pass vacuously,
	// defeating the lane.
	if ran == 0 {
		t.Fatalf("strict-scan differential ran zero matrix-shaped fixtures (non-round-trip=%d, non-matrix=%d) — corpus glob or matrix detection is broken", skippedNonRT, skippedNonMatrix)
	}
	t.Logf("strict-scan differential: strict-scanned %d matrix-shaped fixtures against real ClickHouse (%d non-round-trip + %d non-matrix-shape skipped)", ran, skippedNonRT, skippedNonMatrix)
}

// caseOutcome classifies what runStrictScanCase did with a fixture.
type caseOutcome int

const (
	// caseRan: the fixture's SQL produced the matrix column shape and was
	// strict-scanned through the production cursor.
	caseRan caseOutcome = iota
	// caseNonMatrix: the fixture's SQL is not the (MetricName, Attributes,
	// TimeUnix, Value [, Metadata]) matrix shape the production cursor
	// decodes — it belongs to a different decoder (label values, Tempo
	// search rows, index stats, …) and is out of this lane's scope.
	caseNonMatrix
)

// matrixColumns is the exact column projection the production matrix cursor
// (chclient.rowsCursor) binds positionally. A fixture whose emitted SQL
// produces these columns (optionally + a trailing "Metadata" String for the
// Loki log-stream path) is in scope; anything else belongs to a different
// production decoder and is skipped.
var matrixColumns = []string{"MetricName", "Attributes", "TimeUnix", "Value"}

// metadataColumn is the optional 5th column the Loki log-stream projection
// appends (mirrors chclient's metadataColumn const).
const metadataColumn = "Metadata"

// runStrictScanCase seeds the fixture's tables, probes the emitted SQL's
// result column shape, and — only when that shape is the production matrix
// projection — drains it through the production cursor, failing on any
// strict-scan / type-conversion error. The seed + now64 substitution reuse the
// shared roundtrip_prep.go pipeline so the SQL executed here is byte-identical
// to what the chDB lane runs — except no Map-column toJSONString wrap, which is
// the whole point: the cursor strict-scans the NATIVE Map / DateTime64 / Float64
// columns, exactly as production does.
//
// The column-shape probe scopes the lane to the matrix decoder's corpus: the
// production cursor is only ever fed (MetricName, Attributes, TimeUnix, Value
// [, Metadata]) results, so a fixture whose SQL projects a label-values column,
// a 13-column Tempo search row, an index-stats tuple, etc. belongs to a
// different decoder and would produce a meaningless "expected N destination
// arguments, not 4" mismatch rather than a real type-coercion finding.
func runStrictScanCase(ctx context.Context, t *testing.T, client *chclient.Client, rt *spec.RoundTripSections) caseOutcome {
	t.Helper()

	applyStrictScanSeed(ctx, t, client, rt.Seed)

	query, args := spec.SubstituteNow64(rt.SQL, rt.Args)

	matrix, err := isMatrixShape(ctx, client, query, args)
	if err != nil {
		// The query was rejected by the server before any row decoded. A
		// genuine server-side type reject (NO_COMMON_TYPE etc.) is in scope;
		// an experimental-function gate (timeSeries* aggregates disabled by
		// default) is an environment limitation, not a cerberus bug — record
		// it as non-matrix so it doesn't red the lane.
		if isExperimentalFnError(err) {
			return caseNonMatrix
		}
		if isStrictScanError(err) {
			t.Fatalf("STRICT-SCAN / TYPE ERROR at query open — ClickHouse rejected the emitted SQL on type grounds (chDB goldens are blind to this):\n--- sql ---\n%s\n--- err ---\n%v", query, err)
		}
		t.Fatalf("query open failed against real ClickHouse:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
	}
	if !matrix {
		return caseNonMatrix
	}

	cur, err := client.QueryCursor(ctx, query, args...)
	if err != nil {
		t.Fatalf("QueryCursor (open) failed — query rejected by ClickHouse:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
	}
	defer func() { _ = cur.Close() }()

	for cur.Next() {
		_ = cur.Sample()
	}
	if err := cur.Err(); err != nil {
		if isStrictScanError(err) {
			t.Fatalf("STRICT-SCAN TYPE ERROR — emitted SQL produced a matrix column the production cursor cannot strict-scan (chDB goldens are blind to this):\n--- sql ---\n%s\n--- err ---\n%v", query, err)
		}
		// Any other drain error (transport, resource cap) is still a failure
		// for this fixture's SQL against a real server.
		t.Fatalf("cursor drain failed:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
	}
	return caseRan
}

// isMatrixShape runs the query, reads the result column names, and reports
// whether they are the production matrix projection (MetricName, Attributes,
// TimeUnix, Value) optionally followed by a Metadata column. The probe drains
// no rows — Columns() is populated as soon as the query is open — so it is
// cheap. A non-nil error means the server rejected the query at open time.
func isMatrixShape(ctx context.Context, client *chclient.Client, query string, args []any) (bool, error) {
	rows, err := client.Conn().Query(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	cols := rows.Columns()
	if len(cols) != len(matrixColumns) && len(cols) != len(matrixColumns)+1 {
		return false, nil
	}
	for i, want := range matrixColumns {
		if cols[i] != want {
			return false, nil
		}
	}
	if len(cols) == len(matrixColumns)+1 && cols[len(matrixColumns)] != metadataColumn {
		return false, nil
	}
	return true, nil
}

// isExperimentalFnError reports whether err is ClickHouse refusing an
// experimental aggregate that is disabled by default in this server build (the
// timeSeries* family some Tempo metric lowerings emit). Production enables
// these via session settings; the bare testcontainer does not, so such a
// rejection is an environment gap, not a scan-type bug.
func isExperimentalFnError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "is experimental and disabled by default")
}

// applyStrictScanSeed runs the fixture's seed against the shared DB, reusing
// the build-tag-free split + ResourceAttributes backfill + OR-REPLACE promotion
// the chDB runner uses. CREATE OR REPLACE makes re-seeding idempotent across
// fixtures that share a table name (otel_metrics_gauge appears in ~250).
func applyStrictScanSeed(ctx context.Context, t *testing.T, client *chclient.Client, seed string) {
	t.Helper()
	for _, stmt := range spec.BackfillResourceAttributes(spec.SplitSeedStatements(seed)) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = spec.PromoteCreateTable(stmt)
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// isStrictScanError reports whether err is the strict-scan / type-conversion
// class this lane exists to catch: clickhouse-go's destination-type mismatch
// (the prod-vs-chDB divergence), surfaced through the cursor's `chclient: scan:`
// wrap. The substring set mirrors the error shapes clickhouse-go/v2 emits when
// a column type cannot be assigned to the Scan destination.
func isStrictScanError(err error) bool {
	if err == nil {
		return false
	}
	// Resource-cap and budget errors are wrapped concrete types, not
	// scan-type failures; exclude them explicitly so a genuine over-budget
	// fixture (none today) wouldn't masquerade as a scan bug.
	var tooMany *chclient.TooManySamplesError
	if errors.As(err, &tooMany) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"chclient: scan:",     // the cursor's Scan-failure wrap (cursor.go)
		"cannot convert",      // clickhouse-go destination mismatch
		"converting",          // "converting <T> to <U>" variants
		"unsupported type",    // driver-side destination rejection
		"can't scan",          // sql.Scan destination rejection
		"unexpected type",     // column/destination kind mismatch
		"clickhouse [code=47", // NO_COMMON_TYPE / type-related server reject
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// newStrictScanClient boots one ClickHouse container and returns a connected
// production chclient against it. The breaker is disabled so a single rejected
// fixture query never trips it and short-circuits the rest of the corpus.
func newStrictScanClient(ctx context.Context, t *testing.T) *chclient.Client {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		strictScanCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(strictScanDB),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:            host + ":" + port.Port(),
		Database:        strictScanDB,
		Username:        "cerberus",
		Password:        "cerberus",
		BreakerDisabled: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// repoRootFromTest resolves the repository root from the test's working
// directory (test/spec) so the fixture walk uses absolute paths regardless of
// where `go test` is invoked from.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	// The test binary runs with CWD = the package dir (test/spec). Walk up to
	// the module root by locating go.mod.
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", err)
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
