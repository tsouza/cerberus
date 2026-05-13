package local

import (
	"context"
	"sort"
	"sync"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
)

// FloatSample is a single (timestamp, value) tuple stored in a SampleStore.
// Timestamps are unix milliseconds, matching Prometheus's internal convention.
type FloatSample struct {
	T int64
	V float64
}

// SampleStore is a minimal in-memory implementation of storage.Queryable
// suitable for use as the shadow-mode oracle. Series are keyed by their full
// labelset; per-series samples are kept sorted by timestamp.
//
// The zero value is ready to use, but prefer NewSampleStore for clarity.
type SampleStore struct {
	mu     sync.RWMutex
	series map[uint64]*storedSeries
}

type storedSeries struct {
	lset    labels.Labels
	samples []FloatSample
}

// NewSampleStore returns an empty SampleStore.
func NewSampleStore() *SampleStore {
	return &SampleStore{series: make(map[uint64]*storedSeries)}
}

// Append inserts a (timestamp, value) sample for the given labelset. Samples
// are kept sorted by timestamp; out-of-order inserts are placed in order.
// Duplicate timestamps for the same series overwrite the previous value.
func (s *SampleStore) Append(lset labels.Labels, t int64, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.series == nil {
		s.series = make(map[uint64]*storedSeries)
	}
	key := lset.Hash()
	ser, ok := s.series[key]
	if !ok {
		ser = &storedSeries{lset: lset.Copy()}
		s.series[key] = ser
	}
	idx := sort.Search(len(ser.samples), func(i int) bool {
		return ser.samples[i].T >= t
	})
	if idx < len(ser.samples) && ser.samples[idx].T == t {
		ser.samples[idx].V = v
		return
	}
	ser.samples = append(ser.samples, FloatSample{})
	copy(ser.samples[idx+1:], ser.samples[idx:])
	ser.samples[idx] = FloatSample{T: t, V: v}
}

// Querier implements storage.Queryable.
func (s *SampleStore) Querier(mint, maxt int64) (storage.Querier, error) {
	return &sampleStoreQuerier{store: s, mint: mint, maxt: maxt}, nil
}

type sampleStoreQuerier struct {
	store      *SampleStore
	mint, maxt int64
}

func (q *sampleStoreQuerier) Select(_ context.Context, sortSeries bool, _ *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	q.store.mu.RLock()
	defer q.store.mu.RUnlock()

	var matched []storage.Series
	for _, ser := range q.store.series {
		if !matchAll(ser.lset, matchers) {
			continue
		}
		samples := windowSamples(ser.samples, q.mint, q.maxt)
		if len(samples) == 0 {
			continue
		}
		matched = append(matched, storage.NewListSeries(ser.lset, toChunkSamples(samples)))
	}
	if sortSeries {
		sort.Slice(matched, func(i, j int) bool {
			return labels.Compare(matched[i].Labels(), matched[j].Labels()) < 0
		})
	}
	return &sliceSeriesSet{series: matched, idx: -1}
}

func (q *sampleStoreQuerier) LabelValues(_ context.Context, name string, _ *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.store.mu.RLock()
	defer q.store.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, ser := range q.store.series {
		if !matchAll(ser.lset, matchers) {
			continue
		}
		if v := ser.lset.Get(name); v != "" {
			seen[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *sampleStoreQuerier) LabelNames(_ context.Context, _ *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.store.mu.RLock()
	defer q.store.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, ser := range q.store.series {
		if !matchAll(ser.lset, matchers) {
			continue
		}
		ser.lset.Range(func(l labels.Label) {
			seen[l.Name] = struct{}{}
		})
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *sampleStoreQuerier) Close() error { return nil }

func matchAll(lset labels.Labels, matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		if !m.Matches(lset.Get(m.Name)) {
			return false
		}
	}
	return true
}

func windowSamples(samples []FloatSample, mint, maxt int64) []FloatSample {
	if len(samples) == 0 {
		return nil
	}
	lo := sort.Search(len(samples), func(i int) bool { return samples[i].T >= mint })
	hi := sort.Search(len(samples), func(i int) bool { return samples[i].T > maxt })
	if lo >= hi {
		return nil
	}
	return samples[lo:hi]
}

// chunkSample adapts a FloatSample to the chunks.Sample interface required by
// storage.NewListSeries.
type chunkSample struct {
	t int64
	v float64
}

func (s chunkSample) T() int64                            { return s.t }
func (s chunkSample) ST() int64                           { return 0 }
func (s chunkSample) F() float64                          { return s.v }
func (s chunkSample) H() *histogram.Histogram             { return nil }
func (s chunkSample) FH() *histogram.FloatHistogram       { return nil }
func (s chunkSample) Type() chunkenc.ValueType            { return chunkenc.ValFloat }
func (s chunkSample) Copy() chunks.Sample                 { return s }

func toChunkSamples(in []FloatSample) []chunks.Sample {
	out := make([]chunks.Sample, len(in))
	for i, s := range in {
		out[i] = chunkSample{t: s.T, v: s.V}
	}
	return out
}

// sliceSeriesSet is a trivial storage.SeriesSet backed by a slice.
type sliceSeriesSet struct {
	series []storage.Series
	idx    int
}

func (s *sliceSeriesSet) Next() bool {
	s.idx++
	return s.idx < len(s.series)
}

func (s *sliceSeriesSet) At() storage.Series {
	return s.series[s.idx]
}

func (s *sliceSeriesSet) Err() error { return nil }

func (s *sliceSeriesSet) Warnings() annotations.Annotations { return nil }
