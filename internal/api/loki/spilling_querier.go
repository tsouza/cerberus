package loki

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
)

// spillingQuerier is the Querier the handler's own statements go through: the
// metadata, index, pattern and tail endpoints, which dispatch without the
// engine. Every statement it sends carries the engine's spill thresholds
// (engine.Engine.WithSpillThresholds), so a high-cardinality metadata
// DISTINCT, GROUP BY or sort spills to disk instead of aborting at the
// per-query memory cap.
//
// Each Querier method is wrapped by name rather than embedded, so a method
// added to Querier does not compile until it stamps the thresholds too. The
// Engine is read at dispatch, not captured, so the one in force when the
// statement runs sizes it. The Engine itself keeps the raw client: it stamps
// its own thresholds sized per dispatch route.
type spillingQuerier struct {
	inner Querier
	h     *Handler
}

func (q *spillingQuerier) stamp(ctx context.Context) context.Context {
	return q.h.Engine.WithSpillThresholds(ctx)
}

func (q *spillingQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	return q.inner.Query(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	return q.inner.QueryStrings(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryDetectedFieldRows(ctx context.Context, sql string, args ...any) ([]chclient.DetectedFieldRow, error) {
	return q.inner.QueryDetectedFieldRows(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryTimestampedLines(ctx context.Context, sql string, args ...any) ([]chclient.TimestampedLine, error) {
	return q.inner.QueryTimestampedLines(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryIndexStats(ctx context.Context, sql string, args ...any) (chclient.IndexStatsRow, error) {
	return q.inner.QueryIndexStats(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryIndexVolume(ctx context.Context, sql string, args ...any) ([]chclient.IndexVolumeRow, error) {
	return q.inner.QueryIndexVolume(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error) {
	return q.inner.QueryLabelSets(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryLabelCardinalities(ctx context.Context, sql string, args ...any) ([]chclient.LabelCardinalityRow, error) {
	return q.inner.QueryLabelCardinalities(q.stamp(ctx), sql, args...)
}
