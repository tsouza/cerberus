package main

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// The mirror's whole job is to put the SAME series into Prometheus that
// ClickHouse holds, so the label shape it builds is the comparison itself.
// A label set that sorts differently, or an extra label the OTel side never
// carries, hard-diffs every series for a reason the seeder invented.
func TestBuildPromLabelsPutsNameFirstThenSortsTheRest(t *testing.T) {
	t.Parallel()

	got := buildPromLabels("http_requests_total", map[string]string{"method": "GET", "code": "200"})
	want := []struct{ name, value string }{
		{"__name__", "http_requests_total"},
		{"code", "200"},
		{"method", "GET"},
	}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Value != w.value {
			t.Fatalf("label %d = %s=%s, want %s=%s", i, got[i].Name, got[i].Value, w.name, w.value)
		}
	}

	if bare := buildPromLabels("up", nil); len(bare) != 1 || bare[0].Name != "__name__" {
		t.Fatalf("an attribute-free series = %v, want just __name__", bare)
	}
}

// canonicaliseLabels is the grouping key: two rows of the same series must
// produce the same string whatever order the map iterates in.
func TestCanonicaliseLabelsIsOrderIndependent(t *testing.T) {
	t.Parallel()

	a := canonicaliseLabels(map[string]string{"code": "200", "method": "GET"})
	b := canonicaliseLabels(map[string]string{"method": "GET", "code": "200"})
	if a != b {
		t.Fatalf("the same series keyed two ways: %q != %q", a, b)
	}
	if a != "code=200;method=GET;" {
		t.Fatalf("key = %q, want %q", a, "code=200;method=GET;")
	}
	if got := canonicaliseLabels(nil); got != "" {
		t.Fatalf("an empty label set keyed to %q, want the empty string", got)
	}
	// Two different series must not share a key — grouping by a colliding
	// key would fold two distributions into one mirrored series.
	if canonicaliseLabels(map[string]string{"code": "200"}) == canonicaliseLabels(map[string]string{"code": "500"}) {
		t.Fatal("two label sets differing in a value share a grouping key")
	}
}

// The bucket counts ClickHouse holds are cumulative-free absolute counts;
// the Prom wire form is delta-encoded and its span offset sits one bucket
// ahead of the OTel index. Getting either wrong mirrors a different
// distribution than ClickHouse holds while still producing a valid series.
func TestOtelBucketsToPromSpansDeltaEncodesAndShiftsTheOffset(t *testing.T) {
	t.Parallel()

	spans, deltas, err := otelBucketsToPromSpans(2, []uint64{1, 3, 2})
	if err != nil {
		t.Fatalf("otelBucketsToPromSpans: %v", err)
	}
	if len(spans) != 1 || spans[0].Offset != 3 || spans[0].Length != 3 {
		t.Fatalf("spans = %v, want one span at offset 3 (2 + the one-bucket shift) of length 3", spans)
	}
	if len(deltas) != 3 || deltas[0] != 1 || deltas[1] != 2 || deltas[2] != -1 {
		t.Fatalf("deltas = %v, want [1 2 -1]", deltas)
	}

	// No buckets is not an empty span — it is no span at all.
	spans, deltas, err = otelBucketsToPromSpans(0, nil)
	if err != nil || spans != nil || deltas != nil {
		t.Fatalf("empty buckets = (%v, %v, %v), want all nil", spans, deltas, err)
	}
}

// histogramWireNames is classified with the production router rather than a
// suffix list precisely so a lowering change that adds a fourth companion
// fails here. This pins that the three it does resolve are the three
// distinct families of one base metric.
func TestHistogramWireNamesResolvesThreeDistinctFamilies(t *testing.T) {
	t.Parallel()

	const base = "http_server_duration"
	bucket, count, sum, err := histogramWireNames(base, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("histogramWireNames(%q): %v", base, err)
	}
	for _, got := range []string{bucket, count, sum} {
		if !strings.HasPrefix(got, base) {
			t.Fatalf("wire name %q does not belong to base %q", got, base)
		}
	}
	if bucket == count || bucket == sum || count == sum {
		t.Fatalf("wire names collide: bucket=%q count=%q sum=%q", bucket, count, sum)
	}
}
