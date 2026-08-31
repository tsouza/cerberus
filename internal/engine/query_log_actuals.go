package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/tsouza/cerberus/internal/actuals"
)

// query_log_actuals.go is issue #2789's batch/fallback actuals source: it
// polls system.query_log for cerberus-stamped queries (log_comment carrying
// a "cerb:..." plan-shape id — see plan_shape_id.go's shapeIDPrefix) and
// feeds their (read_rows, read_bytes, memory_usage) into an
// actuals.Tracker as SourceQueryLog, for the queries the native-protocol
// packet fast path (internal/chclient/progress.go) could not observe: one
// that failed before completing (flush() never runs on that path), or a
// deployment mode where packet capture is not wired.
//
// Genuinely a SLOW path by construction — system.query_log's own async
// flush lag (docs/operations.md) means a row surfaces here well after the
// query that produced it finished — so QueryLogActualsReconciler is
// designed around a poll interval and a watermark, never assumed to arrive
// synchronously with the query it describes. Mirrors
// internal/optcorpus.Reconciler's own Run(ctx)/ticker shape (the
// established pattern in this codebase for a background system.query_log
// poller), independently implemented rather than shared: optcorpus reads a
// DIFFERENT row shape for a different purpose (the whole performance
// corpus, joined on query_id) and this package must not import optcorpus
// (.go-arch-lint.yml: optcorpus is wired from cmd only, and importing it
// here would pull its own sink/ring machinery for a feature that needs
// none of it).

// queryLogActualsBatchLimit bounds how many rows a single poll tick reads
// from system.query_log — a hard ceiling on ONE round trip's result size,
// independent of how long the tracker's own resident capacity
// (actuals.trackerCapacity) is, so a burst of cerberus-stamped traffic
// between two polls cannot make one tick's query unboundedly large.
const queryLogActualsBatchLimit = 1000

// QueryLogQuerier is the narrow chclient seam
// QueryLogActualsReconciler depends on — *chclient.Client in production,
// faked in tests so this file's polling/watermark logic is testable
// without a live ClickHouse. Mirrors explain_estimate_wiring.go's own
// Estimator interface.
type QueryLogQuerier interface {
	QueryLogActuals(ctx context.Context, since time.Time, shapeIDPrefix string, limit int) ([]QueryLogActualRow, error)
}

// QueryLogActualRow mirrors chclient.QueryLogActualRow field-for-field —
// declared locally so this package's polling logic depends only on the
// narrow QueryLogQuerier interface, never chclient's concrete type,
// exactly like explain_estimate_wiring.go's Estimator seam. cmd/cerberus's
// wiring adapts *chclient.Client's real return type to this one (the two
// structs are identical; the adapter is a one-line type conversion — see
// buildQueryLogActualsReconciler in cmd/cerberus/main.go).
type QueryLogActualRow struct {
	LogComment  string
	ReadRows    uint64
	ReadBytes   uint64
	MemoryUsage uint64
	EventTime   time.Time
}

// QueryLogActualsReconciler is the OPTIONAL background poller backing this
// file's own doc. Construct with NewQueryLogActualsReconciler; the zero
// value is not usable.
type QueryLogActualsReconciler struct {
	client   QueryLogQuerier
	tracker  *actuals.Tracker
	interval time.Duration
	lookback time.Duration
	logger   *slog.Logger

	now func() time.Time // overridable by tests
}

// NewQueryLogActualsReconciler constructs a reconciler. logger may be nil
// (a poll failure is then silently swallowed rather than logged — see
// poll's own doc for why a failure is never fatal either way).
func NewQueryLogActualsReconciler(client QueryLogQuerier, tracker *actuals.Tracker, cfg actuals.Config, logger *slog.Logger) *QueryLogActualsReconciler {
	return &QueryLogActualsReconciler{
		client:   client,
		tracker:  tracker,
		interval: cfg.QueryLogPollInterval,
		lookback: cfg.QueryLogLookback,
		logger:   logger,
		now:      time.Now,
	}
}

// Run polls until ctx is cancelled — the caller's background-goroutine
// lifecycle owns when this returns (cmd/cerberus wires it into the same
// graceful-shutdown group every other background reconciler in this
// process uses). Mirrors internal/optcorpus.Reconciler.Run's own
// ticker/select shape.
func (r *QueryLogActualsReconciler) Run(ctx context.Context) {
	if r.client == nil || r.tracker == nil {
		<-ctx.Done()
		return
	}
	since := r.now().Add(-r.lookback)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			since = r.poll(ctx, since)
		}
	}
}

// poll reads every cerberus-stamped system.query_log row with
// event_time > since, feeds each into the tracker as SourceQueryLog, emits
// the drift-alert telemetry for any row with enough prediction history to
// compute a ratio, and returns the new watermark: the latest EventTime seen,
// or the UNCHANGED since on any query failure (retry from the same
// watermark next tick, rather than either advancing past unread rows or
// re-reading the whole lookback window forever). A query failure is never
// fatal — this is a best-effort fallback path layered on top of the packet
// fast path, so an operator's query_log misconfiguration (not enabled,
// insufficient permissions) must never crash the process, only leave this
// ONE source degraded — logged when a logger is wired, silently swallowed
// otherwise.
func (r *QueryLogActualsReconciler) poll(ctx context.Context, since time.Time) time.Time {
	rows, err := r.client.QueryLogActuals(ctx, since, shapeIDPrefix, queryLogActualsBatchLimit)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("query_log actuals poll failed", "err", err)
		}
		return since
	}
	next := since
	for _, row := range rows {
		if row.LogComment == "" {
			continue
		}
		report, ok := r.tracker.RecordActual(row.LogComment, actuals.Actual{
			ReadRows:   row.ReadRows,
			ReadBytes:  row.ReadBytes,
			PeakMemory: row.MemoryUsage,
		}, actuals.SourceQueryLog)
		if ok && report.HasPredicted {
			recordEstimateDriftFromQueryLog(ctx, report)
		}
		if row.EventTime.After(next) {
			next = row.EventTime
		}
	}
	return next
}
