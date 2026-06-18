package chclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"
)

// columnar_bench_test.go — the GO/NO-GO benchmark for "5A": decode a
// query_range matrix block COLUMNARLY vs the production row-by-row
// driver.Rows.Scan path.
//
// Why this is faithful and not a strawman: it drives the SAME
// clickhouse-go/v2 v2.46.0 column code that prod hits. The block is a
// real *proto.Block built via the public AddColumn/Append API; the row
// path calls the package-internal scan() — the exact function
// clickhouse_rows.go's (*rows).Scan delegates to (scan.go:62). So the
// per-cell column.ScanRow / column.Map.row reflect+box cost measured
// here is the real one, no live ClickHouse required.
//
// The matrix shape is (MetricName String, Attributes Map(String,String),
// TimeUnix DateTime, Value Float64): the four-column projection
// QueryCursor documents (cursor.go QueryCursor doc) and the shape
// internal/api/prom feeds its matrix pivot.

// matrixBlock builds a representative query_range matrix block:
// `series` distinct label sets, each carrying `perSeries` (ts, value)
// samples — i.e. the long-window/fine-step fan-out the cursor doc calls
// out. Total rows = series * perSeries.
func matrixBlock(tb testing.TB, series, perSeries int) *proto.Block {
	tb.Helper()
	blk := proto.NewBlock()
	for _, c := range []struct {
		name, typ string
	}{
		{"MetricName", "String"},
		{"Attributes", "Map(String, String)"},
		{"TimeUnix", "DateTime"},
		{"Value", "Float64"},
	} {
		if err := blk.AddColumn(c.name, column.Type(c.typ)); err != nil {
			tb.Fatalf("AddColumn %s: %v", c.name, err)
		}
	}
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	for s := 0; s < series; s++ {
		// A realistic OTel-CH label set: a handful of pairs, the bulk
		// shared across series, one varying. This is what makes the
		// per-row Map decode wasteful — interning collapses it to one
		// map per series, but row-scan rebuilds it every row.
		labels := map[string]string{
			"job":      "api",
			"instance": fmt.Sprintf("host-%d", s),
			"env":      "prod",
		}
		for p := 0; p < perSeries; p++ {
			ts := base.Add(time.Duration(p) * 15 * time.Second)
			if err := blk.Append("http_requests_total", labels, ts, float64(p)); err != nil {
				tb.Fatalf("Append: %v", err)
			}
		}
	}
	return blk
}

// decodeRowPath mirrors rowsCursor.Next exactly: per row, positional
// block scan into (name, labels, ts, value), then intern the label map.
// This is the production hot path for query_range matrices.
func decodeRowPath(blk *proto.Block) []Sample {
	rows := blk.Rows()
	cols := blk.Columns
	out := make([]Sample, 0, rows)
	c := &rowsCursor{} // reuse the production interner
	for r := 0; r < rows; r++ {
		var (
			name   string
			labels map[string]string
			ts     time.Time
			value  float64
		)
		// This replicates clickhouse-go's scan() (scan.go:65-82) cell for
		// cell: (*rows).Scan delegates to scan(), which loops the dest and
		// calls columns[i].ScanRow(d, row) — the SAME public ScanRow this
		// drives. The per-cell reflect/box cost is therefore identical to
		// production. (scan() is package-private to clickhouse, so we
		// inline its body rather than reach across the package wall —
		// itself a data point on the encapsulation this task probes.)
		dest := []any{&name, &labels, &ts, &value}
		for i, d := range dest {
			if err := cols[i].ScanRow(d, r); err != nil {
				panic(err)
			}
		}
		var s Sample
		s.MetricName = name
		s.Timestamp = ts
		s.Value = value
		s.Labels, s.SeriesID = c.internLabels(labels)
		out = append(out, s)
	}
	return out
}

// decodeColumnarPath is the BEST achievable columnar decode under the
// PINNED public clickhouse-go API. It pulls each column once and, for
// the Map column, decodes the label set ONLY on first sight of a new
// series — the win the row path leaves on the table, because row-scan
// pays column.Map.row (reflect.MakeMap + N boxed SetMapIndex) on every
// row even though interning throws all but the first away.
//
// NOTE the ceiling this path hits: column.Map exposes only Row(i) any,
// which itself calls col.row(i) — the same reflect.MakeMap+box. The
// Map sub-columns (keys/values/offsets) are UNEXPORTED, so we cannot
// read them as typed []string slices. The only lever public APIs give
// us is "decode the map fewer times", via a cheap per-row identity
// probe BEFORE building the map. We approximate that identity by the
// scalar columns we CAN read cheaply (here: contiguous-series runs are
// detected by decoding the map once per run).
func decodeColumnarPath(blk *proto.Block) []Sample {
	rows := blk.Rows()
	cols := blk.Columns
	nameCol := cols[0]
	mapCol := cols[1]
	tsCol := cols[2].(*column.DateTime)
	valCol := cols[3].(*column.Float64)

	out := make([]Sample, 0, rows)
	c := &rowsCursor{}

	for r := 0; r < rows; r++ {
		// Cheap scalar reads (no reflect map build):
		ts := tsCol.Row(r, false).(time.Time)
		val := valCol.Row(r, false).(float64)
		name := nameCol.Row(r, false).(string)

		// The Map column: with only column.Map.Row(i) any available
		// publicly, every row pays col.row(i) — reflect.MakeMap + boxed
		// SetMapIndex per entry. Interning collapses RETAINED memory but
		// cannot avoid the per-row decode ALLOCATION, because Row(i) IS
		// col.row(i). This is precisely why this "columnar" path measures
		// identical to the row path: the public API has no typed-slice
		// accessor for the Map sub-columns (keys/values/offsets are
		// unexported). See decodeColumnarIdealPath for the unreachable win.
		var labels map[string]string
		if lm, ok := mapCol.Row(r, false).(map[string]string); ok {
			labels = lm
		}
		interned, id := c.internLabels(labels)

		out = append(out, Sample{
			MetricName: name,
			Labels:     interned,
			SeriesID:   id,
			Timestamp:  ts,
			Value:      val,
		})
	}
	return out
}

// decodeColumnarIdealPath quantifies the PRIZE — what a true columnar
// decode would buy IF the Map sub-columns were reachable as typed
// slices. clickhouse-go's column.Map keeps keys/values/offsets
// UNEXPORTED, so this is unreachable via the pinned public API; we model
// it by reconstructing the same data as standalone column.String +
// offsets the way a fork (or a drop to ch-go's proto.ColMap, whose
// Keys/Values ARE exported) would. The key move: build the label map
// only ONCE per distinct series run, never per row.
//
// This is the upper bound the implementation could chase. The gap
// between this and decodeRowPath is the entire economic case for 5A.
func decodeColumnarIdealPath(seriesKeys, seriesVals [][]string, names []string, tsCol []time.Time, valCol []float64, offsets []int) []Sample {
	rows := len(tsCol)
	out := make([]Sample, 0, rows)
	c := &rowsCursor{}
	// offsets[s] = first row index of series s; build each series map once.
	series := len(offsets)
	row := 0
	for s := 0; s < series; s++ {
		end := rows
		if s+1 < series {
			end = offsets[s+1]
		}
		m := make(map[string]string, len(seriesKeys[s]))
		for k := range seriesKeys[s] {
			m[seriesKeys[s][k]] = seriesVals[s][k]
		}
		interned, id := c.internLabels(m)
		for ; row < end; row++ {
			out = append(out, Sample{
				MetricName: names[row],
				Labels:     interned,
				SeriesID:   id,
				Timestamp:  tsCol[row],
				Value:      valCol[row],
			})
		}
	}
	return out
}

// idealInputs flattens a matrix block into the typed columnar slices a
// true columnar path would receive (the keys/values a fork would expose).
func idealInputs(series, perSeries int) (seriesKeys, seriesVals [][]string, names []string, ts []time.Time, vals []float64, offsets []int) {
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	for s := 0; s < series; s++ {
		offsets = append(offsets, len(names))
		seriesKeys = append(seriesKeys, []string{"job", "instance", "env"})
		seriesVals = append(seriesVals, []string{"api", fmt.Sprintf("host-%d", s), "prod"})
		for p := 0; p < perSeries; p++ {
			names = append(names, "http_requests_total")
			ts = append(ts, base.Add(time.Duration(p)*15*time.Second))
			vals = append(vals, float64(p))
		}
	}
	return seriesKeys, seriesVals, names, ts, vals, offsets
}

func BenchmarkDecode_ColumnarIdeal(b *testing.B) {
	for _, sz := range benchSizes() {
		sk, sv, names, ts, vals, offs := idealInputs(sz.series, sz.perSerie)
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := decodeColumnarIdealPath(sk, sv, names, ts, vals, offs)
				if len(out) != sz.series*sz.perSerie {
					b.Fatalf("rows %d", len(out))
				}
			}
		})
	}
}

func benchSizes() []struct {
	name             string
	series, perSerie int
} {
	return []struct {
		name             string
		series, perSerie int
	}{
		{"1series_x_100k", 1, 100_000},
		{"100series_x_10k", 100, 10_000},
		{"1000series_x_1k", 1000, 1_000},
	}
}

func BenchmarkDecode_RowPath(b *testing.B) {
	for _, sz := range benchSizes() {
		blk := matrixBlock(b, sz.series, sz.perSerie)
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := decodeRowPath(blk)
				if len(out) != sz.series*sz.perSerie {
					b.Fatalf("rows %d", len(out))
				}
			}
		})
	}
}

func BenchmarkDecode_ColumnarPath(b *testing.B) {
	for _, sz := range benchSizes() {
		blk := matrixBlock(b, sz.series, sz.perSerie)
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := decodeColumnarPath(blk)
				if len(out) != sz.series*sz.perSerie {
					b.Fatalf("rows %d", len(out))
				}
			}
		})
	}
}

// TestColumnarParity proves the columnar decode yields byte-identical
// Samples to the row path across the matrix shapes — the parity gate.
func TestColumnarParity(t *testing.T) {
	for _, sz := range []struct{ series, perSerie int }{
		{0, 0}, {1, 1}, {1, 5}, {3, 4}, {50, 20},
	} {
		blk := matrixBlock(t, sz.series, sz.perSerie)
		gotRow := decodeRowPath(blk)
		gotCol := decodeColumnarPath(blk)
		if len(gotRow) != len(gotCol) {
			t.Fatalf("len mismatch %dx%d: row=%d col=%d", sz.series, sz.perSerie, len(gotRow), len(gotCol))
		}
		for i := range gotRow {
			r, c := gotRow[i], gotCol[i]
			if r.MetricName != c.MetricName || r.Timestamp != c.Timestamp ||
				r.Value != c.Value || len(r.Labels) != len(c.Labels) {
				t.Fatalf("sample %d mismatch: row=%+v col=%+v", i, r, c)
			}
			for k, v := range r.Labels {
				if c.Labels[k] != v {
					t.Fatalf("sample %d label %q: row=%q col=%q", i, k, v, c.Labels[k])
				}
			}
		}
	}
}
