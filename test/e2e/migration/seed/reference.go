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
