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

// exitStatusColumn is the corpus column holding the terminal outcome. It is
// the one column whose type the running binary can outgrow, so it is named
// once and shared by the DDL, the reconciling ALTER, and the verify read.
const exitStatusColumn = "exit_status"

// exitStatusEnumType renders the exit_status Enum8 from the exitStatuses list,
// so the column type, the string→value mapping (exitEnumValue), and the
// ExitStatus iota are one source of truth rather than three that must agree.
func exitStatusEnumType() chsql.Frag {
	pairs := make([]chsql.EnumPair, 0, len(exitStatuses))
	for _, s := range exitStatuses {
		pairs = append(pairs, chsql.EnumPair{Name: s.String(), Value: int64(s)})
	}
	return chsql.TypeEnum8(pairs...)
}

// NewCHTableSink builds a CH-table sink over conn and reconciles the corpus
// table with the schema this binary writes, in three steps:
//
//	CREATE TABLE IF NOT EXISTS — makes the table on a fresh deployment.
//	ALTER TABLE MODIFY COLUMN  — widens exit_status on a table that predates an
//	                             exit-status member this binary can emit.
//	                             CREATE IF NOT EXISTS alone cannot do this: it
//	                             is a no-op against an existing table however
//	                             its columns are declared.
//	verify                     — reads the deployed exit_status type back and
//	                             fails construction if a member is missing.
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
	widenErr := conn.Exec(ctx, corpusAlterExitStatusSQL())
	if err := verifyExitStatusColumn(ctx, conn, widenErr); err != nil {
		return nil, err
	}
	return &CHTableSink{conn: conn, table: CorpusTableName}, nil
}

// corpusAlterExitStatusSQL renders the statement that retypes the deployed
// exit_status column to the member set this binary writes. Widening an Enum8
// is metadata-only on ClickHouse — no part is rewritten, no mutation is
// scheduled — so it is safe to issue on every start, and IF EXISTS makes it a
// no-op on a table the CREATE above just made with the wide type.
func corpusAlterExitStatusSQL() string {
	return chsql.AlterTableModifyColumn("", CorpusTableName, exitStatusColumn, exitStatusEnumType()).SQL()
}

// verifyExitStatusColumn reads the DEPLOYED exit_status column type back from
// system.columns and fails if it cannot hold every member this binary emits,
// naming the missing ones. Reading the server's own answer — rather than
// trusting that the ALTER did what it was asked — is the only check that
// distinguishes a widened column from one the server left alone.
//
// widenErr is whatever the best-effort widening returned. It is carried here
// rather than acted on at the callsite because it only becomes evidence when
// the column turns out to be too narrow: then, and only then, the failed ALTER
// is the reason and belongs in the message an operator reads.
func verifyExitStatusColumn(ctx context.Context, conn CHConn, widenErr error) error {
	sql, args := corpusExitStatusTypeQuery()
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("optcorpus: read deployed %s type: %w", exitStatusColumn, err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("optcorpus: read deployed %s type: %w", exitStatusColumn, err)
		}
		return fmt.Errorf("optcorpus: %s.%s absent after reconciliation", CorpusTableName, exitStatusColumn)
	}
	var deployed string
	if err := rows.Scan(&deployed); err != nil {
		return fmt.Errorf("optcorpus: scan deployed %s type: %w", exitStatusColumn, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("optcorpus: read deployed %s type: %w", exitStatusColumn, err)
	}

	have := enum8Members(deployed)
	var problems []string
	for _, s := range exitStatuses {
		got, ok := have[s.String()]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s absent", s.String()))
		case got != int64(s):
			problems = append(problems, fmt.Sprintf("%s deployed as %d, this binary writes %d", s.String(), got, int64(s)))
		}
	}
	if len(problems) > 0 {
		if widenErr != nil {
			return fmt.Errorf("optcorpus: deployed %s.%s type %q cannot hold what this binary writes (%s); widening it failed: %w",
				CorpusTableName, exitStatusColumn, deployed, strings.Join(problems, "; "), widenErr)
		}
		return fmt.Errorf("optcorpus: deployed %s.%s type %q cannot hold what this binary writes (%s)",
			CorpusTableName, exitStatusColumn, deployed, strings.Join(problems, "; "))
	}
	return nil
}

// corpusExitStatusTypeQuery selects the deployed exit_status column type from
// system.columns for the corpus table in the connection's own database — the
// same unqualified table the DDL above creates.
func corpusExitStatusTypeQuery() (string, []any) {
	return chsql.NewQuery().
		From(chsql.Qual("system", "columns")).
		Select(chsql.BareIdent("type")).
		Where(
			chsql.Eq(chsql.BareIdent("database"), chsql.Call("currentDatabase")),
			chsql.Eq(chsql.BareIdent("table"), chsql.Lit(CorpusTableName)),
			chsql.Eq(chsql.BareIdent("name"), chsql.Lit(exitStatusColumn)),
		).
		Build()
}

// enum8Members extracts the name→value pairs from a rendered ClickHouse Enum8
// type such as `Enum8('ok' = 0, 'oom' = 1)`. Names are the single-quoted runs,
// with ClickHouse's backslash escaping honoured so a name containing a quote is
// not read as two members.
//
// The VALUE is part of the contract, not decoration: clickhouse-go appends an
// Enum8 column by integer, so a column that carries every expected name at
// different integers rejects every batch just as surely as one missing a name —
// and a name-only check would report that column healthy. A member whose value
// cannot be read is omitted rather than guessed; verify's job is to prove the
// deployed column matches, and an unreadable value is not proof.
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
//	  outer_range UInt32, step UInt32, route Enum8('A'=0,'B'=1),
//	  k_shards UInt8, decision_reason LowCardinality(String),
//	  read_rows UInt64, read_bytes UInt64, query_duration_ms UInt64,
//	  memory_usage UInt64, exit_status <exitStatusEnumType()>
//	) ENGINE = MergeTree ORDER BY (shape_id, n_anchors, fanout)
//	  TTL toDateTime(event_time) + toIntervalDay(30)
func corpusCreateTableSQL() string {
	lcString := chsql.TypeLowCardinality(chsql.TypeRaw("String"))
	routeEnum := chsql.TypeEnum8(
		chsql.EnumPair{Name: "A", Value: 0},
		chsql.EnumPair{Name: "B", Value: 1},
	)
	exitEnum := exitStatusEnumType()
	return chsql.CreateTable(CorpusTableName).
		IfNotExists().
		Columns(
			chsql.ColumnDef{Name: "event_time", Type: chsql.TypeRaw("DateTime")},
			chsql.ColumnDef{Name: "shape_id", Type: chsql.TypeLowCardinality(chsql.TypeRaw("String"))},
			chsql.ColumnDef{Name: "language", Type: lcString},
			chsql.ColumnDef{Name: "normalized_query_hash", Type: chsql.TypeRaw("UInt64")},
			chsql.ColumnDef{Name: "n_anchors", Type: chsql.TypeRaw("UInt32")},
			chsql.ColumnDef{Name: "fanout", Type: chsql.TypeRaw("UInt32")},
			chsql.ColumnDef{Name: "cumulative_d", Type: chsql.TypeRaw("UInt32")},
			chsql.ColumnDef{Name: "outer_range", Type: chsql.TypeRaw("UInt32")},
			chsql.ColumnDef{Name: "step", Type: chsql.TypeRaw("UInt32")},
			chsql.ColumnDef{Name: "route", Type: routeEnum},
			chsql.ColumnDef{Name: "k_shards", Type: chsql.TypeRaw("UInt8")},
			chsql.ColumnDef{Name: "decision_reason", Type: chsql.TypeLowCardinality(chsql.TypeRaw("String"))},
			chsql.ColumnDef{Name: "read_rows", Type: chsql.TypeRaw("UInt64")},
			chsql.ColumnDef{Name: "read_bytes", Type: chsql.TypeRaw("UInt64")},
			chsql.ColumnDef{Name: "query_duration_ms", Type: chsql.TypeRaw("UInt64")},
			chsql.ColumnDef{Name: "memory_usage", Type: chsql.TypeRaw("UInt64")},
			chsql.ColumnDef{Name: exitStatusColumn, Type: exitEnum},
		).
		Engine(chsql.EngineMergeTree()).
		OrderBy("shape_id", "n_anchors", "fanout").
		TTL(chsql.TableTTL("event_time", corpusRetention)).
		SQL()
}

// routeEnumValue maps the Row.Route string to the Enum8 value the column
// stores. An empty / unknown route defaults to 'A' (0) — a row with no routing
// classification is, by construction, a route-A query.
func routeEnumValue(route string) int8 {
	if route == "B" {
		return 1
	}
	return 0
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
