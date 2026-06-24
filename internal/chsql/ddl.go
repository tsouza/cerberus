package chsql

import "time"

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

// TableTTL returns a Frag rendering the ClickHouse table TTL clause
// `TTL toDateTime(<column>) + toIntervalXxx(N)` for retention d, or nil
// when d <= 0 (no retention). column is the bare time column the signal
// keys retention on (TimeUnix for metrics, Timestamp for logs / traces
// spans, Start for the traces lookup table); it is emitted as a bare CH
// identifier (BareIdent), matching the upstream template's unquoted
// toDateTime(<col>) form. N is the coarsest exact interval bucket for d.
func TableTTL(column string, d time.Duration) Frag {
	if d <= 0 {
		return nil
	}
	fn, n := ttlInterval(d)
	expr := Add(
		Call("toDateTime", BareIdent(column)),
		Call(fn, InlineLit(n)),
	)
	return func(b *Builder) {
		ddlToken("TTL ")(b)
		expr(b)
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

// EnumPair is one (name → value) entry of an Enum8 column type.
type EnumPair struct {
	Name  string
	Value int64
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
			InlineLit(p.Value)(b)
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
