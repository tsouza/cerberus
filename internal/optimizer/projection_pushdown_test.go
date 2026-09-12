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
		return &chplan.FuncCall{Fn: chplan.FnMapFromArrays, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
				&chplan.Lambda{Params: []string{"k"}, Body: &chplan.FuncCall{
					Fn: chplan.FnRegexReplaceAll,
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: "k"},
						&chplan.InlineString{V: `[^a-zA-Z0-9_]`},
						&chplan.InlineString{V: "_"},
					},
				}},
				&chplan.FuncCall{Fn: chplan.FnMapKeys, Args: []chplan.Expr{src}},
			}},
			&chplan.FuncCall{Fn: chplan.FnMapValues, Args: []chplan.Expr{src}},
		}}
	}
	return &chplan.FuncCall{Fn: chplan.FnMapSort, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnMapUpdate, Args: []chplan.Expr{
				sanitize(&chplan.ColumnRef{Name: "ResourceAttributes"}),
				&chplan.ColumnRef{Name: "Attributes"},
			}},
			&chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{
				&chplan.InlineString{V: "service_name"},
				&chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{
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
	want := []string{"Attributes", "MetricName", "ResourceAttributes", "ServiceName", "sample_time", "sample_value"}

	node := func(groupBy ...string) *chplan.RangeWindowGridNative {
		keys := make([]chplan.Expr, 0, len(groupBy))
		for _, name := range groupBy {
			keys = append(keys, &chplan.ColumnRef{Name: name})
		}
		return &chplan.RangeWindowGridNative{
			Input: &chplan.Scan{
				Table: "otel_metrics_sum",
				Roles: []chplan.Column{
					{Name: "sample_time", Role: chplan.RoleTimestamp},
					{Name: "sample_value", Role: chplan.RoleValue},
				},
			},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            30 * time.Second,
			TimestampColumn: "public_time",
			ValueColumn:     "public_value",
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

func TestNativeRangeWindowColumns_MalformedRolesFailClosedBeforeExpressionWalk(t *testing.T) {
	t.Parallel()

	validRoles := []chplan.Column{
		{Name: "sample_time", Role: chplan.RoleTimestamp},
		{Name: "sample_value", Role: chplan.RoleValue},
	}
	for _, tc := range []struct {
		name  string
		roles []chplan.Column
	}{
		{name: "missing value", roles: validRoles[:1]},
		{name: "duplicate timestamp", roles: []chplan.Column{
			{Name: "sample_time", Role: chplan.RoleTimestamp},
			{Name: "other_time", Role: chplan.RoleTimestamp},
			{Name: "sample_value", Role: chplan.RoleValue},
		}},
		{name: "conflicting same name", roles: []chplan.Column{
			{Name: "sample", Role: chplan.RoleTimestamp},
			{Name: "sample", Role: chplan.RoleValue},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := &chplan.RangeWindowGridNative{
				Input:   &chplan.Scan{Table: "otel_metrics_sum", Roles: tc.roles},
				GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}},
				Recollapse: []chplan.Projection{{
					Expr:  &chplan.ColumnRef{Name: "ResourceAttributes"},
					Alias: "Attributes",
				}},
			}
			if got := nativeRangeWindowColumns(node); got != nil {
				t.Fatalf("nativeRangeWindowColumns() = %v, want nil for malformed roles", got)
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
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"},
		},
		Having: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FuncCall{
				Fn: chplan.FnThrowIf,
				Args: []chplan.Expr{
					&chplan.Binary{
						Op:    chplan.OpGt,
						Left:  &chplan.FuncCall{Fn: chplan.FnUniqExact, Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}}},
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

// TestAggregateColumns_HavingOnOutputAlias is the other half of
// [TestAggregateColumns_Having]: a Having reference to a name the
// Aggregate PRODUCES must NOT be pushed into the Scan.
//
// `HAVING isNotNull(Value)` over `avg(Duration) AS Value` names the
// aggregate's output column. The input relation has no such column, so a
// narrowed Scan carrying it asks ClickHouse for `otel_traces.Value` and
// gets error 215 (`not under aggregate function and not in GROUP BY
// keys`). Nothing in the tree planted that shape until the TraceQL
// aggregate lowering needed to drop all-NULL groups, which is how the
// gap surfaced.
//
// The `Duration`-vs-`Value` split is what makes this test discriminating:
// [TestAggregateColumns_Having]'s aggregate reads the same name it
// produces (`any(Value) AS Value`), so it cannot tell "kept because the
// AggFunc reads it" from "kept because Having names it".
func TestAggregateColumns_HavingOnOutputAlias(t *testing.T) {
	t.Parallel()

	a := &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_traces"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{"TraceId"},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnAvg, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Duration"}}, Alias: "Value"},
		},
		Having: &chplan.FuncCall{
			Fn:   chplan.FnIsNotNull,
			Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
		},
	}
	want := []string{"Duration", "TraceId"}
	if got := aggregateColumns(a); !reflect.DeepEqual(got, want) {
		t.Errorf("aggregateColumns() = %v, want %v — the aggregate's own output alias "+
			"was pushed into the Scan, which has no such column", got, want)
	}
}

// --- Expr-traversal exhaustiveness: the columns a pruned Scan must keep.
//
// stageColumns delegates its Expr walk to chplan.InspectExpr precisely so
// that the "an unwalked Expr kind hides the columns its subtree reads,
// ProjectionPushdown prunes them, ClickHouse answers error 47" class cannot
// reopen. The three tests below pin that contract behaviourally, one per
// Expr kind whose children the optimizer's former hand-rolled walk did NOT
// descend into (WindowExpr, InSubquery) plus the one whose embedded plan it
// deliberately must NOT descend into (ScalarSubquery). Each is a real
// pruning shape: the surviving projection supplies at least one OTHER
// column, so the rule fires and produces a narrowed Scan.Columns — a set
// the missing column is absent from unless the traversal reaches it.

// TestProjectionPushdown_KeepsWindowExprColumns pins that a column read
// only from inside a WindowExpr (its Args or its PartitionBy) survives the
// narrowing. `max(TimeUnix) OVER (PARTITION BY MetricName)` reads both off
// the Scan the rule is about to prune.
func TestProjectionPushdown_KeepsWindowExprColumns(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
			{
				Expr: &chplan.WindowExpr{
					Fn:          chplan.FnMax,
					Args:        []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}},
					PartitionBy: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}},
				},
				Alias: "series_end",
			},
		},
	}

	out, changed := ProjectionPushdown{}.Apply(plan)
	if !changed {
		t.Fatal("ProjectionPushdown did not fire on Project(Scan)")
	}
	got := narrowedScanColumns(t, out)
	want := []string{"MetricName", "TimeUnix", "Value"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("narrowed Scan.Columns = %v, want %v — a column read only inside a WindowExpr was pruned", got, want)
	}
}

// TestProjectionPushdown_KeepsInSubqueryLeftColumn pins that the LHS of an
// `<col> IN (<subquery>)` predicate survives the narrowing. The predicate
// is evaluated on the narrowed Scan's row shape, so pruning TraceId off it
// emits SQL ClickHouse rejects with UNKNOWN_IDENTIFIER.
func TestProjectionPushdown_KeepsInSubqueryLeftColumn(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_traces"},
			Predicate: &chplan.InSubquery{
				Left:     &chplan.ColumnRef{Name: "TraceId"},
				Subquery: &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId"}},
			},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "SpanName"}, Alias: "name"},
		},
	}

	out, changed := ProjectionPushdown{}.Apply(plan)
	if !changed {
		t.Fatal("ProjectionPushdown did not fire on Project(Filter(Scan))")
	}
	got := narrowedScanColumns(t, out)
	want := []string{"SpanName", "TraceId"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("narrowed Scan.Columns = %v, want %v — the IN-subquery's LHS column was pruned", got, want)
	}
}

// TestProjectionPushdown_IgnoresScalarSubqueryPlanColumns pins the other
// half of the boundary: InspectExpr is called WITHOUT a node visitor, so a
// ScalarSubquery's embedded plan — a separate relation whose column reads
// are satisfied in its own scope — contributes nothing to the outer Scan's
// column set. Widening the traversal to InspectExprNodes would leak
// `InnerOnly` (a column of a different table) into this Scan.
func TestProjectionPushdown_IgnoresScalarSubqueryPlanColumns(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{
				Expr: &chplan.Binary{
					Op:   chplan.OpDiv,
					Left: &chplan.ColumnRef{Name: "Value"},
					Right: &chplan.ScalarSubquery{
						Input: &chplan.Project{
							Input:       &chplan.Scan{Table: "otel_metrics_sum"},
							Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "InnerOnly"}}},
						},
					},
				},
				Alias: "ratio",
			},
		},
	}

	out, changed := ProjectionPushdown{}.Apply(plan)
	if !changed {
		t.Fatal("ProjectionPushdown did not fire on Project(Scan)")
	}
	got := narrowedScanColumns(t, out)
	want := []string{"Value"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("narrowed Scan.Columns = %v, want %v — the ScalarSubquery's own relation leaked into the outer Scan", got, want)
	}
}

// narrowedScanColumns digs the (single) Scan out of a rewritten Project
// tree and returns its Columns. Fails the test if the shape is not the
// Project(Scan) / Project(Filter(Scan)) the pushdown produces.
func narrowedScanColumns(t *testing.T, n chplan.Node) []string {
	t.Helper()
	p, ok := n.(*chplan.Project)
	if !ok {
		t.Fatalf("expected *chplan.Project at the root, got %T", n)
	}
	inner := p.Input
	if f, isFilter := inner.(*chplan.Filter); isFilter {
		inner = f.Input
	}
	s, ok := inner.(*chplan.Scan)
	if !ok {
		t.Fatalf("expected a *chplan.Scan under the Project, got %T", inner)
	}
	return s.Columns
}
