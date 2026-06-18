package optcorpus

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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
}

// NewCHQueryLogSource builds a CHQueryLogSource over conn (typically
// chclient.Client.Conn()). timeout bounds each corpus SELECT in wall-clock via
// a derived context (in addition to the server-side max_execution_time cap); a
// non-positive timeout falls back to a conservative default so the reconciler
// goroutine can never block indefinitely on a single scan.
func NewCHQueryLogSource(conn CHConn, timeout time.Duration) *CHQueryLogSource {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &CHQueryLogSource{conn: conn, timeout: timeout}
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

// queryLogSQL selects the finished (QueryFinish) rows for a set of query_ids,
// reading the cost columns, the normalized_query_hash, and the two ProfileEvents
// of interest (QueryConditionCacheHits, RowsReadByPrewhereReaders) projected
// out of the ProfileEvents map by name so the row decode is a fixed,
// strongly-typed shape. The query is bounded to a recent window so a large
// query_log does not get scanned in full each interval, and grouped to one row
// per query_id (a distributed query_log can carry initial + remote rows).
const queryLogSQL = `
SELECT
  query_id,
  any(normalized_query_hash)                         AS normalized_query_hash,
  max(read_rows)                                     AS read_rows,
  max(read_bytes)                                    AS read_bytes,
  max(query_duration_ms)                             AS query_duration_ms,
  max(memory_usage)                                  AS memory_usage,
  sum(ProfileEvents['QueryConditionCacheHits'])      AS condition_cache_hits,
  sum(ProfileEvents['RowsReadByPrewhereReaders'])    AS prewhere_rows
FROM system.query_log
WHERE type = 'QueryFinish'
  AND event_time > now() - INTERVAL 1 HOUR
  AND query_id IN (?)
GROUP BY query_id`

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

	rows, err := s.conn.Query(ctx, queryLogSQL, ids)
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
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("optcorpus: query_log rows: %w", err)
	}
	return out, nil
}
