package chclient

import (
	"testing"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
)

// columnar_histogram_test.go — the columnar decode of the
// histogram-VALUED matrix shape (issue #1967).
//
// Before this, the columnar path bound four columns and returned
// errMatrixMismatch for the thirteen-column histogram projection. That
// was not a slow path but a DOUBLE one: columnarDecoder.decode responds
// to a decline by re-dispatching the identical query to ClickHouse under
// a fresh query_id, so every histogram-valued query ran twice.

// histogramShapedResults builds a chproto.Results matching matrixColumns
// followed by histogramMatrixColumns — the shape a
// chplan.HistogramProjection publishes — with the types chsql's emitter
// pins each output to.
func histogramShapedResults() chproto.Results {
	res := matrixShapedResults()
	// matrixShapedResults exists for the bind-only shape tests, where a
	// zero-valued ColMap is enough. Appending rows needs its key/value
	// sub-columns actually constructed.
	res[1].Data = chproto.NewMap[string, string](new(chproto.ColStr), new(chproto.ColStr))
	return append(
		res,
		chproto.ResultColumn{Name: "HistogramCount", Data: &chproto.ColFloat64{}},
		chproto.ResultColumn{Name: "HistogramSum", Data: &chproto.ColFloat64{}},
		chproto.ResultColumn{Name: "HistogramScale", Data: &chproto.ColInt32{}},
		chproto.ResultColumn{Name: "HistogramZeroThreshold", Data: &chproto.ColFloat64{}},
		chproto.ResultColumn{Name: "HistogramZeroCount", Data: &chproto.ColFloat64{}},
		chproto.ResultColumn{Name: "HistogramPositiveOffset", Data: &chproto.ColInt32{}},
		chproto.ResultColumn{Name: "HistogramPositiveBucketCounts", Data: &chproto.ColArr[float64]{Data: &chproto.ColFloat64{}}},
		chproto.ResultColumn{Name: "HistogramNegativeOffset", Data: &chproto.ColInt32{}},
		chproto.ResultColumn{Name: "HistogramNegativeBucketCounts", Data: &chproto.ColArr[float64]{Data: &chproto.ColFloat64{}}},
	)
}

// appendHistogramRow pushes one row across all thirteen columns of a
// histogramShapedResults set.
func appendHistogramRow(res chproto.Results, name string, labels map[string]string, ts time.Time, hv HistogramValue) {
	res[0].Data.(*chproto.ColStr).Append(name)
	res[1].Data.(*chproto.ColMap[string, string]).Append(labels)
	res[2].Data.(*chproto.ColDateTime).Append(ts)
	res[3].Data.(*chproto.ColFloat64).Append(0)
	res[4].Data.(*chproto.ColFloat64).Append(hv.Count)
	res[5].Data.(*chproto.ColFloat64).Append(hv.Sum)
	res[6].Data.(*chproto.ColInt32).Append(hv.Scale)
	res[7].Data.(*chproto.ColFloat64).Append(hv.ZeroThreshold)
	res[8].Data.(*chproto.ColFloat64).Append(hv.ZeroCount)
	res[9].Data.(*chproto.ColInt32).Append(hv.PositiveOffset)
	res[10].Data.(*chproto.ColArr[float64]).Append(hv.PositiveBucketCounts)
	res[11].Data.(*chproto.ColInt32).Append(hv.NegativeOffset)
	res[12].Data.(*chproto.ColArr[float64]).Append(hv.NegativeBucketCounts)
}

// TestBindColumns_HistogramShapeBinds pins that the thirteen-column
// histogram projection is accepted rather than declined — the decline is
// what forced the whole query to be re-executed on the row path.
func TestBindColumns_HistogramShapeBinds(t *testing.T) {
	cur := &columnarCursor{results: histogramShapedResults(), responseShape: ResponseShapeMatrix}
	cols, ok := cur.bindColumns()
	if !ok {
		t.Fatal("bindColumns declined the histogram-valued matrix shape — the columnar path would re-dispatch the query to ClickHouse in full")
	}
	if cols.hist == nil {
		t.Fatal("bindColumns accepted the thirteen-column shape without binding the histogram columns")
	}
}

// TestBindColumns_BaseShapeBindsWithoutHistogram pins that widening the
// binder did not change the four-column shape: it still binds, and it
// carries no histogram.
func TestBindColumns_BaseShapeBindsWithoutHistogram(t *testing.T) {
	cur := &columnarCursor{results: matrixShapedResults(), responseShape: ResponseShapeMatrix}
	cols, ok := cur.bindColumns()
	if !ok {
		t.Fatal("bindColumns declined the plain four-column matrix shape")
	}
	if cols.hist != nil {
		t.Error("bindColumns bound histogram columns for a four-column result")
	}
}

// TestBindColumns_HistogramShapeRejectsWrongNames pins that the widened
// binder still tests names, not just the column COUNT: a thirteen-column
// projection that is not the histogram one must fall back rather than
// decode nine unrelated columns as a histogram.
func TestBindColumns_HistogramShapeRejectsWrongNames(t *testing.T) {
	res := histogramShapedResults()
	res[12].Name = "SomethingElse"
	cur := &columnarCursor{results: res, responseShape: ResponseShapeMatrix}
	if _, ok := cur.bindColumns(); ok {
		t.Fatal("bindColumns accepted a thirteen-column result whose trailing column is not a histogram column")
	}
}

// TestBindColumns_HistogramShapeRejectsWrongTypes pins the type half of
// the same contract: the count columns must be the Float64 the emitter
// pins them to. An un-pinned (UInt64) projection is declined rather than
// mis-bound.
func TestBindColumns_HistogramShapeRejectsWrongTypes(t *testing.T) {
	res := histogramShapedResults()
	res[4].Data = &chproto.ColUInt64{}
	cur := &columnarCursor{results: res, responseShape: ResponseShapeMatrix}
	if _, ok := cur.bindColumns(); ok {
		t.Fatal("bindColumns accepted a UInt64 HistogramCount — the emitter pins that column to Float64")
	}
}

// TestColumnarCursor_DecodesHistogramRows is the value-level assertion:
// two rows of one series decode into Samples whose Histogram carries every
// field, with the bucket ladders copied rather than aliased onto the
// block's storage.
func TestColumnarCursor_DecodesHistogramRows(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	labels := map[string]string{"service": "api"}
	want := []HistogramValue{
		{
			Count: 0.1, Sum: 0.05, Scale: 0, ZeroThreshold: 1e-128, ZeroCount: 0.25,
			PositiveOffset: 3, PositiveBucketCounts: []float64{0.05, 0.05},
			NegativeOffset: -2, NegativeBucketCounts: []float64{2.5},
		},
		{
			Count: 0.2, Sum: 0.10, Scale: -1, ZeroThreshold: 1e-128, ZeroCount: 0,
			PositiveOffset: 4, PositiveBucketCounts: []float64{0.1},
			NegativeOffset: 0, NegativeBucketCounts: nil,
		},
	}

	res := histogramShapedResults()
	for i, hv := range want {
		appendHistogramRow(res, "latency_exp_hist", labels, ts.Add(time.Duration(i)*time.Second), hv)
	}

	cur := &columnarCursor{results: res, responseShape: ResponseShapeMatrix}
	cols, ok := cur.bindColumns()
	if !ok {
		t.Fatal("bindColumns declined the histogram-valued matrix shape")
	}
	if err := cur.decodeBlock(cols, len(want)); err != nil {
		t.Fatalf("decodeBlock: %v", err)
	}
	if len(cur.samples) != len(want) {
		t.Fatalf("decoded %d samples, want %d", len(cur.samples), len(want))
	}

	for i, sample := range cur.samples {
		if sample.Histogram == nil {
			t.Fatalf("sample %d carries no Histogram", i)
		}
		assertHistogramEqual(t, i, *sample.Histogram, want[i])
	}

	// Distinct rows must own distinct HistogramValues: a shared pointer
	// would make every point in a series report the last row's buckets.
	if cur.samples[0].Histogram == cur.samples[1].Histogram {
		t.Error("both samples share one *HistogramValue")
	}

	// The ladders must be COPIES, not windows onto the block's own
	// storage — the block is reset when the next one decodes over it, so
	// an aliased slice would report the wrong buckets (or racy garbage)
	// on any multi-block result. ch-go's ColArr.Row appends into a fresh
	// slice, and this pins that we keep relying on it: mutating one
	// decoded ladder must not disturb the column or the sibling row.
	first := cur.samples[0].Histogram.PositiveBucketCounts
	if len(first) == 0 {
		t.Fatal("fixture row 0 has no positive buckets to test aliasing with")
	}
	sentinel := first[0] + 1
	first[0] = sentinel
	if got := cols.hist.posBuckets.Row(0); got[0] == sentinel {
		t.Error("decoded PositiveBucketCounts aliases the block's storage")
	}
}

// assertHistogramEqual compares a decoded histogram field by field.
func assertHistogramEqual(t *testing.T, row int, got, want HistogramValue) {
	t.Helper()
	if got.Count != want.Count || got.Sum != want.Sum || got.Scale != want.Scale ||
		got.ZeroThreshold != want.ZeroThreshold || got.ZeroCount != want.ZeroCount ||
		got.PositiveOffset != want.PositiveOffset || got.NegativeOffset != want.NegativeOffset {
		t.Errorf("row %d scalar fields: got %+v, want %+v", row, got, want)
	}
	assertFloatsEqual(t, row, "PositiveBucketCounts", got.PositiveBucketCounts, want.PositiveBucketCounts)
	assertFloatsEqual(t, row, "NegativeBucketCounts", got.NegativeBucketCounts, want.NegativeBucketCounts)
}

func assertFloatsEqual(t *testing.T, row int, field string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("row %d %s: got %v, want %v", row, field, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d %s[%d]: got %v, want %v", row, field, i, got[i], want[i])
		}
	}
}
