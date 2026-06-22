package prom

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// BenchmarkMatrixFromCursor_InternedRows measures the range-query matrix
// pivot over a PRODUCTION-shaped cursor stream: each series carries one
// shared (interned) Labels map across all its rows, stamped with a stable
// SeriesID — exactly what internal/chclient's rowsCursor produces. This is
// the shape the labelMemo optimisation targets: K rows per series should
// normalise the wire label set ONCE, not K times.
//
// The label set mirrors the rc.5 ResourceAttributes-as-Prom-labels output:
// the per-series map carries the promoted resource keys (k8s_*, service_*,
// cloud_*) on top of the metric attributes, so each normalisation rebuilds
// a fat map — the cost the memo collapses from O(rows) to O(series).
func benchInternedMatrixSamples(series, rowsPerSeries int) []chclient.Sample {
	base := time.Unix(1778457600, 0).UTC()
	out := make([]chclient.Sample, 0, series*rowsPerSeries)
	for s := 0; s < series; s++ {
		// One shared map instance per series — the interning contract.
		lset := map[string]string{
			"route":               fmt.Sprintf("/api/%d", s),
			"method":              "GET",
			"status_code":         "200",
			"k8s_namespace_name":  fmt.Sprintf("ns-%d", s%8),
			"k8s_pod_name":        fmt.Sprintf("pod-%d", s),
			"k8s_node_name":       fmt.Sprintf("node-%d", s%16),
			"k8s_deployment_name": fmt.Sprintf("dep-%d", s%8),
			"service_instance_id": fmt.Sprintf("inst-%d", s),
			"cloud_region":        fmt.Sprintf("us-east-%d", s%4),
		}
		for r := 0; r < rowsPerSeries; r++ {
			out = append(out, chclient.Sample{
				MetricName: "http_requests_total",
				Labels:     lset, // shared instance, as the cursor interns it
				SeriesID:   uint32(s + 1),
				Timestamp:  base.Add(time.Duration(r) * 30 * time.Second),
				Value:      float64(r),
			})
		}
	}
	return out
}

// benchSliceCursor replays a pre-built sample slice through the Cursor
// interface, preserving each sample's SeriesID (so the memo fast path
// fires). zeroSeriesID forces every SeriesID to 0, modelling the
// pre-optimisation (non-interned) per-row normalisation for the baseline
// arm.
type benchSliceCursor struct {
	samples      []chclient.Sample
	idx          int
	zeroSeriesID bool
}

func (c *benchSliceCursor) Next() bool { c.idx++; return c.idx < len(c.samples) }
func (c *benchSliceCursor) Sample() chclient.Sample {
	s := c.samples[c.idx]
	if c.zeroSeriesID {
		s.SeriesID = 0
	}
	return s
}
func (c *benchSliceCursor) Err() error   { return nil }
func (c *benchSliceCursor) Close() error { return nil }
func (c *benchSliceCursor) Inspected() int64 {
	if c.idx > len(c.samples) {
		return int64(len(c.samples))
	}
	return int64(c.idx)
}

func BenchmarkMatrixFromCursor_Memoised(b *testing.B) {
	samples := benchInternedMatrixSamples(50, 60)
	start := time.Unix(1778457600, 0).UTC().Add(-time.Hour)
	end := start.Add(4 * time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cur := &benchSliceCursor{samples: samples, idx: -1}
		if _, err := matrixFromCursor(cur, start, end, 30*time.Second); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMatrixFromCursor_PerRow(b *testing.B) {
	samples := benchInternedMatrixSamples(50, 60)
	start := time.Unix(1778457600, 0).UTC().Add(-time.Hour)
	end := start.Add(4 * time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cur := &benchSliceCursor{samples: samples, idx: -1, zeroSeriesID: true}
		if _, err := matrixFromCursor(cur, start, end, 30*time.Second); err != nil {
			b.Fatal(err)
		}
	}
}
