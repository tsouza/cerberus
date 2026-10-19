package chplan

import (
	"math"
	"testing"
	"time"
)

// TestTimeBounds pins the exact form of each closed bound at the edges of
// the storage span and of the span a time literal names exactly.
func TestTimeBounds(t *testing.T) {
	t.Parallel()
	ns := time.Nanosecond
	pre1900 := time.Date(1800, time.January, 1, 0, 0, 0, 0, time.UTC)
	modern := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	literal := func(op BinaryOp, t time.Time) *TimeBound { return &TimeBound{Op: op, T: t} }
	nanos := func(op BinaryOp, n int64) *TimeBound { return &TimeBound{Op: op, Nanos: true, N: n} }
	cases := []struct {
		name  string
		bound func(time.Time) (TimeBound, bool)
		t     time.Time
		want  *TimeBound // nil: the bound excludes nothing
	}{
		{"at_least/before_storage", AtLeastBound, StorageMinTime.Add(-ns), nil},
		{"at_least/storage_min", AtLeastBound, StorageMinTime, nil},
		{"at_least/after_storage_min", AtLeastBound, StorageMinTime.Add(ns), nanos(OpGe, math.MinInt64+1)},
		{"at_least/1800", AtLeastBound, pre1900, nanos(OpGe, pre1900.UnixNano())},
		{"at_least/1ns_before_literal_min", AtLeastBound, TimeLiteralMin.Add(-ns), nanos(OpGe, TimeLiteralMin.UnixNano()-1)},
		{"at_least/literal_min", AtLeastBound, TimeLiteralMin, literal(OpGe, TimeLiteralMin)},
		{"at_least/modern", AtLeastBound, modern, literal(OpGe, modern)},
		{"at_least/storage_max", AtLeastBound, StorageMaxTime, literal(OpGe, StorageMaxTime)},
		{"at_least/after_storage", AtLeastBound, StorageMaxTime.Add(ns), nanos(OpGt, math.MaxInt64)},
		{"at_most/after_storage", AtMostBound, StorageMaxTime.Add(ns), nil},
		{"at_most/storage_max", AtMostBound, StorageMaxTime, nil},
		{"at_most/before_storage_max", AtMostBound, StorageMaxTime.Add(-ns), literal(OpLe, StorageMaxTime.Add(-ns))},
		{"at_most/modern", AtMostBound, modern, literal(OpLe, modern)},
		{"at_most/literal_min", AtMostBound, TimeLiteralMin, literal(OpLe, TimeLiteralMin)},
		{"at_most/1ns_before_literal_min", AtMostBound, TimeLiteralMin.Add(-ns), nanos(OpLe, TimeLiteralMin.UnixNano()-1)},
		{"at_most/1800", AtMostBound, pre1900, nanos(OpLe, pre1900.UnixNano())},
		{"at_most/storage_min", AtMostBound, StorageMinTime, nanos(OpLe, math.MinInt64)},
		{"at_most/before_storage", AtMostBound, StorageMinTime.Add(-ns), nanos(OpLt, math.MinInt64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := c.bound(c.t)
			switch {
			case c.want == nil && ok:
				t.Fatalf("bound at %s = %+v, want none", c.t, got)
			case c.want == nil:
			case !ok:
				t.Fatalf("bound at %s: none, want %+v", c.t, *c.want)
			case got.Op != c.want.Op || got.Nanos != c.want.Nanos || got.N != c.want.N || !got.T.Equal(c.want.T):
				t.Fatalf("bound at %s = %+v, want %+v", c.t, got, *c.want)
			}
		})
	}
}

// TestTimeBoundExpr pins the two plan shapes a bound builds.
func TestTimeBoundExpr(t *testing.T) {
	t.Parallel()
	x := &ColumnRef{Name: "TimeUnix"}
	modern := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	got := TimeBound{Op: OpGe, T: modern}.Expr(x)
	want := Expr(&Binary{Op: OpGe, Left: x, Right: TimeLiteral(modern)})
	if !got.Equal(want) {
		t.Errorf("literal bound = %#v, want %#v", got, want)
	}
	got = TimeBound{Op: OpLe, Nanos: true, N: -5}.Expr(x)
	want = &Binary{Op: OpLe, Left: &FuncCall{Fn: FnToUnixNanos, Args: []Expr{x}}, Right: &LitInt{V: -5}}
	if !got.Equal(want) {
		t.Errorf("nanosecond bound = %#v, want %#v", got, want)
	}
}
