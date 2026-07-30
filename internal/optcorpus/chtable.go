package optcorpus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
)

// CorpusTableName is the ClickHouse table the CH-table sink writes the
// router-calibration corpus to. The operator owns it; the sink creates it
// (IF NOT EXISTS) at construction so the corpus lands without a separate
// migration step.
const CorpusTableName = "cerberus_router_corpus"

// corpusRetention is the TTL on the corpus table: rows older than this are
// dropped by the MergeTree TTL sweep. 30 days is enough history to see a
// calibration signal (the wrong-route overlap) without unbounded growth on a
// table whose only consumer is the offline go/no-go analysis.
const corpusRetention = 30 * 24 * time.Hour

// CHExecer is the narrow ClickHouse write surface the CH-table sink needs: run
// the CREATE TABLE DDL and open an INSERT batch. clickhouse-go/v2's driver.Conn
// satisfies it (via *chclient.Client.Conn()); a fake satisfies it in tests
// without a server. Keeping it narrow (and separate from CHConn, the read
// surface) means optcorpus does not import chclient, avoiding an import cycle.
type CHExecer interface {
	Exec(ctx context.Context, query string, args ...any) error
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// CHTableConn is the full surface the CH-table sink needs: the write surface
// that runs DDL and opens batches, plus the read surface that reads the
// deployed column type back out of system.columns. clickhouse-go/v2's
// driver.Conn satisfies both halves, so production wiring passes one value.
type CHTableConn interface {
	CHExecer
	CHConn
}

// CHTableSink is the flag-gated ClickHouse-table sink the Row doc-comment
// anticipates: instead of (or alongside) the JSONL file, it appends each
// reconciled Row to a MergeTree the operator can query directly with the
// go/no-go analysis SQL. It is the column-for-column materialisation of Row —
// the JSONL sink and this sink write the same data, so an operator can move
// between them without reshaping the corpus.
//
// Writes go through clickhouse-go's columnar batch API (PrepareBatch + Append),
// so no row-value SQL string is composed; only the CREATE TABLE DDL is a
// statement, and that is built with the typed chsql DDL builder. A write
// failure is returned to the reconciler, which logs it and retries the same ids
// next interval (the corpus is failure-open: a sink outage degrades the corpus,
// never the data plane).
type CHTableSink struct {
	conn  CHExecer
	table string
}

// corpusInsertStmt is the INSERT target statement for the columnar batch. It
// names the table and the column order the batch Appends match; clickhouse-go
// requires the INSERT statement as text, but the row VALUES are streamed
// column-wise via Append — no value SQL is concatenated.
const corpusInsertStmt = "INSERT INTO " + CorpusTableName

// exitStatusColumn and routeColumn are the corpus Enum8 columns whose member
// set the running binary can outgrow. Each is named once and shared by the DDL,
// the reconciling ALTER, and the verify read (see reconciledEnumColumns).
const (
	exitStatusColumn = "exit_status"
	routeColumn      = "route"
)

// RouteUnclassified is the route token a Row carries when no routing
// classification ran — the Solver is off, or the head is one Solver.Classify
// does not classify (every non-PromQL query). It is a MEMBER of the route
// Enum8, not an absence.
//
// It is exported because internal/routerrules re-declares it verbatim (that
// package deliberately imports neither optcorpus nor chclient), and a
// re-declaration nobody can compare against is a comment, not a contract:
// routerrules' route_contract_test.go pins the two in lockstep.
//
// Storing it as a member is the whole point: the rule engine reads `route` to
// select the ROUTABLE population (`route = 'A'` is "classified and declined on
// cost"), and folding an unclassified row into 'A' makes it indistinguishable
// from a genuine route-A classification. Every LogQL / TraceQL row is
// unclassified, so that fold silently enrols the two non-PromQL heads in every
// route-A population — and it disagrees with the JSONL sink, which writes the
// token verbatim, so the same corpus yielded different findings per backend.
//
// The token is the empty string rather than a word so the two sinks are
// byte-identical: JSONL already writes Row.Route verbatim and historical rows
// already carry "".
const RouteUnclassified = ""

// routeUnclassifiedValue is the Enum8 value RouteUnclassified stores as. 'A' and
// 'B' keep the values they have always been stored under — widening a deployed
// column must not remap existing rows — so the new member takes the next one.
const routeUnclassifiedValue = 2

// routeEnumMembers is the route column's member set: the single source of truth
// behind both the column type and routeEnumValue.
func routeEnumMembers() []chsql.EnumPair {
	return []chsql.EnumPair{
		{Name: "A", Value: 0},
		{Name: "B", Value: 1},
		{Name: RouteUnclassified, Value: routeUnclassifiedValue},
	}
}

// exitStatusEnumMembers renders the exit_status member set from the exitStatuses
// list, so the column type, the string→value mapping (exitEnumValue), and the
// ExitStatus iota are one source of truth rather than three that must agree.
func exitStatusEnumMembers() []chsql.EnumPair {
	pairs := make([]chsql.EnumPair, 0, len(exitStatuses))
	for _, s := range exitStatuses {
		pairs = append(pairs, chsql.EnumPair{Name: s.String(), Value: int8(s)})
	}
	return pairs
}

func exitStatusEnumType() chsql.Frag { return chsql.TypeEnum8(exitStatusEnumMembers()...) }

func routeEnumType() chsql.Frag { return chsql.TypeEnum8(routeEnumMembers()...) }

// reconciledEnumColumn is a corpus Enum8 column whose member set this binary can
// outgrow, so the sink widens it and then verifies the deployed type against the
// server at construction. Adding a member to either column is a schema change on
// every already-deployed corpus table, and this list is what makes that change
// land there rather than only on fresh deployments.
type reconciledEnumColumn struct {
	name    string
	members func() []chsql.EnumPair
}

func (c reconciledEnumColumn) enumType() chsql.Frag { return chsql.TypeEnum8(c.members()...) }

var reconciledEnumColumns = []reconciledEnumColumn{
	{name: exitStatusColumn, members: exitStatusEnumMembers},
	{name: routeColumn, members: routeEnumMembers},
}

// NewCHTableSink builds a CH-table sink over conn and reconciles the corpus
// table with the schema this binary writes, in three steps:
//
//	CREATE TABLE IF NOT EXISTS — makes the table on a fresh deployment.
//	ALTER TABLE MODIFY COLUMN  — widens each reconciledEnumColumns entry on a
//	                             table that predates a member this binary can
//	                             emit. CREATE IF NOT EXISTS alone cannot do
//	                             this: it is a no-op against an existing table
//	                             however its columns are declared.
//	verify                     — reads each deployed type back and fails
//	                             construction if a member is missing.
//
// Only the CREATE and the verify can fail construction. The widening is
// BEST-EFFORT by design: a deployment whose CH user holds INSERT and CREATE but
// not ALTER, or whose corpus table is operator-owned and needs ON CLUSTER, is a
// legitimate configuration, and on such a deployment the widening is a no-op in
// every case that matters — the column is either already wide enough (nothing
// to do) or it is not, which the verify catches on the server's own answer
// rather than on whether the ALTER was permitted. That keeps the ALTER from
// turning a working sink into a disabled one while leaving the guarantee
// intact: the sink is never built over a column that cannot hold what this
// binary writes.
//
// Verifying against the server — rather than trusting the ALTER did what it
// was asked — is what makes the reconciliation honest rather than hopeful: a
// column narrower than the member set this binary writes would otherwise
// surface much later as a batch the column rejects, on every reconcile
// interval, forever. A construction failure disables the reconciler (see
// buildCorpusSink); the data plane is untouched either way.
func NewCHTableSink(ctx context.Context, conn CHTableConn) (*CHTableSink, error) {
	if conn == nil {
		return nil, fmt.Errorf("optcorpus: nil CH connection for table sink")
	}
	if err := conn.Exec(ctx, corpusCreateTableSQL()); err != nil {
		return nil, fmt.Errorf("optcorpus: create %s: %w", CorpusTableName, err)
	}
	for _, col := range reconciledEnumColumns {
		widenErr := conn.Exec(ctx, corpusAlterEnumColumnSQL(col))
		if err := verifyEnumColumn(ctx, conn, col, widenErr); err != nil {
			return nil, err
		}
	}
	return &CHTableSink{conn: conn, table: CorpusTableName}, nil
}

// corpusAlterEnumColumnSQL renders the statement that retypes the deployed
// column to the member set this binary writes. Widening an Enum8 is
// metadata-only on ClickHouse — no part is rewritten, no mutation is scheduled —
// so it is safe to issue on every start, and IF EXISTS makes it a no-op on a
// table the CREATE above just made with the wide type.
func corpusAlterEnumColumnSQL(col reconciledEnumColumn) string {
	return chsql.AlterTableModifyColumn("", CorpusTableName, col.name, col.enumType()).SQL()
}

// verifyEnumColumn reads the DEPLOYED column type back from system.columns and
// fails if it cannot hold every member this binary emits, naming the missing
// ones. Reading the server's own answer — rather than trusting that the ALTER did
// what it was asked — is the only check that distinguishes a widened column from
// one the server left alone.
//
// widenErr is whatever the best-effort widening returned. It is carried here
// rather than acted on at the callsite because it only becomes evidence when
// the column turns out to be too narrow: then, and only then, the failed ALTER
// is the reason and belongs in the message an operator reads.
func verifyEnumColumn(ctx context.Context, conn CHConn, col reconciledEnumColumn, widenErr error) error {
	sql, args := corpusColumnTypeQuery(col.name)
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("optcorpus: read deployed %s type: %w", col.name, err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("optcorpus: read deployed %s type: %w", col.name, err)
		}
		return fmt.Errorf("optcorpus: %s.%s absent after reconciliation", CorpusTableName, col.name)
	}
	var deployed string
	if err := rows.Scan(&deployed); err != nil {
		return fmt.Errorf("optcorpus: scan deployed %s type: %w", col.name, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("optcorpus: read deployed %s type: %w", col.name, err)
	}

	have := enum8Members(deployed)
	var problems []string
	for _, m := range col.members() {
		got, ok := have[m.Name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%q absent", m.Name))
		// The deployed side is parsed out of a server-supplied type string, so it
		// is read as int64 and compared against the widened member value: a
		// deployed value outside the Enum8 domain must read as a mismatch, not
		// wrap into one that agrees.
		case got != int64(m.Value):
			problems = append(problems, fmt.Sprintf("%q deployed as %d, this binary writes %d", m.Name, got, m.Value))
		}
	}
	if len(problems) > 0 {
		if widenErr != nil {
			return fmt.Errorf("optcorpus: deployed %s.%s type %q cannot hold what this binary writes (%s); widening it failed: %w",
				CorpusTableName, col.name, deployed, strings.Join(problems, "; "), widenErr)
		}
		return fmt.Errorf("optcorpus: deployed %s.%s type %q cannot hold what this binary writes (%s)",
			CorpusTableName, col.name, deployed, strings.Join(problems, "; "))
	}
	return nil
}

// corpusColumnTypeQuery selects the deployed type of one corpus column from
// system.columns for the corpus table in the connection's own database — the
// same unqualified table the DDL above creates.
func corpusColumnTypeQuery(column string) (string, []any) {
	return chsql.NewQuery().
		From(chsql.Qual("system", "columns")).
		Select(chsql.BareIdent("type")).
		Where(
			chsql.Eq(chsql.BareIdent("database"), chsql.Call("currentDatabase")),
			chsql.Eq(chsql.BareIdent("table"), chsql.Lit(CorpusTableName)),
			chsql.Eq(chsql.BareIdent("name"), chsql.Lit(column)),
		).
		Build()
}

// enum8Members extracts the name→value pairs from a rendered ClickHouse Enum8
// type such as `Enum8('ok' = 0, 'oom' = 1)`. Names are the single-quoted runs,
// with ClickHouse's backslash escaping honoured so a name containing a quote is
// not read as two members.
//
// The VALUE is part of the contract, not decoration. Write appends this column
// as an int8 (exitEnumValue), and clickhouse-go's Enum8.AppendRow keys the int
// path on the INTEGER: it checks the value against the deployed type's defined
// set and stores it verbatim, resolving no name. A deployed column carrying
// every expected name at different integers therefore fails in whichever of two
// ways is worse for the operator, and a name-only check calls it healthy in
// both:
//
//   - the integer this binary writes is defined in the deployed type but under
//     a DIFFERENT name — the batch is accepted and every such row is stored
//     under the wrong label, silently, forever; or
//   - the integer is not defined there at all — every batch is rejected with
//     "unknown element N", exactly as for an absent member.
//
// A member whose value cannot be read is omitted rather than guessed; verify's
// job is to prove the deployed column matches, and an unreadable value is not
// proof.
func enum8Members(chType string) map[string]int64 {
	members := map[string]int64{}
	var cur strings.Builder
	inName, escaped := false, false
	rs := []rune(chType)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case inName && r == '\\':
			escaped = true
		case r == '\'':
			if inName {
				if v, n, ok := parseEnum8Value(rs[i+1:]); ok {
					members[cur.String()] = v
					i += n
				}
				cur.Reset()
			}
			inName = !inName
		case inName:
			cur.WriteRune(r)
		}
	}
	return members
}

// parseEnum8Value reads the `= <int>` that follows a member name in a rendered
// Enum8 type, returning the value and how many runes it consumed past the
// closing quote. Reports false when the assignment is absent or unparseable.
func parseEnum8Value(rs []rune) (int64, int, bool) {
	i := 0
	skipSpace := func() {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
			i++
		}
	}
	skipSpace()
	if i >= len(rs) || rs[i] != '=' {
		return 0, 0, false
	}
	i++
	skipSpace()
	start := i
	if i < len(rs) && (rs[i] == '-' || rs[i] == '+') {
		i++
	}
	digits := i
	for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
		i++
	}
	if i == digits {
		return 0, 0, false
	}
	v, err := strconv.ParseInt(string(rs[start:i]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return v, i, true
}

// corpusCreateTableSQL renders the corpus MergeTree DDL via the typed chsql
// builder. The schema mirrors Row column-for-column:
//
//	cerberus_router_corpus (
//	  event_time DateTime, shape_id LowCardinality(String),
//	  language LowCardinality(String), normalized_query_hash UInt64,
//	  n_anchors UInt32, fanout UInt32, cumulative_d UInt32,
//	  outer_range UInt32, step UInt32, route <routeEnumType()>,
//	  k_shards UInt8, decision_reason LowCardinality(String),
//	  read_rows UInt64, read_bytes UInt64, query_duration_ms UInt64,
//	  memory_usage UInt64, exit_status <exitStatusEnumType()>
//	) ENGINE = MergeTree ORDER BY (shape_id, n_anchors, fanout)
//	  TTL toDateTime(event_time) + toIntervalDay(30)
func corpusCreateTableSQL() string {
	return chsql.CreateTable(CorpusTableName).
		IfNotExists().
		Columns(CorpusColumns()...).
		Engine(chsql.EngineMergeTree()).
		OrderBy("shape_id", "n_anchors", "fanout").
		TTL(chsql.TableTTL("event_time", corpusRetention)).
		SQL()
}

// CorpusColumns returns the corpus table's column list in WIRE ORDER — the single
// source of truth behind the CREATE TABLE DDL, the columnar INSERT batch's Append
// order (see Write), and every test substrate that stands the table up itself.
//
// The list is deliberately NOT derivable from Row: Row is the JSONL wire form and
// the two shapes differ on purpose. Row carries opts + profile_events, which the
// table does not store, and the table carries event_time, which the sink stamps
// per batch rather than reading off a Row.
//
// It is exported for the same reason RouteUnclassified is: the corpus is read by
// internal/routerrules and stood up on a chDB substrate by that package's parity
// lane, neither of which may import this package from production code. Both used
// to restate the shape — and the route column's Enum8 member list had already
// drifted between the restatements, which is precisely the bug that made an
// unclassified row read back as route 'A'. One list, three consumers, no copies.
func CorpusColumns() []chsql.ColumnDef {
	lcString := chsql.TypeLowCardinality(chsql.TypeRaw("String"))
	uint32Type := chsql.TypeRaw("UInt32")
	uint64Type := chsql.TypeRaw("UInt64")
	return []chsql.ColumnDef{
		{Name: "event_time", Type: chsql.TypeRaw("DateTime")},
		{Name: "shape_id", Type: lcString},
		{Name: "language", Type: lcString},
		{Name: "normalized_query_hash", Type: uint64Type},
		{Name: "n_anchors", Type: uint32Type},
		{Name: "fanout", Type: uint32Type},
		{Name: "cumulative_d", Type: uint32Type},
		{Name: "outer_range", Type: uint32Type},
		{Name: "step", Type: uint32Type},
		{Name: routeColumn, Type: routeEnumType()},
		{Name: "k_shards", Type: chsql.TypeRaw("UInt8")},
		{Name: "decision_reason", Type: lcString},
		{Name: "read_rows", Type: uint64Type},
		{Name: "read_bytes", Type: uint64Type},
		{Name: "query_duration_ms", Type: uint64Type},
		{Name: "memory_usage", Type: uint64Type},
		{Name: exitStatusColumn, Type: exitStatusEnumType()},
	}
}

// routeEnumValue maps the Row.Route token to the Enum8 value the column stores,
// over the same routeEnumMembers list the column type is rendered from — so a
// token and its stored value cannot disagree. A token outside the member set is
// stored as RouteUnclassified: an unrecognised classifier read-out is not
// evidence that the query took route A.
func routeEnumValue(route string) int8 {
	for _, m := range routeEnumMembers() {
		if m.Name == route {
			return m.Value
		}
	}
	return routeUnclassifiedValue
}

// exitEnumValue maps the Row.ExitStatus token to the Enum8 value the column
// stores, over the same exitStatuses list the column type is rendered from —
// so a token and its stored value cannot disagree. An empty / unrecognised
// token defaults to 'ok' (0), matching the ExitStatus zero value.
func exitEnumValue(status string) int8 {
	for _, s := range exitStatuses {
		if s.String() == status {
			return int8(s)
		}
	}
	return int8(ExitOK)
}

// Write appends each Row to the corpus table via a columnar batch. event_time
// is stamped at write time (the reconcile instant) — the corpus keys retention
// and recency on it. An empty slice is a no-op. The column order MUST match
// corpusCreateTableSQL / corpusInsertStmt.
func (s *CHTableSink) Write(rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	ctx := context.Background()
	batch, err := s.conn.PrepareBatch(ctx, corpusInsertStmt)
	if err != nil {
		return fmt.Errorf("optcorpus: prepare batch: %w", err)
	}
	now := time.Now()
	for i := range rows {
		r := rows[i]
		if err := batch.Append(
			now,
			r.ShapeID,
			r.Language,
			r.NormalizedQueryHash,
			r.NAnchors,
			r.Fanout,
			r.CumulativeD,
			r.OuterRange,
			r.Step,
			routeEnumValue(r.Route),
			r.KShards,
			r.DecisionReason,
			r.ReadRows,
			r.ReadBytes,
			r.QueryDurationMS,
			r.MemoryUsage,
			exitEnumValue(r.ExitStatus),
		); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("optcorpus: append corpus row: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("optcorpus: send corpus batch: %w", err)
	}
	return nil
}

// Close is a no-op: the sink does not own the shared driver.Conn (the chclient
// pool owns its lifecycle), and the columnar batch is finalized per Write.
func (s *CHTableSink) Close() error { return nil }
