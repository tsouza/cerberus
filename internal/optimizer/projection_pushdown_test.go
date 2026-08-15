package optimizer

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// recollapseTower is the deferred label-shaping expression the PromQL lowering
// hoists above the native aggregate:
//
//	mapSort(mapConcat(mapUpdate(<sanitised ResourceAttributes>, Attributes),
//	                  map('service_name', toString(ServiceName))))
//
// It reads three base columns — ResourceAttributes, Attributes, ServiceName —
// and binds one lambda parameter, `k`, which is a chplan.BareIdent rather than a
// ColumnRef and therefore must NOT land in the narrowed Scan column set.
func recollapseTower() chplan.Expr {
	sanitize := func(src chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "mapFromArrays", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
				&chplan.Lambda{Params: []string{"k"}, Body: &chplan.FuncCall{
					Name: "replaceRegexpAll",
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: "k"},
						&chplan.InlineString{V: `[^a-zA-Z0-9_]`},
						&chplan.InlineString{V: "_"},
					},
				}},
				&chplan.FuncCall{Name: "mapKeys", Args: []chplan.Expr{src}},
			}},
			&chplan.FuncCall{Name: "mapValues", Args: []chplan.Expr{src}},
		}}
	}
	return &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{
		&chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "mapUpdate", Args: []chplan.Expr{
				sanitize(&chplan.ColumnRef{Name: "ResourceAttributes"}),
				&chplan.ColumnRef{Name: "Attributes"},
			}},
			&chplan.FuncCall{Name: "map", Args: []chplan.Expr{
				&chplan.InlineString{V: "service_name"},
				&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "ServiceName"},
				}},
			}},
		}},
	}}
}

// TestNativeRangeWindowColumns_Recollapse pins that the narrowed Scan column set
// covers what the DEFERRED shaping expressions read, not just the node's GroupBy
// keys. The native emit references those columns at its middle level; a Scan
// narrowed without them 502s at runtime with `Unknown expression identifier` —
// the #860/#861 dropped-identity-column class.
func TestNativeRangeWindowColumns_Recollapse(t *testing.T) {
	t.Parallel()

	// Every column the three-level emit names off the Scan: the per-sample
	// (timestamp, value) pair, the pass-through identity key, and the three
	// inputs of the shaping tower. `k`, the tower's lambda parameter, is
	// deliberately absent.
	want := []string{"Attributes", "MetricName", "ResourceAttributes", "ServiceName", "TimeUnix", "Value"}

	node := func(groupBy ...string) *chplan.RangeWindowNative {
		keys := make([]chplan.Expr, 0, len(groupBy))
		for _, name := range groupBy {
			keys = append(keys, &chplan.ColumnRef{Name: name})
		}
		return &chplan.RangeWindowNative{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            30 * time.Second,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         keys,
			Recollapse:      []chplan.Projection{{Expr: recollapseTower(), Alias: "Attributes"}},
		}
	}

	for _, tc := range []struct {
		name    string
		groupBy []string
	}{
		{
			// The shape the lowering actually produces: containment holds, so
			// every shaping input is also a GroupBy key.
			name:    "shaping inputs are also GroupBy keys",
			groupBy: []string{"MetricName", "Attributes", "ResourceAttributes", "ServiceName"},
		},
		{
			// The same set must come out when containment does NOT hold. chsql
			// rejects this node today (requireRecollapseColumnsGrouped), and
			// this case is why both ship: an enumeration that leaned on the
			// invariant would be one invariant-relaxation away from narrowing
			// the Scan below what the emit reads.
			name:    "shaping inputs are not GroupBy keys",
			groupBy: []string{"MetricName"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := nativeRangeWindowColumns(node(tc.groupBy...)); !reflect.DeepEqual(got, want) {
				t.Errorf("nativeRangeWindowColumns() = %v, want %v", got, want)
			}
		})
	}
}

// TestRangeWindowColumns_Temporality pins the #2127 fix: rangeWindowColumns
// must include TemporalityColumn whenever it is set, because
// emitWindowedArrayExtrapolated (chsql/range_window.go) reads
// `any(<TemporalityColumn>)` off the same Input this pushdown narrows,
// unconditionally. Reached in production by a `rate()` / `increase()` over
// a schema that clears ResourceAttributesColumn (and has no dedicated
// top-level-column overlay) with AggregationTemporalityColumn set: on such
// a schema augmentSelectorAttributes (internal/promql/lower.go) skips the
// Project wrap that would otherwise carry TemporalityColumn, so the
// RangeWindow's Input is a bare Filter(Scan) with TemporalityColumn still
// populated — exactly the shape applyStageScan narrows directly.
func TestRangeWindowColumns_Temporality(t *testing.T) {
	t.Parallel()

	r := &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	want := []string{"AggregationTemporality", "Attributes", "TimeUnix", "Value"}
	if got := rangeWindowColumns(r); !reflect.DeepEqual(got, want) {
		t.Errorf("rangeWindowColumns() = %v, want %v", got, want)
	}
}

// TestRangeWindowColumns_TemporalityReachesNarrowedScan proves the gap end
// to end through the rule itself, not just the enumerator: applying
// ProjectionPushdown to `RangeWindow(Filter(Scan))` with TemporalityColumn
// set must leave AggregationTemporality in the narrowed Scan.Columns.
// Before the #2127 fix this narrowed the Scan to {Attributes, TimeUnix,
// Value}, dropping AggregationTemporality out from under the emitter and
// 502ing with UNKNOWN_IDENTIFIER.
func TestRangeWindowColumns_TemporalityReachesNarrowedScan(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	filter := &chplan.Filter{
		Input:     scan,
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.InlineString{V: "x"}},
	}
	r := &chplan.RangeWindow{
		Input:             filter,
		Func:              "rate",
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	got, changed := (ProjectionPushdown{}).Apply(r)
	if !changed {
		t.Fatalf("ProjectionPushdown.Apply() reported no change")
	}
	rewritten, ok := got.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("ProjectionPushdown.Apply() returned %T, want *chplan.RangeWindow", got)
	}
	rewrittenFilter, ok := rewritten.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("rewritten RangeWindow.Input = %T, want *chplan.Filter", rewritten.Input)
	}
	rewrittenScan, ok := rewrittenFilter.Input.(*chplan.Scan)
	if !ok {
		t.Fatalf("rewritten Filter.Input = %T, want *chplan.Scan", rewrittenFilter.Input)
	}

	want := []string{"AggregationTemporality", "Attributes", "MetricName", "TimeUnix", "Value"}
	if !reflect.DeepEqual(rewrittenScan.Columns, want) {
		t.Errorf("narrowed Scan.Columns = %v, want %v", rewrittenScan.Columns, want)
	}
}

// TestRangeWindowColumns_Variants pins the second #2127 latent gap: a fused
// multi-arm RangeWindow (LogQL's variants(...)) must contribute every arm's
// own ValueColumn, since range_window_variants.go reads each arm's column
// directly off Input. Currently reached only through a Project gate
// applyStageScan declines, but the enumerator itself must still be a
// complete description of what the emit reads.
func TestRangeWindowColumns_Variants(t *testing.T) {
	t.Parallel()

	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "logs"},
		TimestampColumn: "TimeUnix",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Variants: []chplan.RangeWindowVariant{
			{Func: "count_over_time", ValueColumn: "CountValue", Label: "0"},
			{Func: "bytes_over_time", ValueColumn: "BytesValue", Label: "1"},
		},
		VariantColumn: "__variant__",
	}
	want := []string{"Attributes", "BytesValue", "CountValue", "TimeUnix"}
	if got := rangeWindowColumns(r); !reflect.DeepEqual(got, want) {
		t.Errorf("rangeWindowColumns() = %v, want %v", got, want)
	}
}

// TestAggregateColumns_Having pins the third #2127 latent gap:
// aggregateColumns must include the columns Having references, since
// emitAggregate (chsql/emit_node.go) renders Having as a real SQL HAVING
// clause evaluated against the same narrowed row shape. Mirrors
// duplicateLabelsetGuardExpr's shape (internal/promql/lower.go): a
// `throwIf(uniqExact(MetricName) > 1, …) = 0` guard where MetricName is
// deliberately absent from both GroupBy and AggFuncs.
func TestAggregateColumns_Having(t *testing.T) {
	t.Parallel()

	a := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"},
		},
		Having: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FuncCall{
				Name: "throwIf",
				Args: []chplan.Expr{
					&chplan.Binary{
						Op:    chplan.OpGt,
						Left:  &chplan.FuncCall{Name: "uniqExact", Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}}},
						Right: &chplan.LitInt{V: 1},
					},
					&chplan.InlineString{V: "duplicate labelset"},
				},
			},
			Right: &chplan.LitInt{V: 0},
		},
	}
	want := []string{"Attributes", "MetricName", "Value"}
	if got := aggregateColumns(a); !reflect.DeepEqual(got, want) {
		t.Errorf("aggregateColumns() = %v, want %v", got, want)
	}
}
