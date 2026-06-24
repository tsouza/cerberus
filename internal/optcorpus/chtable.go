package optcorpus

import (
	"context"
	"fmt"
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

// NewCHTableSink builds a CH-table sink over conn and ensures the corpus table
// exists (CREATE TABLE IF NOT EXISTS). The DDL is rendered from the typed chsql
// builder. A DDL failure is returned so the caller can fall back to the JSONL
// sink rather than silently dropping the corpus.
func NewCHTableSink(ctx context.Context, conn CHExecer) (*CHTableSink, error) {
	if conn == nil {
		return nil, fmt.Errorf("optcorpus: nil CH connection for table sink")
	}
	if err := conn.Exec(ctx, corpusCreateTableSQL()); err != nil {
		return nil, fmt.Errorf("optcorpus: create %s: %w", CorpusTableName, err)
	}
	return &CHTableSink{conn: conn, table: CorpusTableName}, nil
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
//	  memory_usage UInt64, exit_status Enum8('ok'=0,'oom'=1,'timeout'=2)
//	) ENGINE = MergeTree ORDER BY (shape_id, n_anchors, fanout)
//	  TTL toDateTime(event_time) + toIntervalDay(30)
func corpusCreateTableSQL() string {
	lcString := chsql.TypeLowCardinality(chsql.TypeRaw("String"))
	routeEnum := chsql.TypeEnum8(
		chsql.EnumPair{Name: "A", Value: 0},
		chsql.EnumPair{Name: "B", Value: 1},
	)
	exitEnum := chsql.TypeEnum8(
		chsql.EnumPair{Name: "ok", Value: 0},
		chsql.EnumPair{Name: "oom", Value: 1},
		chsql.EnumPair{Name: "timeout", Value: 2},
		// Cerberus-side terminal outcomes query_log cannot reflect: the
		// sample-budget 422 (after a clean CH finish), and the pre-dispatch
		// breaker 503 / cap 400 rejections (no CH query at all). The Enum8
		// values MUST stay in lockstep with the ExitStatus iota + its String()
		// tokens (optcorpus.go) and exitEnumValue below.
		chsql.EnumPair{Name: "sample_budget", Value: 3},
		chsql.EnumPair{Name: "breaker", Value: 4},
		chsql.EnumPair{Name: "rejected", Value: 5},
	)
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
			chsql.ColumnDef{Name: "exit_status", Type: exitEnum},
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

// exitEnumValue maps the Row.ExitStatus string to the Enum8 value. The values
// MUST match the corpusCreateTableSQL Enum8 DDL and the ExitStatus iota.
func exitEnumValue(status string) int8 {
	switch status {
	case "oom":
		return 1
	case "timeout":
		return 2
	case "sample_budget":
		return 3
	case "breaker":
		return 4
	case "rejected":
		return 5
	default:
		return 0
	}
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
