package prom

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
)

// spillingQuerier is the Querier the handler's own statements go through: the
// metadata endpoints and the exemplars lookup, which dispatch without the
// engine. Every statement it sends carries the engine's spill thresholds
// (engine.Engine.WithSpillThresholds), so a high-cardinality metadata
// DISTINCT, GROUP BY or sort spills to disk instead of aborting at the
// per-query memory cap.
//
// Each Querier method is wrapped by name rather than embedded, so a method
// added to Querier does not compile until it stamps the thresholds too. The
// Engine is read at dispatch, not captured, because cmd/cerberus replaces it
// after New. The Engine itself keeps the raw client: it stamps its own
// thresholds sized per dispatch route, and wrapping would hide the client's
// optional cursor and memory-cap surfaces.
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

func (q *spillingQuerier) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	return q.inner.QueryCursor(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	return q.inner.QueryStrings(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error) {
	return q.inner.QueryLabelSets(q.stamp(ctx), sql, args...)
}

func (q *spillingQuerier) QueryMetricMeta(ctx context.Context, sql, metricType string, args ...any) ([]chclient.MetricMetaRow, error) {
	return q.inner.QueryMetricMeta(q.stamp(ctx), sql, metricType, args...)
}

func (q *spillingQuerier) QueryExemplars(ctx context.Context, sql string, args ...any) ([]chclient.ExemplarRow, error) {
	return q.inner.QueryExemplars(q.stamp(ctx), sql, args...)
}
