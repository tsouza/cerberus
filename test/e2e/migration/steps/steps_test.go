package steps

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	messages "github.com/cucumber/messages/go/v34"
)

// pickleTags builds the tag list godog hands a Before hook.
func pickleTags(names ...string) []*messages.PickleTag {
	tags := make([]*messages.PickleTag, 0, len(names))
	for _, name := range names {
		tags = append(tags, &messages.PickleTag{Name: name})
	}
	return tags
}

// TestArchetypesOfSelectsSortsAndDeduplicates asserts the archetype vocabulary
// is read off the tags with its prefix stripped, sorted and deduplicated, and
// that the story and tier vocabularies are left alone.
func TestArchetypesOfSelectsSortsAndDeduplicates(t *testing.T) {
	t.Parallel()

	got := ArchetypesOf(pickleTags(
		"@MIG-01",
		"@tier0",
		"@archetype:victoriametrics",
		"@archetype:already-otel",
		"@archetype:victoriametrics",
	))
	want := []string{"already-otel", "victoriametrics"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ArchetypesOf = %v, want %v", got, want)
	}
}

// TestArchetypesOfIgnoresAnEmptyName asserts a bare `@archetype:` selects
// nothing rather than an empty-named fixture directory.
func TestArchetypesOfIgnoresAnEmptyName(t *testing.T) {
	t.Parallel()

	if got := ArchetypesOf(pickleTags("@archetype:", "@tier1")); len(got) != 0 {
		t.Fatalf("ArchetypesOf = %v, want none", got)
	}
}

// TestRequireArchetypeFixturesRejectsAMissingDirectory asserts a scenario
// tagged with an archetype that has no fixtures fails, instead of looping over
// nothing and passing while asserting nothing.
func TestRequireArchetypeFixturesRejectsAMissingDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	present := harnessPath(root, archetypeDir, "already-otel")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}

	if err := requireArchetypeFixtures(root, []string{"already-otel"}); err != nil {
		t.Fatalf("requireArchetypeFixtures on a present fixture: %v", err)
	}

	err := requireArchetypeFixtures(root, []string{"already-otel", "not-a-real-one"})
	if err == nil {
		t.Fatal("requireArchetypeFixtures accepted an archetype with no fixture directory")
	}
	if !strings.Contains(err.Error(), "not-a-real-one") {
		t.Errorf("error does not name the offending archetype: %v", err)
	}

	// A file where a directory belongs is just as broken as an absent one.
	file := harnessPath(root, archetypeDir, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("create fixture file: %v", err)
	}
	if err := requireArchetypeFixtures(root, []string{"a-file"}); err == nil {
		t.Fatal("requireArchetypeFixtures accepted a file as an archetype fixture directory")
	}
}

// TestSchemaCasesCarryTheNumbers asserts the schema-case table — not the
// feature file — is where a case's environment lives, and that the override
// case actually sets the retention variable the renderer reads.
func TestSchemaCasesCarryTheNumbers(t *testing.T) {
	t.Parallel()

	if env := schemaCases["default"]; len(env) != 0 {
		t.Errorf("the default case sets %v, want no override at all", env)
	}
	override := schemaCases["metrics-ttl-override"]
	want := []string{schemaTTLMetricsEnv + "=" + metricsTTLOverride}
	if !reflect.DeepEqual(override, want) {
		t.Fatalf("the override case sets %v, want %v", override, want)
	}
}

// TestGivenSchemaEnvironmentRejectsAnUnknownCase asserts a case name the Go
// table does not carry is a hard failure — otherwise a mistyped Examples row
// would render the default and assert against the wrong golden.
func TestGivenSchemaEnvironmentRejectsAnUnknownCase(t *testing.T) {
	t.Parallel()

	w := NewWorld(t.TempDir(), "")
	if err := w.givenSchemaEnvironment("no-such-case"); err == nil {
		t.Fatal("givenSchemaEnvironment accepted a case the harness does not declare")
	}
	if err := w.givenSchemaEnvironment("metrics-ttl-override"); err != nil {
		t.Fatalf("givenSchemaEnvironment on a declared case: %v", err)
	}
	if w.schema.caseName != "metrics-ttl-override" {
		t.Fatalf("caseName = %q, want the selected case", w.schema.caseName)
	}
}

// TestThenStepsFailBeforeTheRenderRuns asserts every schema assertion refuses
// to report success when the When step has not run, so a scenario missing its
// action cannot pass on stale or zero-valued state.
func TestThenStepsFailBeforeTheRenderRuns(t *testing.T) {
	t.Parallel()

	w := NewWorld(t.TempDir(), "")
	assertions := map[string]func() error{
		"matches the golden":     w.thenSchemaMatchesGolden,
		"declares signal tables": w.thenSchemaDeclaresEverySignalTable,
		"succeeded offline":      w.thenRendererSucceededOffline,
		"applies nothing":        w.thenRendererAppliesNothing,
		"renders under a case":   w.whenRenderSchema,
	}
	for name, fn := range assertions {
		if err := fn(); err == nil {
			t.Errorf("%q passed before the renderer ran", name)
		}
	}
}

// TestSchemaStatementsAndDeclaredObjects asserts the render is split into whole
// statements and that a CREATE's object name is read unquoted and unqualified,
// whichever quoting style the template used.
func TestSchemaStatementsAndDeclaredObjects(t *testing.T) {
	t.Parallel()

	render := []byte(`CREATE DATABASE IF NOT EXISTS default;

CREATE TABLE IF NOT EXISTS "default"."otel_metrics_gauge"  (
    MetricName String
) ENGINE = MergeTree()
;

CREATE TABLE IF NOT EXISTS ` + "`default`.`otel_logs`" + `  (
    Body String
) ENGINE = MergeTree();

ALTER TABLE default.otel_metrics_gauge ADD PROJECTION IF NOT EXISTS proj_series (SELECT MetricName);
`)

	stmts := schemaStatements(render)
	if len(stmts) != 4 {
		t.Fatalf("schemaStatements returned %d statements, want 4: %q", len(stmts), stmts)
	}

	declared := sortedKeys(declaredObjects(stmts))
	want := []string{"default", "otel_logs", "otel_metrics_gauge"}
	if !reflect.DeepEqual(declared, want) {
		t.Fatalf("declaredObjects = %v, want %v", declared, want)
	}
}

// TestThenRendererAppliesNothingRejectsAMutation asserts the "applies nothing"
// claim is a property of the rendered statements: a destructive statement
// fails, and so does an ALTER that is not the idempotent projection addition
// the renderer legitimately emits.
func TestThenRendererAppliesNothingRejectsAMutation(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		render string
		wantOK bool
	}{
		"declarations only": {
			render: "CREATE TABLE a (x String) ENGINE = MergeTree();\n" +
				"ALTER TABLE a ADD PROJECTION IF NOT EXISTS proj (SELECT x);\n",
			wantOK: true,
		},
		"a dropped table": {
			render: "CREATE TABLE a (x String) ENGINE = MergeTree();\nDROP TABLE a;\n",
		},
		"a rewriting alter": {
			render: "CREATE TABLE a (x String) ENGINE = MergeTree();\nALTER TABLE a MODIFY COLUMN x UInt8;\n",
		},
		"an insert": {
			render: "CREATE TABLE a (x String) ENGINE = MergeTree();\nINSERT INTO a VALUES ('x');\n",
		},
	}

	for name, tc := range cases {
		w := NewWorld(t.TempDir(), "")
		w.schema = schemaRender{caseName: "default", ran: true}
		w.schema.result.Stdout = []byte(tc.render)

		err := w.thenRendererAppliesNothing()
		if tc.wantOK && err != nil {
			t.Errorf("%s: rejected a pure declaration render: %v", name, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("%s: accepted a render that would change data", name)
		}
	}
}

// TestSignalTablesCoverEveryReadPath asserts the expected-table list is derived
// from cerberus's own schema defaults and names a table for each signal, so a
// renamed table breaks the scenario rather than passing under an outdated
// hardcoded name.
func TestSignalTablesCoverEveryReadPath(t *testing.T) {
	t.Parallel()

	tables := signalTables()
	for _, want := range []string{"otel_metrics_gauge", "otel_logs", "otel_traces"} {
		found := false
		for _, got := range tables {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("signalTables() = %v, missing %s", tables, want)
		}
	}
	for _, name := range tables {
		if name == "" {
			t.Fatalf("signalTables() = %v, carries an empty table name", tables)
		}
	}
}

// TestHarnessPathResolvesUnderTheTree asserts fixture paths land inside the
// harness tree rather than at the repository root.
func TestHarnessPathResolvesUnderTheTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	got := harnessPath(root, archetypeDir, "already-otel")
	want := filepath.Join(root, "test", "e2e", "migration", archetypeDir, "already-otel")
	if got != want {
		t.Fatalf("harnessPath = %s, want %s", got, want)
	}
}
