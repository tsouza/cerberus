package promql

import (
	"sort"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// VectorRow is one (labels, ts, value) tuple in an instant-vector
// result. The timestamp is the evaluation timestamp (Prom convention:
// every output sample is stamped at eval ts, not the source sample's
// ts).
type VectorRow struct {
	Labels map[string]string
	T      int64
	V      float64
}

// RangePoints is one (labels, samples) tuple in a range-vector result.
// The samples are the per-series window samples in (T-range, T] order.
type RangePoints struct {
	Labels  map[string]string
	Samples []Sample
}

// evalVectorSelector evaluates a bare instant-vector selector at
// timestamp evalTsMs (unix milliseconds). It applies the matchers,
// offset, and @ modifier, then for each matching series returns the
// most recent sample with `T <= effectiveTs` AND
// `effectiveTs - T < lookback`. The output row's timestamp is the
// caller-visible eval ts (the original evalTsMs, NOT shifted by
// offset).
func (e *Evaluator) evalVectorSelector(v *parser.VectorSelector, evalTsMs int64) []VectorRow {
	// @ modifier overrides the effective eval timestamp entirely.
	effective := effectiveEvalTs(v, evalTsMs, e.startMs, e.endMs)
	// Offset shifts the lookup backward.
	effective -= v.OriginalOffset.Milliseconds()

	out := make([]VectorRow, 0, len(e.model.Series))
	lookbackMs := e.lookback.Milliseconds()
	for _, s := range e.model.Series {
		if !matchSeries(s, v.LabelMatchers) {
			continue
		}
		sample, ok := latestSampleAtOrBefore(s.Samples, effective, lookbackMs)
		if !ok {
			continue
		}
		out = append(out, VectorRow{
			Labels: CopyLabels(s.Labels),
			T:      evalTsMs,
			V:      sample.V,
		})
	}
	sortVectorRows(out)
	return out
}

// evalMatrixSelector evaluates a range-vector selector (`m[range]`) at
// evalTsMs. For each matching series it returns the samples whose
// timestamps fall in (effective - range, effective], where effective
// includes any @ modifier + offset.
func (e *Evaluator) evalMatrixSelector(m *parser.MatrixSelector, evalTsMs int64) []RangePoints {
	v, ok := m.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil
	}
	effective := effectiveEvalTs(v, evalTsMs, e.startMs, e.endMs)
	effective -= v.OriginalOffset.Milliseconds()

	rangeMs := m.Range.Milliseconds()
	lo := effective - rangeMs // EXCLUSIVE
	hi := effective           // INCLUSIVE

	out := make([]RangePoints, 0, len(e.model.Series))
	for _, s := range e.model.Series {
		if !matchSeries(s, v.LabelMatchers) {
			continue
		}
		samples := windowSamples(s.Samples, lo, hi)
		if len(samples) == 0 {
			continue
		}
		out = append(out, RangePoints{
			Labels:  CopyLabels(s.Labels),
			Samples: samples,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out
}

// latestSampleAtOrBefore returns the most recent sample with
// `T <= effectiveTs` AND `effectiveTs - T < lookbackMs`. The samples
// slice MUST be sorted by T ascending.
//
// This is the canonical PromQL instant-selector LWR rule (Latest With
// Respect to T). Samples beyond the lookback window are treated as
// stale and skipped — those are the "no fresh sample" series that
// disappear from the output.
func latestSampleAtOrBefore(samples []Sample, effectiveTs, lookbackMs int64) (Sample, bool) {
	if len(samples) == 0 {
		return Sample{}, false
	}
	// Binary search: largest i with samples[i].T <= effectiveTs.
	idx := sort.Search(len(samples), func(i int) bool {
		return samples[i].T > effectiveTs
	}) - 1
	if idx < 0 {
		return Sample{}, false
	}
	if effectiveTs-samples[idx].T >= lookbackMs {
		return Sample{}, false
	}
	return samples[idx], true
}

// windowSamples returns the samples whose timestamps fall in
// (lo, hi] — i.e. STRICTLY greater than lo and AT-MOST hi. The Prom
// range-selector spec is exclusive on the left, inclusive on the
// right. The input slice MUST be sorted by T ascending.
func windowSamples(samples []Sample, lo, hi int64) []Sample {
	if len(samples) == 0 {
		return nil
	}
	first := sort.Search(len(samples), func(i int) bool {
		return samples[i].T > lo
	})
	last := sort.Search(len(samples), func(i int) bool {
		return samples[i].T > hi
	})
	if first >= last {
		return nil
	}
	out := make([]Sample, last-first)
	copy(out, samples[first:last])
	return out
}

// effectiveEvalTs resolves the eval timestamp for a vector selector,
// honoring any @ modifier (start()/end()/<unix_seconds>). Offset is
// applied separately by the caller (so the per-series sample lookup
// can shift independently of the result timestamp).
func effectiveEvalTs(v *parser.VectorSelector, evalTsMs, startMs, endMs int64) int64 {
	if v.Timestamp != nil {
		return *v.Timestamp
	}
	switch v.StartOrEnd {
	case parser.START:
		return startMs
	case parser.END:
		return endMs
	}
	return evalTsMs
}

// sortVectorRows sorts rows by their canonical label-set string for
// deterministic comparator output.
func sortVectorRows(rows []VectorRow) {
	sort.Slice(rows, func(i, j int) bool {
		return labelKey(rows[i].Labels) < labelKey(rows[j].Labels)
	})
}

// instantSampleTimestampForOffset returns the effective evaluation
// timestamp millis after applying offset. Unused as a standalone
// function today — callers compute it inline — but kept here so the
// LWR semantics are documented in one place. Reserved for future
// expansion (e.g. when subquery offsets need to recurse).
func instantSampleTimestampForOffset(evalTsMs int64, offset time.Duration) int64 {
	return evalTsMs - offset.Milliseconds()
}

// _ silences unused-function lints on instantSampleTimestampForOffset.
var _ = instantSampleTimestampForOffset
