// histogram_projection.go emits the SQL for
// chplan.HistogramProjection: a subquery whose FINAL output columns are
// the OTel exponential (native) histogram's raw structural fields —
// Count, Sum, Scale, ZeroThreshold, ZeroCount, PositiveOffset,
// PositiveBucketCounts, NegativeOffset, NegativeBucketCounts — aliased
// to the fixed chplan.Histogram*Column names, rather than collapsed to
// a scalar the way histogram_quantile_native.go's emitter is.
//
// internal/promql's native-histogram lowerings — a bare selector and
// `sum [by/without] (<selector>)` — are what build the node this emits;
// see that type's doc comment for the output contract their shared cap
// guarantees (#1926 / #1967).
package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramProjection renders a chplan.HistogramProjection. The
// outer QueryBuilder projects the GroupBy columns aliased per
// GroupByAliases, then the nine histogram columns under their fixed
// chplan.Histogram*Column aliases — matching the shape a future
// decode-side consumer (chclient.Sample.Histogram) binds by name.
func (e *emitter) emitHistogramProjection(h *chplan.HistogramProjection) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramProjection.Input is nil", ErrUnsupported)
	}
	// ZeroThresholdColumn is intentionally NOT required, mirroring
	// HistogramQuantileNative: the upstream OTel-CH exp-histogram DDL
	// does not persist the OTLP zero_threshold field, so the default
	// schema leaves it empty and the projection renders a constant `0.`
	// literal for the HistogramZeroThreshold output column instead —
	// the output shape stays nine columns regardless of what the input
	// schema persists.
	if h.CountColumn == "" || h.SumColumn == "" || h.ScaleColumn == "" ||
		h.ZeroCountColumn == "" || h.PositiveOffsetColumn == "" ||
		h.PositiveBucketCountsColumn == "" || h.NegativeOffsetColumn == "" ||
		h.NegativeBucketCountsColumn == "" {
		return fmt.Errorf("%w: HistogramProjection requires Count / Sum / Scale / ZeroCount / "+
			"PositiveOffset / PositiveBucketCounts / NegativeOffset / NegativeBucketCounts column names",
			ErrUnsupported)
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	sb := NewQuery().From(sub)
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	sb.SelectAs(histogramFloatFrag(Col(h.CountColumn)), chplan.HistogramCountColumn)
	sb.SelectAs(histogramFloatFrag(Col(h.SumColumn)), chplan.HistogramSumColumn)
	sb.SelectAs(histogramIndexFrag(Col(h.ScaleColumn)), chplan.HistogramScaleColumn)
	sb.SelectAs(histogramFloatFrag(histogramProjectionZeroThresholdFrag(h)), chplan.HistogramZeroThresholdColumn)
	sb.SelectAs(histogramFloatFrag(Col(h.ZeroCountColumn)), chplan.HistogramZeroCountColumn)
	sb.SelectAs(histogramIndexFrag(Col(h.PositiveOffsetColumn)), chplan.HistogramPositiveOffsetColumn)
	sb.SelectAs(histogramBucketsFrag(Col(h.PositiveBucketCountsColumn)), chplan.HistogramPositiveBucketCountsColumn)
	sb.SelectAs(histogramIndexFrag(Col(h.NegativeOffsetColumn)), chplan.HistogramNegativeOffsetColumn)
	sb.SelectAs(histogramBucketsFrag(Col(h.NegativeBucketCountsColumn)), chplan.HistogramNegativeBucketCountsColumn)
	return e.emitSelect(sb)
}

// The nine output columns carry a PINNED ClickHouse type, independent of
// which lowering built the node and of what the physical schema stores.
// Without the casts below the emitted types depend on the shape: a bare
// selector and `sum()` forward the OTel-CH columns verbatim (`Count`
// UInt64, `PositiveBucketCounts` Array(UInt64)), while `rate()` /
// `increase()` divide those by the window and merge the ladders, landing
// on Float64 / Array(Float64). One output contract cannot be two types:
// [chclient.HistogramValue] binds ONE set of scan destinations by
// position, and the ch-go columnar decoder type-ASSERTS one concrete
// proto column per slot. A UInt64-shaped `rate()` result is not merely
// awkward there — `clickhouse-go/v2`'s `Float64.ScanRow` rejects a
// `*uint64` destination outright, so the un-pinned shape failed to decode
// at all (issue #1967).
//
// Float64 (not UInt64) is the correct pin, not just the convenient one:
// `rate()` over a native histogram yields FRACTIONAL observation counts,
// exactly as reference Prometheus's own `rate()` returns a
// `*histogram.FloatHistogram` rather than an integer `Histogram`.
// Rounding a 0.1/s rate back to an integer count would answer 0.
//
// Scale and the two bucket offsets are BUCKET INDICES, not counts: they
// stay integral, pinned to Int32 because the merge expression
// (`arrayMin(arrayMap((s, off) -> bitShiftRight(off, s - <scale>), …))`)
// promotes to Int64 on some ClickHouse versions while the physical
// column is Int32.

// histogramFloatFrag pins a real-valued slot — the counts, the sum and
// the zero threshold — to Float64.
func histogramFloatFrag(f Frag) Frag { return Call("toFloat64", f) }

// histogramIndexFrag pins a bucket-index slot (Scale, the two offsets) to
// Int32.
func histogramIndexFrag(f Frag) Frag { return Call("toInt32", f) }

// histogramBucketsFrag pins a bucket-count ladder to Array(Float64),
// element-wise. Mirrors the identical widening histogram_quantile.go
// already applies before its own bucket walk.
func histogramBucketsFrag(f Frag) Frag {
	return Call("arrayMap", Lambda1("x", Call("toFloat64", BareIdent("x"))), f)
}

// histogramProjectionZeroThresholdFrag renders the HistogramZeroThreshold
// output column: the stored per-row value when the schema persists one,
// or the CH-portable shape token `0.` when ZeroThresholdColumn is empty
// (see zeroBandOrigin in histogram_quantile_native.go, the same
// precedent for a constant Float64 literal that rides verbatim rather
// than through InlineLit).
func histogramProjectionZeroThresholdFrag(h *chplan.HistogramProjection) Frag {
	if h.ZeroThresholdColumn == "" {
		return zeroBandOrigin()
	}
	return Col(h.ZeroThresholdColumn)
}
