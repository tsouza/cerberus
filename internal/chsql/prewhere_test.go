package chsql

import (
	"reflect"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestFlattenAnd verifies AND-tree flattening returns the leaf set in
// left-to-right order, regardless of how the input was associated.
func TestFlattenAnd(t *testing.T) {
	t.Parallel()
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitInt{V: 1}}
	b := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "B"}, Right: &chplan.LitInt{V: 2}}
	c := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "C"}, Right: &chplan.LitInt{V: 3}}

	// Left-associative: (A AND B) AND C.
	left := &chplan.Binary{Op: chplan.OpAnd, Left: &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: b}, Right: c}
	if got := flattenAnd(left); len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("flattenAnd(left-assoc): got %v want [A B C]", got)
	}

	// Right-associative: A AND (B AND C).
	right := &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: &chplan.Binary{Op: chplan.OpAnd, Left: b, Right: c}}
	if got := flattenAnd(right); len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("flattenAnd(right-assoc): got %v want [A B C]", got)
	}

	// Non-AND root: returned as a single-element slice.
	if got := flattenAnd(a); len(got) != 1 || got[0] != a {
		t.Errorf("flattenAnd(non-and): got %v want [A]", got)
	}

	// OR root is opaque to flattenAnd: a single leaf.
	or := &chplan.Binary{Op: chplan.OpOr, Left: a, Right: b}
	if got := flattenAnd(or); len(got) != 1 || got[0] != or {
		t.Errorf("flattenAnd(OR): got %v want [or]", got)
	}
}

// TestCollectColumnRefs covers the column-extraction walk: bare refs,
// nested Binary, MapAccess, FuncCall, NestedArrayExists, and the
// qualified-ref skip path.
func TestCollectColumnRefs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr chplan.Expr
		want []string
	}{
		{
			name: "bare column",
			expr: &chplan.ColumnRef{Name: "ServiceName"},
			want: []string{"ServiceName"},
		},
		{
			name: "qualified column refs dropped",
			expr: &chplan.ColumnRef{Name: "SpanName", Qualifier: "R"},
			want: nil,
		},
		{
			name: "binary AND",
			expr: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitInt{V: 1}},
				Right: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "B"}, Right: &chplan.LitInt{V: 2}},
			},
			want: []string{"A", "B"},
		},
		{
			name: "MapAccess records the map column",
			expr: &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: "job"}},
			want: []string{"Attributes"},
		},
		{
			name: "FuncCall walks args",
			expr: &chplan.FuncCall{Name: "lower", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
			want: []string{"Body"},
		},
		{
			name: "NestedArrayExists records the parent column",
			expr: &chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}},
			want: []string{"Links"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := collectColumnRefs(tc.expr)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("collectColumnRefs() = %v, want %v", got, want)
			}
			if len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Errorf("collectColumnRefs() = %v, want %v", got, want)
			}
		})
	}
}

// TestIsCheapPredicate covers the cheap-predicate allowlist used by
// PREWHERE promotion.
func TestIsCheapPredicate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr chplan.Expr
		want bool
	}{
		{"literal int", &chplan.LitInt{V: 1}, true},
		{"literal string", &chplan.LitString{V: "x"}, true},
		{"literal bool", &chplan.LitBool{V: true}, true},
		{"column ref", &chplan.ColumnRef{Name: "A"}, true},
		{
			name: "binary comparison",
			expr: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitInt{V: 1}},
			want: true,
		},
		{
			name: "binary regex match is cheap (CH match builtin)",
			expr: &chplan.Binary{Op: chplan.OpMatch, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitString{V: "p"}},
			want: true,
		},
		{
			name: "MapAccess cheap when both sides are cheap",
			expr: &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: "job"}},
			want: true,
		},
		{
			name: "FuncCall not cheap (conservative)",
			expr: &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
			want: false,
		},
		{
			name: "NestedArrayExists not cheap (arrayExists per row)",
			expr: &chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}},
			want: false,
		},
		{
			name: "MapWithoutKeys not cheap (mapFilter)",
			expr: &chplan.MapWithoutKeys{Map: &chplan.ColumnRef{Name: "Attributes"}, Keys: []string{"x"}},
			want: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCheapPredicate(tc.expr); got != tc.want {
				t.Errorf("isCheapPredicate() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyPredicate covers the (cols, cheap, touchesWide) tuple.
func TestClassifyPredicate(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		SortColumns: []string{"ServiceName", "SeverityText"},
		WideColumns: []string{"Body", "ResourceAttributes"},
	}
	// Cheap, no wide ref → safe.
	cols, cheap, wide := classifyPredicate(
		&chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
		shape,
	)
	if !cheap || wide || !reflect.DeepEqual(cols, []string{"ServiceName"}) {
		t.Errorf("ServiceName predicate: cols=%v cheap=%v wide=%v want cols=[ServiceName] cheap=true wide=false", cols, cheap, wide)
	}
	// Cheap but touches Body → not PREWHERE-safe.
	_, cheap, wide = classifyPredicate(
		&chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Body"}, Right: &chplan.LitString{V: "x"}},
		shape,
	)
	if !cheap || !wide {
		t.Errorf("Body predicate: cheap=%v wide=%v want cheap=true wide=true", cheap, wide)
	}
	// FuncCall: not cheap.
	_, cheap, _ = classifyPredicate(
		&chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
		shape,
	)
	if cheap {
		t.Errorf("FuncCall(JSONExtract) classified as cheap; want false")
	}
}

// TestSortRankFor returns the lowest sort-column rank among the
// predicate's column refs.
func TestSortRankFor(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "SeverityText", "Timestamp"}}
	if got := sortRankFor([]string{"Timestamp", "ServiceName"}, shape); got != 0 {
		t.Errorf("rank(Timestamp,ServiceName) = %d, want 0 (ServiceName wins)", got)
	}
	if got := sortRankFor([]string{"Body"}, shape); got != -1 {
		t.Errorf("rank(Body) = %d, want -1", got)
	}
	if got := sortRankFor(nil, shape); got != -1 {
		t.Errorf("rank(nil) = %d, want -1", got)
	}
}

// TestOrderedConjuncts verifies the three-bucket partition produces a
// deterministic ordering: sort-prefix first (by rank), then skip-index,
// then rest. Within each bucket the input order survives.
func TestOrderedConjuncts(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		SortColumns: []string{"ServiceName", "Timestamp"},
		WideColumns: []string{"Body"},
	}
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Body"}, Right: &chplan.LitString{V: "x"}}
	b := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: 1}}
	c := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}
	// Input: [Body, Timestamp, ServiceName]; expected: [ServiceName (rank 0),
	// Timestamp (rank 1), Body (rest)].
	got := orderedConjuncts([]chplan.Expr{a, b, c}, shape)
	if len(got) != 3 || got[0] != c || got[1] != b || got[2] != a {
		t.Errorf("orderedConjuncts: got %v want [ServiceName, Timestamp, Body]", got)
	}

	// Empty shape: input order preserved.
	if got := orderedConjuncts([]chplan.Expr{a, b, c}, TableShape{}); len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("orderedConjuncts(zero shape): order changed; want input order")
	}

	// Single-element fast path.
	if got := orderedConjuncts([]chplan.Expr{a}, shape); len(got) != 1 || got[0] != a {
		t.Errorf("orderedConjuncts(single): %v want [a]", got)
	}
}

// TestPartitionPrewhere covers the PREWHERE / WHERE split.
func TestPartitionPrewhere(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		SortColumns: []string{"ServiceName"},
		WideColumns: []string{"Body"},
	}
	cheapNoWide := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}
	cheapWide := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Body"}, Right: &chplan.LitString{V: "x"}}
	notCheap := &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}}}

	// Mixed: cheap-no-wide → PREWHERE; rest → WHERE.
	pre, where := partitionPrewhere([]chplan.Expr{cheapNoWide, cheapWide, notCheap}, shape)
	if len(pre) != 1 || pre[0] != cheapNoWide {
		t.Errorf("partitionPrewhere PREWHERE = %v, want [cheapNoWide]", pre)
	}
	if len(where) != 2 || where[0] != cheapWide || where[1] != notCheap {
		t.Errorf("partitionPrewhere WHERE = %v, want [cheapWide, notCheap]", where)
	}

	// All qualify: last cheap-no-wide one stays in WHERE.
	another := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "b"}}
	pre, where = partitionPrewhere([]chplan.Expr{cheapNoWide, another}, shape)
	if len(pre) != 1 || pre[0] != cheapNoWide {
		t.Errorf("partitionPrewhere (all qualify) PREWHERE = %v, want [cheapNoWide]", pre)
	}
	if len(where) != 1 || where[0] != another {
		t.Errorf("partitionPrewhere (all qualify) WHERE = %v, want [another]", where)
	}

	// Shape with no wide columns: everything stays in WHERE.
	pre, where = partitionPrewhere([]chplan.Expr{cheapNoWide}, TableShape{SortColumns: []string{"X"}})
	if len(pre) != 0 || len(where) != 1 {
		t.Errorf("partitionPrewhere (no wide cols): pre=%v where=%v", pre, where)
	}

	// Sole conjunct is a leading-sort-key equality: it must STAY in
	// PREWHERE — demoting it forfeits the granule pruning the primary-key
	// binary-search enables (the resource-Project MetricName=? regression).
	metricsShape := TableShape{
		SortColumns: []string{"MetricName", "Attributes"},
		WideColumns: []string{"ResourceAttributes"},
	}
	metricNameEq := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitString{V: "up"}}
	pre, where = partitionPrewhere([]chplan.Expr{metricNameEq}, metricsShape)
	if len(pre) != 1 || pre[0] != metricNameEq {
		t.Errorf("partitionPrewhere (sole sort-key eq) PREWHERE = %v, want [metricNameEq]", pre)
	}
	if len(where) != 0 {
		t.Errorf("partitionPrewhere (sole sort-key eq) WHERE = %v, want []", where)
	}
}

// TestProjectionTouchesWide covers the wide-column projection check.
func TestProjectionTouchesWide(t *testing.T) {
	t.Parallel()
	shape := TableShape{WideColumns: []string{"Body", "ResourceAttributes"}}

	// Empty projection (SELECT *) → reads all columns including wide.
	if !projectionTouchesWide(nil, shape) {
		t.Errorf("projectionTouchesWide(nil) = false, want true (SELECT * reads everything)")
	}
	// Projection with wide column.
	if !projectionTouchesWide([]string{"Timestamp", "Body"}, shape) {
		t.Errorf("projectionTouchesWide([Timestamp, Body]) = false, want true")
	}
	// Projection without wide column.
	if projectionTouchesWide([]string{"Timestamp", "ServiceName"}, shape) {
		t.Errorf("projectionTouchesWide([Timestamp, ServiceName]) = true, want false")
	}
	// Shape with no wide columns → always false.
	if projectionTouchesWide(nil, TableShape{}) {
		t.Errorf("projectionTouchesWide(nil, zero-shape) = true, want false")
	}
}

// TestTableShapeForKnownTables covers the default OTel-CH tables; the
// codegen depends on these shapes for the PREWHERE rewrites.
func TestTableShapeForKnownTables(t *testing.T) {
	t.Parallel()
	for _, tbl := range []string{"otel_logs", "otel_traces", "otel_metrics_gauge", "otel_metrics_sum"} {
		shape := tableShapeFor(tbl)
		if len(shape.SortColumns) == 0 {
			t.Errorf("tableShapeFor(%q): empty SortColumns; expected an OTel-CH order key", tbl)
		}
		if len(shape.WideColumns) == 0 {
			t.Errorf("tableShapeFor(%q): empty WideColumns; expected at least one wide payload", tbl)
		}
	}
	// Unknown table returns zero shape.
	if shape := tableShapeFor("not_a_table"); len(shape.SortColumns) != 0 || len(shape.WideColumns) != 0 {
		t.Errorf("tableShapeFor(unknown) = %+v, want zero shape", shape)
	}
}
