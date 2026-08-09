package chsql

import (
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// Typed ClickHouse DDL surface.
//
// chsql is otherwise a SELECT / expression builder; this file adds the
// narrow DDL vocabulary cerberus owns: the CREATE DATABASE statement —
// which is 100% cerberus-authored (no upstream template backs it) — plus
// the ON CLUSTER, table-engine, database-engine, and TTL CLAUSE
// constructors that internal/schema/ddl injects into the upstream OTel
// ClickHouse exporter's table templates. The upstream templates remain
// the source of truth for the table column BODIES (columns, indexes,
// PARTITION BY / ORDER BY / SETTINGS); this surface only types the
// parameterisation cerberus controls, replacing the fmt.Sprintf /
// strings.ReplaceAll string-building the ddl package used before.
//
// Everything here composes from the same primitives as the query builder
// (Call / InlineLit / BareIdent / Col / ddlToken), so no constructor in
// this file writes a raw token of its own — token emission stays in
// builder.go, the closed surface — and the typed-Frag trust contract
// extends to DDL.

// OnCluster returns a Frag rendering `ON CLUSTER <name>` with name
// backtick-quoted (embedded backticks doubled, via Col → Builder.Ident) —
// the ClickHouse distributed-DDL clause. name must be non-empty; a
// single-node deployment omits the clause by not emitting this Frag at
// all. Mutually exclusive with a Replicated database engine, which
// replicates DDL itself.
func OnCluster(name string) Frag {
	return func(b *Builder) {
		ddlToken("ON CLUSTER ")(b)
		Col(name)(b)
	}
}

// DatabaseEngineReplicated returns
// `Replicated('<zooPath>', '<shard>', '<replica>')` — the ClickHouse
// Replicated database engine, which auto-replicates all DDL across replicas
// (so ON CLUSTER is neither needed nor used with it). It does NOT, however,
// silently turn MergeTree tables into ReplicatedMergeTree on every server —
// the tables must be created with an explicit ReplicatedMergeTree engine
// (see EngineReplicatedMergeTree) to replicate their DATA. The three
// arguments are single-quoted CH string literals; shard and replica are
// typically the server macros "{shard}" / "{replica}".
func DatabaseEngineReplicated(zooPath, shard, replica string) Frag {
	return Call("Replicated", InlineLit(zooPath), InlineLit(shard), InlineLit(replica))
}

// EngineReplicatedMergeTree returns the BARE `ReplicatedMergeTree` table
// engine clause — no positional arguments. This is the form required for the
// tables of a Replicated database: the database's own
// Replicated('<path>', '{shard}', '{replica}') coordinates plus the server's
// default_replica_path / default_replica_name supply the Keeper path and
// replica name automatically, so the engine needs no args. ClickHouse 24.8+
// REJECTS explicit (path, replica) arguments inside a Replicated database with
// code 36 (database_replicated_allow_replicated_engine_arguments defaults to
// 0), which is exactly why the args must be omitted. Only a ReplicatedMergeTree
// engine replicates the table DATA — a plain MergeTree leaves each replica with
// an independent copy — so this is what cerberus emits for a Replicated DB.
//
// A classic ON CLUSTER deployment that instead needs an explicit
// `ReplicatedMergeTree('/path', '{replica}')` (no Replicated database to supply
// the coordinates) passes the full engine string through the operator-facing
// CERBERUS_SCHEMA_TABLE_ENGINE knob — cerberus does not compose that shape.
func EngineReplicatedMergeTree() Frag {
	return BareIdent("ReplicatedMergeTree")
}

// ttlInterval picks the coarsest exact ClickHouse interval bucket for d:
// the toIntervalXxx function name and its integer count. Mirrors the
// retention granularity a CH TTL clause accepts — week / day / hour /
// minute / second, all of which are exact (fixed-length) durations a Go
// time.Duration represents losslessly. Calendar units (month / quarter /
// year) are intentionally NOT produced: they are variable-length and cannot
// round-trip through a time.Duration, so a "1 year" retention arrives here as
// 365 days and renders as toIntervalDay(365), not the calendar-aware
// toIntervalYear(1). Only called with d > 0 — TableTTL guards the zero case.
func ttlInterval(d time.Duration) (fn string, n int64) {
	const week = 7 * 24 * time.Hour
	switch {
	case d%week == 0:
		return "toIntervalWeek", int64(d / week)
	case d%(24*time.Hour) == 0:
		return "toIntervalDay", int64(d / (24 * time.Hour))
	case d%time.Hour == 0:
		return "toIntervalHour", int64(d / time.Hour)
	case d%time.Minute == 0:
		return "toIntervalMinute", int64(d / time.Minute)
	default:
		return "toIntervalSecond", int64(d / time.Second)
	}
}

// ttlAge renders the age expression every TTL rule is keyed on —
// `toDateTime(<column>) + toIntervalXxx(N)`. column is the bare time column
// the signal keys its lifecycle on (TimeUnix for metrics, Timestamp for logs /
// traces spans, Start for the traces lookup table); it is emitted as a bare CH
// identifier (BareIdent), matching the upstream template's unquoted
// toDateTime(<col>) form. N is the coarsest exact interval bucket for d.
// Only called with d > 0 — the exported constructors guard the zero case.
func ttlAge(column string, d time.Duration) Frag {
	fn, n := ttlInterval(d)
	return Add(
		Call("toDateTime", BareIdent(column)),
		Call(fn, InlineLit(n)),
	)
}

// ttlClause renders `TTL <rule>[, <rule>…]` from the given non-nil rules, or
// nil when none are supplied. A ClickHouse table carries ONE TTL clause whose
// actions are comma-separated (`TTL d + INTERVAL 7 DAY TO VOLUME 'cold',
// d + INTERVAL 30 DAY DELETE`), not one clause per action — so every rule this
// package emits for a table flows through here.
func ttlClause(rules ...Frag) Frag {
	kept := make([]Frag, 0, len(rules))
	for _, r := range rules {
		if r != nil {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return func(b *Builder) {
		ddlToken("TTL ")(b)
		for i, r := range kept {
			if i > 0 {
				ddlToken(", ")(b)
			}
			r(b)
		}
	}
}

// ttlMoveRule renders one TIERING rule — `<age> TO VOLUME '<volume>'` — the
// ClickHouse action that moves a part onto another volume of the table's
// storage policy once it reaches the given age (the hot/cold tiering
// mechanism). Returns nil when either half is unset, since a move with no age
// or no destination is not a rule. The volume name is a single-quoted CH
// string literal (InlineLit), the form `TO VOLUME` requires.
func ttlMoveRule(column string, d time.Duration, volume string) Frag {
	if d <= 0 || volume == "" {
		return nil
	}
	age := ttlAge(column, d)
	return func(b *Builder) {
		age(b)
		ddlToken(" TO VOLUME ")(b)
		InlineLit(volume)(b)
	}
}

// ttlDeleteRule renders one RETENTION rule with an EXPLICIT `DELETE` action —
// `<age> DELETE`. ClickHouse infers DELETE for a bare action, which is what
// the single-rule TableTTL form emits; the explicit keyword is used only in
// the multi-rule (tiered) clause, where spelling each action out is how the
// documented `TO VOLUME …, … DELETE` shape reads. Returns nil when d <= 0.
func ttlDeleteRule(column string, d time.Duration) Frag {
	if d <= 0 {
		return nil
	}
	age := ttlAge(column, d)
	return func(b *Builder) {
		age(b)
		ddlToken(" DELETE")(b)
	}
}

// TableTTL returns a Frag rendering the ClickHouse table TTL clause
// `TTL toDateTime(<column>) + toIntervalXxx(N)` for retention d, or nil
// when d <= 0 (no retention). The single action carries no keyword, so
// ClickHouse infers DELETE — the shape the upstream OTel-CH exporter
// templates expect. See TableTTLTiered for the hot/cold variant.
func TableTTL(column string, d time.Duration) Frag {
	if d <= 0 {
		return nil
	}
	return ttlClause(ttlAge(column, d))
}

// TableTTLTiered returns the full table TTL clause for a signal that combines
// STORAGE TIERING with retention:
//
//	TTL toDateTime(Timestamp) + toIntervalDay(7) TO VOLUME 'cold', toDateTime(Timestamp) + toIntervalDay(30) DELETE
//
// The move rule is what makes a multi-volume `storage_policy` do anything: the
// policy alone only tells ClickHouse which volumes a table MAY use, and without
// a `TO VOLUME` action every part stays on the first (hot) volume until
// retention deletes it. moveAfter is the age at which a part moves to volume;
// retention is the age at which it is deleted.
//
// The degenerate cases collapse to exactly what shipped before tiering existed,
// so a deployment that configures none of it renders byte-identical DDL:
// with no move rule (moveAfter <= 0 or volume == "") this IS TableTTL, and with
// neither rule it is nil (no TTL clause at all). With a move rule and no
// retention it emits the move alone — a valid tiering-only table.
func TableTTLTiered(column string, retention, moveAfter time.Duration, volume string) Frag {
	move := ttlMoveRule(column, moveAfter, volume)
	if move == nil {
		return TableTTL(column, retention)
	}
	return ttlClause(move, ttlDeleteRule(column, retention))
}

// TableSettings returns a Frag rendering a leading-comma-continued SETTINGS
// tail — `, <k> = <v>, <k2> = <v2>` — for the given ordered entries, or nil
// when none are supplied (so an unset settings slice appends nothing and the
// rendered DDL stays byte-identical to the bare upstream template). The leading
// comma is deliberate: the upstream templates already bake a `SETTINGS ...`
// clause, so this fragment continues that existing clause rather than opening a
// second one. Keys are emitted bare (CH setting identifiers), values via
// InlineLit so the RHS quoting is type-inferred (see schema.KV). Like TableTTL
// it composes only builder.go primitives — no raw token is written here.
//
// The KV data type lives in internal/schema (schema.KV), not here: chsql
// already imports internal/schema (late_mat.go reads the default OTel schema),
// so a chsql-owned KV that schema's env parser had to reference would form an
// import cycle. The token-emitting Frag stays in chsql (the sanctioned
// primitive zone); only the plain value-carrier struct sits one layer down.
func TableSettings(kv ...schema.KV) Frag {
	if len(kv) == 0 {
		return nil
	}
	return func(b *Builder) {
		for _, e := range kv {
			ddlToken(", ")(b)
			BareIdent(e.Key)(b)
			ddlToken(" = ")(b)
			InlineLit(e.Value)(b)
		}
	}
}

// CreateDatabaseBuilder builds a `CREATE DATABASE` statement. Construct
// via CreateDatabase, chain IfNotExists / OnCluster / Engine, and
// terminate with SQL. DDL carries no positional `?` bindings, so SQL
// returns just the statement text (it renders via RenderDDL, which
// enforces the no-bindings invariant).
type CreateDatabaseBuilder struct {
	name        string
	ifNotExists bool
	cluster     string // "" => no ON CLUSTER clause
	engine      Frag   // nil => no ENGINE clause (server default: Atomic)
}

// CreateDatabase starts a CREATE DATABASE statement for the named
// database. The name is emitted as a bare identifier, matching the
// established cerberus + upstream-exporter CREATE DATABASE form.
func CreateDatabase(name string) *CreateDatabaseBuilder {
	return &CreateDatabaseBuilder{name: name}
}

// IfNotExists adds the IF NOT EXISTS guard so a re-create is idempotent.
func (c *CreateDatabaseBuilder) IfNotExists() *CreateDatabaseBuilder {
	c.ifNotExists = true
	return c
}

// OnCluster adds an `ON CLUSTER <name>` clause. Mutually exclusive with a
// Replicated database engine (a Replicated database replicates DDL
// itself) — pick one. An empty name leaves the clause off.
func (c *CreateDatabaseBuilder) OnCluster(name string) *CreateDatabaseBuilder {
	c.cluster = name
	return c
}

// Engine sets the database ENGINE clause (e.g. DatabaseEngineReplicated).
// Leaving it unset emits no ENGINE clause, so ClickHouse applies its
// default (Atomic) — the single-node shape.
func (c *CreateDatabaseBuilder) Engine(e Frag) *CreateDatabaseBuilder {
	c.engine = e
	return c
}

// frag assembles the statement as a single composed Frag: keyword tokens
// via ddlToken, the bare database name via BareIdent, and the optional
// ON CLUSTER / ENGINE clauses via their typed constructors. No raw write
// happens here — every token is emitted by a builder.go primitive.
func (c *CreateDatabaseBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("CREATE DATABASE ")(b)
		if c.ifNotExists {
			ddlToken("IF NOT EXISTS ")(b)
		}
		BareIdent(c.name)(b)
		if c.cluster != "" {
			ddlToken(" ")(b)
			OnCluster(c.cluster)(b)
		}
		if c.engine != nil {
			ddlToken(" ENGINE = ")(b)
			c.engine(b)
		}
	}
}

// SQL renders the statement to its ClickHouse text. There is no args slice
// to return — a CREATE DATABASE statement binds no positional `?` values —
// so SQL renders through RenderDDL, which asserts that invariant rather
// than letting a stray binding be silently dropped.
func (c *CreateDatabaseBuilder) SQL() string {
	return RenderDDL(c.frag())
}

// RenderDDL renders a DDL Frag to its ClickHouse text. Unlike a query, a
// DDL statement carries NO positional `?` bindings: every value is part of
// the statement shape, emitted inline via Ident / InlineLit / Call, never
// bound with Lit / Arg. RenderDDL enforces that — it panics if the fragment
// bound any args, since a `?` placeholder in DDL would reach conn.Exec with
// nothing to fill it (broken SQL). Surfacing it as a panic at render time
// turns a silent footgun into an immediate test failure, the same
// fail-at-test-time stance as InlineLit's unsupported-type panic. (Render
// returns (sql, args) — the second value is the bindings slice, never an
// error; RenderDDL is the DDL-shaped terminator that asserts it's empty.)
func RenderDDL(f Frag) string {
	sql, args := Render(f)
	if len(args) != 0 {
		panic("chsql: DDL fragment bound positional args (Lit/Arg); DDL values must be inline (InlineLit/Ident/Call)")
	}
	return sql
}

// --- CREATE TABLE surface (cerberus-owned tables) ---
//
// CreateDatabaseBuilder covers the database statement; the table side below
// builds a CREATE TABLE for tables cerberus authors in full (no upstream
// template backs them), such as the router-calibration corpus. Like the
// database builder, every token flows through builder.go primitives
// (ddlToken / BareIdent / InlineLit / Call) so no constructor here writes a raw
// token of its own — the closed-surface discipline extends to table DDL.

// ColumnType is a Frag rendering a ClickHouse column type. The constructors
// below (TypeRaw / TypeLowCardinality / TypeEnum8) compose it; a ColumnDef
// pairs it with a column name.

// TypeRaw renders a bare ClickHouse type name (UInt32, UInt64, String,
// DateTime, …). The name MUST be a CH type token, not user data — it is
// emitted as a bare identifier, the same trust contract as BareIdent.
func TypeRaw(name string) Frag { return BareIdent(name) }

// TypeLowCardinality wraps an inner type in LowCardinality(...), the CH
// dictionary-encoded wrapper used for the low-arity string columns (shape_id,
// language, decision_reason).
func TypeLowCardinality(inner Frag) Frag {
	return Call("LowCardinality", inner)
}

// EnumPair is one (name → value) entry of an Enum8 column type. Value is int8
// because that IS an Enum8 member's domain — the narrow type means a caller that
// also writes the column (clickhouse-go appends an Enum8 as an int8) can hand the
// same value to both without a conversion the compiler cannot prove is lossless.
type EnumPair struct {
	Name  string
	Value int8
}

// TypeEnum8 renders an Enum8('a'=0,'b'=1,...) column type. Each name is
// emitted as a single-quoted CH string literal via InlineLit and each value as
// a bare integer, so the rendered type is byte-identical to a hand-written
// Enum8 declaration without any raw token write.
func TypeEnum8(pairs ...EnumPair) Frag {
	return func(b *Builder) {
		ddlToken("Enum8(")(b)
		for i, p := range pairs {
			if i > 0 {
				ddlToken(", ")(b)
			}
			InlineLit(p.Name)(b)
			ddlToken(" = ")(b)
			InlineLit(int64(p.Value))(b)
		}
		ddlToken(")")(b)
	}
}

// ColumnDef is one `name Type` entry inside a CREATE TABLE column list.
type ColumnDef struct {
	Name string
	Type Frag
}

// frag renders the column as `<name> <type>` with the name backtick-quoted
// (Col) and the type composed from the typed type constructors.
func (c ColumnDef) frag() Frag {
	return func(b *Builder) {
		Col(c.Name)(b)
		ddlToken(" ")(b)
		c.Type(b)
	}
}

// CreateTableBuilder builds a `CREATE TABLE` statement for a cerberus-owned
// table. Construct via CreateTable, set the columns, the engine, ORDER BY, and
// optional TTL, and terminate with SQL. Like CreateDatabaseBuilder it binds no
// positional `?` values, so SQL renders through RenderDDL.
type CreateTableBuilder struct {
	name        string
	ifNotExists bool
	columns     []ColumnDef
	engine      Frag
	orderBy     []string
	ttl         Frag
}

// CreateTable starts a CREATE TABLE builder for the named table.
func CreateTable(name string) *CreateTableBuilder {
	return &CreateTableBuilder{name: name}
}

// IfNotExists adds the IF NOT EXISTS guard so re-create is idempotent.
func (c *CreateTableBuilder) IfNotExists() *CreateTableBuilder {
	c.ifNotExists = true
	return c
}

// Columns sets the column list.
func (c *CreateTableBuilder) Columns(cols ...ColumnDef) *CreateTableBuilder {
	c.columns = cols
	return c
}

// Engine sets the table ENGINE clause (e.g. EngineMergeTree).
func (c *CreateTableBuilder) Engine(e Frag) *CreateTableBuilder {
	c.engine = e
	return c
}

// OrderBy sets the ORDER BY key column list.
func (c *CreateTableBuilder) OrderBy(cols ...string) *CreateTableBuilder {
	c.orderBy = cols
	return c
}

// TTL sets the TTL clause Frag (typically TableTTL). A nil Frag omits it.
func (c *CreateTableBuilder) TTL(t Frag) *CreateTableBuilder {
	c.ttl = t
	return c
}

// EngineMergeTree renders the bare `MergeTree` table engine (no arguments) —
// the single-node corpus table engine.
func EngineMergeTree() Frag { return BareIdent("MergeTree") }

// frag assembles the whole CREATE TABLE statement from typed pieces.
func (c *CreateTableBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("CREATE TABLE ")(b)
		if c.ifNotExists {
			ddlToken("IF NOT EXISTS ")(b)
		}
		BareIdent(c.name)(b)
		ddlToken(" (")(b)
		for i, col := range c.columns {
			if i > 0 {
				ddlToken(", ")(b)
			}
			col.frag()(b)
		}
		ddlToken(")")(b)
		if c.engine != nil {
			ddlToken(" ENGINE = ")(b)
			c.engine(b)
		}
		if len(c.orderBy) > 0 {
			ddlToken(" ORDER BY (")(b)
			for i, k := range c.orderBy {
				if i > 0 {
					ddlToken(", ")(b)
				}
				Col(k)(b)
			}
			ddlToken(")")(b)
		}
		if c.ttl != nil {
			ddlToken(" ")(b)
			c.ttl(b)
		}
	}
}

// SQL renders the CREATE TABLE statement to ClickHouse text via RenderDDL
// (which asserts the no-positional-bindings DDL invariant).
func (c *CreateTableBuilder) SQL() string {
	return RenderDDL(c.frag())
}

// --- ALTER TABLE ... ADD PROJECTION surface ---
//
// AddProjectionBuilder renders `ALTER TABLE <db>.<table> ADD PROJECTION IF
// NOT EXISTS <name> (<body>)`, where <body> is a SELECT/GROUP BY query the
// caller composes with the same typed QueryBuilder used for read queries.
// The merge engine maintains the projection automatically; cerberus uses it
// to make the metric-name catalog enumeration (`GROUP BY MetricName`) read
// an aggregating projection instead of full-scanning the metrics fact table.
//
// IF NOT EXISTS makes ADD PROJECTION metadata-only and idempotent, so the
// same DDL Apply path covers both freshly-created and pre-existing tables.
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// AddProjectionBuilder builds an ALTER TABLE ADD PROJECTION statement.
type AddProjectionBuilder struct {
	database string
	table    string
	name     string
	cluster  string // "" => no ON CLUSTER clause
	body     *QueryBuilder
}

// AlterTableAddProjection starts an ADD PROJECTION builder for the named
// projection on <database>.<table>. body is the projection definition — a
// QueryBuilder carrying the SELECT (+ optional GROUP BY) shape the engine
// pre-aggregates; it must bind no positional args (DDL invariant).
func AlterTableAddProjection(database, table, name string, body *QueryBuilder) *AddProjectionBuilder {
	return &AddProjectionBuilder{database: database, table: table, name: name, body: body}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *AddProjectionBuilder) OnCluster(name string) *AddProjectionBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table/projection identifiers via BareIdent, the
// optional ON CLUSTER clause via the typed constructor, and the projection
// body via the QueryBuilder's own Frag — no raw token is written here.
func (a *AddProjectionBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("ALTER TABLE ")(b)
		BareIdent(a.database)(b)
		ddlToken(".")(b)
		BareIdent(a.table)(b)
		if a.cluster != "" {
			ddlToken(" ")(b)
			OnCluster(a.cluster)(b)
		}
		ddlToken(" ADD PROJECTION IF NOT EXISTS ")(b)
		BareIdent(a.name)(b)
		ddlToken(" ")(b)
		// QueryBuilder.Frag already wraps the body in the single pair of
		// parentheses the projection-definition grammar requires.
		a.body.Frag()(b)
	}
}

// SQL renders the ALTER TABLE ADD PROJECTION statement to ClickHouse text
// via RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *AddProjectionBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... MODIFY COLUMN surface ---
//
// ModifyColumnBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] MODIFY COLUMN IF EXISTS <col>
// <type>`, the statement that reconciles a deployed column's declared type
// with the one the running binary writes. Widening an Enum8 with new members
// is metadata-only on ClickHouse — no part is rewritten and no mutation is
// scheduled — so the same statement is safe to run on every start, and IF
// EXISTS makes it a no-op on a table that was just created with the wide type.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// ModifyColumnBuilder builds an ALTER TABLE MODIFY COLUMN statement.
type ModifyColumnBuilder struct {
	database string // "" => unqualified table reference
	table    string
	column   string
	cluster  string // "" => no ON CLUSTER clause
	colType  Frag
}

// AlterTableModifyColumn starts a MODIFY COLUMN builder retyping <column> on
// [<database>.]<table> to colType. An empty database emits no qualifier, so a
// table the connection's own database owns is referenced bare.
func AlterTableModifyColumn(database, table, column string, colType Frag) *ModifyColumnBuilder {
	return &ModifyColumnBuilder{database: database, table: table, column: column, colType: colType}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *ModifyColumnBuilder) OnCluster(name string) *ModifyColumnBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table/column identifiers via BareIdent, the optional
// ON CLUSTER clause via the typed constructor, and the column type via the
// caller's type Frag — no raw token is written here.
func (a *ModifyColumnBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("ALTER TABLE ")(b)
		if a.database != "" {
			BareIdent(a.database)(b)
			ddlToken(".")(b)
		}
		BareIdent(a.table)(b)
		if a.cluster != "" {
			ddlToken(" ")(b)
			OnCluster(a.cluster)(b)
		}
		ddlToken(" MODIFY COLUMN IF EXISTS ")(b)
		Col(a.column)(b)
		ddlToken(" ")(b)
		a.colType(b)
	}
}

// SQL renders the ALTER TABLE MODIFY COLUMN statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *ModifyColumnBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... ADD COLUMN surface ---
//
// AddColumnBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] ADD COLUMN IF NOT EXISTS <col>
// <type>`, the statement that brings a deployed table up to a column the
// running binary writes but the table was created without. It is the sibling
// MODIFY COLUMN cannot stand in for: MODIFY IF EXISTS is a no-op on an absent
// column, and `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table
// however its columns are declared, so without this statement a new column
// reaches only fresh deployments.
//
// Adding a column to a MergeTree is metadata-only on ClickHouse — existing
// parts materialise the type's default on read, no mutation is scheduled — so
// the statement is safe to run on every start, and IF NOT EXISTS makes it a
// no-op once the column is there.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// AddColumnBuilder builds an ALTER TABLE ADD COLUMN statement.
type AddColumnBuilder struct {
	database string // "" => unqualified table reference
	table    string
	column   string
	cluster  string // "" => no ON CLUSTER clause
	colType  Frag
}

// AlterTableAddColumn starts an ADD COLUMN builder appending <column> of
// colType to [<database>.]<table>. An empty database emits no qualifier, so a
// table the connection's own database owns is referenced bare.
func AlterTableAddColumn(database, table, column string, colType Frag) *AddColumnBuilder {
	return &AddColumnBuilder{database: database, table: table, column: column, colType: colType}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *AddColumnBuilder) OnCluster(name string) *AddColumnBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via ddlToken,
// bare database/table identifiers via BareIdent, the quoted column via Col, the
// optional ON CLUSTER clause via the typed constructor, and the column type via
// the caller's type Frag — no raw token is written here.
func (a *AddColumnBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("ALTER TABLE ")(b)
		if a.database != "" {
			BareIdent(a.database)(b)
			ddlToken(".")(b)
		}
		BareIdent(a.table)(b)
		if a.cluster != "" {
			ddlToken(" ")(b)
			OnCluster(a.cluster)(b)
		}
		ddlToken(" ADD COLUMN IF NOT EXISTS ")(b)
		Col(a.column)(b)
		ddlToken(" ")(b)
		a.colType(b)
	}
}

// SQL renders the ALTER TABLE ADD COLUMN statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *AddColumnBuilder) SQL() string {
	return RenderDDL(a.frag())
}
