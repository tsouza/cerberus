package prom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	promengine "github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/qlcommon"
)

// referenceEvaluationTimeout is the engine's own per-query timeout. The
// request context already carries cerberus's query timeout, which the
// engine honours, so this is only a ceiling that never binds first.
const referenceEvaluationTimeout = 24 * time.Hour

// referenceEngine builds the reference PromQL engine that answers an
// expression whose evaluation leaves the span the lowering represents
// exactly ([promql.LeavesEvaluationDomain]). Its options are the ones
// cerberus's own lowering follows: the five-minute staleness lookback,
// the one-minute default subquery step, `@` and negative offsets on, and
// the immediate `__name__` removal cerberus implements. It parses with
// the handler's own parser, so the two cannot disagree on the grammar.
//
// Samples are read from ClickHouse through [sampleQueryable], so the
// reference engine sees exactly the series a cerberus range selector
// returns.
func (h *Handler) referenceEngine() *promengine.Engine {
	maxSamples := math.MaxInt
	if h.Engine.MaxQuerySamples > 0 {
		maxSamples = int(h.Engine.MaxQuerySamples)
	}
	return promengine.NewEngine(promengine.EngineOpts{
		MaxSamples:    maxSamples,
		Timeout:       referenceEvaluationTimeout,
		LookbackDelta: qlcommon.InstantLookback,
		NoStepSubqueryIntervalFn: func(int64) int64 {
			return promql.DefaultSubqueryStep.Milliseconds()
		},
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
		Parser:               h.parser,
	})
}

// evaluateByReference answers query with the reference engine — as an
// instant query at start when step is zero, else as a range query over
// [start, end] — and returns the result already in the wire shape.
func (h *Handler) evaluateByReference(ctx context.Context, query string, start, end time.Time, step time.Duration) (*QueryData, error) {
	ng := h.referenceEngine()
	q := &sampleQueryable{h: h}
	qs := normalizeDottedSelectors(query)
	var (
		qry promengine.Query
		err error
	)
	if step == 0 {
		qry, err = ng.NewInstantQuery(ctx, q, nil, qs, start)
	} else {
		qry, err = ng.NewRangeQuery(ctx, q, nil, qs, start, end, step)
	}
	if err != nil {
		return nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	defer qry.Close()
	res := qry.Exec(ctx)
	if res.Err != nil {
		return nil, classifyReferenceError(res.Err)
	}
	return referenceQueryData(res.Value)
}

// respondByReference writes the response for a query answered by
// [Handler.evaluateByReference].
func (h *Handler) respondByReference(ctx context.Context, w http.ResponseWriter, r *http.Request, query string, start, end time.Time, step time.Duration) {
	data, err := h.evaluateByReference(ctx, query, start, end, step)
	if err != nil {
		h.respondError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Status: "success", Data: data})
}

// classifyReferenceError maps a reference-engine failure to the wire
// error reference Prometheus's HTTP API answers for it. A sample read
// failure carries cerberus's own classification through unchanged.
func classifyReferenceError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var tooMany promengine.ErrTooManySamples
	if errors.As(err, &tooMany) {
		return tooManySamplesAPIError()
	}
	var timeout promengine.ErrQueryTimeout
	if errors.As(err, &timeout) {
		return classifySentinelError(context.DeadlineExceeded)
	}
	var canceled promengine.ErrQueryCanceled
	if errors.As(err, &canceled) {
		return classifySentinelError(context.Canceled)
	}
	return &apiError{Kind: ErrExecution, Err: err, Status: http.StatusUnprocessableEntity}
}

// referenceQueryData converts a reference-engine value into the wire
// shape the lowered path answers with.
func referenceQueryData(v promparser.Value) (*QueryData, error) {
	switch val := v.(type) {
	case promengine.Scalar:
		return &QueryData{ResultType: "scalar", Result: scalarPoint(time.UnixMilli(val.T), val.V)}, nil
	case promengine.String:
		return &QueryData{ResultType: "string", Result: Sample{float64(val.T) / 1e3, val.V}}, nil
	case promengine.Vector:
		out := make([]VectorSample, 0, len(val))
		for _, s := range val {
			vs := VectorSample{Metric: s.Metric.Map()}
			if s.H != nil {
				hv, err := histogramValueFromFloat(s.H)
				if err != nil {
					return nil, err
				}
				p := histogramSample(time.UnixMilli(s.T), hv)
				vs.Histogram = &p
			} else {
				p := scalarPoint(time.UnixMilli(s.T), s.F)
				vs.Value = &p
			}
			out = append(out, vs)
		}
		return &QueryData{ResultType: "vector", Result: out}, nil
	case promengine.Matrix:
		sort.Sort(val)
		out := make([]MatrixSample, 0, len(val))
		for _, series := range val {
			ms := MatrixSample{Metric: series.Metric.Map()}
			for _, p := range series.Floats {
				ms.Values = append(ms.Values, scalarPoint(time.UnixMilli(p.T), p.F))
			}
			for _, p := range series.Histograms {
				hv, err := histogramValueFromFloat(p.H)
				if err != nil {
					return nil, err
				}
				ms.Histograms = append(ms.Histograms, histogramSample(time.UnixMilli(p.T), hv))
			}
			out = append(out, ms)
		}
		return &QueryData{ResultType: "matrix", Result: out}, nil
	default:
		return nil, fmt.Errorf("reference evaluation returned an unexpected %T", v)
	}
}

// sampleQueryable is the reference engine's storage: every Select reads
// the matching raw samples from ClickHouse with the same pinned range
// selector lowering a cerberus query uses ([promql.SampleReads]).
type sampleQueryable struct {
	h *Handler
}

func (q *sampleQueryable) Querier(int64, int64) (storage.Querier, error) {
	return &sampleQuerier{h: q.h}, nil
}

// sampleQuerier answers the reference engine's Selects. The engine asks
// for label names and values only on paths a query expression does not
// reach, so those answer empty.
type sampleQuerier struct {
	h *Handler
}

func (*sampleQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*sampleQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*sampleQuerier) Close() error { return nil }

// Select reads every stored sample matching matchers in the hinted
// window [hints.Start, hints.End], grouped into series sorted by label
// set.
func (q *sampleQuerier) Select(ctx context.Context, _ bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	startMs, endMs := int64(math.MinInt64), int64(math.MaxInt64)
	if hints != nil {
		startMs, endMs = hints.Start, hints.End
	}
	bySeries := map[string]*promengine.Series{}
	for _, sel := range promql.SampleReads(matchers, startMs, endMs) {
		samples, err := q.h.executeSampleRead(ctx, sel)
		if err != nil {
			return storage.ErrSeriesSet(err)
		}
		memo := newLabelMemo(len(samples))
		for _, s := range samples {
			lset := labels.FromMap(memo.normalize(s))
			key := lset.String()
			series, ok := bySeries[key]
			if !ok {
				series = &promengine.Series{Metric: lset}
				bySeries[key] = series
			}
			t := s.Timestamp.UnixMilli()
			if s.Histogram != nil {
				fh, err := floatHistogramFromValue(s.Histogram)
				if err != nil {
					return storage.ErrSeriesSet(err)
				}
				series.Histograms = append(series.Histograms, promengine.HPoint{T: t, H: fh})
				continue
			}
			series.Floats = append(series.Floats, promengine.FPoint{T: t, F: s.Value})
		}
	}
	out := make([]storage.Series, 0, len(bySeries))
	for _, series := range bySeries {
		sort.Slice(series.Floats, func(i, j int) bool { return series.Floats[i].T < series.Floats[j].T })
		sort.Slice(series.Histograms, func(i, j int) bool { return series.Histograms[i].T < series.Histograms[j].T })
		out = append(out, promengine.NewStorageSeries(*series))
	}
	sort.Slice(out, func(i, j int) bool { return labels.Compare(out[i].Labels(), out[j].Labels()) < 0 })
	return &seriesSlice{series: out, i: -1}
}

// executeSampleRead runs one sample read through the ordinary pipeline:
// lowering, optimisation, emission and execution.
func (h *Handler) executeSampleRead(ctx context.Context, sel *promparser.MatrixSelector) ([]chclient.Sample, error) {
	l := &sampleReadLang{
		lang: lang{
			Parser:                 h.parser,
			Schema:                 h.Schema,
			Lowerers:               h.lowerers(),
			ResourceBounds:         h.ResourceBounds,
			TagGroups:              h.TagGroups,
			ThrowDuplicateSeriesIf: h.ThrowDuplicateSeriesIf,
		},
		sel: sel,
	}
	res, err := h.Engine.Query(h.withSampleDrainBudget(ctx), l, sel.String())
	if err != nil {
		return nil, classifyEngineError(err)
	}
	return res.Samples, nil
}

// sampleReadLang is the PromQL adapter for one [promql.SampleRead]: it
// lowers the selector it carries with [promql.LowerSampleRead] instead
// of parsing the query text, which only names the read in logs and
// spans.
type sampleReadLang struct {
	lang
	sel *promparser.MatrixSelector
}

func (l *sampleReadLang) Parse(ctx context.Context, _ string) (chplan.Node, engine.Meta, error) {
	var guards []promql.ScalarGuard
	plan, err := promql.LowerSampleRead(ctx, l.sel, l.Schema, promql.LowerOpts{
		Lowerers:               l.Lowerers,
		Guards:                 &guards,
		ResourceBounds:         l.ResourceBounds,
		TagGroups:              l.TagGroups,
		ThrowDuplicateSeriesIf: l.ThrowDuplicateSeriesIf,
	})
	if err != nil {
		return nil, engine.Meta{}, &parseStageError{stage: "lower", err: err}
	}
	return plan, engine.Meta{IsMetric: true, Guards: engineGuards(guards)}, nil
}

// seriesSlice is a storage.SeriesSet over an in-memory, sorted slice.
type seriesSlice struct {
	series []storage.Series
	i      int
}

func (s *seriesSlice) Next() bool {
	s.i++
	return s.i < len(s.series)
}

func (s *seriesSlice) At() storage.Series              { return s.series[s.i] }
func (*seriesSlice) Err() error                        { return nil }
func (*seriesSlice) Warnings() annotations.Annotations { return nil }

// floatHistogramFromValue converts a decoded OTel exponential histogram
// into the reference engine's native histogram, shifting each offset by
// [otelToPromBucketIndex]. A scale finer than Prometheus's finest
// schema is merged down to it, exactly as Prometheus's own OTLP ingestion
// does; a coarser one has no Prometheus representation.
func floatHistogramFromValue(hv *chclient.HistogramValue) (*histogram.FloatHistogram, error) {
	positiveSpans, err := bucketSpans(hv.PositiveOffset, len(hv.PositiveBucketCounts))
	if err != nil {
		return nil, err
	}
	negativeSpans, err := bucketSpans(hv.NegativeOffset, len(hv.NegativeBucketCounts))
	if err != nil {
		return nil, err
	}
	fh := &histogram.FloatHistogram{
		Schema:          hv.Scale,
		ZeroThreshold:   hv.ZeroThreshold,
		ZeroCount:       hv.ZeroCount,
		Count:           hv.Count,
		Sum:             hv.Sum,
		PositiveSpans:   positiveSpans,
		PositiveBuckets: append([]float64(nil), hv.PositiveBucketCounts...),
		NegativeSpans:   negativeSpans,
		NegativeBuckets: append([]float64(nil), hv.NegativeBucketCounts...),
	}
	if fh.Schema > histogram.ExponentialSchemaMax {
		if err := fh.ReduceResolution(histogram.ExponentialSchemaMax); err != nil {
			return nil, err
		}
	}
	if fh.Schema < histogram.ExponentialSchemaMin {
		return nil, fmt.Errorf("exponential histogram scale %d is coarser than the coarsest Prometheus schema %d",
			fh.Schema, histogram.ExponentialSchemaMin)
	}
	return fh, nil
}

// otelToPromBucketIndex is the shift from an OTel exponential bucket index
// to the Prometheus native bucket index naming the same span of values:
// OTel's bucket k covers (base^k, base^(k+1)], Prometheus's k+1.
const otelToPromBucketIndex = 1

// bucketSpans is the single span covering n consecutive OTel buckets
// starting at otelOffset, in Prometheus's bucket indexing. A span's
// length is a uint32, so a longer run has no single-span form.
func bucketSpans(otelOffset int32, n int) ([]histogram.Span, error) {
	if n == 0 {
		return nil, nil
	}
	if n < 0 || n > math.MaxUint32 {
		return nil, fmt.Errorf("%d histogram buckets do not fit one span", n)
	}
	return []histogram.Span{{Offset: otelOffset + otelToPromBucketIndex, Length: uint32(n)}}, nil
}

// histogramValueFromFloat is [floatHistogramFromValue]'s inverse, for a
// histogram the reference engine computed. Gaps between spans become
// zero-count buckets, which the wire encoding omits.
func histogramValueFromFloat(fh *histogram.FloatHistogram) (*chclient.HistogramValue, error) {
	if !histogram.IsExponentialSchema(fh.Schema) {
		return nil, fmt.Errorf("reference evaluation returned a histogram with non-exponential schema %d", fh.Schema)
	}
	posOffset, pos := denseBuckets(fh.PositiveSpans, fh.PositiveBuckets)
	negOffset, neg := denseBuckets(fh.NegativeSpans, fh.NegativeBuckets)
	return &chclient.HistogramValue{
		Count:                fh.Count,
		Sum:                  fh.Sum,
		Scale:                fh.Schema,
		ZeroThreshold:        fh.ZeroThreshold,
		ZeroCount:            fh.ZeroCount,
		PositiveOffset:       posOffset,
		PositiveBucketCounts: pos,
		NegativeOffset:       negOffset,
		NegativeBucketCounts: neg,
	}, nil
}

// denseBuckets expands Prometheus spans into one OTel offset and a dense
// count array. The first span's offset is an absolute bucket index, each
// later one the gap after the previous span.
func denseBuckets(spans []histogram.Span, buckets []float64) (otelOffset int32, dense []float64) {
	var (
		idx     int32 // Prometheus index of the next bucket
		emitted int32 // len(dense), kept in the index type
		b       int
		first   = true
	)
	for _, sp := range spans {
		idx += sp.Offset
		for range sp.Length {
			if first {
				otelOffset = idx - otelToPromBucketIndex
				first = false
			}
			for emitted < idx-otelToPromBucketIndex-otelOffset {
				dense = append(dense, 0)
				emitted++
			}
			dense = append(dense, buckets[b])
			emitted++
			b++
			idx++
		}
	}
	return otelOffset, dense
}
