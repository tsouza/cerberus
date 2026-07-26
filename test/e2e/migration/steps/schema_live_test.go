package steps

import (
	"os"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// readDefaultSchemaGolden returns the committed render of the default schema
// case — the exact stdout `cerberus migrate schema` produces, which is what
// MIG-10's tier-1 step parses at run time. Parsing the committed golden here
// is what lets the parser be verified offline, without the Docker stack.
func readDefaultSchemaGolden(t *testing.T) []renderedObject {
	t.Helper()
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	buf, err := os.ReadFile(lib.HarnessPath(root, schemaGoldenDir, "default.sql"))
	if err != nil {
		t.Fatalf("read the committed default schema render: %v", err)
	}
	objects, err := parseRenderedSchema(schemaStatements(buf))
	if err != nil {
		t.Fatalf("parse the committed default schema render: %v", err)
	}
	return objects
}

// TestParseRenderedSchemaCoversEveryTableCerberusReads proves the parser reads
// the rendered DDL rather than agreeing with cerberus's schema config by
// construction: every table the read-side config names must be found IN THE
// RENDERED TEXT, qualified with the database the renderer was pointed at.
func TestParseRenderedSchemaCoversEveryTableCerberusReads(t *testing.T) {
	t.Parallel()

	byName := map[string]renderedObject{}
	for _, obj := range readDefaultSchemaGolden(t) {
		byName[obj.name] = obj
	}
	for _, want := range tablesCerberusReads() {
		obj, ok := byName[want]
		if !ok {
			t.Fatalf("the rendered schema declares no table %q, which cerberus reads", want)
		}
		// The golden is rendered under the default CERBERUS_CH_DATABASE; the
		// tier-1 step renders under the live stack's own. Either way the
		// qualifier must be there — an unqualified render would make the
		// step's misqualified-object check silently unreachable.
		if obj.database == "" {
			t.Fatalf("the rendered CREATE for %q carries no database qualifier", want)
		}
		if len(obj.columns) == 0 {
			t.Fatalf("the rendered CREATE for %q declares no column", want)
		}
	}
}

// TestParseRenderedSchemaExpandsNestedBlocks pins the one column shape that
// does not appear verbatim in system.columns: a Nested block, which ClickHouse
// materialises as dotted sibling columns. A parser that emitted the bare block
// name would report every Nested column as missing on a perfectly healthy
// database.
func TestParseRenderedSchemaExpandsNestedBlocks(t *testing.T) {
	t.Parallel()

	metrics := schema.DefaultOTelMetrics()
	var gauge renderedObject
	for _, obj := range readDefaultSchemaGolden(t) {
		if obj.name == metrics.GaugeTable {
			gauge = obj
		}
	}
	if gauge.name == "" {
		t.Fatalf("the rendered schema declares no %s table", metrics.GaugeTable)
	}
	want := map[string]bool{
		"Exemplars.FilteredAttributes": false,
		"Exemplars.TimeUnix":           false,
		"Exemplars.Value":              false,
		"Exemplars.SpanId":             false,
		"Exemplars.TraceId":            false,
	}
	for _, col := range gauge.columns {
		if _, ok := want[col.name]; ok {
			want[col.name] = true
			if col.nestedBlock != "Exemplars" {
				t.Fatalf("column %q reports nested block %q, want %q", col.name, col.nestedBlock, "Exemplars")
			}
		}
		if col.name == "Exemplars" {
			t.Fatalf("the parser kept the bare Nested block name %q, which no live table carries", col.name)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("the parser did not expand the Exemplars block into %q", name)
		}
	}
}

// TestParseRenderedSchemaKeepsQuotedNamesWhole pins the logs table's
// backtick-quoted materialised columns, whose names carry dots. Splitting one
// on its dots would invent columns no database has.
func TestParseRenderedSchemaKeepsQuotedNamesWhole(t *testing.T) {
	t.Parallel()

	const materialised = "__otel_materialized_k8s.cluster.name"
	logsTable := schema.DefaultOTelLogs().LogsTable
	for _, obj := range readDefaultSchemaGolden(t) {
		if obj.name != logsTable {
			continue
		}
		for _, col := range obj.columns {
			if col.name == materialised {
				return
			}
		}
		t.Fatalf("the rendered %s declares no column %q; parsed %d columns", logsTable, materialised, len(obj.columns))
	}
	t.Fatalf("the rendered schema declares no %s table", logsTable)
}

// TestParseRenderedSchemaSkipsMaterializedViewColumns pins the deliberate gap:
// a MATERIALIZED VIEW declares its shape through its SELECT, so the diff can
// only assert that it exists. Parsing its `TO`-target as a column list would
// invent a diff that means nothing.
func TestParseRenderedSchemaSkipsMaterializedViewColumns(t *testing.T) {
	t.Parallel()

	const mv = "otel_traces_trace_id_ts_mv"
	for _, obj := range readDefaultSchemaGolden(t) {
		if obj.name != mv {
			continue
		}
		if len(obj.columns) != 0 {
			t.Fatalf("the parser read %d columns off the %s materialized view, which declares none", len(obj.columns), mv)
		}
		return
	}
	t.Fatalf("the rendered schema declares no %s materialized view", mv)
}

// TestParseRenderedSchemaRejectsAColumnlessTable proves the parser fails loudly
// rather than returning an empty column list a diff would find nothing wrong
// with — the exact failure mode that would turn MIG-10's tier-1 half green
// while comparing nothing.
func TestParseRenderedSchemaRejectsAColumnlessTable(t *testing.T) {
	t.Parallel()

	if _, err := parseRenderedSchema([]string{`CREATE TABLE "otel"."otel_metrics_gauge" ENGINE = MergeTree()`}); err == nil {
		t.Fatal("parseRenderedSchema accepted a CREATE TABLE with no column list")
	}
	if _, err := parseRenderedSchema([]string{`CREATE TABLE "otel"."otel_metrics_gauge" ( ) ENGINE = MergeTree()`}); err == nil {
		t.Fatal("parseRenderedSchema accepted a CREATE TABLE whose column list is empty")
	}
	if _, err := parseRenderedSchema([]string{`CREATE TABLE "otel_metrics_gauge" (Value Float64) ENGINE = MergeTree()`}); err == nil {
		t.Fatal("parseRenderedSchema accepted an unqualified CREATE TABLE, which the database check relies on")
	}
}

// TestMissingRenderedColumnsReportsWhatIsAbsent proves the diff's core is
// falsifiable in both directions, including the two shapes ClickHouse may
// materialise a Nested block as.
func TestMissingRenderedColumnsReportsWhatIsAbsent(t *testing.T) {
	t.Parallel()

	rendered := []renderedColumn{
		{name: "Value"},
		{name: "Exemplars.Value", nestedBlock: "Exemplars"},
		{name: "Exemplars.TraceId", nestedBlock: "Exemplars"},
	}
	flattened := map[string]struct{}{"Value": {}, "Exemplars.Value": {}, "Exemplars.TraceId": {}}
	if got := missingRenderedColumns(rendered, flattened); got != nil {
		t.Fatalf("a live table carrying every flattened column reports %v missing", got)
	}
	unflattened := map[string]struct{}{"Value": {}, "Exemplars": {}}
	if got := missingRenderedColumns(rendered, unflattened); got != nil {
		t.Fatalf("a live table carrying the unflattened Nested column reports %v missing", got)
	}
	renamed := map[string]struct{}{"MetricValue": {}, "Exemplars": {}}
	got := missingRenderedColumns(rendered, renamed)
	if len(got) != 1 || got[0] != "Value" {
		t.Fatalf("a live table that renamed Value reports %v missing, want exactly [Value]", got)
	}
	if got := missingRenderedColumns(rendered, map[string]struct{}{"Value": {}}); len(got) != 1 || got[0] != "Exemplars" {
		t.Fatalf("a live table with no Exemplars in either shape reports %v missing, want exactly [Exemplars]", got)
	}
}

// TestReadSideChecksFollowTheEnvironment proves the read-side legs read the
// schema cerberus ACTUALLY resolves rather than the compiled-in defaults: a
// deployment that renames a table through the documented CERBERUS_SCHEMA_*
// override must move both what the diff looks for and what it demands
// coverage of, or the check silently stops describing the stack it runs
// against.
func TestReadSideChecksFollowTheEnvironment(t *testing.T) {
	const renamed = "tenant_metrics_gauge"
	t.Setenv(schema.EnvMetricsGaugeTable, renamed)

	if _, ok := metricsExpectedColumns(schema.DefaultOTelMetricsFromEnv())[renamed]; !ok {
		t.Fatalf("metricsExpectedColumns ignores %s=%s and still keys on the compiled-in default",
			schema.EnvMetricsGaugeTable, renamed)
	}
	if !containsString(tablesCerberusReads(), renamed) {
		t.Fatalf("tablesCerberusReads() = %v, which does not follow %s=%s",
			tablesCerberusReads(), schema.EnvMetricsGaugeTable, renamed)
	}
	if containsString(tablesCerberusReads(), schema.DefaultOTelMetrics().GaugeTable) {
		t.Fatalf("tablesCerberusReads() still demands the compiled-in default gauge table")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestForeignTenantPlantMatchesGaugeProbeLayout holds MIG-15's plant in the
// SAME gauge column layout the harness's own recorded-series probe writes. The
// plant's whole evidentiary value is that it lands in a shape cerberus reads;
// a drifted column list would quietly make it a bespoke row again, which is
// the hollow assertion this scenario was rebuilt to remove.
func TestForeignTenantPlantMatchesGaugeProbeLayout(t *testing.T) {
	t.Parallel()

	want := insertColumnList(t, insertRecordedSeriesSQL)
	got := insertColumnList(t, foreignTenantInsertSQL("tenant_b", "otel_metrics_gauge"))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the foreign-tenant plant writes columns %v, but the recorded-series probe writes %v", got, want)
	}
}

// insertColumnList pulls the column names out of an `INSERT INTO t (a, b, …)`
// statement, whitespace-normalised.
func insertColumnList(t *testing.T, stmt string) []string {
	t.Helper()
	open := strings.Index(stmt, "(")
	shut := strings.LastIndex(stmt, ")")
	if open < 0 || shut < open {
		t.Fatalf("statement %q carries no column list", stmt)
	}
	var out []string
	for _, part := range strings.Split(stmt[open+1:shut], ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatalf("statement %q declares an empty column list", stmt)
	}
	return out
}
