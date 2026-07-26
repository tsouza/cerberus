package seed

// The reference side of the diff is rendered from the SAME in-memory fixture
// the ClickHouse side is written from — these functions only re-shape it into
// each backend's wire vocabulary. Nothing here re-derives a value, a timestamp
// or a label: a re-derivation is exactly how the two sides of a parity diff
// stop being comparable by construction.

// PromSeries renders every fixture metric series in the reference-Prometheus
// shape, including the classic histogram's `_bucket` / `_count` / `_sum`
// expansion.
func (f Fixture) PromSeries() []Series {
	out := make([]Series, 0, len(f.Gauge)+len(f.Counter)+len(f.Histogram)*(len(histogramBucketWeights)+2))
	for _, s := range f.Gauge {
		out = append(out, s.PromSeries())
	}
	for _, s := range f.Counter {
		out = append(out, s.PromSeries())
	}
	for _, h := range f.Histogram {
		out = append(out, h.PromSeries()...)
	}
	return out
}

// preIngestValue is the constant sample value every incumbent-only history
// sample carries. The pre-ingest history exists to prove the incumbent still
// ANSWERS at an instant ClickHouse cannot serve, not to exercise sample
// arithmetic, so its value carries no meaning beyond "present" — the same
// posture churnSeriesValue takes for the churn dimension.
const preIngestValue = 1.0

// PromPreIngestSeries renders the history the INCUMBENT alone holds, from
// strictly before ClickHouse's ingest-start, in the reference-Prometheus
// shape. It is deliberately asymmetric: no ClickHouse writer consumes it,
// because that asymmetry IS the migration fact — an operator who has just
// pointed ingest at ClickHouse still needs the old read path for anything
// older than the moment ingest started.
//
// It re-uses the gauge series' own label sets, so it adds no series to either
// side's label surface: a cardinality inventory, a label-mapping diff or a
// head-series count sees exactly what it saw before. Only the samples are
// new, and every one of them sits before the first instant either the parity
// lane or a corpus range selector can reach.
func (f Fixture) PromPreIngestSeries() []Series {
	times := f.Window.PreIngestSampleTimes()
	out := make([]Series, 0, len(f.Gauge))
	for _, s := range f.Gauge {
		samples := make([]Sample, 0, len(times))
		for _, t := range times {
			samples = append(samples, Sample{Time: t, Value: preIngestValue})
		}
		out = append(out, Series{Labels: s.PromSeries().Labels, Samples: samples})
	}
	return out
}

// LokiStreams renders the fixture's log streams in the Loki push shape. The
// stream label set is the same map the ClickHouse side writes into
// ResourceAttributes, and structured metadata is restricted to detected_level
// on both sides.
func (f Fixture) LokiStreams() []Stream {
	out := make([]Stream, 0, len(f.LogStreams))
	for _, s := range f.LogStreams {
		entries := make([]Entry, 0, len(s.Records))
		for _, r := range s.Records {
			entries = append(entries, Entry{
				Time:               r.Time,
				Line:               r.Line,
				StructuredMetadata: map[string]string{detectedLevelKey: r.Level},
			})
		}
		out = append(out, Stream{Labels: s.Labels, Entries: entries})
	}
	return out
}

// PromSeriesCount is the number of reference-Prometheus series the fixture
// expands to. It is a derived count rather than a pinned literal, so a change
// to the declared cardinality moves it and the manifest together.
func (f Fixture) PromSeriesCount() int {
	return len(f.PromSeries())
}

// LogRecordCount is the total number of fixture log records across every
// stream.
func (f Fixture) LogRecordCount() int {
	var n int
	for _, s := range f.LogStreams {
		n += len(s.Records)
	}
	return n
}
