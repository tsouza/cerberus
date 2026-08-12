package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The three tests here pin #2074, whose two halves reinforce each other: a
// cross-test interaction in internal/chsql's chDB suite that failed 100% of
// the time, and the absence of any CI lane that ran that suite. Either half
// alone is survivable; together they let a deterministic failure sit on main.
//
// chdb-go caches ONE session per process (#1987), so every chDB test in
// internal/chsql shares one engine. When they also shared one `default`
// database, table DDL leaked forward: a hand-rolled three-column
// `otel_metrics_sum` seeded by the fused-subquery differential stayed behind,
// and the next test to emit a metric selector — which fans over
// `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')`, and
// so needs every regex-matched table to carry the referenced column — could
// not resolve `MetricName`. internal/chsql/chdb_harness_test.go now gives
// each test its own database.

const chsqlPkgDir = "../../internal/chsql"

// chdbOpenRE matches a direct chDB connection open. The harness is the only
// legitimate site; everywhere else must go through OpenIsolatedChDB so the
// per-test database, and its drop-on-cleanup, cannot be bypassed.
var chdbOpenRE = regexp.MustCompile(`sql\.Open\(\s*"chdb"`)

// chsqlHarnessFile owns the one permitted sql.Open("chdb", ...) call.
const chsqlHarnessFile = "chdb_harness_test.go"

// TestChSQLChDBTestsUseIsolatedDatabase pins that no chDB test in
// internal/chsql opens the session directly. A direct open lands in whatever
// database the previous test left current and registers no cleanup that drops
// what it creates, which is exactly the shape that broke
// TestAbsentOverTime_OffsetOutputGrid. Routing every test through
// OpenIsolatedChDB makes the leak structurally impossible rather than a rule
// each new seed has to remember.
func TestChSQLChDBTestsUseIsolatedDatabase(t *testing.T) {
	t.Parallel()

	sawHarness := false
	for _, path := range chsqlTestFiles(t) {
		base := filepath.Base(path)
		// The harness owns the one permitted open, and its doc comment quotes
		// the call it replaces.
		if base == chsqlHarnessFile {
			sawHarness = true
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !chdbOpenRE.MatchString(line) {
				continue
			}
			t.Errorf("internal/chsql/%s:%d opens chDB directly: %s\n"+
				"chdb-go caches one session per process, so a direct open inherits the "+
				"previous test's current database and leaves its tables behind (#2074). "+
				"Use chsql.OpenIsolatedChDB(t) (unqualified inside `package chsql`), which "+
				"creates a per-test database and drops it on cleanup.",
				base, i+1, strings.TrimSpace(line))
		}
	}
	if !sawHarness {
		t.Errorf("internal/chsql/%s is gone — the per-test-database harness every chDB "+
			"test in the package depends on. Without it this scan passes vacuously.",
			chsqlHarnessFile)
	}
}

// TestChSQLChDBTestsAreSerial pins the precondition OpenIsolatedChDB's
// isolation rests on. The per-test database is selected with `USE`, and
// because chdb-go caches one session per process that switch is
// process-global: two chDB tests running concurrently would share whichever
// database `USE` ran last, silently restoring the very coupling #2074 removed.
// Nothing here constrains the package's non-chDB tests, which are pure
// string-shape assertions and run concurrently by design.
func TestChSQLChDBTestsAreSerial(t *testing.T) {
	t.Parallel()

	for _, path := range chsqlTestFiles(t) {
		base := filepath.Base(path)
		if !strings.HasSuffix(base, "_chdb_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "t.Parallel()") {
				continue
			}
			t.Errorf("internal/chsql/%s:%d marks a chDB test parallel.\n"+
				"OpenIsolatedChDB selects its per-test database with `USE`, which is "+
				"process-global because chdb-go caches one session per process (#1987). "+
				"Concurrent chDB tests would collapse back onto one database (#2074).",
				base, i+1)
		}
	}
}

// TestChSQLIsInTestChDBLane pins the CI half of #2074. Fixing the cross-test
// interaction is unenforced while no lane executes the suite: internal/chsql's
// only chdb-tagged CI touch was `chdb-build`, which is build+vet by design,
// and the mutation lane builds untagged. A green board therefore proved
// nothing about this package under this tag. If the package list loses
// ./internal/chsql/..., the suite goes back to being a developer-only
// invocation and this test says so.
func TestChSQLIsInTestChDBLane(t *testing.T) {
	t.Parallel()

	const wantPkg = "./internal/chsql/..."

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	// The chdb.yml `Run handler tests (chDB)` step invokes this recipe by name,
	// so justRecipeBody's own t.Fatal on a missing recipe is the right failure.
	recipe := justRecipeBody(t, string(buf), "test-chdb")
	if !strings.Contains(recipe, wantPkg) {
		t.Errorf("Justfile `test-chdb` recipe does not run %s.\n"+
			"internal/chsql's chDB round-trip suite is the only layer that executes emitted "+
			"SQL against a real engine from inside the emitter package. Without it in this "+
			"lane, a failure that reproduces 100%% of the time locally is invisible to CI "+
			"(#2074).\nrecipe: %s", wantPkg, strings.TrimSpace(recipe))
	}
}

// chsqlTestFiles lists internal/chsql's Go test files.
func chsqlTestFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(chsqlPkgDir, "*_test.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", chsqlPkgDir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no test files under %s — the scan would pass vacuously", chsqlPkgDir)
	}
	return paths
}
