package local

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
)

// ResultKind tags which variant a Result carries.
type ResultKind int

const (
	// ResultKindVector indicates an instant-query result (one value per series
	// at a single timestamp).
	ResultKindVector ResultKind = iota
	// ResultKindMatrix indicates a range-query result (a time series per
	// matching series).
	ResultKindMatrix
	// ResultKindScalar indicates a single-valued numeric result.
	ResultKindScalar
)

// VectorSample mirrors promql.Sample but is decoupled from Prometheus's
// internal types so downstream diff code can compare against cerberus's own
// chclient sample shape without dragging in promql/parser internals.
type VectorSample struct {
	Metric labels.Labels
	T      int64
	V      float64
}

// MatrixSeries is one labelled time series in a range-query result.
type MatrixSeries struct {
	Metric labels.Labels
	Points []FloatSample
}

// Result is the cerberus-shaped output of an evaluator query. Exactly one of
// Vector / Matrix / Scalar is populated depending on Kind.
type Result struct {
	Kind     ResultKind
	Vector   []VectorSample
	Matrix   []MatrixSeries
	Scalar   *VectorSample // Scalar carries a single (T, V) pair; Metric is empty.
	Warnings annotations.Annotations
}

// Instant evaluates query at instant ts against the supplied SampleStore and
// returns a cerberus-shaped Result.
func (e *Engine) Instant(ctx context.Context, store storage.Queryable, query string, ts time.Time) (Result, error) {
	q, err := e.engine.NewInstantQuery(ctx, store, nil, query, ts)
	if err != nil {
		return Result{}, fmt.Errorf("local: prepare instant query: %w", err)
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return Result{}, fmt.Errorf("local: exec instant query: %w", res.Err)
	}
	return toResult(res)
}

// Range evaluates query over [start, end] at the given step against the
// supplied SampleStore and returns a cerberus-shaped Result. Step must be > 0.
func (e *Engine) Range(ctx context.Context, store storage.Queryable, query string, start, end time.Time, step time.Duration) (Result, error) {
	if step <= 0 {
		return Result{}, fmt.Errorf("local: range step must be > 0, got %s", step)
	}
	q, err := e.engine.NewRangeQuery(ctx, store, nil, query, start, end, step)
	if err != nil {
		return Result{}, fmt.Errorf("local: prepare range query: %w", err)
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return Result{}, fmt.Errorf("local: exec range query: %w", res.Err)
	}
	return toResult(res)
}

func toResult(res *promql.Result) (Result, error) {
	switch v := res.Value.(type) {
	case promql.Vector:
		out := Result{Kind: ResultKindVector, Warnings: res.Warnings, Vector: make([]VectorSample, 0, len(v))}
		for _, s := range v {
			out.Vector = append(out.Vector, VectorSample{Metric: s.Metric, T: s.T, V: s.F})
		}
		return out, nil
	case promql.Matrix:
		out := Result{Kind: ResultKindMatrix, Warnings: res.Warnings, Matrix: make([]MatrixSeries, 0, len(v))}
		for _, s := range v {
			pts := make([]FloatSample, 0, len(s.Floats))
			for _, p := range s.Floats {
				pts = append(pts, FloatSample{T: p.T, V: p.F})
			}
			out.Matrix = append(out.Matrix, MatrixSeries{Metric: s.Metric, Points: pts})
		}
		return out, nil
	case promql.Scalar:
		return Result{
			Kind:     ResultKindScalar,
			Warnings: res.Warnings,
			Scalar:   &VectorSample{T: v.T, V: v.V},
		}, nil
	default:
		return Result{}, fmt.Errorf("local: unsupported result value type %T", res.Value)
	}
}
