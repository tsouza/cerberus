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

// alterProjectionRe captures the target table and the projection name of the
// one non-CREATE shape the renderer emits. schema.go's addProjectionRe only
// recognises the shape; the live diff needs both halves, because the ALTER
// names a table the live database must already hold for the operator to be
// able to apply it at all.
var alterProjectionRe = regexp.MustCompile(
	`(?is)^ALTER\s+TABLE\s+(\S+)\s+ADD\s+PROJECTION\s+IF\s+NOT\s+EXISTS\s+(\S+)`,
)

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

// renderedProjection is one `ALTER TABLE … ADD PROJECTION IF NOT EXISTS` the
// renderer emits. Only the ALTER's HEADER is diffable against a
// collector-provisioned stack: the projection BODY is a cerberus-side read
// accelerator the OTel exporter never creates (the tier-1 compose stack makes
// the exporter the sole schema authority), so demanding it be present live
// would assert that a human already applied the render — the exact deliberate
// step MIG-10 exists to keep deliberate. What IS asserted is that the ALTER
// targets the live tenant database and names a table that is really there, so
// an operator piping the render into a client cannot hit a missing target.
type renderedProjection struct {
	database string
	table    string
	name     string
}

// renderedSchema is everything the render declares, split by what each half
// can be diffed against. Keeping the projections rather than dropping them is
// what stops a parser change from silently shrinking the diff's input.
type renderedSchema struct {
	objects     []renderedObject
	projections []renderedProjection
}

// readColumn is one column cerberus's read-side schema config addresses on one
// live table: the config FIELD that names it, and the name the environment
// resolved that field to. The field travels with the name so a field that
// resolves to nothing fails BY NAMING THE FIELD, instead of the diff quietly
// checking one column fewer than its step text claims.
type readColumn struct {
	field string
	name  string
	// nested marks a ClickHouse Nested block. The server materialises one
	// either as the block column itself (flatten_nested=0) or as its dotted
	// sibling columns (flatten_nested=1, the default), so either shape
	// satisfies the declaration — the same rule missingRenderedColumns
	// applies on the rendered side.
	nested bool
}

// readTable is one table cerberus's read-side schema config will query, and
// every column it will address there. Like readColumn it carries the config
// field, so a table field that names nothing fails by name rather than sending
// the diff to look at a table called "".
type readTable struct {
	field   string
	name    string
	columns []readColumn
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
	// projections counts the ADD PROJECTION ALTERs the diff reached. It is the
	// same coverage oracle visited is, for the half of the render that carries
	// no column list: a parser that stopped recognising the ALTERs would
	// otherwise leave their target-table check trivially satisfied.
	projections int
	// readMissing names, per table cerberus reads, the columns its
	// env-resolved READ-side schema config addresses that the live table does
	// not carry. The rendered diff above covers what the DDL declares; this
	// covers what the running server will actually select, which is the claim
	// MIG-10's PASS cell makes ("rendered CREATE = the schema cerberus reads").
	readMissing map[string][]string
	// unresolvedTables / unresolvedColumns name the read-side schema-config
	// FIELDS that resolved to the empty string. An empty name addresses
	// nothing, so treating it as "one fewer thing to check" would let a
	// schema-config field that names no table or column shrink this diff
	// without anything going red.
	unresolvedTables  []string
	unresolvedColumns map[string][]string
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
// still gets its own leg (readMissing), env-resolved and spanning all three
// signals, because "the schema cerberus reads" is a second claim in the same
// PASS cell — one the rendered diff cannot make, since a render and a live
// database can agree with each other while neither carries a column the
// running server selects.
//
// The live side is read from system.tables / system.columns rather than from
// SHOW CREATE's text: SHOW CREATE renders from those same catalogs, and
// diffing structured names sidesteps the formatting, CODEC and TTL noise that
// a text diff of two independently-formatted CREATE statements would drown in.
//
// The ADD PROJECTION ALTERs are diffed on their TARGET only; see
// renderedProjection for why their bodies cannot be.
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
	rendered, err := parseRenderedSchema(stmts)
	if err != nil {
		return err
	}
	if len(rendered.objects) == 0 {
		return fmt.Errorf("the rendered schema declares no object at all; a diff against the live database would be vacuous")
	}
	if len(rendered.projections) == 0 {
		return fmt.Errorf("the rendered schema adds no projection at all; the renderer emits one per catalog " +
			"table, so an empty set means the parse stopped reading them and their targets go unchecked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	diff := schemaLiveDiff{
		ran:               true,
		visited:           map[string]struct{}{},
		missingColumns:    map[string][]string{},
		readMissing:       map[string][]string{},
		unresolvedColumns: map[string][]string{},
		projections:       len(rendered.projections),
	}
	// liveCols caches one system.columns read per table, so the read-side leg
	// below reuses what the rendered leg already fetched.
	liveCols := map[string]map[string]struct{}{}

	for _, obj := range rendered.objects {
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

	for _, proj := range rendered.projections {
		if proj.database != w.live.CHDatabase {
			diff.misqualified = append(diff.misqualified,
				fmt.Sprintf("%s.%s's projection %s (live tenant database is %s)",
					proj.database, proj.table, proj.name, w.live.CHDatabase))
			continue
		}
		exists, err := tableExistsLive(ctx, conn, w.live.CHDatabase, proj.table)
		if err != nil {
			return err
		}
		if !exists {
			diff.missingTables = append(diff.missingTables,
				fmt.Sprintf("%s (target of projection %s)", proj.table, proj.name))
		}
	}

	for _, rt := range readSurface() {
		if rt.name == "" {
			diff.unresolvedTables = append(diff.unresolvedTables, rt.field)
			continue
		}
		cols, err := w.liveColumnsCached(ctx, conn, liveCols, rt.name)
		if err != nil {
			return err
		}
		missing, unresolved := diffReadColumns(rt.columns, cols)
		if len(unresolved) > 0 {
			diff.unresolvedColumns[rt.name] = unresolved
		}
		if len(missing) > 0 {
			diff.readMissing[rt.name] = missing
		}
	}

	sort.Strings(diff.misqualified)
	sort.Strings(diff.missingTables)
	sort.Strings(diff.unresolvedTables)
	w.schemaLive = diff
	return nil
}

// diffReadColumns splits one table's read-side columns against what the live
// table carries: the resolved names it does not hold, and the config FIELDS
// that named nothing at all.
//
// An unresolved field is a failure, not one fewer column to look at. Passing
// over it would mean a read-side schema field that names no column shrinks the
// check silently — the step's text would still claim every column cerberus
// reads was verified while the broken one was the only thing it stopped
// looking at.
func diffReadColumns(want []readColumn, live map[string]struct{}) (missing, unresolved []string) {
	for _, col := range want {
		if col.name == "" {
			unresolved = append(unresolved, col.field)
			continue
		}
		if !liveCarriesReadColumn(live, col) {
			missing = append(missing, col.name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unresolved)
	return missing, unresolved
}

// liveCarriesReadColumn reports whether the live column set satisfies one
// read-side column. A Nested block is satisfied by either materialisation
// ClickHouse may choose: the block column itself, or any of its dotted sibling
// columns.
func liveCarriesReadColumn(live map[string]struct{}, col readColumn) bool {
	if _, ok := live[col.name]; ok {
		return true
	}
	if !col.nested {
		return false
	}
	prefix := col.name + "."
	for name := range live {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
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
	if len(w.schemaLive.unresolvedTables) > 0 {
		return fmt.Errorf("cerberus's schema config resolves %v to the empty string, so the diff cannot know "+
			"which live table those signals read", w.schemaLive.unresolvedTables)
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
// other than the one under test. Each ADD PROJECTION ALTER is held to the same
// two claims about its target table; its projection BODY is deliberately out
// of scope (see renderedProjection).
func (w *World) thenNoTableMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if w.schemaLive.projections == 0 {
		return fmt.Errorf("the diff reached no ADD PROJECTION ALTER, so no projection target was checked")
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

// thenNoReadColumnMissingLive asserts the second claim on the same tables:
// every column cerberus's env-resolved read-side config will actually address
// exists live. The rendered diff catches DDL drift; this catches the case
// where the render and the live database agree with each other but neither
// carries a column the running server reads.
//
// The column set is every `*Column` field of schema.Metrics / schema.Logs /
// schema.Traces, mapped to the table that carries it;
// TestReadSurfaceCoversEveryReadSideSchemaField holds that mapping total, so
// "every column cerberus reads" stays the whole set rather than drifting into
// a hand-picked subset of it. A field the environment resolves to nothing
// fails here rather than being passed over — an override that silently
// resolves to "" is precisely the drift this leg exists to catch.
func (w *World) thenNoReadColumnMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.unresolvedColumns) > 0 {
		return fmt.Errorf("cerberus's schema config resolves columns it reads to the empty string: %s",
			formatTableColumns(w.schemaLive.unresolvedColumns))
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

// readSurface is cerberus's whole read side as the environment resolves it:
// every table the server will query, and on each one every column its schema
// config can address. It is built from the SAME typed config the live server
// resolves names from, read through the environment
// (schema.Default*FromEnv) rather than from the compiled-in defaults — a
// CERBERUS_SCHEMA_* override that renames a table or a column moves what the
// server reads, so it must move what MIG-10 checks.
//
// The per-table split is cerberus's own knowledge of which signal's columns
// live where; TestReadSurfaceCoversEveryReadSideSchemaField pins it TOTAL
// against the config structs, so a new `*Column` field cannot enter
// schema.Metrics / schema.Logs / schema.Traces without either landing on a
// table here or resolving to nothing.
func readSurface() []readTable {
	out := metricsReadTables(schema.DefaultOTelMetricsFromEnv())
	out = append(out, logsReadTables(schema.DefaultOTelLogsFromEnv())...)
	out = append(out, tracesReadTables(schema.DefaultOTelTracesFromEnv())...)
	return out
}

// These prefixes qualify a read-side config field name for an error message
// and for the totality pin. The three structs share field names
// (ServiceNameColumn is on all of them), so an unqualified name would neither
// say which one broke nor keep the pin's coverage set unambiguous.
const (
	metricsSchemaField = "schema.Metrics."
	logsSchemaField    = "schema.Logs."
	tracesSchemaField  = "schema.Traces."
)

// metricsReadTables splits schema.Metrics across the five metrics tables. The
// split follows the OTel-CH exporter's own per-type DDL: every table carries
// the identity/scope/timestamp block, and each metric type adds the columns
// its own encoding needs (a gauge has a Value; a classic histogram decomposes
// into Count/Sum/BucketCounts/ExplicitBounds; a summary into ValueAtQuantiles).
//
// ZeroThresholdColumn is absent because the exporter's exp-histogram DDL
// declares no such column and the default config resolves it to the empty
// string — an emitter that finds it empty substitutes a constant zero-bucket
// width instead of selecting anything.
func metricsReadTables(m schema.Metrics) []readTable {
	common := []readColumn{
		{field: metricsSchemaField + "ResourceAttributesColumn", name: m.ResourceAttributesColumn},
		{field: metricsSchemaField + "ScopeNameColumn", name: m.ScopeNameColumn},
		{field: metricsSchemaField + "ScopeVersionColumn", name: m.ScopeVersionColumn},
		{field: metricsSchemaField + "ScopeAttributesColumn", name: m.ScopeAttributesColumn},
		{field: metricsSchemaField + "ServiceNameColumn", name: m.ServiceNameColumn},
		{field: metricsSchemaField + "MetricNameColumn", name: m.MetricNameColumn},
		{field: metricsSchemaField + "MetricDescriptionColumn", name: m.MetricDescriptionColumn},
		{field: metricsSchemaField + "MetricUnitColumn", name: m.MetricUnitColumn},
		{field: metricsSchemaField + "AttributesColumn", name: m.AttributesColumn},
		{field: metricsSchemaField + "StartTimeColumn", name: m.StartTimeColumn},
		{field: metricsSchemaField + "TimestampColumn", name: m.TimestampColumn},
		{field: metricsSchemaField + "FlagsColumn", name: m.FlagsColumn},
	}
	exemplars := readColumn{field: metricsSchemaField + "ExemplarsColumn", name: m.ExemplarsColumn, nested: true}
	count := readColumn{field: metricsSchemaField + "CountColumn", name: m.CountColumn}
	sum := readColumn{field: metricsSchemaField + "SumColumn", name: m.SumColumn}
	minimum := readColumn{field: metricsSchemaField + "MinColumn", name: m.MinColumn}
	maximum := readColumn{field: metricsSchemaField + "MaxColumn", name: m.MaxColumn}
	temporality := readColumn{
		field: metricsSchemaField + "AggregationTemporalityColumn",
		name:  m.AggregationTemporalityColumn,
	}
	value := readColumn{field: metricsSchemaField + "ValueColumn", name: m.ValueColumn}

	return []readTable{
		{
			field:   metricsSchemaField + "GaugeTable",
			name:    m.GaugeTable,
			columns: append(append([]readColumn{}, common...), value, exemplars),
		},
		{
			field: metricsSchemaField + "SumTable",
			name:  m.SumTable,
			columns: append(append([]readColumn{}, common...), value, exemplars, temporality,
				readColumn{field: metricsSchemaField + "IsMonotonicColumn", name: m.IsMonotonicColumn}),
		},
		{
			field: metricsSchemaField + "HistogramTable",
			name:  m.HistogramTable,
			columns: append(append([]readColumn{}, common...), count, sum, minimum, maximum, exemplars, temporality,
				readColumn{field: metricsSchemaField + "BucketCountsColumn", name: m.BucketCountsColumn},
				readColumn{field: metricsSchemaField + "ExplicitBoundsColumn", name: m.ExplicitBoundsColumn}),
		},
		{
			field: metricsSchemaField + "ExpHistogramTable",
			name:  m.ExpHistogramTable,
			columns: append(append([]readColumn{}, common...), count, sum, minimum, maximum, exemplars, temporality,
				readColumn{field: metricsSchemaField + "ScaleColumn", name: m.ScaleColumn},
				readColumn{field: metricsSchemaField + "ZeroCountColumn", name: m.ZeroCountColumn},
				readColumn{field: metricsSchemaField + "PositiveOffsetColumn", name: m.PositiveOffsetColumn},
				readColumn{
					field: metricsSchemaField + "PositiveBucketCountsColumn",
					name:  m.PositiveBucketCountsColumn,
				},
				readColumn{field: metricsSchemaField + "NegativeOffsetColumn", name: m.NegativeOffsetColumn},
				readColumn{
					field: metricsSchemaField + "NegativeBucketCountsColumn",
					name:  m.NegativeBucketCountsColumn,
				}),
		},
		{
			field: metricsSchemaField + "SummaryTable",
			name:  m.SummaryTable,
			columns: append(append([]readColumn{}, common...), count, sum,
				readColumn{
					field:  metricsSchemaField + "ValueAtQuantilesColumn",
					name:   m.ValueAtQuantilesColumn,
					nested: true,
				}),
		},
	}
}

// logsReadTables lists what the LogQL read path addresses on the one logs
// table.
func logsReadTables(l schema.Logs) []readTable {
	return []readTable{{
		field: logsSchemaField + "LogsTable",
		name:  l.LogsTable,
		columns: []readColumn{
			{field: logsSchemaField + "TimestampColumn", name: l.TimestampColumn},
			{field: logsSchemaField + "BodyColumn", name: l.BodyColumn},
			{field: logsSchemaField + "SeverityColumn", name: l.SeverityColumn},
			{field: logsSchemaField + "SeverityNumberColumn", name: l.SeverityNumberColumn},
			{field: logsSchemaField + "AttributesColumn", name: l.AttributesColumn},
			{field: logsSchemaField + "ResourceAttributesColumn", name: l.ResourceAttributesColumn},
			{field: logsSchemaField + "ScopeNameColumn", name: l.ScopeNameColumn},
			{field: logsSchemaField + "ScopeVersionColumn", name: l.ScopeVersionColumn},
			{field: logsSchemaField + "ScopeAttributesColumn", name: l.ScopeAttributesColumn},
			{field: logsSchemaField + "TraceIDColumn", name: l.TraceIDColumn},
			{field: logsSchemaField + "SpanIDColumn", name: l.SpanIDColumn},
			{field: logsSchemaField + "TraceFlagsColumn", name: l.TraceFlagsColumn},
			{field: logsSchemaField + "ServiceNameColumn", name: l.ServiceNameColumn},
			{field: logsSchemaField + "EventNameColumn", name: l.EventNameColumn},
		},
	}}
}

// tracesReadTables covers both trace tables: the spans table TraceQL scans,
// and the trace-id/timestamp index table the trace-by-id lookup narrows its
// span scan with.
//
// EndTimeColumn resolves to the same column as StartTimeColumn on OTel-CH (the
// exporter stores a duration and cerberus derives the end), so it appears as
// its own entry rather than being folded away — an override that repointed one
// and not the other has to be checked as two separate reads.
//
// schema.Traces.ScopeAttributesColumn is absent for the same reason
// ZeroThresholdColumn is: the upstream traces DDL declares no such column and
// the default resolves it to the empty string, which the TraceQL lowering
// reads as "this deployment has no instrumentation-scope attributes".
func tracesReadTables(t schema.Traces) []readTable {
	return []readTable{
		{
			field: tracesSchemaField + "SpansTable",
			name:  t.SpansTable,
			columns: []readColumn{
				{field: tracesSchemaField + "TimestampColumn", name: t.TimestampColumn},
				{field: tracesSchemaField + "StartTimeColumn", name: t.StartTimeColumn},
				{field: tracesSchemaField + "EndTimeColumn", name: t.EndTimeColumn},
				{field: tracesSchemaField + "DurationColumn", name: t.DurationColumn},
				{field: tracesSchemaField + "TraceIDColumn", name: t.TraceIDColumn},
				{field: tracesSchemaField + "SpanIDColumn", name: t.SpanIDColumn},
				{field: tracesSchemaField + "ParentSpanIDColumn", name: t.ParentSpanIDColumn},
				{field: tracesSchemaField + "TraceStateColumn", name: t.TraceStateColumn},
				{field: tracesSchemaField + "SpanNameColumn", name: t.SpanNameColumn},
				{field: tracesSchemaField + "SpanKindColumn", name: t.SpanKindColumn},
				{field: tracesSchemaField + "ServiceNameColumn", name: t.ServiceNameColumn},
				{field: tracesSchemaField + "StatusCodeColumn", name: t.StatusCodeColumn},
				{field: tracesSchemaField + "StatusMessageColumn", name: t.StatusMessageColumn},
				{field: tracesSchemaField + "AttributesColumn", name: t.AttributesColumn},
				{field: tracesSchemaField + "ResourceAttributesColumn", name: t.ResourceAttributesColumn},
				{field: tracesSchemaField + "ScopeNameColumn", name: t.ScopeNameColumn},
				{field: tracesSchemaField + "ScopeVersionColumn", name: t.ScopeVersionColumn},
				{field: tracesSchemaField + "EventsColumn", name: t.EventsColumn, nested: true},
				{field: tracesSchemaField + "LinksColumn", name: t.LinksColumn, nested: true},
			},
		},
		{
			field: tracesSchemaField + "TraceIDTsTable",
			name:  t.TraceIDTsTable,
			columns: []readColumn{
				{field: tracesSchemaField + "TraceIDColumn", name: t.TraceIDColumn},
				{field: tracesSchemaField + "TraceIDTsStartColumn", name: t.TraceIDTsStartColumn},
				{field: tracesSchemaField + "TraceIDTsEndColumn", name: t.TraceIDTsEndColumn},
			},
		},
	}
}

// tablesCerberusReads names every table the env-resolved read-side schema will
// query — the coverage oracle the rendered set is measured against.
func tablesCerberusReads() []string {
	tables := readSurface()
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		out = append(out, t.name)
	}
	return out
}

// parseRenderedSchema turns the statements `cerberus migrate schema` emitted
// into the objects they declare. It reads the SQL text the CLI actually wrote,
// which is the only way the tier-1 diff can be about the RENDER rather than
// about the config the render was built from.
//
// The leading CREATE DATABASE carries nothing to diff against system.columns
// and is dropped. Everything else must be recognised: a statement matching
// neither a CREATE nor an ADD PROJECTION is a shape the renderer grew and this
// parser does not know about, so it FAILS rather than being passed over —
// silently ignoring it is how a whole class of rendered statement would stop
// being diffed without anything going red.
func parseRenderedSchema(stmts []string) (renderedSchema, error) {
	var out renderedSchema
	for _, stmt := range stmts {
		m := createObjectKindRe.FindStringSubmatchIndex(stmt)
		if m == nil {
			proj, err := parseAddProjection(stmt)
			if err != nil {
				return renderedSchema{}, err
			}
			out.projections = append(out.projections, proj)
			continue
		}
		kind := strings.ToUpper(strings.Join(strings.Fields(stmt[m[2]:m[3]]), " "))
		if kind == "DATABASE" {
			continue
		}
		database, name, err := splitQualifiedName(stmt[m[4]:m[5]])
		if err != nil {
			return renderedSchema{}, fmt.Errorf("the rendered schema declares %s: %w", firstLine(stmt), err)
		}
		obj := renderedObject{database: database, name: name}
		if kind == "TABLE" {
			body, ok := columnBody(stmt, m[5])
			if !ok {
				return renderedSchema{}, fmt.Errorf("the rendered CREATE TABLE for %s.%s carries no column list: %s",
					database, name, firstLine(stmt))
			}
			cols, err := parseColumns(body)
			if err != nil {
				return renderedSchema{}, fmt.Errorf("the rendered CREATE TABLE for %s.%s: %w", database, name, err)
			}
			if len(cols) == 0 {
				return renderedSchema{}, fmt.Errorf("the rendered CREATE TABLE for %s.%s declares no column",
					database, name)
			}
			obj.columns = cols
		}
		out.objects = append(out.objects, obj)
	}
	return out, nil
}

// parseAddProjection reads one `ALTER TABLE … ADD PROJECTION IF NOT EXISTS …`
// into the target it names, failing on any other non-CREATE statement.
func parseAddProjection(stmt string) (renderedProjection, error) {
	m := alterProjectionRe.FindStringSubmatch(stmt)
	if m == nil {
		return renderedProjection{}, fmt.Errorf(
			"the rendered schema carries a statement that is neither a CREATE nor an ADD PROJECTION, "+
				"so the diff would pass over it unchecked: %s", firstLine(stmt),
		)
	}
	database, table, err := splitQualifiedName(m[1])
	if err != nil {
		return renderedProjection{}, fmt.Errorf("the rendered projection ALTER %s: %w", firstLine(stmt), err)
	}
	return renderedProjection{database: database, table: table, name: strings.Trim(m[2], "`\"")}, nil
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
