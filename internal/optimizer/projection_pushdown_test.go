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
