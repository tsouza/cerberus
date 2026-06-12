package prom

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMatrixFromCursor_SortsSeriesByCanonicalKey pins the deterministic,
// canonical-label-sorted output order of the range-query matrix pivot.
//
// Reference Prometheus returns query_range series sorted by labels, and
// the prometheus/compliance differential tester compares the two
// `model.Matrix` results ORDER-SENSITIVELY. matrixFromCursor previously
// emitted series in Go-map iteration order (non-deterministic), so a
// label-only selector whose matched series have varying label counts
// (e.g. `{job="demo", __name__!~"..."}` mixes `up{instance,job}` with
// `demo_disk_total_bytes{device,instance,job}`) returned an unsorted
// matrix that diffed against reference even though every series + sample
// was identical. This guard feeds samples in a deliberately jumbled,
// label-count-varying order and asserts the output is canonical-key
// sorted — matching the instant sibling matrixFromSamples and Prom's
// API contract.
func TestMatrixFromCursor_SortsSeriesByCanonicalKey(t *testing.T) {
	t.Parallel()
	ts := time.Unix(1778457600, 0).UTC()
	// Deliberately jumbled input order, mixing 2-label and 3-label series
	// (the label-count variance that tripped the upstream tester's sort).
	samples := []chclient.Sample{
		{MetricName: "demo_num_cpus", Labels: map[string]string{"instance": "h2", "job": "demo"}, Timestamp: ts, Value: 4},
		{MetricName: "demo_disk_total_bytes", Labels: map[string]string{"device": "sda1", "instance": "h1", "job": "demo"}, Timestamp: ts, Value: 100},
		{MetricName: "up", Labels: map[string]string{"instance": "h1", "job": "demo"}, Timestamp: ts, Value: 1},
		{MetricName: "demo_num_cpus", Labels: map[string]string{"instance": "h1", "job": "demo"}, Timestamp: ts, Value: 4},
		{MetricName: "demo_disk_total_bytes", Labels: map[string]string{"device": "sda1", "instance": "h0", "job": "demo"}, Timestamp: ts, Value: 100},
		{MetricName: "up", Labels: map[string]string{"instance": "h0", "job": "demo"}, Timestamp: ts, Value: 1},
	}

	out, err := matrixFromCursor(&orderTestCursor{samples: samples, idx: -1}, ts.Add(-time.Hour), ts.Add(time.Hour), 10*time.Second)
	if err != nil {
		t.Fatalf("matrixFromCursor: %v", err)
	}
	if len(out) != len(samples) {
		t.Fatalf("expected %d series, got %d", len(samples), len(out))
	}

	// The output order must be non-decreasing by the SAME canonical key
	// the pivot groups + sorts on (format.CanonicalKey over the
	// normalised label map). The emitted ms.Metric already carries the
	// normalised labels incl. __name__, so CanonicalKey of it reproduces
	// the sort key.
	keys := make([]string, len(out))
	for i, ms := range out {
		keys[i] = format.CanonicalKey(ms.Metric)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Errorf("matrix series not sorted: series[%d]=%q > series[%d]=%q\nfull order: %v",
				i-1, keys[i-1], i, keys[i], keys)
		}
	}

	// Determinism: a second pivot over the reversed input yields the
	// identical order.
	out2, err := matrixFromCursor(&orderTestCursor{samples: reverseSamples(samples), idx: -1}, ts.Add(-time.Hour), ts.Add(time.Hour), 10*time.Second)
	if err != nil {
		t.Fatalf("matrixFromCursor (run 2): %v", err)
	}
	for i := range out {
		if format.CanonicalKey(out[i].Metric) != format.CanonicalKey(out2[i].Metric) {
			t.Fatalf("non-deterministic order at %d: %v vs %v", i, out[i].Metric, out2[i].Metric)
		}
	}
}

func reverseSamples(in []chclient.Sample) []chclient.Sample {
	out := make([]chclient.Sample, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// orderTestCursor is a minimal in-package chclient.Cursor over a fixed
// sample slice (handler_test.go's sliceCursor lives in the external
// prom_test package, out of reach of this internal-package test that
// must call the unexported matrixFromCursor).
type orderTestCursor struct {
	samples []chclient.Sample
	idx     int
}

func (c *orderTestCursor) Next() bool {
	c.idx++
	return c.idx < len(c.samples)
}
func (c *orderTestCursor) Sample() chclient.Sample { return c.samples[c.idx] }
func (c *orderTestCursor) Err() error              { return nil }
func (c *orderTestCursor) Close() error            { return nil }
