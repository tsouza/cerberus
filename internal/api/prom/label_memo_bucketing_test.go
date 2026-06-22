package prom

import (
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// seriesIDCursor is a minimal Cursor over a slice of pre-built Samples that
// stamps each row a SeriesID exactly the way a real cursor's interning does:
// a 1-based ordinal per distinct CanonicalKey(labels), stable across rows,
// presenting ONE consistent SeriesID namespace for the whole drain. Each row
// keeps its OWN map instance (the real CH driver allocates a fresh map per
// Scan), so interning dedups by content, not by pointer.
//
// This is the contract the chclient rowsCursor AND the solver composed
// shardCursor both uphold; the label memo relies on it.
type seriesIDCursor struct {
	rows     []chclient.Sample
	idx      int
	interned map[string]uint32
	seq      uint32
}

func (c *seriesIDCursor) Next() bool {
	if c.idx >= len(c.rows) {
		return false
	}
	c.idx++
	return true
}

func (c *seriesIDCursor) Sample() chclient.Sample {
	s := c.rows[c.idx-1]
	if c.interned == nil {
		c.interned = map[string]uint32{}
	}
	k := format.CanonicalKey(s.Labels) // interning ignores MetricName
	id, ok := c.interned[k]
	if !ok {
		c.seq++
		id = c.seq
		c.interned[k] = id
	}
	s.SeriesID = id
	return s
}

func (c *seriesIDCursor) Err() error       { return nil }
func (c *seriesIDCursor) Close() error     { return nil }
func (c *seriesIDCursor) Inspected() int64 { return int64(c.idx) }

// TestMatrixFromCursor_MemoPreservesBucketing pins that matrixFromCursor with
// the label memo produces exactly the matrix the un-memoised
// `NormalizeLabelMap(WithMetricName(...))`-per-row path would: two distinct
// series of the same metric, samples interleaved per timestamp (the realistic
// CH row order), must bucket by their OWN labels — never blend one series'
// values into the other. This is the prom-side guard for the rc.5 memo class
// (the solver-side cross-shard SeriesID guard lives in
// TestExecute_CrossShardSeriesIDBijective).
func TestMatrixFromCursor_MemoPreservesBucketing(t *testing.T) {
	start := time.Unix(0, 0).UTC()
	end := start.Add(100 * time.Minute)
	step := time.Minute

	var rows []chclient.Sample
	for i := 0; i < 100; i++ {
		ts := start.Add(time.Duration(i) * time.Minute)
		rows = append(
			rows,
			chclient.Sample{
				MetricName: "demo_memory_usage_bytes",
				Labels:     map[string]string{"instance": "a", "type": "free"},
				Timestamp:  ts,
				Value:      float64(2_149_000_000 + i),
			},
			chclient.Sample{
				MetricName: "demo_memory_usage_bytes",
				Labels:     map[string]string{"instance": "a", "type": "used"},
				Timestamp:  ts,
				Value:      float64(2_169_000_000 + i),
			},
		)
	}

	got, err := matrixFromCursor(&seriesIDCursor{rows: rows}, start, end, step)
	if err != nil {
		t.Fatalf("matrixFromCursor: %v", err)
	}

	// Reference: bucket without the memo (fresh normalise per row).
	type ref struct{ vals []string }
	refBy := map[string]*ref{}
	for _, s := range rows {
		labels := format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))
		k := format.CanonicalKey(labels)
		r := refBy[k]
		if r == nil {
			r = &ref{}
			refBy[k] = r
		}
		r.vals = append(r.vals, strconv.FormatFloat(s.Value, 'f', -1, 64))
	}

	if len(got) != len(refBy) {
		t.Fatalf("got %d series, want %d", len(got), len(refBy))
	}
	for _, ms := range got {
		k := format.CanonicalKey(ms.Metric)
		r := refBy[k]
		if r == nil {
			t.Fatalf("series %v not in reference", ms.Metric)
		}
		if len(ms.Values) != len(r.vals) {
			t.Fatalf("series %v: got %d values, want %d", ms.Metric, len(ms.Values), len(r.vals))
		}
		for i, sv := range ms.Values {
			if got := sv[1].(string); got != r.vals[i] {
				t.Fatalf("series %v value[%d]: got %s want %s (memo blended a foreign series)",
					ms.Metric, i, got, r.vals[i])
			}
		}
	}
}
