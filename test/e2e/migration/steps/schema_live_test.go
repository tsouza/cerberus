package steps

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// readDefaultSchemaGolden returns the committed render of the default schema
// case — the exact stdout `cerberus migrate schema` produces, which is what
// MIG-10's tier-1 step parses at run time. Parsing the committed golden here
// is what lets the parser be verified offline, without the Docker stack.
func readDefaultSchemaGolden(t *testing.T) renderedSchema {
	t.Helper()
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	buf, err := os.ReadFile(lib.HarnessPath(root, schemaGoldenDir, "default.sql"))
	if err != nil {
		t.Fatalf("read the committed default schema render: %v", err)
	}
	rendered, err := parseRenderedSchema(schemaStatements(buf))
	if err != nil {
		t.Fatalf("parse the committed default schema render: %v", err)
	}
	return rendered
}

// gaugeRenderedColumns is the FULL column list the rendered gauge table
// declares, Nested block expanded the way ClickHouse materialises it. It is
// the shape pin for the parser: a regression that drops, merges or truncates
// column definitions still satisfies "every read column is present" (the
// read-side config names a subset), but cannot survive an exact set compare.
// The one table is enough — every metrics CREATE shares this element grammar.
var gaugeRenderedColumns = []string{
	"Attributes",
	"Exemplars.FilteredAttributes",
	"Exemplars.SpanId",
	"Exemplars.TimeUnix",
	"Exemplars.TraceId",
	"Exemplars.Value",
	"Flags",
	"MetricDescription",
	"MetricName",
	"MetricUnit",
	"ResourceAttributes",
	"ResourceSchemaUrl",
	"ScopeAttributes",
	"ScopeDroppedAttrCount",
	"ScopeName",
	"ScopeSchemaUrl",
	"ScopeVersion",
	"ServiceName",
	"StartTimeUnix",
	"TimeUnix",
	"Value",
}

// TestParseRenderedSchemaCoversEveryTableCerberusReads proves the parser reads
// the rendered DDL rather than agreeing with cerberus's schema config by
// construction: every table the read-side config names must be found IN THE
// RENDERED TEXT, qualified with the database the renderer was pointed at, and
// carrying every column that table's read-side config addresses.
//
// The column leg is what stops a parser that finds a table but reads two
// columns off it from passing: the read-side config is an INDEPENDENT oracle
// here (it is not derived from the rendered text), so it pins a real shape
// rather than restating the parse.
func TestParseRenderedSchemaCoversEveryTableCerberusReads(t *testing.T) {
	t.Parallel()

	byName := map[string]renderedObject{}
	for _, obj := range readDefaultSchemaGolden(t).objects {
		byName[obj.name] = obj
	}
	for _, rt := range readSurface() {
		obj, ok := byName[rt.name]
		if !ok {
			t.Fatalf("the rendered schema declares no table %q, which cerberus reads", rt.name)
		}
		// The golden is rendered under the default CERBERUS_CH_DATABASE; the
		// tier-1 step renders under the live stack's own. Either way the
		// qualifier must be there — an unqualified render would make the
		// step's misqualified-object check silently unreachable.
		if obj.database == "" {
			t.Fatalf("the rendered CREATE for %q carries no database qualifier", rt.name)
		}
		parsed := map[string]struct{}{}
		for _, col := range obj.columns {
			parsed[col.name] = struct{}{}
		}
		for _, col := range rt.columns {
			if col.name == "" {
				t.Fatalf("%s resolves to the empty string, so %q's read-side check would address nothing",
					col.field, rt.name)
			}
			if !liveCarriesReadColumn(parsed, col) {
				t.Fatalf("the rendered %s declares no %q (%s); the parser read %d columns: %v",
					rt.name, col.name, col.field, len(obj.columns), sortedKeys(parsed))
			}
		}
	}
}

// TestParseRenderedSchemaReadsEveryGaugeColumn pins the parser's output for
// one whole table, so a regression that keeps the columns cerberus reads but
// loses the rest still fails.
func TestParseRenderedSchemaReadsEveryGaugeColumn(t *testing.T) {
	t.Parallel()

	gaugeTable := schema.DefaultOTelMetrics().GaugeTable
	for _, obj := range readDefaultSchemaGolden(t).objects {
		if obj.name != gaugeTable {
			continue
		}
		got := make([]string, 0, len(obj.columns))
		for _, col := range obj.columns {
			got = append(got, col.name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(gaugeRenderedColumns, ",") {
			t.Fatalf("the parser read %v off the rendered %s, want %v", got, gaugeTable, gaugeRenderedColumns)
		}
		return
	}
	t.Fatalf("the rendered schema declares no %s table", gaugeTable)
}

// TestParseRenderedSchemaReadsEveryAddProjection pins the half of the render
// that carries no column list. The renderer emits one ADD PROJECTION per
// curated projection per catalog table; dropping them on the floor is how the
// diff stopped seeing them at all, so the parse is held to their exact set.
//
// Only the target is pinned, not the projection body: the tier-1 stack makes
// the collector's exporter the sole schema authority, so cerberus's own
// projections are genuinely absent live and MIG-10 leaves applying them the
// operator's deliberate step.
func TestParseRenderedSchemaReadsEveryAddProjection(t *testing.T) {
	t.Parallel()

	metrics := schema.DefaultOTelMetrics()
	want := []string{}
	for _, table := range []string{metrics.GaugeTable, metrics.SumTable, metrics.HistogramTable} {
		want = append(want, table+"/proj_series", table+"/proj_metric_metadata")
	}
	sort.Strings(want)

	var got []string
	for _, proj := range readDefaultSchemaGolden(t).projections {
		if proj.database == "" {
			t.Fatalf("the rendered ADD PROJECTION for %s.%s carries no database qualifier", proj.table, proj.name)
		}
		got = append(got, proj.table+"/"+proj.name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the parser read projections %v off the rendered schema, want %v", got, want)
	}
}

// TestParseRenderedSchemaRejectsAnUnknownStatement proves a rendered statement
// the parser does not recognise fails the parse rather than being passed over.
// Passing over it is what would let a whole new statement kind enter the
// render and never be diffed against anything.
func TestParseRenderedSchemaRejectsAnUnknownStatement(t *testing.T) {
	t.Parallel()

	if _, err := parseRenderedSchema([]string{`ALTER TABLE "otel"."otel_metrics_gauge" MODIFY TTL TimeUnix + toIntervalDay(1)`}); err == nil {
		t.Fatal("parseRenderedSchema accepted an ALTER that adds no projection")
	}
	if _, err := parseRenderedSchema([]string{`OPTIMIZE TABLE "otel"."otel_metrics_gauge" FINAL`}); err == nil {
		t.Fatal("parseRenderedSchema accepted a statement that declares no schema object")
	}
}

// TestReadSurfaceCoversEveryReadSideSchemaField is what makes MIG-10's
// read-side step text honest. readSurface() maps schema fields onto tables by
// hand — cerberus has no machine-readable per-table column registry — so
// without this pin the step would claim "every column cerberus reads" while
// checking whichever subset someone last remembered to list.
//
// Every `*Table` / `*Column` field on the three read-side config structs must
// therefore appear in readSurface(), UNLESS the environment resolves it to the
// empty string. An empty field addresses no column at all: cerberus's emitters
// branch on exactly that (schema.Metrics.ZeroThresholdColumn,
// schema.Traces.ScopeAttributesColumn), and the live step fails loudly on any
// field readSurface DOES list that resolves to nothing.
func TestReadSurfaceCoversEveryReadSideSchemaField(t *testing.T) {
	t.Parallel()

	covered := map[string]struct{}{}
	for _, rt := range readSurface() {
		covered[rt.field] = struct{}{}
		for _, col := range rt.columns {
			covered[col.field] = struct{}{}
		}
	}

	for _, cfg := range []struct {
		prefix string
		value  any
	}{
		{metricsSchemaField, schema.DefaultOTelMetricsFromEnv()},
		{logsSchemaField, schema.DefaultOTelLogsFromEnv()},
		{tracesSchemaField, schema.DefaultOTelTracesFromEnv()},
	} {
		v := reflect.ValueOf(cfg.value)
		for i := 0; i < v.NumField(); i++ {
			name := v.Type().Field(i).Name
			if v.Field(i).Kind() != reflect.String {
				continue
			}
			if !strings.HasSuffix(name, "Table") && !strings.HasSuffix(name, "Column") {
				continue
			}
			if v.Field(i).String() == "" {
				continue
			}
			if _, ok := covered[cfg.prefix+name]; !ok {
				t.Fatalf("%s%s resolves to %q but readSurface() maps it onto no table, so MIG-10's "+
					"read-side step would claim to check a column it never looks at",
					cfg.prefix, name, v.Field(i).String())
			}
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
	for _, obj := range readDefaultSchemaGolden(t).objects {
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
	for _, obj := range readDefaultSchemaGolden(t).objects {
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
	for _, obj := range readDefaultSchemaGolden(t).objects {
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

	var renamedColumns int
	var seen bool
	for _, rt := range readSurface() {
		if rt.name == renamed {
			seen = true
			renamedColumns = len(rt.columns)
		}
	}
	if !seen {
		t.Fatalf("readSurface() ignores %s=%s and still keys on the compiled-in default",
			schema.EnvMetricsGaugeTable, renamed)
	}
	if renamedColumns == 0 {
		t.Fatalf("readSurface() carries %s but no column to check on it", renamed)
	}
	if !containsString(tablesCerberusReads(), renamed) {
		t.Fatalf("tablesCerberusReads() = %v, which does not follow %s=%s",
			tablesCerberusReads(), schema.EnvMetricsGaugeTable, renamed)
	}
	if containsString(tablesCerberusReads(), schema.DefaultOTelMetrics().GaugeTable) {
		t.Fatalf("tablesCerberusReads() still demands the compiled-in default gauge table")
	}
}

// TestDiffReadColumnsFailsAnUnresolvedField proves the read-side leg treats a
// schema field that names no column as a FAILURE rather than as one fewer
// column to look at. The skip it replaces was the shape that let a broken
// resolution green the scenario: nothing to compare, so nothing to report.
func TestDiffReadColumnsFailsAnUnresolvedField(t *testing.T) {
	t.Parallel()

	live := map[string]struct{}{"Value": {}, "Exemplars.Value": {}}
	want := []readColumn{
		{field: "schema.Metrics.ValueColumn", name: "Value"},
		{field: "schema.Metrics.FlagsColumn", name: ""},
		{field: "schema.Metrics.CountColumn", name: "Count"},
		{field: "schema.Metrics.ExemplarsColumn", name: "Exemplars", nested: true},
	}
	missing, unresolved := diffReadColumns(want, live)
	if strings.Join(missing, ",") != "Count" {
		t.Fatalf("diffReadColumns reports %v missing, want exactly [Count]", missing)
	}
	if strings.Join(unresolved, ",") != "schema.Metrics.FlagsColumn" {
		t.Fatalf("diffReadColumns reports %v unresolved, want exactly [schema.Metrics.FlagsColumn]", unresolved)
	}

	full := map[string]struct{}{"Value": {}, "Count": {}, "Exemplars": {}}
	missing, unresolved = diffReadColumns(want[:1], full)
	if len(missing) != 0 || len(unresolved) != 0 {
		t.Fatalf("diffReadColumns reports %v missing / %v unresolved on a table that carries everything",
			missing, unresolved)
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
