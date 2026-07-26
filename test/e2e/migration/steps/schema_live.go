package steps

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/schema"
)

// liveSchemaCaseName names the schema-render case MIG-10's tier-1 half runs:
// the renderer bound to the live stack's own tenant database, so the DDL it
// emits is the DDL for THIS stack rather than for the compiled-in default
// database. It deliberately sits outside schemaCases (the golden-backed Tier-0
// matrix) — the database name is read off the live stack at run time, so no
// committed golden could exist for it.
const liveSchemaCaseName = "live-database"

// chDatabaseEnv is the environment variable the renderer qualifies every
// rendered object with, and the one the live server resolves its own reads
// against.
const chDatabaseEnv = "CERBERUS_CH_DATABASE"

// createObjectKindRe captures BOTH halves of a rendered CREATE header: which
// kind of object the statement declares and the (still quoted, still
// qualified) name it declares. schema.go's createObjectRe captures the name
// only; the live diff needs the kind too, because a MATERIALIZED VIEW carries
// no column list — its shape lives in its SELECT — while a TABLE does.
var createObjectKindRe = regexp.MustCompile(
	`(?is)^CREATE\s+(DATABASE|TABLE|MATERIALIZED\s+VIEW)\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)`,
)

// nonColumnElementRe matches the elements of a CREATE body that declare
// something other than a column — indices, constraints, keys. Everything else
// in the body is a column definition.
var nonColumnElementRe = regexp.MustCompile(`(?is)^(INDEX|CONSTRAINT|PROJECTION|PRIMARY\s+KEY)\b`)

// nestedTypeRe matches a column whose type is a Nested block, whose fields
// ClickHouse materialises as dotted sibling columns.
var nestedTypeRe = regexp.MustCompile(`(?is)^NESTED\s*\(`)

// renderedColumn is one column a rendered CREATE TABLE declares.
type renderedColumn struct {
	// name is the column the live table must carry.
	name string
	// nestedBlock is non-empty when name is one field of a Nested block.
	// ClickHouse materialises `Events Nested(Name String, …)` either as the
	// flattened `Events.Name` sibling columns (flatten_nested=1, the default)
	// or as a single `Events` Array(Tuple(…)) column (flatten_nested=0). Both
	// are the same declaration, so the diff accepts either materialisation and
	// names the BLOCK when the live table carries neither.
	nestedBlock string
}

// renderedObject is one object the rendered schema declares, as the renderer
// actually wrote it.
type renderedObject struct {
	// database and name are the CREATE header's qualifier and object name,
	// unquoted. The qualifier is asserted on: DDL rendered against a different
	// database would provision a stack other than the one under test.
	database string
	name     string
	// columns is empty for a MATERIALIZED VIEW.
	columns []renderedColumn
}

// schemaLiveDiff is what MIG-10's tier-1 step produced: the diff between the
// schema `cerberus migrate schema` RENDERED and the schema the collector's
// clickhouseexporter actually CREATED on the live stack.
type schemaLiveDiff struct {
	ran bool
	// visited is the set of rendered object names the diff actually reached —
	// the coverage oracle, so a parse regression that silently found fewer
	// tables fails instead of greening the scenario.
	visited map[string]struct{}
	// misqualified names rendered objects whose CREATE header targets a
	// database other than the live stack's tenant database.
	misqualified []string
	// missingTables names rendered objects the live database does not hold.
	missingTables []string
	// missingColumns names, per rendered table, the rendered columns the live
	// table does not carry — a missing OR renamed column lands here.
	missingColumns map[string][]string
	// readMissing names, per metrics table, the columns cerberus's env-resolved
	// READ-side schema config expects that the live table does not carry. The
	// rendered diff above covers what the DDL declares; this covers what the
	// running server will actually select, which is the claim MIG-10's PASS
	// cell makes ("rendered CREATE = the schema cerberus reads").
	readMissing map[string][]string
}

// registerSchemaLiveSteps binds MIG-10's tier-1 steps: rendering the schema
// under the live stack's own database, then diffing what was rendered against
// what the collector created.
func (w *World) registerSchemaLiveSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the cerberus schema environment bound to the live tenant database$`, w.givenLiveSchemaEnvironment)
	ctx.Step(`^the operator diffs the rendered schema against the live database$`, w.whenDiffSchemaAgainstLive)
	ctx.Step(`^the diff covers every table cerberus reads$`, w.thenDiffCoversEveryReadTable)
	ctx.Step(`^every rendered table exists in the live database$`, w.thenNoTableMissingLive)
	ctx.Step(`^every rendered column exists on its live table$`, w.thenNoColumnMissingLive)
	ctx.Step(`^every column cerberus reads exists on its live table$`, w.thenNoReadColumnMissingLive)
}

// givenLiveSchemaEnvironment selects the render case the tier-1 diff runs:
// the renderer pointed at the live stack's own tenant database. Rendering
// against the compiled-in default would emit DDL for a database that does not
// exist on this stack, which makes "would applying this render produce what is
// there?" unanswerable.
func (w *World) givenLiveSchemaEnvironment() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	if w.live.CHDatabase == "" {
		return fmt.Errorf("the tier-1 stack reports no ClickHouse database to render against")
	}
	w.schema = schemaRender{
		caseName: liveSchemaCaseName,
		env:      []string{chDatabaseEnv + "=" + w.live.CHDatabase},
	}
	return nil
}

// whenDiffSchemaAgainstLive parses the DDL `cerberus migrate schema` actually
// emitted and diffs THAT against the live, collector-created database.
//
// It reads the rendered SQL rather than re-deriving an expected column list
// from cerberus's typed schema config: the story's claim is that what the
// renderer EMITS matches what the collector CREATED, and a check built from
// the config alone can only ever restate the config. The read-side config
// still gets its own leg (readMissing), env-resolved, because "the schema
// cerberus reads" is a second, narrower claim in the same PASS cell.
//
// The live side is read from system.tables / system.columns rather than from
// SHOW CREATE's text: SHOW CREATE renders from those same catalogs, and
// diffing structured names sidesteps the formatting, CODEC and TTL noise that
// a text diff of two independently-formatted CREATE statements would drown in.
func (w *World) whenDiffSchemaAgainstLive() error {
	stmts, err := w.renderedStatements()
	if err != nil {
		return err
	}
	// A renderer that failed leaves empty stdout, which would otherwise reach
	// the parse as "no objects" and lose the reason it failed.
	if w.schema.result.ExitCode != 0 {
		return fmt.Errorf("the %s render exited %d: %s", w.schema.caseName, w.schema.result.ExitCode,
			strings.TrimSpace(string(w.schema.result.Stderr)))
	}
	objects, err := parseRenderedSchema(stmts)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return fmt.Errorf("the rendered schema declares no object at all; a diff against the live database would be vacuous")
	}

	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	diff := schemaLiveDiff{
		ran:            true,
		visited:        map[string]struct{}{},
		missingColumns: map[string][]string{},
		readMissing:    map[string][]string{},
	}
	// liveCols caches one system.columns read per table, so the read-side leg
	// below reuses what the rendered leg already fetched.
	liveCols := map[string]map[string]struct{}{}

	for _, obj := range objects {
		if obj.database != w.live.CHDatabase {
			diff.misqualified = append(diff.misqualified,
				fmt.Sprintf("%s.%s (live tenant database is %s)", obj.database, obj.name, w.live.CHDatabase))
			continue
		}
		diff.visited[obj.name] = struct{}{}
		exists, err := tableExistsLive(ctx, conn, w.live.CHDatabase, obj.name)
		if err != nil {
			return err
		}
		if !exists {
			diff.missingTables = append(diff.missingTables, obj.name)
			continue
		}
		if len(obj.columns) == 0 {
			// A MATERIALIZED VIEW declares its shape through its SELECT, so
			// existence is the whole claim the render makes about it.
			continue
		}
		cols, err := w.liveColumnsCached(ctx, conn, liveCols, obj.name)
		if err != nil {
			return err
		}
		if missing := missingRenderedColumns(obj.columns, cols); len(missing) > 0 {
			diff.missingColumns[obj.name] = missing
		}
	}

	for table, want := range metricsExpectedColumns(schema.DefaultOTelMetricsFromEnv()) {
		cols, err := w.liveColumnsCached(ctx, conn, liveCols, table)
		if err != nil {
			return err
		}
		var missing []string
		for _, col := range want {
			if col == "" {
				continue
			}
			if _, ok := cols[col]; !ok {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			diff.readMissing[table] = missing
		}
	}

	sort.Strings(diff.misqualified)
	sort.Strings(diff.missingTables)
	w.schemaLive = diff
	return nil
}

// liveColumnsCached reads a live table's column set once per scenario.
func (w *World) liveColumnsCached(
	ctx context.Context, conn driver.Conn, cache map[string]map[string]struct{}, table string,
) (map[string]struct{}, error) {
	if cols, ok := cache[table]; ok {
		return cols, nil
	}
	cols, err := liveColumnSet(ctx, conn, w.live.CHDatabase, table)
	if err != nil {
		return nil, err
	}
	cache[table] = cols
	return cols, nil
}

// missingRenderedColumns returns the rendered column names the live table does
// not carry, sorted. A Nested field is satisfied by either materialisation
// ClickHouse can pick (see renderedColumn.nestedBlock), so only a block that
// is absent in BOTH shapes is reported — once, under its block name.
func missingRenderedColumns(rendered []renderedColumn, live map[string]struct{}) []string {
	missing := map[string]struct{}{}
	for _, col := range rendered {
		if _, ok := live[col.name]; ok {
			continue
		}
		if col.nestedBlock != "" {
			if _, ok := live[col.nestedBlock]; ok {
				continue
			}
			missing[col.nestedBlock] = struct{}{}
			continue
		}
		missing[col.name] = struct{}{}
	}
	if len(missing) == 0 {
		return nil
	}
	out := make([]string, 0, len(missing))
	for name := range missing {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// thenDiffCoversEveryReadTable asserts the diff actually reached every table
// cerberus's env-resolved read-side schema will query. Without it, a parse
// regression that found no CREATE TABLE at all — or found only the ones that
// happen to be fine — would leave the two Then steps below trivially green.
func (w *World) thenDiffCoversEveryReadTable() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	want := tablesCerberusReads()
	if len(want) == 0 {
		return fmt.Errorf("cerberus's schema config names no table to read; the diff would be vacuous")
	}
	var uncovered []string
	for _, table := range want {
		if _, ok := w.schemaLive.visited[table]; !ok {
			uncovered = append(uncovered, table)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		return fmt.Errorf("the rendered schema never declared %v, which cerberus reads; it declared %v",
			uncovered, sortedKeys(w.schemaLive.visited))
	}
	return nil
}

// thenNoTableMissingLive asserts every object the render declares exists in
// the live, collector-created database — and that the render targeted that
// database in the first place, since DDL aimed elsewhere provisions a stack
// other than the one under test.
func (w *World) thenNoTableMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.misqualified) > 0 {
		return fmt.Errorf("the rendered schema targets objects outside the live tenant database: %v",
			w.schemaLive.misqualified)
	}
	if len(w.schemaLive.missingTables) > 0 {
		return fmt.Errorf("the live database is missing objects the rendered schema declares: %v",
			w.schemaLive.missingTables)
	}
	return nil
}

// thenNoColumnMissingLive asserts every column the RENDERED DDL declares is
// present on the live table — the "diff vs collector-created tables lists
// every missing/renamed column" half of MIG-10's PASS assertion. A column the
// exporter renamed shows up here as a rendered column with no live match.
func (w *World) thenNoColumnMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.missingColumns) == 0 {
		return nil
	}
	return fmt.Errorf("the live database deviates from the rendered schema: %s",
		formatTableColumns(w.schemaLive.missingColumns))
}

// thenNoReadColumnMissingLive asserts the narrower claim on the same tables:
// every column cerberus's env-resolved read-side config will actually SELECT
// exists live. The rendered diff catches DDL drift; this catches the case
// where the render and the live database agree with each other but neither
// carries a column the running server reads.
func (w *World) thenNoReadColumnMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.readMissing) == 0 {
		return nil
	}
	return fmt.Errorf("the live database is missing columns cerberus reads: %s",
		formatTableColumns(w.schemaLive.readMissing))
}

// formatTableColumns renders a per-table deviation map deterministically.
func formatTableColumns(byTable map[string][]string) string {
	tables := make([]string, 0, len(byTable))
	for t := range byTable {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "%s missing %v; ", t, byTable[t])
	}
	return b.String()
}

// metricsExpectedColumns names, for each metrics table, the columns cerberus's
// own read-side schema config expects to find there. It is built from the SAME
// typed config the live server resolves column names from, resolved through
// the environment (schema.DefaultOTelMetricsFromEnv) rather than from the
// compiled-in defaults — a CERBERUS_SCHEMA_* override that renames a column
// moves what the server reads, so it must move what this checks.
func metricsExpectedColumns(m schema.Metrics) map[string][]string {
	common := []string{
		m.ResourceAttributesColumn, m.ServiceNameColumn, m.MetricNameColumn,
		m.MetricDescriptionColumn, m.MetricUnitColumn, m.AttributesColumn,
		m.TimestampColumn, m.FlagsColumn,
	}
	valueCols := append(append([]string{}, common...), m.StartTimeColumn, m.ValueColumn)
	sumCols := append(append([]string{}, valueCols...), m.AggregationTemporalityColumn, m.IsMonotonicColumn)
	histCols := append(append([]string{}, common...),
		m.CountColumn, m.SumColumn, m.BucketCountsColumn, m.ExplicitBoundsColumn, m.AggregationTemporalityColumn)
	expHistCols := append(append([]string{}, common...),
		m.CountColumn, m.SumColumn, m.ScaleColumn, m.ZeroCountColumn,
		m.PositiveOffsetColumn, m.PositiveBucketCountsColumn,
		m.NegativeOffsetColumn, m.NegativeBucketCountsColumn, m.AggregationTemporalityColumn)
	return map[string][]string{
		m.GaugeTable:        valueCols,
		m.SumTable:          sumCols,
		m.HistogramTable:    histCols,
		m.ExpHistogramTable: expHistCols,
	}
}

// tablesCerberusReads names every table the env-resolved read-side schema will
// query — the coverage oracle the rendered set is measured against. It mirrors
// schema.go's signalTables(), resolved through the environment for the same
// reason metricsExpectedColumns is.
func tablesCerberusReads() []string {
	m := schema.DefaultOTelMetricsFromEnv()
	return []string{
		m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable, m.SummaryTable,
		schema.DefaultOTelLogsFromEnv().LogsTable,
		schema.DefaultOTelTracesFromEnv().SpansTable,
	}
}

// parseRenderedSchema turns the statements `cerberus migrate schema` emitted
// into the objects they declare. It reads the SQL text the CLI actually wrote,
// which is the only way the tier-1 diff can be about the RENDER rather than
// about the config the render was built from.
//
// Statements that declare no object shape — the leading CREATE DATABASE, the
// idempotent ALTER … ADD PROJECTION statements — carry nothing to diff against
// system.columns and are skipped; the Tier-0 steps already pin that nothing
// else is ever rendered.
func parseRenderedSchema(stmts []string) ([]renderedObject, error) {
	var out []renderedObject
	for _, stmt := range stmts {
		m := createObjectKindRe.FindStringSubmatchIndex(stmt)
		if m == nil {
			continue
		}
		kind := strings.ToUpper(strings.Join(strings.Fields(stmt[m[2]:m[3]]), " "))
		if kind == "DATABASE" {
			continue
		}
		database, name, err := splitQualifiedName(stmt[m[4]:m[5]])
		if err != nil {
			return nil, fmt.Errorf("the rendered schema declares %s: %w", firstLine(stmt), err)
		}
		obj := renderedObject{database: database, name: name}
		if kind == "TABLE" {
			body, ok := columnBody(stmt, m[5])
			if !ok {
				return nil, fmt.Errorf("the rendered CREATE TABLE for %s.%s carries no column list: %s",
					database, name, firstLine(stmt))
			}
			cols, err := parseColumns(body)
			if err != nil {
				return nil, fmt.Errorf("the rendered CREATE TABLE for %s.%s: %w", database, name, err)
			}
			if len(cols) == 0 {
				return nil, fmt.Errorf("the rendered CREATE TABLE for %s.%s declares no column", database, name)
			}
			obj.columns = cols
		}
		out = append(out, obj)
	}
	return out, nil
}

// splitQualifiedName unquotes a rendered `"db"."table"` / “ `db`.`table` “
// header name into its two halves. The renderer always qualifies, so an
// unqualified name means the render changed shape and the diff's database
// assertion would silently stop meaning anything.
func splitQualifiedName(raw string) (database, name string, err error) {
	unquoted := strings.NewReplacer("`", "", `"`, "").Replace(strings.TrimSpace(raw))
	i := strings.LastIndex(unquoted, ".")
	if i < 0 {
		return "", "", fmt.Errorf("object name %q carries no database qualifier", raw)
	}
	return unquoted[:i], unquoted[i+1:], nil
}

// columnBody returns the text inside the CREATE statement's top-level column
// list, starting the search at from (just past the object name).
func columnBody(stmt string, from int) (string, bool) {
	open := indexUnquoted(stmt, from, '(')
	if open < 0 {
		return "", false
	}
	shut := matchParen(stmt, open)
	if shut < 0 {
		return "", false
	}
	return stmt[open+1 : shut], true
}

// indexUnquoted returns the index of the first occurrence of want at or after
// from that is not inside a backtick- or single-quote-delimited run.
func indexUnquoted(s string, from int, want byte) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '`', '\'':
			i = skipQuoted(s, i)
		case want:
			return i
		}
	}
	return -1
}

// matchParen returns the index of the ')' closing the '(' at open, or -1.
func matchParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '`', '\'':
			i = skipQuoted(s, i)
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// skipQuoted returns the index of the quote closing the one at i, or the last
// index of s when the run is unterminated (an unterminated quote is malformed
// DDL, which the caller surfaces as a parse failure rather than by looping).
func skipQuoted(s string, i int) int {
	q := s[i]
	if j := strings.IndexByte(s[i+1:], q); j >= 0 {
		return i + 1 + j
	}
	return len(s) - 1
}

// splitTopLevel splits a CREATE body on the commas separating its top-level
// elements, leaving the commas nested inside a type, a CODEC or a Nested block
// alone.
func splitTopLevel(body string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '`', '\'':
			i = skipQuoted(body, i)
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	trimmed := make([]string, 0, len(out))
	for _, e := range out {
		if e := strings.TrimSpace(e); e != "" {
			trimmed = append(trimmed, e)
		}
	}
	return trimmed
}

// parseColumns extracts the column names a CREATE body declares, expanding a
// Nested block into the dotted sibling columns ClickHouse materialises it as.
func parseColumns(body string) ([]renderedColumn, error) {
	var out []renderedColumn
	for _, element := range splitTopLevel(body) {
		if nonColumnElementRe.MatchString(element) {
			continue
		}
		name, rest, err := leadingIdentifier(element)
		if err != nil {
			return nil, err
		}
		if !nestedTypeRe.MatchString(rest) {
			out = append(out, renderedColumn{name: name})
			continue
		}
		open := indexUnquoted(rest, 0, '(')
		if open < 0 {
			return nil, fmt.Errorf("column %q declares a Nested type with no field list", name)
		}
		shut := matchParen(rest, open)
		if shut < 0 {
			return nil, fmt.Errorf("column %q declares an unterminated Nested block", name)
		}
		for _, field := range splitTopLevel(rest[open+1 : shut]) {
			inner, _, err := leadingIdentifier(field)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", name, err)
			}
			out = append(out, renderedColumn{name: name + "." + inner, nestedBlock: name})
		}
	}
	return out, nil
}

// leadingIdentifier splits a column definition into its name and the rest of
// the definition, handling the backtick quoting the logs DDL uses for names
// that carry dots (`__otel_materialized_k8s.cluster.name`).
func leadingIdentifier(element string) (name, rest string, err error) {
	element = strings.TrimSpace(element)
	if element == "" {
		return "", "", fmt.Errorf("empty column definition")
	}
	if element[0] == '`' {
		end := strings.IndexByte(element[1:], '`')
		if end < 0 {
			return "", "", fmt.Errorf("column definition %q carries an unterminated quoted name", element)
		}
		return element[1 : 1+end], strings.TrimSpace(element[end+2:]), nil
	}
	if i := strings.IndexAny(element, " \t\n"); i >= 0 {
		return element[:i], strings.TrimSpace(element[i:]), nil
	}
	return "", "", fmt.Errorf("column definition %q declares a name with no type", element)
}
