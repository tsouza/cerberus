package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
)

// query_log_actuals.go is the query-log actuals source (issue #2789): it
// polls the server's query log for cerberus-stamped queries (log_comment
// carrying a "cerb:..." plan-shape id — see plan_shape_id.go's
// shapeIDPrefix) and feeds their (read_rows, read_bytes, memory_usage) into
// an actuals.Tracker as SourceQueryLog.
//
// What it can add. Every dispatch this process arms for actuals capture is
// claimed by the packet path at dispatch (Tracker.MarkPacketObserved), and
// the packet path observes it on the dispatching connection whichever server
// ran it and whatever later happens to that server's log. This source
// therefore records only rows no packet observation in this process covers:
// a query stamped without capture armed (CERBERUS_LOG_COMMENT_SHAPE on a path
// that never classified), and a query another process dispatched (another
// cerberus replica, or this process before a restart). Which of those rows it
// can see is decided by the table it reads — the local system.query_log holds
// only the queries the connected server itself initiated since its log last
// rotated; system.all_query_log (the query_log_union chopt feature) holds
// every rotated table and, when the server's union section names a cluster,
// every replica.
//
// Reading. The reconciler keeps a cursor in the reader's total order (event
// time in microseconds, hostname, query_id) and reads strictly after it, one
// bounded page at a time, up to queryLogActualsMaxPagesPerPoll pages per
// poll; the cursor survives across polls, so a backlog drains over several
// polls rather than in one unbounded read. Rows younger than the settle delay
// are left for a later poll (chclient.QueryLogActualsRequest.SettleDelay), so
// an asynchronously flushed row is never skipped by the forward-only cursor.
// Rows whose query started more than the lookback ago are never read; the
// cursor is clamped to the lookback at the start of every poll, which also
// places the first poll.
//
// Genuinely a SLOW path by construction: a row surfaces here a settle delay
// and a poll interval after the query that produced it finished. Mirrors
// internal/optcorpus.Reconciler's own Run(ctx)/ticker shape, independently
// implemented rather than shared: optcorpus reads a DIFFERENT row shape for a
// different purpose and this package must not import optcorpus
// (.go-arch-lint.yml).

// queryLogActualsBatchLimit bounds how many rows a single page read returns
// — a hard ceiling on ONE round trip's result size.
const queryLogActualsBatchLimit = 1000

// queryLogActualsMaxPagesPerPoll bounds how many pages one poll reads, so a
// backlog (a burst of stamped traffic, or a reconciler that fell behind)
// costs at most this many bounded reads per poll interval; the cursor
// carries the rest to the next poll.
const queryLogActualsMaxPagesPerPoll = 10

// QueryLogQuerier is the narrow chclient seam QueryLogActualsReconciler
// depends on — *chclient.Client in production, faked in tests so the polling
// and cursor logic is testable without a live ClickHouse.
type QueryLogQuerier interface {
	QueryLogActuals(ctx context.Context, req chclient.QueryLogActualsRequest) ([]chclient.QueryLogActualRow, error)
}

// QueryLogActualsReconciler is the OPTIONAL background poller backing this
// file's own doc. Construct with NewQueryLogActualsReconciler; the zero
// value is not usable. Not safe for concurrent use: Run owns it.
type QueryLogActualsReconciler struct {
	client   QueryLogQuerier
	tracker  *actuals.Tracker
	interval time.Duration
	lookback time.Duration
	settle   time.Duration
	// union reports whether the query_log_union feature is in force right
	// now (the live chopt resolution). nil reads the local log only.
	union  func() bool
	logger *slog.Logger

	cursor chclient.QueryLogCursor
	// unionRefused is whether the last union read was refused and fell back
	// to the local log, so the fallback is logged once per transition rather
	// than once per poll.
	unionRefused bool

	now func() time.Time // overridable by tests
}

// NewQueryLogActualsReconciler constructs a reconciler. union may be nil
// (local log only); logger may be nil (a poll failure is then silently
// swallowed rather than logged — see Poll's own doc for why a failure is
// never fatal either way).
func NewQueryLogActualsReconciler(client QueryLogQuerier, tracker *actuals.Tracker, cfg actuals.Config, union func() bool, logger *slog.Logger) *QueryLogActualsReconciler {
	return &QueryLogActualsReconciler{
		client:   client,
		tracker:  tracker,
		interval: cfg.QueryLogPollInterval,
		lookback: cfg.QueryLogLookback,
		settle:   cfg.QueryLogSettleDelay,
		union:    union,
		logger:   logger,
		now:      time.Now,
	}
}

// Run polls until ctx is cancelled — the caller's background-goroutine
// lifecycle owns when this returns. Mirrors internal/optcorpus.Reconciler.Run's
// own ticker/select shape.
func (r *QueryLogActualsReconciler) Run(ctx context.Context) {
	if r.client == nil || r.tracker == nil {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Poll(ctx)
		}
	}
}

// Poll runs one reconciliation pass: it reads up to
// queryLogActualsMaxPagesPerPoll pages after the cursor, feeds each row into
// the tracker as SourceQueryLog, emits the drift-alert telemetry for any row
// with enough prediction history, and advances the cursor past every row it read — recorded or refused alike, since a read row
// is never read again. A read failure leaves the cursor where it is, so the
// next poll retries the same page. A failure is never fatal: this is a
// best-effort source layered on top of the packet path, so a query-log
// misconfiguration must only leave this ONE source degraded — logged when a
// logger is wired, silently swallowed otherwise.
func (r *QueryLogActualsReconciler) Poll(ctx context.Context) {
	if floor := r.now().Add(-r.lookback); r.cursor.EventTime.Before(floor) {
		r.cursor = chclient.QueryLogCursor{EventTime: floor}
	}
	union := r.union != nil && r.union()
	for range queryLogActualsMaxPagesPerPoll {
		rows, err := r.readPage(ctx, &union)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("query_log actuals poll failed", "err", err)
			}
			return
		}
		for _, row := range rows {
			r.record(ctx, row)
			r.cursor = chclient.QueryLogCursor{EventTime: row.EventTime, Hostname: row.Hostname, QueryID: row.QueryID}
		}
		if len(rows) < queryLogActualsBatchLimit {
			return
		}
	}
}

// readPage reads one page after the cursor from the union table when *union
// is set, falling back to the local log — for this read and the rest of the
// poll — when the server refuses the union read (the table was dropped, the
// grant revoked, or a member answered with an error between two capability
// probes). The fallback and the recovery are each logged once per transition.
func (r *QueryLogActualsReconciler) readPage(ctx context.Context, union *bool) ([]chclient.QueryLogActualRow, error) {
	req := chclient.QueryLogActualsRequest{
		Union:         *union,
		After:         r.cursor,
		SettleDelay:   r.settle,
		Window:        r.lookback,
		ShapeIDPrefix: shapeIDPrefix,
		Limit:         queryLogActualsBatchLimit,
	}
	rows, err := r.client.QueryLogActuals(ctx, req)
	if !*union {
		return rows, err
	}
	if errors.Is(err, chclient.ErrQueryLogUnionRefused) {
		if !r.unionRefused && r.logger != nil {
			r.logger.Warn("system.all_query_log refused; reading the local system.query_log", "err", err)
		}
		r.unionRefused = true
		*union = false
		req.Union = false
		return r.client.QueryLogActuals(ctx, req)
	}
	if err == nil && r.unionRefused {
		r.unionRefused = false
		if r.logger != nil {
			r.logger.Info("system.all_query_log readable again")
		}
	}
	return rows, err
}

// record feeds one row into the tracker unless the packet path already owns
// its query id.
func (r *QueryLogActualsReconciler) record(ctx context.Context, row chclient.QueryLogActualRow) {
	if row.LogComment == "" {
		return
	}
	// Set difference against the progress-packet path: this poller and the
	// packet path feed the SAME Tracker for the SAME physical query, and
	// without this every completed query was recorded TWICE — enough, on its
	// own, to satisfy a MinObservations=2 corroboration floor, and on route B
	// enough to drag the EMA toward per-shard rows with K fractional samples
	// per request (cerberus issue #3184).
	if !r.tracker.ClaimQueryLogRow(row.QueryID) {
		return
	}
	report, ok := r.tracker.RecordActual(row.LogComment, actuals.Actual{
		ReadRows:   row.ReadRows,
		ReadBytes:  row.ReadBytes,
		PeakMemory: row.MemoryUsage,
	}, actuals.SourceQueryLog)
	if ok && report.HasPredicted {
		recordEstimateDriftFromQueryLog(ctx, report)
	}
}
