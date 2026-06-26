package promql

import (
	"sort"
	"strings"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

// DefaultLookbackDelta is the default sample-staleness window the
// instant selector uses to decide whether the latest sample is "fresh
// enough" to surface for an eval timestamp T. Matches Prometheus's
// default (5min).
const DefaultLookbackDelta = 5 * time.Minute

// Sample is one (timestamp, value) point. Timestamp is unix
// milliseconds, matching Prom convention and what the dataset
// generator emits.
type Sample struct {
	T int64
	V float64
}

// Series is one labeled time series in the in-memory model. The
// label map is canonical: __name__ is folded in alongside the
// user-defined labels so matchers (which may target __name__
// explicitly) see a single uniform view.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// Name returns the series's metric name (the value of the __name__
// label), or empty if none was set.
func (s *Series) Name() string {
	return s.Labels[MetricNameLabel]
}

// SeriesKey returns the stable string key for the series identity —
// i.e. its label set MINUS __name__, in canonical sorted form. This is
// the "series identity" the aggregation and binary-matching paths
// group/join on, mirroring Prom's identity convention.
func (s *Series) SeriesKey() string {
	return labelKeyExcluding(s.Labels, MetricNameLabel)
}

// MetricNameLabel is the Prometheus convention for the synthetic
// label that carries the metric name. Defined locally so this
// package doesn't pull in prometheus/model/labels just for a string
// constant.
const MetricNameLabel = "__name__"

// Model is the in-memory dataset the evaluator reads. It's built from
// a property.MetricsModel via [FromDataset]; the resulting Series
// slice is sorted by SeriesKey for deterministic iteration. Samples
// inside each series are sorted by timestamp.
type Model struct {
	// Series holds every series in the dataset. The slice is the
	// canonical iteration order: range over Series rather than over a
	// map so series matching produces a deterministic order.
	Series []*Series
}

// FromDataset builds the in-memory Model from a property.Dataset's
// MetricsModel. The MetricsModel's __name__ is implicit (stored as
// SeriesData.MetricName); FromDataset folds it back into the label
// map so matchers can see it.
func FromDataset(d property.Dataset) *Model {
	if d.Metrics == nil {
		return &Model{}
	}
	out := make([]*Series, 0, len(d.Metrics.Series))
	for _, s := range d.Metrics.Series {
		lbls := make(map[string]string, len(s.Labels)+1)
		for k, v := range s.Labels {
			lbls[k] = v
		}
		lbls[MetricNameLabel] = s.MetricName

		samples := make([]Sample, 0, len(s.Points))
		for _, p := range s.Points {
			samples = append(samples, Sample{T: p.TimestampMs, V: p.Value})
		}
		// Sort by (timestamp asc, value asc) so dedupSamplesByTimestamp's
		// last-of-equal-ts-run is the max-valued sample at that timestamp.
		sort.Slice(samples, func(i, j int) bool {
			if samples[i].T != samples[j].T {
				return samples[i].T < samples[j].T
			}
			return samples[i].V < samples[j].V
		})
		out = append(out, &Series{Labels: lbls, Samples: dedupSamplesByTimestamp(samples)})
	}
	sort.Slice(out, func(i, j int) bool {
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return &Model{Series: out}
}

// dedupSamplesByTimestamp collapses a (timestamp asc, value asc)-sorted
// sample slice to one sample per distinct timestamp, keeping the LAST
// sample of each equal-timestamp run — i.e. the max-valued sample at that
// timestamp.
//
// This models Prometheus's single-sample-per-timestamp invariant: a
// Prometheus series carries exactly one sample at any given timestamp, so
// the from-scratch oracle must too. OTel/ClickHouse ingestion, by
// contrast, can write two rows sharing an (Attributes, TimeUnix); the
// dataset mirror faithfully carries that duplicate so the DDL reproduces
// it, but the oracle's Prometheus-correct view of the series must drop it.
// Keeping the max value matches BOTH cerberus rate-family paths on the
// same duplicate data: the row path's arraySort+dedup keeps the
// sorted-last (max) tuple, and the native timeSeriesRateToGrid aggregate
// makes the same insertion-order-independent choice. Production
// duplicates are same-valued, so only the sample COUNT actually differs —
// the bug class this dedup pins is the inflated count that shrinks the
// extrapolation interval.
//
// For every dataset whose series already carry unique timestamps (the
// random generator's 15s-spaced points), this is a no-op.
func dedupSamplesByTimestamp(in []Sample) []Sample {
	if len(in) < 2 {
		return in
	}
	out := make([]Sample, 0, len(in))
	for i, s := range in {
		if i > 0 && s.T == in[i-1].T {
			// Same timestamp as the previous (already appended) sample;
			// the input is value-ascending within an equal-ts run, so this
			// sample's value is >= the one we appended. Keep the max.
			out[len(out)-1] = s
			continue
		}
		out = append(out, s)
	}
	return out
}

// CopyLabels returns a shallow copy of the input label map. The
// evaluator copies labels every time it produces an output row so
// callers can mutate without aliasing back into the dataset.
func CopyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// DropLabel returns a new label map without the given key.
// Convenience for the __name__-stripping every aggregation does.
func DropLabel(in map[string]string, key string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}

// KeepLabels returns a new label map keeping only the keys in keep.
// Used by aggregations' `by(...)` grouping path.
func KeepLabels(in map[string]string, keep []string) map[string]string {
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	out := make(map[string]string, len(keep))
	for k, v := range in {
		if _, ok := keepSet[k]; ok {
			out[k] = v
		}
	}
	return out
}

// DropLabels returns a new label map without any of the keys in
// drop. Used by aggregations' `without(...)` grouping path; the
// __name__ label is always dropped as well, per Prom convention.
func DropLabels(in map[string]string, drop []string) map[string]string {
	dropSet := map[string]struct{}{MetricNameLabel: {}}
	for _, k := range drop {
		dropSet[k] = struct{}{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if _, ok := dropSet[k]; ok {
			continue
		}
		out[k] = v
	}
	return out
}

// labelKey is the canonical sorted-form string key for a label set.
// Used both for series identity (with __name__ dropped) and for
// model-level ordering.
func labelKey(m map[string]string) string {
	return labelKeyExcluding(m, "")
}

// labelKeyExcluding is labelKey, but drops the named key first.
func labelKeyExcluding(m map[string]string, exclude string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == exclude {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString("=\"")
		b.WriteString(m[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
