package wire

import (
	"encoding/json"
	"testing"
)

func TestDecodeVectorHistogram(t *testing.T) {
	raw := json.RawMessage(`[{"metric":{"__name__":"latency_exp_hist","job":"api"},"histogram":[1717171717.5,{"count":"3.5","sum":"7","buckets":[[1,"-4","-2","1.5"],[0,"1","2","2"]]}]}]`)

	out := decodeVector(raw)
	if out.Err != nil {
		t.Fatalf("decodeVector: %v", out.Err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Rows))
	}
	row := out.Rows[0]
	if row.TimestampMs != 1717171717500 || row.Labels["job"] != "api" {
		t.Errorf("row identity = %+v @ %d", row.Labels, row.TimestampMs)
	}
	if _, ok := row.Labels["__name__"]; ok {
		t.Errorf("row labels retain __name__: %v", row.Labels)
	}
	if row.Histogram == nil {
		t.Fatal("Histogram is nil")
	}
	if row.Histogram.Count != 3.5 || row.Histogram.Sum != 7 || len(row.Histogram.Buckets) != 2 {
		t.Fatalf("Histogram = %+v", row.Histogram)
	}
	if got := row.Histogram.Buckets[0]; got.Boundaries != 1 || got.Lower != -4 || got.Upper != -2 || got.Count != 1.5 {
		t.Errorf("first bucket = %+v", got)
	}
}
