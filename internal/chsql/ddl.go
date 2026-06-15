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

// EngineReplicatedMergeTree returns
// `ReplicatedMergeTree('<zooPath>', '<replica>')` — the replicated table
// engine whose DATA replicates across replicas via Keeper. Inside a
// Replicated database the table DDL is replicated either way, but only a
// ReplicatedMergeTree engine shares the actual rows; a plain MergeTree leaves
// each replica with an independent copy. zooPath and replica are single-quoted
// CH string literals; pass the ClickHouse-standard
// "/clickhouse/tables/{uuid}/{shard}" / "{replica}" (the server expands the
// {uuid}/{shard}/{replica} macros), or whatever the cluster's
// default_replica_path convention is.
func EngineReplicatedMergeTree(zooPath, replica string) Frag {
	return Call("ReplicatedMergeTree", InlineLit(zooPath), InlineLit(replica))
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
