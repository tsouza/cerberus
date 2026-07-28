package optcorpus

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
)

// CHConn is the narrow ClickHouse read surface the production QueryLogSource
// needs: a single typed Query against system.query_log. clickhouse-go/v2's
// driver.Conn satisfies it (via *chclient.Client.Conn()), and a fake satisfies
// it in tests without standing up a server. Keeping the interface narrow means
// optcorpus does not import chclient (avoiding an import cycle) and stays
// trivially testable.
type CHConn interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// Conservative resource caps stamped on EVERY system.query_log corpus SELECT
// so the background reconciler can never starve the data plane, no matter how
// large system.query_log grows on a busy cluster. These honour the
// "rate-limited" contract: the scan is single-threaded, deprioritised behind
// data-plane queries, hard-capped in wall-clock and in rows/bytes read, and
// flagged read-only so it can never mutate state.
const (
	// corpusMaxExecutionTime aborts a pathological scan rather than letting it
	// pin the reconciler goroutine until shutdown. Seconds (ClickHouse Float64).
	corpusMaxExecutionTime = 10.0
	// corpusMaxThreads keeps the scan single-threaded so it competes minimally
	// for CH CPU.
	corpusMaxThreads = 1
	// corpusPriority deprioritises the scan behind data-plane queries (higher
	// value == lower priority in ClickHouse's priority scheduler).
	corpusPriority = 10
	// corpusMaxRowsToRead / corpusMaxBytesToRead bound the scan volume; 'break'
	// overflow mode returns what was read so far instead of throwing, so a
	// large query_log degrades the corpus gracefully rather than erroring.
	corpusMaxRowsToRead  = 50_000_000
	corpusMaxBytesToRead = 5 << 30 // 5 GiB
)

// CHQueryLogSource is the production QueryLogSource: it reads finished rows
// from system.query_log for a batch of query_ids over a CHConn. It is wired
// only when CERBERUS_CH_OPT_CORPUS_ENABLED is set, and never under chDB (which
// has no system.query_log).
type CHQueryLogSource struct {
	conn    CHConn
	timeout time.Duration
	window  time.Duration
}

// defaultQueryLogWindow is the fallback event_time lookback for the corpus
// SELECT when a non-positive window is supplied. It matches the historical
// hardcoded `INTERVAL 1 HOUR` and is also the floor QueryLogWindow enforces.
const defaultQueryLogWindow = time.Hour

// defaultQueryLogSourceTimeout is the fallback wall-clock bound on one corpus
// SELECT when a non-positive timeout is supplied, so the reconciler goroutine
// can never block indefinitely on a single scan.
const defaultQueryLogSourceTimeout = 15 * time.Second

// NewCHQueryLogSource builds a CHQueryLogSource over conn (typically
// chclient.Client.Conn()). timeout bounds each corpus SELECT in wall-clock via
// a derived context (in addition to the server-side max_execution_time cap); a
// non-positive timeout falls back to a conservative default so the reconciler
// goroutine can never block indefinitely on a single scan. window is the
// event_time lookback the SELECT bounds itself to (derived from the reconcile
// interval via QueryLogWindow so it always comfortably covers two intervals); a
// non-positive window falls back to the 1h default.
func NewCHQueryLogSource(conn CHConn, timeout, window time.Duration) *CHQueryLogSource {
	if timeout <= 0 {
		timeout = defaultQueryLogSourceTimeout
	}
	if window <= 0 {
		window = defaultQueryLogWindow
	}
	return &CHQueryLogSource{conn: conn, timeout: timeout, window: window}
}

// QueryLogWindow derives the system.query_log event_time lookback from the
// reconcile interval. The window must comfortably cover more than one interval
// so a query observed just after one scan is still inside the lookback on the
// next scan (which runs ~one interval later); it is max(2*interval, 1h). The 1h
// floor keeps the window sane for very short intervals.
func QueryLogWindow(interval time.Duration) time.Duration {
	w := 2 * interval
	if w < defaultQueryLogWindow {
		return defaultQueryLogWindow
	}
	return w
}

// corpusSettings is the conservative ClickHouse settings map stamped on every
// corpus SELECT. It mirrors the data-plane query-settings discipline but biases
// hard toward never disturbing the data plane.
func corpusSettings() clickhouse.Settings {
	return clickhouse.Settings{
		"max_execution_time":    corpusMaxExecutionTime,
		"timeout_overflow_mode": "throw",
		"max_threads":           corpusMaxThreads,
		"priority":              corpusPriority,
		"max_rows_to_read":      corpusMaxRowsToRead,
		"max_bytes_to_read":     corpusMaxBytesToRead,
		"read_overflow_mode":    "break",
	}
}

// ClickHouse server error codes the corpus maps to a non-OK exit_status,
// sourced from ClickHouse's ErrorCodes.cpp and named here so the classifier
// carries its own explanation.
//
// They fall in three families: the terminal-cost outcomes route B (time-slice
// sharding) exists to avoid (memory limit, deadline), and the client-side
// abandonment codes a query picks up when the caller walks away mid-flight.
// The abandonment family matters because those queries are NOT part of the
// healthy cost population the calibration watermarks are learnt from: the cost
// they report is the cost accrued up to the abort, not the cost of the query.
const (
	// chErrAttemptToReadAfterEOF is ATTEMPT_TO_READ_AFTER_EOF — the client
	// socket ended mid-packet.
	chErrAttemptToReadAfterEOF = 32
	// chErrUnknownPacketFromClient is UNKNOWN_PACKET_FROM_CLIENT — the client
	// connection was destroyed rather than closed in protocol.
	chErrUnknownPacketFromClient = 99
	// chErrTimeoutExceeded is TIMEOUT_EXCEEDED.
	chErrTimeoutExceeded = 159
	// chErrTooSlow is TOO_SLOW — max_execution_time / result-row caps tripped;
	// the corpus folds it into the timeout class (a deadline-style abort).
	chErrTooSlow = 160
	// chErrNetworkError is NETWORK_ERROR — a broken pipe to the client.
	chErrNetworkError = 210
	// chErrMemoryLimitExceeded is MEMORY_LIMIT_EXCEEDED — the OOM signal.
	chErrMemoryLimitExceeded = 241
	// chErrQueryWasCancelled is QUERY_WAS_CANCELLED — the query was killed,
	// the clean counterpart of a destroyed socket.
	chErrQueryWasCancelled = 394
	// chErrQueryWasCancelledByClient is QUERY_WAS_CANCELLED_BY_CLIENT — the
	// client sent a Cancel packet over the live connection.
	chErrQueryWasCancelledByClient = 735
)

// The system.query_log.type column and the Enum8 members the corpus reads.
// The names are ClickHouse's, spelled exactly; queryLogTypeMember resolves
// them against the column's own declared type so a wrong spelling is a server
// error rather than a predicate that matches nothing.
const (
	queryLogTypeColumn      = "type"
	queryLogFinishMember    = "QueryFinish"
	queryLogExceptionMember = "ExceptionWhileProcessing"
)

// queryLogTypeMember renders one system.query_log.type Enum8 member, resolved
// against the column's OWN declared type.
//
// This is the difference between a predicate that fails loudly and one that
// lies. Comparing the Enum8 column against a bare string literal that is not a
// member coerces the comparison to String, matches zero rows, and reports no
// error — a whole terminal class silently invisible. CAST against
// toTypeName(type) resolves the name in the column's enum, so an unknown
// member is ClickHouse error 691 (UNKNOWN_ELEMENT_OF_ENUM) naming the members
// it did find.
func queryLogTypeMember(name string) chsql.Frag {
	return chsql.Call(
		"CAST",
		chsql.InlineLit(name),
		chsql.Call("toTypeName", chsql.BareIdent(queryLogTypeColumn)),
	)
}

// terminalFailedFrag reduces a query_id's grouped terminal rows to whether the
// query FAILED. A distributed query_log carries one row per participating node
// for the same query_id, so any single row being an exception makes the query
// a failure.
//
// Comparing each constituent row against QueryFinish and taking max() makes an
// exception row dominate regardless of how the enum orders its members —
// reducing the type itself (by name or by value) instead would let the member
// ordering decide, and reclassify a failed query whose remote shard finished
// as a clean exit. toUInt8 pins the aggregate's width, because clickhouse-go's
// strict Scan refuses a widened aggregate type into the Go destination.
func terminalFailedFrag() chsql.Frag {
	return chsql.Call(
		"toUInt8",
		chsql.Call(
			"max",
			chsql.Neq(chsql.BareIdent(queryLogTypeColumn),
				queryLogTypeMember(queryLogFinishMember)),
		),
	)
}

// queryLogQuery builds the TERMINAL-row SELECT for a set of query_ids — clean
// finishes and exception exits — reading the cost columns, the
// normalized_query_hash, the two ProfileEvents of interest
// (QueryConditionCacheHits, RowsReadByPrewhereReaders) projected out of the
// ProfileEvents map by name, plus the terminal-failed flag and exception_code
// the reconciler derives exit_status from. The exception code is taken with
// max() so a non-zero code on any constituent row wins over the 0 a clean
// finish carries.
//
// ExceptionBeforeStart is deliberately outside the predicate. Those rows carry
// no cost — the query never started, so read_rows / memory_usage are 0 by
// construction — and the corpus exists to hold a COST distribution, so
// ingesting them would deflate every watermark learnt from it. The
// cerberus-side pre-dispatch rejections that matter (breaker 503, cap 400) are
// recorded at the engine seam instead.
//
// The query is bounded to a recent window so a large query_log is not scanned
// in full each interval, and grouped to one row per query_id. The window is
// bound at call time (derived from the reconcile interval via QueryLogWindow)
// so a longer interval still covers more than one scan worth of dispatches;
// args emit window-first, then the id list.
func queryLogQuery(windowSeconds int64, ids []string) (string, []any) {
	profileEvent := func(name string) chsql.Frag {
		return chsql.Call("sum",
			chsql.Subscript(chsql.BareIdent("ProfileEvents"), chsql.InlineLit(name)))
	}
	maxCol := func(name string) chsql.Frag {
		return chsql.Call("max", chsql.BareIdent(name))
	}
	return chsql.NewQuery().
		From(chsql.Qual("system", "query_log")).
		Select(chsql.BareIdent("query_id")).
		SelectAs(chsql.Call("any", chsql.BareIdent("normalized_query_hash")), "normalized_query_hash").
		SelectAs(maxCol("read_rows"), "read_rows").
		SelectAs(maxCol("read_bytes"), "read_bytes").
		SelectAs(maxCol("query_duration_ms"), "query_duration_ms").
		SelectAs(maxCol("memory_usage"), "memory_usage").
		SelectAs(profileEvent("QueryConditionCacheHits"), "condition_cache_hits").
		SelectAs(profileEvent("RowsReadByPrewhereReaders"), "prewhere_rows").
		SelectAs(terminalFailedFrag(), "terminal_failed").
		SelectAs(maxCol("exception_code"), "exception_code").
		Where(
			chsql.In(chsql.BareIdent(queryLogTypeColumn),
				queryLogTypeMember(queryLogFinishMember),
				queryLogTypeMember(queryLogExceptionMember)),
			chsql.Gt(chsql.BareIdent("event_time"),
				chsql.Sub(chsql.Call("now"),
					chsql.Call("toIntervalSecond", chsql.Lit(windowSeconds)))),
			chsql.In(chsql.BareIdent("query_id"), chsql.Lit(ids)),
		).
		GroupBy(chsql.BareIdent("query_id")).
		Build()
}

// FinishedByQueryID runs the bounded system.query_log SELECT for ids and
// decodes each row into a SourceRow. The two named ProfileEvents are folded
// back into the ProfileEvents map so the corpus Row carries them under their
// canonical names. ids is never empty when called by the reconciler.
func (s *CHQueryLogSource) FinishedByQueryID(ctx context.Context, ids []string) ([]SourceRow, error) {
	// Hard wall-clock bound on top of the server-side max_execution_time, so a
	// stuck scan can never pin the reconciler goroutine until process shutdown.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	// Stamp the conservative resource caps onto this query only, so the
	// background corpus scan cannot starve the data plane on a huge query_log.
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(corpusSettings()))

	windowSeconds := int64(s.window / time.Second)
	sql, args := queryLogQuery(windowSeconds, ids)
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("optcorpus: query system.query_log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SourceRow
	for rows.Next() {
		var (
			queryID            string
			normalizedHash     uint64
			readRows           uint64
			readBytes          uint64
			queryDurationMS    uint64
			memoryUsage        uint64
			conditionCacheHits uint64
			prewhereRows       uint64
			terminalFailed     uint8
			exceptionCode      int32
		)
		if err := rows.Scan(
			&queryID,
			&normalizedHash,
			&readRows,
			&readBytes,
			&queryDurationMS,
			&memoryUsage,
			&conditionCacheHits,
			&prewhereRows,
			&terminalFailed,
			&exceptionCode,
		); err != nil {
			return nil, fmt.Errorf("optcorpus: scan query_log row: %w", err)
		}
		out = append(out, SourceRow{
			QueryID:             queryID,
			NormalizedQueryHash: normalizedHash,
			ReadRows:            readRows,
			ReadBytes:           readBytes,
			QueryDurationMS:     queryDurationMS,
			MemoryUsage:         memoryUsage,
			ProfileEvents: map[string]int64{
				"QueryConditionCacheHits":   int64(conditionCacheHits),
				"RowsReadByPrewhereReaders": int64(prewhereRows),
			},
			ExitStatus: exitStatusFor(terminalFailed != 0, exceptionCode),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("optcorpus: query_log rows: %w", err)
	}
	return out, nil
}

// exitStatusFor maps a terminal query_log row to the corpus ExitStatus: a
// clean finish is ExitOK regardless of the code it carries; a failure is
// classified by its ClickHouse error code (memory limit → oom, deadline →
// timeout, client abandonment → aborted).
//
// The classifier takes the terminal class as a BOOLEAN, never as a type
// string. Handing it a spelling would let a test and the code agree on a name
// neither the server nor the enum recognises — the exact shape of a predicate
// that silently matches nothing. The only source of the boolean is
// terminalFailedFrag, evaluated by ClickHouse against the real column.
//
// An unmapped code is ExitError, never ExitOK: the row failed by definition,
// and ExitError states exactly what is known without inventing a cause or
// laundering a failure into the healthy cost population the calibration
// watermarks are learnt from.
func exitStatusFor(failed bool, exceptionCode int32) ExitStatus {
	if !failed {
		return ExitOK
	}
	switch exceptionCode {
	case chErrMemoryLimitExceeded:
		return ExitOOM
	case chErrTimeoutExceeded, chErrTooSlow:
		return ExitTimeout
	case chErrAttemptToReadAfterEOF, chErrUnknownPacketFromClient, chErrNetworkError,
		chErrQueryWasCancelled, chErrQueryWasCancelledByClient:
		return ExitAborted
	default:
		return ExitError
	}
}
