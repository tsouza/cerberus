package chsql

import (
	"fmt"
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
// already imports internal/schema (tableshape.go reads the default OTel
// schema), so a chsql-owned KV that schema's env parser had to reference
// would form an import cycle. The token-emitting Frag stays in chsql (the
// sanctioned primitive zone); only the plain value-carrier struct sits one
// layer down.
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

// TypeNullable wraps an inner type in Nullable(...), the CH wrapper for a
// column whose NULL means "no value" distinctly from the type's own zero —
// used by the numeric-typed materialized attribute columns (cerberus issue
// #2869), where a toXOrNull(...) DEFAULT expression needs a real NULL slot
// for an absent or non-numeric source attribute.
func TypeNullable(inner Frag) Frag {
	return Call("Nullable", inner)
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
	database    string // "" => unqualified table reference
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

// Database qualifies the table with a `<database>.` prefix. Unset (the
// default) renders the bare table name, matching the pre-existing
// unqualified corpus-table callsite; a cerberus-owned table that lives
// alongside the OTel-CH schema (e.g. the DELTA-prefix aggregate table)
// calls this to land in the same configured database as the tables
// around it.
func (c *CreateTableBuilder) Database(name string) *CreateTableBuilder {
	c.database = name
	return c
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

// EngineAggregatingMergeTree renders the bare `AggregatingMergeTree` table
// engine (no arguments) — required for a table carrying a
// SimpleAggregateFunction / AggregateFunction column, whose read-time
// `sum()`/… re-aggregation is correct regardless of how much background
// merging has happened (unlike ReplacingMergeTree, which needs FINAL or a
// dedup step). See EngineReplicatedAggregatingMergeTree for the Replicated-
// database form.
func EngineAggregatingMergeTree() Frag { return BareIdent("AggregatingMergeTree") }

// EngineReplicatedAggregatingMergeTree renders the bare
// `ReplicatedAggregatingMergeTree` table engine — the AggregatingMergeTree
// analogue of EngineReplicatedMergeTree. Like that constructor, no
// positional arguments: inside a Replicated database the Keeper path and
// replica name come from the database's own Replicated(...) coordinates,
// and explicit arguments are rejected (code 36).
func EngineReplicatedAggregatingMergeTree() Frag {
	return BareIdent("ReplicatedAggregatingMergeTree")
}

// frag assembles the whole CREATE TABLE statement from typed pieces.
func (c *CreateTableBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("CREATE TABLE ")(b)
		if c.ifNotExists {
			ddlToken("IF NOT EXISTS ")(b)
		}
		if c.database != "" {
			BareIdent(c.database)(b)
			ddlToken(".")(b)
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

// --- ALTER TABLE ... MODIFY COLUMN ... CODEC surface (cerberus issue #2768) ---
//
// ModifyColumnCodecBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] MODIFY COLUMN IF EXISTS <col>
// CODEC(...)` — retuning ONLY a column's compression codec, deliberately
// WITHOUT re-declaring its type: ClickHouse's MODIFY COLUMN grammar treats
// [type] as optional (https://clickhouse.com/docs/sql-reference/statements/alter/column#modify-column),
// so a codec-only change never needs to duplicate a type fact the upstream
// OTel-CH exporter template already owns (see this package's doc comment) —
// unlike ModifyColumnBuilder above, which exists specifically to retype a
// column and so must carry one.
//
// A codec change is legal on a sorting-key column and applies to NEW parts
// only; existing parts keep their original codec until a background merge
// (or an operator-run `OPTIMIZE ... FINAL`) rewrites them — see
// docs/operations.md. Re-running the identical MODIFY COLUMN CODEC
// statement is a no-op: ClickHouse compares the declared codec against the
// column's current one and only schedules work when they actually differ,
// so applying this on every boot never re-triggers a conversion once the
// codec has converged. IF EXISTS is the only guard the statement itself
// carries — it makes MODIFY a no-op on a column that doesn't exist (e.g. a
// signal not yet auto-created); the safety of re-running it against an
// ALREADY-CONVERGED column comes entirely from ClickHouse's own
// no-op-on-unchanged behavior above, not from any guard clause cerberus
// writes — there is no `IF NOT EXISTS`-shaped guard for MODIFY COLUMN.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// ModifyColumnCodecBuilder builds an ALTER TABLE MODIFY COLUMN CODEC
// statement.
type ModifyColumnCodecBuilder struct {
	database string // "" => unqualified table reference
	table    string
	column   string
	cluster  string // "" => no ON CLUSTER clause
	codec    Frag
}

// AlterTableModifyColumnCodec starts a MODIFY COLUMN CODEC builder retuning
// the compression codec of <column> on [<database>.]<table> to codec — a
// Frag built via Codec, e.g. Codec(BareIdent("DoubleDelta"),
// Call("ZSTD", InlineLit(1))). An empty database emits no qualifier, so a
// table the connection's own database owns is referenced bare.
func AlterTableModifyColumnCodec(database, table, column string, codec Frag) *ModifyColumnCodecBuilder {
	return &ModifyColumnCodecBuilder{database: database, table: table, column: column, codec: codec}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *ModifyColumnCodecBuilder) OnCluster(name string) *ModifyColumnCodecBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table identifiers via BareIdent, the quoted
// column via Col, the optional ON CLUSTER clause via the typed constructor,
// and the codec via the caller's Frag (built via Codec) — no raw token is
// written here, and no type is emitted (see the package doc comment above).
func (a *ModifyColumnCodecBuilder) frag() Frag {
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
		a.codec(b)
	}
}

// SQL renders the ALTER TABLE MODIFY COLUMN CODEC statement to ClickHouse
// text via RenderDDL (which asserts the no-positional-bindings DDL
// invariant).
func (a *ModifyColumnCodecBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... MODIFY COLUMN ... TTL surface (cerberus issue #2769) ---
//
// ModifyColumnTTLBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] MODIFY COLUMN IF EXISTS <col>
// <type> TTL <age-expr>` — an operator-opt-in COLUMN-level TTL that expires
// ONE column (or Nested subcolumn) earlier than the row itself: the row
// (severity, attributes, materialized k8s columns, trace correlation)
// survives the signal's own retention TTL while a single heavy payload
// column (the logs table's Body; the traces table's Events.Attributes /
// Links.Attributes Nested subcolumns) empties sooner — see
// internal/schema/ddl's ColumnTTL doc comment for the column choices.
//
// UNLIKE ModifyColumnCodecBuilder, colType is REQUIRED, not optional —
// verified empirically against a real ClickHouse 25.9 server:
// `ALTER TABLE t MODIFY COLUMN Body TTL <expr>` with no type is a syntax
// error, while `ALTER TABLE t MODIFY COLUMN Body String TTL <expr>`
// succeeds and SHOW CREATE TABLE confirms the column's existing CODEC
// clause is preserved untouched (MODIFY COLUMN only overwrites the
// attributes it names). ClickHouse's MODIFY COLUMN grammar only treats
// [type] as optional when no TTL clause follows — the one place this
// package's MODIFY COLUMN family cannot omit it. colType must therefore
// reproduce the column's ACTUAL declared type exactly, or ClickHouse
// silently RETYPES the column instead of just adding a TTL.
//
// A column TTL is legal on a column carrying a data-skipping index —
// verified empirically with idx_lower_body (tokenbf_v1 over lower(Body))
// in place on the target column: the ALTER succeeds, SHOW CREATE TABLE
// keeps the index declaration unchanged, `ALTER TABLE ... MATERIALIZE
// INDEX` still succeeds after the TTL has expired data, and a query
// filtering through the index returns the same (correct, now-empty)
// result a query against the bare column would — no index drop/recreate
// is needed to TTL an indexed column, so cerberus's curated ALTER never
// touches idx_lower_body / idx_body_text.
//
// A Nested column's subcolumns (materialized by ClickHouse as ordinary
// Array(...) columns, e.g. `Events.Attributes Array(Map(...))`) accept a
// TTL the same way ordinary columns do — verified empirically that
// `ALTER TABLE ... MATERIALIZE TTL` clears an expired row's Nested
// subcolumn to its default (empty array) independently of a fresh row
// sharing the same part, while every other subcolumn (and the row itself)
// is untouched.
//
// Like every column TTL, this materializes lazily: the ALTER is
// metadata-only, and existing parts keep their current values until a
// background merge — or an operator-run `ALTER TABLE ... MATERIALIZE TTL`
// — rewrites them; see docs/operations.md's "Materializing a column TTL on
// existing parts" for the full guidance, including why `OPTIMIZE ... FINAL`
// (the codec back-fill's own recommended tool) is the WRONG statement here:
// on a real ClickHouse 25.9 server it reliably raises a client-visible
// `Code: 10. NOT_FOUND_COLUMN_IN_BLOCK` error against a table where a
// data-skipping index depends on a column that also carries a column TTL
// (idx_lower_body + a Body TTL is exactly this shape) — reproduced on a
// minimal two-column table carrying only that combination, so it is not
// specific to cerberus's own schema. The underlying data is unaffected (a
// plain SELECT after the error still shows the correct post-TTL values,
// and MATERIALIZE TTL on the identical table completes without error), but
// it makes OPTIMIZE ... FINAL the wrong tool for a column TTL specifically.
// A table's `ttl_only_drop_parts=1` setting governs ONLY the table-level
// DELETE TTL (whether ClickHouse may drop a whole part without rewriting
// it); it does NOT block or otherwise affect a column TTL, which
// materializes via a normal merge or MATERIALIZE TTL regardless of that
// setting — verified empirically against a table carrying
// ttl_only_drop_parts=1 throughout every probe above.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// ModifyColumnTTLBuilder builds an ALTER TABLE MODIFY COLUMN TTL statement.
type ModifyColumnTTLBuilder struct {
	database string // "" => unqualified table reference
	table    string
	column   string
	cluster  string // "" => no ON CLUSTER clause
	colType  Frag
	age      Frag
}

// AlterTableModifyColumnTTL starts a MODIFY COLUMN TTL builder installing a
// column-level TTL on <column> (an ordinary column or a `Parent.Child`
// Nested subcolumn) of [<database>.]<table>, expiring it `toDateTime(<
// tsColumn>) + toIntervalXxx(N)` after ttl — the same age-expression shape
// TableTTL renders for the table's own row-level TTL. colType is the
// column's exact currently-declared ClickHouse type (see the package doc
// comment above for why MODIFY COLUMN TTL cannot omit it). ttl must be
// positive — a zero or negative duration is "no column TTL", the caller's
// job to not call this for, so an invalid value panics at construction
// time rather than rendering DDL ClickHouse would reject at apply time.
func AlterTableModifyColumnTTL(database, table, column string, colType Frag, tsColumn string, ttl time.Duration) *ModifyColumnTTLBuilder {
	if ttl <= 0 {
		panic("chsql: AlterTableModifyColumnTTL requires a positive ttl")
	}
	return &ModifyColumnTTLBuilder{
		database: database,
		table:    table,
		column:   column,
		colType:  colType,
		age:      ttlAge(tsColumn, ttl),
	}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *ModifyColumnTTLBuilder) OnCluster(name string) *ModifyColumnTTLBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table identifiers via BareIdent, the quoted
// column via Col (which backtick-quotes a `Parent.Child` Nested subcolumn
// name as the single identifier ClickHouse expects — NOT Qual, which would
// wrongly split it into two separately-quoted parts), the caller's type
// Frag, the optional ON CLUSTER clause via the typed constructor, and the
// TTL age expression via the same ttlAge helper TableTTL uses — no raw
// token is written here.
func (a *ModifyColumnTTLBuilder) frag() Frag {
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
		ddlToken(" TTL ")(b)
		a.age(b)
	}
}

// SQL renders the ALTER TABLE MODIFY COLUMN TTL statement to ClickHouse
// text via RenderDDL (which asserts the no-positional-bindings DDL
// invariant).
func (a *ModifyColumnTTLBuilder) SQL() string {
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
	database    string // "" => unqualified table reference
	table       string
	column      string
	cluster     string // "" => no ON CLUSTER clause
	colType     Frag
	defaultExpr Frag // nil => no DEFAULT clause
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

// Default adds a `DEFAULT <expr>` clause to the added column. Unlike a
// MATERIALIZED column (computed once at insert time and frozen), a DEFAULT
// column's value for a row in a part that predates the ALTER is computed
// LAZILY at read time from that row's other already-stored columns — see
// cerberus issue #2776's PR body for the real-ClickHouse (26.6) evidence
// this relies on: a fresh ADD COLUMN ... DEFAULT <mapCol>[<key>] reads
// byte-identical to the map on every existing row immediately, with zero
// mutation queued, before any MATERIALIZE COLUMN backfill runs. This is
// what lets a materialized attribute column be read safely the instant it
// exists, with no "backfill verified" operator declaration required (see
// schema.Traces.MaterializedSpanAttributeColumns' doc for the read-side
// consequence).
func (a *AddColumnBuilder) Default(expr Frag) *AddColumnBuilder {
	a.defaultExpr = expr
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via ddlToken,
// bare database/table identifiers via BareIdent, the quoted column via Col, the
// optional ON CLUSTER clause via the typed constructor, the column type via
// the caller's type Frag, and the optional DEFAULT clause via the caller's
// expr Frag — no raw token is written here.
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
		if a.defaultExpr != nil {
			ddlToken(" DEFAULT ")(b)
			a.defaultExpr(b)
		}
	}
}

// SQL renders the ALTER TABLE ADD COLUMN statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *AddColumnBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... MATERIALIZE COLUMN surface ---
//
// MaterializeColumnBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] MATERIALIZE COLUMN <col>`, the
// one-time backfill ALTER that forces a DEFAULT (or MATERIALIZED) column's
// value to be physically written into every EXISTING part, instead of left
// to the lazy per-read DEFAULT-expression evaluation AddColumnBuilder's own
// doc describes. Correctness does not depend on this statement ever
// running — see AddColumnBuilder.Default's doc — only the read-performance
// win does: an un-backfilled old part still answers correctly, just by
// decomposing the source Map on every read instead of reading the narrow
// column directly.
//
// Unlike ADD COLUMN, ClickHouse has no IF NOT EXISTS-shaped guard for
// MATERIALIZE COLUMN against a column that is already fully backfilled —
// only IF EXISTS (a no-op on an absent column). Re-issuing it against an
// already-backfilled table queues a mutation with zero parts left to touch
// (confirmed against real ClickHouse 26.6 — see the PR body), so the
// idempotent-on-every-boot contract this package's other ALTERs rely on
// still holds; the residual cost is a near-instant no-op mutation record,
// not a wasted rewrite.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// MaterializeColumnBuilder builds an ALTER TABLE MATERIALIZE COLUMN
// statement.
type MaterializeColumnBuilder struct {
	database string // "" => unqualified table reference
	table    string
	column   string
	cluster  string // "" => no ON CLUSTER clause
}

// AlterTableMaterializeColumn starts a MATERIALIZE COLUMN builder
// backfilling <column> on [<database>.]<table>. An empty database emits no
// qualifier, so a table the connection's own database owns is referenced
// bare.
func AlterTableMaterializeColumn(database, table, column string) *MaterializeColumnBuilder {
	return &MaterializeColumnBuilder{database: database, table: table, column: column}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *MaterializeColumnBuilder) OnCluster(name string) *MaterializeColumnBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table identifiers via BareIdent, the quoted
// column via Col, and the optional ON CLUSTER clause via the typed
// constructor — no raw token is written here.
func (a *MaterializeColumnBuilder) frag() Frag {
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
		ddlToken(" MATERIALIZE COLUMN ")(b)
		Col(a.column)(b)
	}
}

// SQL renders the ALTER TABLE MATERIALIZE COLUMN statement to ClickHouse
// text via RenderDDL (which asserts the no-positional-bindings DDL
// invariant).
func (a *MaterializeColumnBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... ADD INDEX surface ---
//
// AddIndexBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] ADD INDEX IF NOT EXISTS <name>
// <expr> TYPE <type> GRANULARITY <n>`, the statement that installs a
// ClickHouse data-skipping index on a column already present on a table
// cerberus does not otherwise own the CREATE TABLE body of (the upstream
// OTel exporter template — see the package doc comment). Adding a skip
// index is metadata-only for NEW parts; it does not reorder or duplicate
// existing data (unlike a PROJECTION, which stores a second physical copy),
// but existing parts need a separate `ALTER TABLE ... MATERIALIZE INDEX`
// backfill to benefit retroactively — see docs/operations.md.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// AddIndexBuilder builds an ALTER TABLE ADD INDEX statement.
type AddIndexBuilder struct {
	database    string // "" => unqualified table reference
	table       string
	name        string
	cluster     string // "" => no ON CLUSTER clause
	expr        Frag
	indexType   string
	granularity int
}

// AlterTableAddIndex starts an ADD INDEX builder installing a data-skipping
// index named <name> over expr on [<database>.]<table>, of the given
// ClickHouse index type ("minmax", "set(N)", "bloom_filter", …) and
// GRANULARITY. An empty database emits no qualifier, so a table the
// connection's own database owns is referenced bare. granularity must be
// positive — GRANULARITY 0 or negative is not a valid ClickHouse skip-index
// clause, so a non-positive value panics at construction time rather than
// rendering DDL ClickHouse would reject at apply time.
func AlterTableAddIndex(database, table, name string, expr Frag, indexType string, granularity int) *AddIndexBuilder {
	if granularity <= 0 {
		panic(fmt.Sprintf("chsql: AlterTableAddIndex granularity must be positive, got %d", granularity))
	}
	return &AddIndexBuilder{database: database, table: table, name: name, expr: expr, indexType: indexType, granularity: granularity}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *AddIndexBuilder) OnCluster(name string) *AddIndexBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table/index identifiers via BareIdent, the
// indexed expression via the caller's Frag, the index type via BareIdent
// (a fixed ClickHouse type keyword, never data), the GRANULARITY value via
// InlineLit, and the optional ON CLUSTER clause via the typed constructor —
// no raw token is written here.
func (a *AddIndexBuilder) frag() Frag {
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
		ddlToken(" ADD INDEX IF NOT EXISTS ")(b)
		BareIdent(a.name)(b)
		ddlToken(" ")(b)
		a.expr(b)
		ddlToken(" TYPE ")(b)
		BareIdent(a.indexType)(b)
		ddlToken(" GRANULARITY ")(b)
		InlineLit(a.granularity)(b)
	}
}

// SQL renders the ALTER TABLE ADD INDEX statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *AddIndexBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... DROP INDEX surface ---
//
// DropIndexBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] DROP INDEX IF EXISTS <name>`,
// the statement that retires a ClickHouse data-skipping index a table
// carries — the inverse of AddIndexBuilder above. Unlike ADD INDEX, this is
// deliberately NOT part of any boot-time DDL apply path: dropping an index a
// running deployment's queries might still be planning against is a real
// production-cluster decision an operator makes deliberately (cerberus
// issue #2839's `cerberus schema retire-idx-lower-body` verb is the current
// caller), never something rendered on every server start the way
// AddIndexBuilder's callers are. DROP INDEX is metadata-only — ClickHouse
// discards the index's on-disk data for every part in the background; no
// separate MATERIALIZE step applies to a drop the way one does to an add.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// DropIndexBuilder builds an ALTER TABLE DROP INDEX statement.
type DropIndexBuilder struct {
	database string // "" => unqualified table reference
	table    string
	name     string
	cluster  string // "" => no ON CLUSTER clause
}

// AlterTableDropIndex starts a DROP INDEX builder retiring the data-skipping
// index named <name> from [<database>.]<table>. An empty database emits no
// qualifier, so a table the connection's own database owns is referenced
// bare. The rendered statement always carries IF EXISTS, mirroring
// AddIndexBuilder's own IF NOT EXISTS — an operator re-running the verb
// against a table that has already had the index dropped gets a clean
// no-op, not a client-visible "unknown index" error.
func AlterTableDropIndex(database, table, name string) *DropIndexBuilder {
	return &DropIndexBuilder{database: database, table: table, name: name}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *DropIndexBuilder) OnCluster(name string) *DropIndexBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table/index identifiers via BareIdent, and the
// optional ON CLUSTER clause via the typed constructor — no raw token is
// written here.
func (a *DropIndexBuilder) frag() Frag {
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
		ddlToken(" DROP INDEX IF EXISTS ")(b)
		BareIdent(a.name)(b)
	}
}

// SQL renders the ALTER TABLE DROP INDEX statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *DropIndexBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- ALTER TABLE ... ADD STATISTICS surface ---
//
// AddStatisticsBuilder renders
// `ALTER TABLE [<db>.]<table> [ON CLUSTER x] ADD STATISTICS IF NOT EXISTS
// <col1>[, <col2>, ...] TYPE <type1>[, <type2>, ...]`, the statement that
// installs ClickHouse column statistics (cerberus issue #2766) on columns of
// a table cerberus does not otherwise own the CREATE TABLE body of (the
// upstream OTel exporter template — see the package doc comment). Statistics
// give the query planner real cardinality/selectivity estimates for
// PREWHERE-pushdown and join-ordering decisions, in place of cerberus's own
// hand-rolled static heuristic (internal/chsql/prewhere.go).
//
// Grammar confirmed against
// https://clickhouse.com/docs/sql-reference/statements/alter/statistics:
// both the column list and the TYPE list are bare comma-separated lists — NO
// enclosing parentheses (unlike, say, a tuple literal) — e.g.
// `ALTER TABLE t1 MODIFY STATISTICS c, d TYPE tdigest, uniq_v2`.
//
// Adding statistics is metadata-only for NEW parts; existing parts need a
// separate `ALTER TABLE ... MATERIALIZE STATISTICS` backfill to benefit
// retroactively — see docs/operations.md. ClickHouse Cloud does not support
// column statistics at all and refuses ADD STATISTICS outright; the apply
// path tolerates that refusal rather than treating it as fatal — see
// internal/schema/ddl's isColumnStatisticsUnsupported.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// AddStatisticsBuilder builds an ALTER TABLE ADD STATISTICS statement.
type AddStatisticsBuilder struct {
	database string // "" => unqualified table reference
	table    string
	cluster  string // "" => no ON CLUSTER clause
	columns  []string
	types    []string
}

// AlterTableAddStatistics starts an ADD STATISTICS builder installing every
// entry of types on every entry of columns of [<database>.]<table>. An empty
// database emits no qualifier, so a table the connection's own database owns
// is referenced bare. Both columns and types must carry at least one entry —
// ClickHouse's grammar requires both lists non-empty, so an empty slice
// panics at construction time rather than rendering DDL ClickHouse would
// reject at apply time.
func AlterTableAddStatistics(database, table string, columns, types []string) *AddStatisticsBuilder {
	if len(columns) == 0 {
		panic("chsql: AlterTableAddStatistics requires at least one column")
	}
	if len(types) == 0 {
		panic("chsql: AlterTableAddStatistics requires at least one statistics type")
	}
	return &AddStatisticsBuilder{database: database, table: table, columns: columns, types: types}
}

// OnCluster adds an `ON CLUSTER <name>` clause so the ALTER replicates the
// same way the CREATE statements do under a classic ON CLUSTER deployment.
// A Replicated database replicates the DDL itself and needs no clause.
func (a *AddStatisticsBuilder) OnCluster(name string) *AddStatisticsBuilder {
	a.cluster = name
	return a
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table identifiers via BareIdent, each column via
// Col (backtick-quoted), each statistics type via BareIdent (a fixed
// ClickHouse type keyword, never data), and the optional ON CLUSTER clause
// via the typed constructor — no raw token is written here.
func (a *AddStatisticsBuilder) frag() Frag {
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
		ddlToken(" ADD STATISTICS IF NOT EXISTS ")(b)
		for i, col := range a.columns {
			if i > 0 {
				ddlToken(", ")(b)
			}
			Col(col)(b)
		}
		ddlToken(" TYPE ")(b)
		for i, typ := range a.types {
			if i > 0 {
				ddlToken(", ")(b)
			}
			BareIdent(typ)(b)
		}
	}
}

// SQL renders the ALTER TABLE ADD STATISTICS statement to ClickHouse text
// via RenderDDL (which asserts the no-positional-bindings DDL invariant).
func (a *AddStatisticsBuilder) SQL() string {
	return RenderDDL(a.frag())
}

// --- CREATE MATERIALIZED VIEW surface (cerberus-owned aggregate tables) ---
//
// CreateMaterializedViewBuilder renders the `CREATE MATERIALIZED VIEW ...
// TO <target> AS <SELECT>` form — the ONE cerberus uses. A materialized
// view whose target is a first-class table this package ALSO creates (as
// opposed to a bare `CREATE MATERIALIZED VIEW ... AS SELECT ...`, which
// implicitly owns an internal, unnamed target table). The explicit `TO`
// form is what lets the target survive a `DROP TABLE <mv>` — dropping the
// view alone never drops the accumulated aggregate data — and lets an
// operator (or a read-side emitter) query the target table directly by
// name, same as any other table.
//
// Like the other DDL builders it binds no positional `?` values, so SQL
// renders through RenderDDL.

// CreateMaterializedViewBuilder builds a CREATE MATERIALIZED VIEW ... TO
// ... AS ... statement, optionally as a REFRESH-scheduled (rather than
// on-insert-triggered) view — see RefreshEveryMinutes.
type CreateMaterializedViewBuilder struct {
	database            string // "" => unqualified view reference
	name                string
	ifNotExists         bool
	cluster             string // "" => no ON CLUSTER clause
	refreshEveryMinutes int    // 0 => on-insert-triggered (no REFRESH clause)
	toDatabase          string // "" => unqualified target reference
	toTable             string
	body                *QueryBuilder
}

// CreateMaterializedView starts a builder for the named materialized view.
func CreateMaterializedView(name string) *CreateMaterializedViewBuilder {
	return &CreateMaterializedViewBuilder{name: name}
}

// Database qualifies the view's own name with a `<database>.` prefix.
func (c *CreateMaterializedViewBuilder) Database(name string) *CreateMaterializedViewBuilder {
	c.database = name
	return c
}

// IfNotExists adds the IF NOT EXISTS guard so re-create is idempotent.
func (c *CreateMaterializedViewBuilder) IfNotExists() *CreateMaterializedViewBuilder {
	c.ifNotExists = true
	return c
}

// OnCluster adds an `ON CLUSTER <name>` clause so the CREATE replicates the
// same way the other DDL builders' statements do under a classic ON
// CLUSTER deployment. A Replicated database replicates the DDL itself and
// needs no clause.
func (c *CreateMaterializedViewBuilder) OnCluster(name string) *CreateMaterializedViewBuilder {
	c.cluster = name
	return c
}

// RefreshEveryMinutes turns the view into a REFRESH-scheduled materialized
// view — `REFRESH EVERY <n> MINUTE` — instead of the default on-insert
// trigger. Each scheduled refresh re-runs the whole body query and, on
// success, ATOMICALLY swaps it into the target table (an EXCHANGE TABLES
// under the hood — see ClickHouse's refreshable-materialized-view design
// doc: https://clickhouse.com/docs/materialize/refreshable-materialized-view
// and upstream PR #70550, which GA'd the feature in 24.10 by dropping the
// `allow_experimental_refreshable_materialized_view` flag requirement); a
// refresh that ERRORS leaves the target holding whatever the last
// successful swap produced, never a partial or empty result — the whole
// reason this shape suits an interactive-read catalog table over the
// on-insert form. n must be > 0 — callers gate emitting this clause at all
// behind their own capability check, so a non-positive n here is a caller
// bug, not a "no REFRESH clause" request; it panics immediately rather
// than silently falling back to the on-insert form.
func (c *CreateMaterializedViewBuilder) RefreshEveryMinutes(n int) *CreateMaterializedViewBuilder {
	if n <= 0 {
		panic("chsql: CreateMaterializedViewBuilder.RefreshEveryMinutes requires n > 0")
	}
	c.refreshEveryMinutes = n
	return c
}

// To sets the `<database>.<table>` target table the view writes into —
// already created by the caller earlier in the same statement sequence
// (the MV references it, so it must exist first).
func (c *CreateMaterializedViewBuilder) To(database, table string) *CreateMaterializedViewBuilder {
	c.toDatabase = database
	c.toTable = table
	return c
}

// As sets the SELECT body. body must bind no positional args (the DDL
// invariant) — every value in a materialized-view definition is part of
// the statement shape, never a `?` binding (use InlineLit, never Lit,
// inside body).
func (c *CreateMaterializedViewBuilder) As(body *QueryBuilder) *CreateMaterializedViewBuilder {
	c.body = body
	return c
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/view/target identifiers via BareIdent, the
// optional ON CLUSTER clause via the typed constructor, and the SELECT body
// via QueryBuilder's own unexported writeInto (the bare `SELECT ...` text,
// NOT wrapped in the parens QueryBuilder.Frag() would add — a materialized
// view's AS clause takes the bare SELECT, unlike a nested subquery) — no
// raw token is written here.
func (c *CreateMaterializedViewBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("CREATE MATERIALIZED VIEW ")(b)
		if c.ifNotExists {
			ddlToken("IF NOT EXISTS ")(b)
		}
		if c.database != "" {
			BareIdent(c.database)(b)
			ddlToken(".")(b)
		}
		BareIdent(c.name)(b)
		if c.cluster != "" {
			ddlToken(" ")(b)
			OnCluster(c.cluster)(b)
		}
		if c.refreshEveryMinutes > 0 {
			ddlToken(" REFRESH EVERY ")(b)
			InlineLit(int64(c.refreshEveryMinutes))(b)
			ddlToken(" MINUTE")(b)
		}
		ddlToken(" TO ")(b)
		if c.toDatabase != "" {
			BareIdent(c.toDatabase)(b)
			ddlToken(".")(b)
		}
		BareIdent(c.toTable)(b)
		ddlToken(" AS ")(b)
		c.body.writeInto(b)
	}
}

// SQL renders the CREATE MATERIALIZED VIEW statement to ClickHouse text via
// RenderDDL (which asserts the no-positional-bindings DDL invariant).
// Panics if To or As was never called: without a target table, frag would
// render a bare ` TO  AS ...` with an empty identifier — syntactically
// invalid SQL that ClickHouse would only reject at apply time; without a
// body, frag's writeInto call on a nil *QueryBuilder panics anyway, just
// with a far less legible stack. Both fail fast, at statement-construction
// time, with a message naming the missing call.
func (c *CreateMaterializedViewBuilder) SQL() string {
	if c.toTable == "" {
		panic("chsql: CreateMaterializedViewBuilder.SQL called without To(...) — the view has no target table")
	}
	if c.body == nil {
		panic("chsql: CreateMaterializedViewBuilder.SQL called without As(...) — the view has no SELECT body")
	}
	return RenderDDL(c.frag())
}

// --- INSERT INTO ... SELECT surface (cerberus-owned backfill statements) ---
//
// InsertSelectBuilder renders `INSERT INTO <database>.<table> (<cols...>)
// <SELECT ...>` — the shape a one-time historical backfill uses to populate
// a cerberus-owned aggregate table (e.g. `cerberus schema
// delta-prefix-backfill`, internal/deltaprefix). Unlike the CREATE/ALTER
// builders above this is a genuine DML statement that DOES carry
// positional `?` bindings (a backfill cutoff timestamp is real data, bound
// via Lit inside body's WHERE — never baked into the statement shape via
// InlineLit), so it renders through Render's plain (sql, args) contract,
// never RenderDDL.

// InsertSelectBuilder builds an INSERT INTO ... SELECT statement.
type InsertSelectBuilder struct {
	database string // "" => unqualified table reference
	table    string
	columns  []string
	body     *QueryBuilder
}

// InsertSelect starts a builder inserting into `<database>.<table>`
// (<database> may be "" for an unqualified reference), naming the target
// column list explicitly (so the SELECT's column order is pinned rather
// than left to table-definition order) with body as the SELECT source.
func InsertSelect(database, table string, columns []string, body *QueryBuilder) *InsertSelectBuilder {
	return &InsertSelectBuilder{database: database, table: table, columns: columns, body: body}
}

// frag assembles the statement from typed pieces: keyword tokens via
// ddlToken, bare database/table identifiers via BareIdent, backtick-quoted
// column identifiers via Col, and the SELECT body via QueryBuilder's own
// unexported writeInto (the bare `SELECT ...` text, matching the INSERT ...
// SELECT grammar) — no raw token is written here.
func (i *InsertSelectBuilder) frag() Frag {
	return func(b *Builder) {
		ddlToken("INSERT INTO ")(b)
		if i.database != "" {
			BareIdent(i.database)(b)
			ddlToken(".")(b)
		}
		BareIdent(i.table)(b)
		ddlToken(" (")(b)
		for idx, col := range i.columns {
			if idx > 0 {
				ddlToken(", ")(b)
			}
			Col(col)(b)
		}
		ddlToken(") ")(b)
		i.body.writeInto(b)
	}
}

// Build renders the statement to (sql, args) — unlike the CREATE/ALTER
// builders' SQL() method, args is non-empty whenever body binds real values
// via Lit (the intended shape for a backfill cutoff bound).
func (i *InsertSelectBuilder) Build() (string, []any) {
	return Render(i.frag())
}

// TruncateTable renders `TRUNCATE TABLE <database>.<table>` — the statement
// internal/downsampletier's Rebuild issues before a full re-populate
// (cerberus issue #2751): unlike Backfill's incremental INSERT ... SELECT,
// a rebuild must start from an empty table, since AggregatingMergeTree
// merges are additive and re-running the same INSERT twice would double
// every state rather than overwrite it. database may be "" for an
// unqualified table reference, matching InsertSelect's own convention. No
// positional bindings, so this returns a plain string like the CREATE/ALTER
// builders' SQL() method, not InsertSelect's own (sql, args) pair.
func TruncateTable(database, table string) string {
	return RenderDDL(func(b *Builder) {
		ddlToken("TRUNCATE TABLE ")(b)
		if database != "" {
			BareIdent(database)(b)
			ddlToken(".")(b)
		}
		BareIdent(table)(b)
	})
}
