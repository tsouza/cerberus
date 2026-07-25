package steps

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// sampleExplain is the shape `cerberus migrate explain` writes: a header
// carrying its own tally, one block per query — emitted or rejected — and a
// trailing skip list.
const sampleExplain = `# cerberus migrate explain
#
# Offline preview
#
# 2 queries, 1 skipped

== [record] rule:a.yml/g/rps
   expr:  sum(rate(http_requests_total[5m]))
   sql:   SELECT 1
   tables: otel_metrics_gauge, otel_metrics_sum
   risk:  none flagged offline (cardinality unknown)

== [panel] dashboard:d.json/Latency/A
   expr:  week_before(x)
   UNSUPPORTED: engine: parse: unknown function with name "week_before"

== skipped (1)
   dashboard:d.json/Empty/A: target has an empty expr
`

// TestParseExplainReportReadsEveryField proves the report is read back as
// values, so a scenario can assert that a query yielded SQL and named its
// tables rather than that some substring appeared somewhere in the text.
func TestParseExplainReportReadsEveryField(t *testing.T) {
	rep, err := parseExplainReport([]byte(sampleExplain))
	if err != nil {
		t.Fatal(err)
	}
	if rep.HeaderQueries != 2 || rep.HeaderSkipped != 1 {
		t.Fatalf("header tally = %d queries / %d skipped, want 2 / 1", rep.HeaderQueries, rep.HeaderSkipped)
	}
	if len(rep.Queries) != 2 {
		t.Fatalf("parsed %d blocks, want 2: %+v", len(rep.Queries), rep.Queries)
	}

	emitted := rep.Queries[0]
	if emitted.Kind != migrate.KindRecord || emitted.Source != "rule:a.yml/g/rps" {
		t.Errorf("first block = kind %q from %q", emitted.Kind, emitted.Source)
	}
	if emitted.Expr != "sum(rate(http_requests_total[5m]))" || emitted.SQL != "SELECT 1" {
		t.Errorf("first block expr %q / sql %q", emitted.Expr, emitted.SQL)
	}
	if want := []string{"otel_metrics_gauge", "otel_metrics_sum"}; !reflect.DeepEqual(emitted.Tables, want) {
		t.Errorf("first block tables = %v, want %v", emitted.Tables, want)
	}
	if emitted.Unsupported != "" {
		t.Errorf("an emitted block also reported a rejection: %q", emitted.Unsupported)
	}
	if len(emitted.Risks) != 1 {
		t.Errorf("first block risks = %v, want one line", emitted.Risks)
	}

	rejected := rep.Queries[1]
	if rejected.SQL != "" || rejected.Tables != nil {
		t.Errorf("a rejected block carries sql %q and tables %v", rejected.SQL, rejected.Tables)
	}
	if !strings.Contains(rejected.Unsupported, "week_before") {
		t.Errorf("rejected block does not name the construct: %q", rejected.Unsupported)
	}

	if len(rep.Skipped) != 1 || rep.Skipped[0].Source != "dashboard:d.json/Empty/A" {
		t.Fatalf("skips = %+v", rep.Skipped)
	}
	if rep.Skipped[0].Reason != "target has an empty expr" {
		t.Errorf("skip reason = %q", rep.Skipped[0].Reason)
	}
}

// TestParseExplainReportRefusesWhatItCannotRead proves the parser surfaces a
// report shape it does not understand instead of ignoring the line. A parser
// that skipped unknown lines would keep passing while every field it feeds went
// silently empty.
func TestParseExplainReportRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no header":            "== [record] rule:a.yml/g/n\n   expr:  up\n   sql:   SELECT 1\n",
		"unknown field":        sampleExplain + "\n== [record] rule:b.yml/g/n\n   plan:  something\n",
		"line outside a block": "# 0 queries, 0 skipped\n   expr:  up\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseExplainReport([]byte(text)); err == nil {
				t.Fatal("an unreadable report parsed cleanly")
			}
		})
	}
}

// TestRetentionBySignalReadsTheShortestTTL proves the retention a scenario
// compares against is read out of the DDL that would actually be applied, and
// that the binding figure per signal is the SHORTEST TTL across its tables —
// the first table to expire is what makes a panel go blank.
func TestRetentionBySignalReadsTheShortestTTL(t *testing.T) {
	metrics := firstTwoMetricsTables()
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS " + metrics[0] + " (a Int) ENGINE = MergeTree TTL toDateTime(TimeUnix) + toIntervalDay(90)",
		"CREATE TABLE IF NOT EXISTS " + metrics[1] + " (a Int) ENGINE = MergeTree TTL toDateTime(TimeUnix) + toIntervalWeek(2)",
		"CREATE TABLE IF NOT EXISTS otel_logs (a Int) ENGINE = MergeTree TTL toDateTime(Timestamp) + toIntervalHour(12)",
		"CREATE TABLE IF NOT EXISTS otel_traces (a Int) ENGINE = MergeTree",
		"CREATE TABLE IF NOT EXISTS something_else (a Int) ENGINE = MergeTree TTL toDateTime(t) + toIntervalDay(1)",
	}
	got, err := retentionBySignal(stmts)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{
		signalMetrics: 14 * 24 * time.Hour,
		signalLogs:    12 * time.Hour,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retention = %v, want %v", got, want)
	}
	if _, ok := got[signalTraces]; ok {
		t.Errorf("a table with no TTL clause was read as carrying retention: %v", got)
	}
}

// TestRetentionBySignalRefusesAnUnknownUnit proves an interval unit the harness
// cannot convert fails loudly. Reading it as no retention would make the
// runway comparison silently pass on a signal nobody bounded.
func TestRetentionBySignalRefusesAnUnknownUnit(t *testing.T) {
	stmt := "CREATE TABLE IF NOT EXISTS otel_logs (a Int) ENGINE = MergeTree TTL toDateTime(t) + toIntervalQuarter(1)"
	if _, err := retentionBySignal([]string{stmt}); err == nil {
		t.Fatal("an unconvertible interval unit was accepted")
	}
}

// firstTwoMetricsTables returns two of cerberus's metrics tables for the
// retention table above, taken from the schema defaults rather than spelled out.
func firstTwoMetricsTables() []string {
	all := signalTables()
	return all[:2]
}

// TestSignalOfLangCoversEveryHead proves each corpus language maps to the signal
// whose retention bounds it, and that an unknown language is rejected rather
// than silently folded into metrics.
func TestSignalOfLangCoversEveryHead(t *testing.T) {
	for lang, want := range map[string]string{
		migrate.LangPromQL:  signalMetrics,
		migrate.LangLogQL:   signalLogs,
		migrate.LangTraceQL: signalTraces,
	} {
		got, ok := signalOfLang(lang)
		if !ok || got != want {
			t.Errorf("%s maps to (%q, %v), want %q", lang, got, ok, want)
		}
	}
	if _, ok := signalOfLang("sql"); ok {
		t.Error("an unknown query language resolved to a signal")
	}
}

// TestRequireStatedReasonsRejectsABlankDrop proves a dropped input with no
// reason fails. A drop nobody explained is an input that vanished without the
// operator learning it existed.
func TestRequireStatedReasonsRejectsABlankDrop(t *testing.T) {
	ok := []migrate.SkippedEntry{{Source: "rule:a.yml/g/n", Reason: "empty expr"}}
	if err := requireStatedReasons("a", "skipped", ok); err != nil {
		t.Fatalf("a well-formed drop was rejected: %v", err)
	}
	for name, bad := range map[string][]migrate.SkippedEntry{
		"blank reason": {{Source: "rule:a.yml/g/n", Reason: "  "}},
		"blank source": {{Source: "", Reason: "empty expr"}},
	} {
		if err := requireStatedReasons("a", "skipped", bad); err == nil {
			t.Errorf("%s: an unexplained drop was accepted", name)
		}
	}
}

// TestHasProvenancePrefixAcceptsOnlyTheEmittedForms proves the provenance check
// accepts exactly the two forms the harvester stamps and nothing else.
func TestHasProvenancePrefixAcceptsOnlyTheEmittedForms(t *testing.T) {
	for _, good := range []string{"rule:a.yml/g/n", "dashboard:d.json/Panel/A"} {
		if !hasProvenancePrefix(good) {
			t.Errorf("%q was not recognised as provenance", good)
		}
	}
	for _, bad := range []string{"", "a.yml", "rules:a.yml", "Rule:a.yml"} {
		if hasProvenancePrefix(bad) {
			t.Errorf("%q was accepted as provenance", bad)
		}
	}
}

// TestRequireArchetypesRefusesAnEmptyLoop proves a scenario whose archetype
// tags select nothing fails rather than running every Then over an empty list
// and passing while asserting nothing.
func TestRequireArchetypesRefusesAnEmptyLoop(t *testing.T) {
	w := NewWorld(t.TempDir(), "")
	if err := w.requireArchetypes("the corpus assertion"); err == nil {
		t.Fatal("a scenario with no tagged archetype was allowed to loop over nothing")
	}
	w.archetypes = []string{"kube-prometheus-stack"}
	if err := w.requireArchetypes("the corpus assertion"); err != nil {
		t.Fatalf("a tagged scenario was refused: %v", err)
	}
}

// TestEveryCommittedArchetypeShipsItsInputsAndDecision proves no committed
// fixture sits in the tree unread: every archetype directory carries rule
// files, dashboards, and the cutover decision the whole-run fold pins. An
// archetype added without them fails here, in `just test`, rather than only in
// the tagged lane.
func TestEveryCommittedArchetypeShipsItsInputsAndDecision(t *testing.T) {
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	archetypes, err := CommittedArchetypes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(archetypes) == 0 {
		t.Fatal("no archetype fixtures are committed")
	}
	for _, a := range archetypes {
		t.Run(a, func(t *testing.T) {
			for _, sub := range []string{rulesDir, dashboardsDir} {
				entries, err := os.ReadDir(harnessPath(root, archetypeDir, a, sub))
				if err != nil {
					t.Fatalf("%s fixtures: %v", sub, err)
				}
				var files int
				for _, e := range entries {
					if !e.IsDir() {
						files++
					}
				}
				if files == 0 {
					t.Errorf("%s directory is empty", sub)
				}
			}
			// Every archetype is read by the harvest, explain and gate-fold
			// assertions, so these three goldens are mandatory.
			for _, golden := range []string{corpusGolden, explainGolden, gateGolden} {
				if _, err := os.Stat(goldenFixturePath(root, a, golden)); err != nil {
					t.Errorf("golden %s: %v", golden, err)
				}
			}
			// The remaining goldens are read only by the scenarios that tag
			// this archetype. Anything under expected/ whose name no assertion
			// knows is a fixture nobody reads.
			read := map[string]struct{}{
				corpusGolden: {}, explainGolden: {}, gateGolden: {},
				classifyGolden: {}, ruleGraphGolden: {}, lookbackGolden: {},
			}
			entries, err := os.ReadDir(harnessPath(root, archetypeDir, a, expectedDir))
			if err != nil {
				t.Fatalf("read the committed goldens: %v", err)
			}
			for _, e := range entries {
				if _, ok := read[e.Name()]; !ok {
					t.Errorf("committed golden %s is read by no assertion", e.Name())
				}
			}
		})
	}
}

// goldenFixturePath resolves one archetype's committed golden. It exists so the test above
// reads the same path the assertions do rather than re-deriving it.
func goldenFixturePath(root, archetype, name string) string {
	return filepath.Join(harnessPath(root, archetypeDir, archetype, expectedDir), name)
}
