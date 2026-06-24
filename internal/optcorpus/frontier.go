package optcorpus

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Per-deployment self-tuning corpus reader.
//
// This is the LOCAL data path for route self-tuning: it reads aggregated cost
// frontier buckets from THIS deployment's cerberus_router_corpus table and
// hands them to the GENERIC calibrator (internal/solver). It never reads all
// rows — it runs a small set of aggregate SELECTs (one GROUP BY over the
// (n_anchors, fanout, cumulative_d, route, danger) buckets) and returns one
// FrontierBucket per group. It reuses the exact CH read discipline the
// query_log source uses: bounded wall-clock context, single-threaded,
// deprioritised, hard row/byte caps, read-only, failure-open.
//
// FrontierBucket is intentionally a neutral aggregate type that carries NO
// solver dependency, so optcorpus stays decoupled from the calibrator (the
// cmd/cerberus wiring maps FrontierBucket → solver.CorpusSample). This keeps
// the generic/local boundary clean: optcorpus owns the table + read discipline;
// solver owns the calibration math; neither imports the other.

// FrontierBucket is one aggregated cost-grid bucket from the local corpus: the
// (N, F, D) coordinates plus the observed cost summary and whether queries in
// this bucket reached the OOM/cost danger zone. It is the per-deployment input
// the self-tuner aggregates into a frontier.
type FrontierBucket struct {
	// NAnchors / Fanout / CumulativeD are the cost-grid coordinates (the
	// corpus n_anchors / fanout / cumulative_d columns).
	NAnchors    uint32
	Fanout      uint32
	CumulativeD uint32

	// Route is the routing read-out: "A" or "B".
	Route string

	// BelowThreshold is true when this bucket's decision_reason was
	// below-threshold (eligible but kept on route A by the cost thresholds) —
	// the evidence the thresholds actually gated a plan.
	BelowThreshold bool

	// Danger is true when this bucket's exit_status reached the OOM/cost
	// danger zone (oom / timeout / sample_budget) — the frontier-defining
	// catastrophes route B exists to avoid.
	Danger bool

	// MaxMemoryUsage / MaxQueryDurationMS are the worst observed cost in the
	// bucket (bytes / ms).
	MaxMemoryUsage     uint64
	MaxQueryDurationMS uint64

	// Count is how many corpus rows this bucket aggregates.
	Count uint64
}

// frontierAggSQL aggregates the local corpus into cost-grid buckets for
// self-tuning. It groups by the (N, F, D) coordinates plus two derived
// booleans — whether the decision was below-threshold, and whether the exit
// reached the OOM/cost danger zone — so each returned row is one
// (coordinate × below-threshold × danger) bucket with its worst observed cost
// and row count. The event_time lookback is bounded by a parameter (derived
// from the recalibration interval) so a huge corpus is never scanned in full.
//
// The danger set (oom / timeout / sample_budget) mirrors solver's
// CorpusSample.OOM contract and the corpus Enum8 tokens (chtable.go). It is a
// fixed const query with a single parameterised window — no value is
// concatenated into the SQL — matching the queryLogSQL precedent in
// chsource.go.
const frontierAggSQL = `SELECT
		n_anchors,
		fanout,
		cumulative_d,
		toString(route) AS route_token,
		decision_reason = 'below-threshold' AS below_threshold,
		toString(exit_status) IN ('oom', 'timeout', 'sample_budget') AS danger,
		max(memory_usage) AS max_memory_usage,
		max(query_duration_ms) AS max_query_duration_ms,
		count() AS row_count
	FROM ` + CorpusTableName + `
	WHERE event_time > now() - INTERVAL ? SECOND
	GROUP BY n_anchors, fanout, cumulative_d, route_token, below_threshold, danger`

// CHFrontierSource reads aggregated frontier buckets from the local corpus
// table over a narrow CH read surface. It mirrors CHQueryLogSource: a bounded
// per-read context plus server-side conservative settings, so the self-tune
// reader can never starve the data plane.
type CHFrontierSource struct {
	conn    CHConn
	timeout time.Duration
	window  time.Duration
}

// NewCHFrontierSource builds a frontier source over conn (typically
// chclient.Client.Conn()). A non-positive timeout / window falls back to the
// same conservative defaults the query_log source uses, so the reader goroutine
// can never block indefinitely and always bounds its lookback.
func NewCHFrontierSource(conn CHConn, timeout, window time.Duration) *CHFrontierSource {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if window <= 0 {
		window = defaultQueryLogWindow
	}
	return &CHFrontierSource{conn: conn, timeout: timeout, window: window}
}

// ReadFrontier runs the bounded aggregate SELECT and decodes the buckets. It is
// read-only and failure-open: on any error it returns the error (the caller —
// the self-tune loop — keeps the current Config and retries next interval). An
// empty result (no corpus yet) returns (nil, nil), a legitimate "no signal".
func (s *CHFrontierSource) ReadFrontier(ctx context.Context) ([]FrontierBucket, error) {
	// Hard wall-clock bound on top of server-side max_execution_time so a
	// stuck scan can never pin the self-tune goroutine until shutdown.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	// Stamp the conservative resource caps so this background read cannot
	// disturb the data plane, exactly as the query_log source does.
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(corpusSettings()))

	windowSeconds := int64(s.window / time.Second)
	rows, err := s.conn.Query(ctx, frontierAggSQL, windowSeconds)
	if err != nil {
		return nil, fmt.Errorf("optcorpus: query %s frontier: %w", CorpusTableName, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FrontierBucket
	for rows.Next() {
		var b FrontierBucket
		if err := rows.Scan(
			&b.NAnchors,
			&b.Fanout,
			&b.CumulativeD,
			&b.Route,
			&b.BelowThreshold,
			&b.Danger,
			&b.MaxMemoryUsage,
			&b.MaxQueryDurationMS,
			&b.Count,
		); err != nil {
			return nil, fmt.Errorf("optcorpus: scan frontier bucket: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("optcorpus: frontier rows: %w", err)
	}
	return out, nil
}
