package chsql

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file gathers targeted boundary-value tests that kill the LIVED
// gremlins mutants reported by the phase-2 mutation matrix in
// `.github/workflows/mutation.yml`. Each test sits in the internal
// `package chsql` so it can reach unexported helpers (orderedConjuncts,
// flattenAnd, classifyPredicate, sortRankFor, etc.) where the public
// surface alone wouldn't expose the boundary.
//
// Conventions:
//   - one Test... per source-file cluster of related mutants
//   - table-driven where the cluster shares boundary inputs
//   - assertions name the original behaviour explicitly, so a `&&` ↔
//     `||` or `==` ↔ `!=` flip on the named operator falls out of
//     scope and gets killed.

// TestSortRankFor_ContinueVsBreak kills the `continue` → `break` flip
// at prewhere.go:128. Input order matters: an unknown column ahead of
// a known one only resolves to the known column's rank if the loop
// keeps iterating (continue) rather than bailing on the first miss
// (break).
func TestSortRankFor_ContinueVsBreak(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "Timestamp"}}
	// Unknown column first → continue path returns 0 (ServiceName).
	// break-mutant would short-circuit to -1.
	if got := sortRankFor([]string{"Unknown", "ServiceName"}, shape); got != 0 {
		t.Errorf("sortRankFor([Unknown, ServiceName]) = %d, want 0", got)
	}
	// Two unknowns ahead of a known column.
	if got := sortRankFor([]string{"Foo", "Bar", "Timestamp"}, shape); got != 1 {
		t.Errorf("sortRankFor([Foo, Bar, Timestamp]) = %d, want 1", got)
	}
}

// TestSortRankFor_BestNegativeBoundary kills the `<` ↔ `<=` boundary
// flip at prewhere.go:130:11 (`best < 0`). With the mutant `best <= 0`
// the loop overwrites the rank-0 best when it sees any larger rank,
// returning the wrong (later) sort column.
func TestSortRankFor_BestNegativeBoundary(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "Timestamp"}}
	// ServiceName(rank=0) processed first → best=0. Timestamp(rank=1)
	// must NOT overwrite. Mutant `<=` would (0<=0 true).
	if got := sortRankFor([]string{"ServiceName", "Timestamp"}, shape); got != 0 {
		t.Errorf("sortRankFor([ServiceName, Timestamp]) = %d, want 0", got)
	}
}

// TestOrderedConjuncts_StableSortLogicalOr kills the `||` → `&&` flip
// at prewhere.go:183:23 inside the insertion-sort swap condition. When
// the rank-comparison conjunct disagrees with the index-tiebreaker the
// original `||` swaps; the mutant `&&` requires both, which never
// holds at the same time, so the swap never happens.
func TestOrderedConjuncts_StableSortLogicalOr(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "Timestamp"}}
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: 1}}
	b := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}
	// Input order: Timestamp(rank 1), ServiceName(rank 0). Original
	// sorts to [ServiceName, Timestamp]; `&&` mutant leaves them in
	// input order.
	got := orderedConjuncts([]chplan.Expr{a, b}, shape)
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Errorf("orderedConjuncts: got %v, want [ServiceName, Timestamp]", got)
	}
}

// TestIsCheapPredicate_AsymmetricBinary kills the `&&` → `||` flip in
// `isCheapPredicate` for `Binary` (prewhere.go:90). Setting one side
// cheap and the other side not-cheap distinguishes:
//   - original (&&): false (not both cheap → predicate not cheap)
//   - mutant   (||): true  (at least one side cheap → wrongly cheap)
func TestIsCheapPredicate_AsymmetricBinary(t *testing.T) {
	t.Parallel()
	// Cheap left, non-cheap right (JSONExtract FuncCall is not cheap).
	expr := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "A"},
		Right: &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
	}
	if got := isCheapPredicate(expr); got != false {
		t.Errorf("isCheapPredicate(cheap && !cheap) = %v, want false", got)
	}
	// Mirror: non-cheap left, cheap right.
	expr = &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
		Right: &chplan.ColumnRef{Name: "A"},
	}
	if got := isCheapPredicate(expr); got != false {
		t.Errorf("isCheapPredicate(!cheap && cheap) = %v, want false", got)
	}
}

// TestIsCheapPredicate_AsymmetricMapAccess kills the `&&` → `||` flip
// at prewhere.go:92 — same pattern as the Binary case but for the
// MapAccess shape. Synthesising a non-cheap Map side requires a
// FuncCall in the Map slot.
func TestIsCheapPredicate_AsymmetricMapAccess(t *testing.T) {
	t.Parallel()
	// Map = FuncCall (not cheap), Key = literal (cheap).
	expr := &chplan.MapAccess{
		Map: &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
		Key: &chplan.LitString{V: "k"},
	}
	if got := isCheapPredicate(expr); got != false {
		t.Errorf("isCheapPredicate(MapAccess{!cheap, cheap}) = %v, want false", got)
	}
	// Map = cheap, Key = FuncCall (not cheap).
	expr = &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "Attributes"},
		Key: &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
	}
	if got := isCheapPredicate(expr); got != false {
		t.Errorf("isCheapPredicate(MapAccess{cheap, !cheap}) = %v, want false", got)
	}
}

// TestFlattenAnd_NonAndOp kills the `bin.Op != chplan.OpAnd` → `==`
// flip at prewhere.go:13 (the negation mutator on the conjunction
// type-switch guard). A Binary{OpOr} or Binary{OpEq} must be treated
// as a single opaque leaf — flattenAnd must not recurse through it.
func TestFlattenAnd_NonAndOp(t *testing.T) {
	t.Parallel()
	a := &chplan.ColumnRef{Name: "A"}
	b := &chplan.ColumnRef{Name: "B"}
	cases := []struct {
		name string
		op   chplan.BinaryOp
	}{
		{"or", chplan.OpOr},
		{"eq", chplan.OpEq},
		{"lt", chplan.OpLt},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := &chplan.Binary{Op: c.op, Left: a, Right: b}
			got := flattenAnd(root)
			if len(got) != 1 || got[0] != root {
				t.Errorf("flattenAnd(Binary{%v}) = %v, want [root]", c.op, got)
			}
		})
	}
}

// TestClassifyPredicate_CheapAndWide pins both `cheap` and `wide` for
// each branch combination so a flip on any of the contributing
// helpers surfaces here.
func TestClassifyPredicate_CheapAndWide(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		SortColumns: []string{"ServiceName"},
		WideColumns: []string{"Body", "ResourceAttributes"},
	}
	cases := []struct {
		name  string
		expr  chplan.Expr
		cheap bool
		wide  bool
	}{
		{
			name:  "cheap non-wide",
			expr:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
			cheap: true,
			wide:  false,
		},
		{
			name:  "cheap wide (Body)",
			expr:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Body"}, Right: &chplan.LitString{V: "x"}},
			cheap: true,
			wide:  true,
		},
		{
			name:  "non-cheap wide-referencing (FuncCall over Body)",
			expr:  &chplan.FuncCall{Name: "JSONExtract", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
			cheap: false,
			wide:  true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, cheap, wide := classifyPredicate(c.expr, shape)
			if cheap != c.cheap || wide != c.wide {
				t.Errorf("classifyPredicate(%s): cheap=%v wide=%v, want cheap=%v wide=%v", c.name, cheap, wide, c.cheap, c.wide)
			}
		})
	}
}

// TestPartitionPrewhere_EmptyConjunctsBoundary kills the
// `len(prewhere) > 0` boundary at prewhere.go:225. With empty
// conjuncts, the post-loop guard must NOT enter; the mutant `>= 0`
// would index prewhere[-1] and panic.
func TestPartitionPrewhere_EmptyConjunctsBoundary(t *testing.T) {
	t.Parallel()
	shape := TableShape{WideColumns: []string{"Body"}}
	pre, where := partitionPrewhere(nil, shape)
	if len(pre) != 0 || len(where) != 0 {
		t.Errorf("partitionPrewhere(empty conjuncts) = pre=%v where=%v, want both empty", pre, where)
	}
}

// TestPartitionPrewhere_NoWideColumnsShape pins that the early-return
// branch on the `len(shape.WideColumns) == 0` guard returns the
// conjuncts untouched.
func TestPartitionPrewhere_NoWideColumnsShape(t *testing.T) {
	t.Parallel()
	cheap := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitInt{V: 1}}
	pre, where := partitionPrewhere([]chplan.Expr{cheap}, TableShape{})
	if len(pre) != 0 {
		t.Errorf("partitionPrewhere(no-wide) PREWHERE = %v, want empty", pre)
	}
	if len(where) != 1 || where[0] != cheap {
		t.Errorf("partitionPrewhere(no-wide) WHERE = %v, want [cheap]", where)
	}
}

// TestPartitionPrewhere_AllQualifyLastInWhere stresses the "every
// conjunct qualifies for PREWHERE; keep the last in WHERE" branch and
// pins both the count split and the identity of the retained conjunct.
func TestPartitionPrewhere_AllQualifyLastInWhere(t *testing.T) {
	t.Parallel()
	shape := TableShape{WideColumns: []string{"Body"}}
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "A"}, Right: &chplan.LitInt{V: 1}}
	b := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "B"}, Right: &chplan.LitInt{V: 2}}
	c := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "C"}, Right: &chplan.LitInt{V: 3}}
	pre, where := partitionPrewhere([]chplan.Expr{a, b, c}, shape)
	if len(pre) != 2 || pre[0] != a || pre[1] != b {
		t.Errorf("PREWHERE = %v, want [a, b]", pre)
	}
	if len(where) != 1 || where[0] != c {
		t.Errorf("WHERE = %v, want [c] (last cheap-non-wide retained)", where)
	}
}

// TestProjectionTouchesWide_BoundaryCases pins the boundary behaviours
// at projectionTouchesWide. Each case targets one mutator type.
func TestProjectionTouchesWide_BoundaryCases(t *testing.T) {
	t.Parallel()
	shape := TableShape{WideColumns: []string{"Body"}}
	cases := []struct {
		name string
		cols []string
		want bool
	}{
		{"nil projection (SELECT *)", nil, true},
		{"empty slice projection", []string{}, true},
		{"only non-wide", []string{"A", "B"}, false},
		{"only wide", []string{"Body"}, true},
		{"mixed wide+narrow", []string{"A", "Body", "C"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := projectionTouchesWide(c.cols, shape); got != c.want {
				t.Errorf("projectionTouchesWide(%v) = %v, want %v", c.cols, got, c.want)
			}
		})
	}
}

// TestEmitMetricsExemplars_StepBoundary kills the `<=` ↔ `<` boundary
// flip at exemplars.go:58 (`rw.Step <= 0`). The boundary case Step=0
// must error; the mutant `< 0` would let it through.
func TestEmitMetricsExemplars_StepBoundary(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            0,
		Range:           time.Minute,
		TimestampColumn: "Timestamp",
	}
	_, _, err := EmitMetricsExemplars(context.Background(), plan,
		plan.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1)
	if err == nil {
		t.Fatalf("expected error for Step=0, got nil")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestEmitMetricsExemplars_NilInner pins the `m.Inner == nil` guard
// (exemplars.go:63). Killing both negation and the implied boundary
// requires asserting the error AND asserting success when Inner is
// supplied.
func TestEmitMetricsExemplars_NilInner(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      nil,
		},
		Step:            time.Minute,
		Range:           time.Minute,
		TimestampColumn: "Timestamp",
	}
	_, _, err := EmitMetricsExemplars(context.Background(), plan,
		plan.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1)
	if err == nil {
		t.Fatalf("expected error for nil Inner, got nil")
	}
}

// TestEmitMetricsExemplars_NilMetricsAggregate pins the early-return
// on m==nil (exemplars.go:48).
func TestEmitMetricsExemplars_NilMetricsAggregate(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Step:            time.Minute,
		Range:           time.Minute,
		TimestampColumn: "Timestamp",
	}
	_, _, err := EmitMetricsExemplars(context.Background(), rw, nil, "TraceId", "SpanId", 1)
	if err == nil {
		t.Fatalf("expected error for nil MetricsAggregate, got nil")
	}
}

// TestEmitMetricsExemplars_EmptyTimestampColumn pins the
// TimestampColumn=="" guard.
func TestEmitMetricsExemplars_EmptyTimestampColumn(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:  time.Minute,
		Range: time.Minute,
	}
	_, _, err := EmitMetricsExemplars(context.Background(), rw,
		rw.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1)
	if err == nil {
		t.Fatalf("expected error for empty TimestampColumn, got nil")
	}
}

// TestEmitMetricsExemplars_RangeDurationFallback kills the `==` flip
// at exemplars.go:104 (`rangeDur == 0`). When Range is unset, the
// fallback uses Step. With the negation-mutant the fallback runs when
// Range is set instead, producing a different windowTsLowerBound.
func TestEmitMetricsExemplars_RangeDurationFallback(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}

	cases := []struct {
		name string
		rw   *chplan.RangeWindow
		// rangeNS is the duration (in ns) the windowTsLowerBound must
		// use as the half-window offset from anchor_ts. Asserted via
		// substring on the rendered SQL.
		wantRangeNS int64
	}{
		{
			name: "Range zero → falls back to Step",
			rw: &chplan.RangeWindow{
				Input:           m,
				Step:            2 * time.Minute,
				Range:           0, // triggers fallback
				Start:           start,
				End:             end,
				TimestampColumn: "Timestamp",
			},
			wantRangeNS: int64((2 * time.Minute).Nanoseconds()),
		},
		{
			name: "Range non-zero → used directly",
			rw: &chplan.RangeWindow{
				Input:           m,
				Step:            2 * time.Minute,
				Range:           5 * time.Minute, // does NOT trigger fallback
				Start:           start,
				End:             end,
				TimestampColumn: "Timestamp",
			},
			wantRangeNS: int64((5 * time.Minute).Nanoseconds()),
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := EmitMetricsExemplars(context.Background(), c.rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("EmitMetricsExemplars: %v", err)
			}
			// The rangeNS literal appears as the toIntervalNanosecond
			// argument inside the windowTsLowerBound predicate.
			wantSub := "toIntervalNanosecond(" + strconv.FormatInt(c.wantRangeNS, 10) + ")"
			if !strings.Contains(sql, wantSub) {
				t.Errorf("SQL missing %q.\nSQL: %s", wantSub, sql)
			}
		})
	}
}

// TestEmitMetricsExemplars_NumAnchorsBoundary kills both
// CONDITIONAL_BOUNDARY at exemplars.go:115 and ARITHMETIC mutators at
// 118 (`span/stepNS + 1` and surrounds). End == Start hits the
// boundary: span == 0 → numAnchors = 0/step + 1 = 1.
func TestEmitMetricsExemplars_NumAnchorsBoundary(t *testing.T) {
	t.Parallel()

	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}

	cases := []struct {
		name        string
		start       time.Time
		end         time.Time
		wantAnchors string
	}{
		{
			name:        "End==Start (span=0)",
			start:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:         time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			wantAnchors: "range(0, 1)",
		},
		{
			name:        "End-Start = 1 step",
			start:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:         time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC),
			wantAnchors: "range(0, 2)",
		},
		{
			name:        "End-Start = 5 steps",
			start:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:         time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
			wantAnchors: "range(0, 6)",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rw := &chplan.RangeWindow{
				Input:           m,
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.wantAnchors) {
				t.Errorf("SQL missing %q.\nSQL: %s", c.wantAnchors, sql)
			}
		})
	}
}

// TestEmitMetricsExemplars_GroupByDisplayNamesFallback kills the `!=
// ""` mutant at exemplars.go:139:65. The branch falls back to the SQL
// alias only when the display name slot is empty; when both alias and
// display name are present, the display name wins. The check inspects
// the bound args (the labels render as `?` placeholders bound to
// string args).
func TestEmitMetricsExemplars_GroupByDisplayNamesFallback(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	cases := []struct {
		name        string
		displayName []string
		wantArg     string
	}{
		{
			name:        "display name present → used as map key",
			displayName: []string{"resource.service.name"},
			wantArg:     "resource.service.name",
		},
		{
			name:        "display name empty → fallback to alias",
			displayName: []string{""},
			wantArg:     "service",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:                  chplan.MetricsOpRate,
				GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
				GroupByAliases:      []string{"service"},
				GroupByDisplayNames: c.displayName,
				ValueAlias:          "Value",
				Inner:               &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input: m, Step: time.Minute, Range: time.Minute,
				Start: start, End: end, TimestampColumn: "Timestamp",
			}
			_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// One of the args must be the expected map-key string.
			found := false
			for _, a := range args {
				if s, ok := a.(string); ok && s == c.wantArg {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected arg %q in bound args; got %v", c.wantArg, args)
			}
		})
	}
}

// TestEmitMetricsExemplars_MetricArgEmission_Op154 kills the three
// `==` ↔ `!=` flips and the `&&` ↔ `||` flip at exemplars.go:154 —
// `m.Op != Rate && m.Op != CountOverTime && m.Attr != nil`. The
// boundary is whether `metric_arg` appears in the inner SELECT.
func TestEmitMetricsExemplars_MetricArgEmission_Op154(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)

	cases := []struct {
		name string
		op   chplan.MetricsOp
		attr chplan.Expr
		want bool // true = metric_arg present
	}{
		{"Rate + Attr → no metric_arg", chplan.MetricsOpRate, &chplan.ColumnRef{Name: "Duration"}, false},
		{"CountOverTime + Attr → no metric_arg", chplan.MetricsOpCountOverTime, &chplan.ColumnRef{Name: "Duration"}, false},
		{"SumOverTime + Attr → metric_arg present", chplan.MetricsOpSumOverTime, &chplan.ColumnRef{Name: "Duration"}, true},
		{"SumOverTime + nil Attr → no metric_arg", chplan.MetricsOpSumOverTime, nil, false},
		{"AvgOverTime + Attr → metric_arg present", chplan.MetricsOpAvgOverTime, &chplan.ColumnRef{Name: "Duration"}, true},
		{"Rate + nil Attr → no metric_arg", chplan.MetricsOpRate, nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:         c.op,
				Attr:       c.attr,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input: m, Step: time.Minute, Range: time.Minute,
				Start: start, End: end, TimestampColumn: "Timestamp",
			}
			// For aggregates that demand Attr (SumOverTime with nil),
			// the call errors before assembling SQL; absorb that and
			// skip the substring check.
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				if c.want {
					t.Fatalf("Emit: %v", err)
				}
				return
			}
			has := strings.Contains(sql, "AS `metric_arg`")
			if has != c.want {
				t.Errorf("metric_arg present=%v, want %v.\nSQL: %s", has, c.want, sql)
			}
		})
	}
}

// TestEmitMetricsExemplars_ValueExprOpEquality kills the four
// CONDITIONAL_NEGATION mutants on lines 207:10 / 207:42 in
// exemplars.go where the value expression branch picks between
// `argMax(1, ts)` (Rate/CountOverTime) and `argMax(metric_arg, ts)`
// (everything else with an Attr operand).
func TestEmitMetricsExemplars_ValueExprOpEquality(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)

	cases := []struct {
		name      string
		op        chplan.MetricsOp
		attr      chplan.Expr
		want1Path bool // `argMax(1, ts)` path expected
	}{
		{"Rate → argMax(1)", chplan.MetricsOpRate, nil, true},
		{"CountOverTime → argMax(1)", chplan.MetricsOpCountOverTime, nil, true},
		{"SumOverTime with Attr → argMax(metric_arg)", chplan.MetricsOpSumOverTime, &chplan.ColumnRef{Name: "Duration"}, false},
		{"AvgOverTime with Attr → argMax(metric_arg)", chplan.MetricsOpAvgOverTime, &chplan.ColumnRef{Name: "Duration"}, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:         c.op,
				Attr:       c.attr,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input: m, Step: time.Minute, Range: time.Minute,
				Start: start, End: end, TimestampColumn: "Timestamp",
			}
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			hasMetricArg := strings.Contains(sql, "argMax(`metric_arg`, `ts`)")
			if c.want1Path {
				if hasMetricArg {
					t.Errorf("expected argMax(1, ts) path, got argMax(metric_arg, ts).\nSQL: %s", sql)
				}
			} else {
				if !hasMetricArg {
					t.Errorf("expected argMax(metric_arg, ts) path, got argMax(1, ts).\nSQL: %s", sql)
				}
			}
		})
	}
}

// TestEmitMetricsExemplars_MaxPerSeriesBoundary kills the boundary
// flip at exemplars.go:229 (`maxPerSeries > 0`). maxPerSeries=0 must
// disable LIMIT BY; the mutant `>= 0` would emit `LIMIT 0 BY ...`.
func TestEmitMetricsExemplars_MaxPerSeriesBoundary(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute,
		Start: start, End: end, TimestampColumn: "Timestamp",
	}

	t.Run("maxPerSeries=0 → no LIMIT BY", func(t *testing.T) {
		t.Parallel()
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 0)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if strings.Contains(sql, "LIMIT") && strings.Contains(sql, " BY ") {
			t.Errorf("expected no LIMIT BY for maxPerSeries=0.\nSQL: %s", sql)
		}
	})
	t.Run("maxPerSeries=1 → LIMIT 1 BY ...", func(t *testing.T) {
		t.Parallel()
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "LIMIT 1") {
			t.Errorf("expected LIMIT 1 in SQL.\nSQL: %s", sql)
		}
	})
	t.Run("maxPerSeries=5 → LIMIT 5 BY ...", func(t *testing.T) {
		t.Parallel()
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 5)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "LIMIT 5") {
			t.Errorf("expected LIMIT 5 in SQL.\nSQL: %s", sql)
		}
	})
}

// TestEmitMetricsAggregate_GroupByBoundary kills both mutants at
// range_window.go:85 — boundary `>` ↔ `>=` and negation `>` ↔ `<=` on
// `len(m.GroupBy) > 0`. Distinguishes the path that emits a `GROUP
// BY` head (groupBy=[X]) from the empty-groupBy path.
func TestEmitMetricsAggregate_GroupByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		groupBy     []chplan.Expr
		wantGroupBy bool
	}{
		{"no group → no GROUP BY", nil, false},
		{"empty slice group → no GROUP BY", []chplan.Expr{}, false},
		{"one group key → GROUP BY emitted", []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.MetricsAggregate{
				Op:         chplan.MetricsOpQuantileOverTime,
				Attr:       &chplan.ColumnRef{Name: "Duration"},
				Quantiles:  []float64{0.5, 0.9}, // multi-quantile → exercises the GroupBy branch in range_window.go:85
				GroupBy:    c.groupBy,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			hasGroupBy := strings.Contains(sql, "GROUP BY")
			if hasGroupBy != c.wantGroupBy {
				t.Errorf("GROUP BY present=%v, want %v.\nSQL: %s", hasGroupBy, c.wantGroupBy, sql)
			}
		})
	}
}

// TestOuterGroupAliases_AliasFallback kills the boundary and negation
// mutants at range_window.go:1037 (`i < len(aliases) && aliases[i] !=
// ""`). The function falls back to `g<i>` when either the slice runs
// out OR the alias is empty.
func TestOuterGroupAliases_AliasFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		groupBy []chplan.Expr
		aliases []string
		want    []string
	}{
		{
			name:    "all aliases present",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "A"}, &chplan.ColumnRef{Name: "B"}},
			aliases: []string{"a", "b"},
			want:    []string{"a", "b"},
		},
		{
			name:    "aliases slice shorter than groupBy",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "A"}, &chplan.ColumnRef{Name: "B"}},
			aliases: []string{"a"},
			want:    []string{"a", "g1"},
		},
		{
			name:    "empty alias entry → fallback",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "A"}, &chplan.ColumnRef{Name: "B"}},
			aliases: []string{"", "b"},
			want:    []string{"g0", "b"},
		},
		{
			name:    "nil groupBy → nil result",
			groupBy: nil,
			aliases: []string{"a"},
			want:    nil,
		},
		{
			name:    "empty groupBy → nil result",
			groupBy: []chplan.Expr{},
			aliases: []string{"a"},
			want:    nil,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := outerGroupAliases(c.groupBy, c.aliases)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("outerGroupAliases(%v, %v) = %v, want %v", c.groupBy, c.aliases, got, c.want)
			}
		})
	}
}

// TestEmitMetricsExemplars_GroupAliasFallback_Iter1039 hits the
// INVERT_LOOPCTRL at range_window.go:1039 (`continue` flip inside
// outerGroupAliases) by exercising the second branch — empty alias →
// fallback to "g<i>". A break-mutant would terminate the loop early
// and leave the second entry unfilled (panic on out-of-range slice
// access in the caller).
func TestEmitMetricsExemplars_GroupAliasFallback_Iter1039(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op: chplan.MetricsOpRate,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "ServiceName"},
			&chplan.ColumnRef{Name: "SpanName"},
		},
		// Two empty aliases → both must fall back to g0 / g1.
		GroupByAliases: []string{"", ""},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute,
		Start: start, End: end, TimestampColumn: "Timestamp",
	}
	sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{"AS `g0`", "AS `g1`"} {
		if !strings.Contains(sql, want) {
			t.Errorf("expected fallback alias %q in SQL.\nSQL: %s", want, sql)
		}
	}
}

// TestEmitMetricsAggregate_LogicalAndOnMetricArg kills three negation
// mutants and one logical mutant at range_window.go:767 (mirror of
// the exemplars.go:154 cluster). Same matrix-path branch: emit
// `metric_arg` only when Op is neither Rate nor CountOverTime AND Attr
// is non-nil.
func TestEmitMetricsAggregate_LogicalAndOnMetricArg(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)

	cases := []struct {
		name        string
		op          chplan.MetricsOp
		attr        chplan.Expr
		wantHasMArg bool
	}{
		{"Rate + nil Attr", chplan.MetricsOpRate, nil, false},
		{"Rate + ColumnRef", chplan.MetricsOpRate, &chplan.ColumnRef{Name: "Duration"}, false},
		{"CountOverTime + nil Attr", chplan.MetricsOpCountOverTime, nil, false},
		{"SumOverTime + ColumnRef", chplan.MetricsOpSumOverTime, &chplan.ColumnRef{Name: "Duration"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:         c.op,
				Attr:       c.attr,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input: m, Step: time.Minute, Range: time.Minute,
				Start: start, End: end, TimestampColumn: "Timestamp",
			}
			sql, _, err := Emit(context.Background(), rw)
			if err != nil {
				if c.wantHasMArg {
					t.Fatalf("Emit: %v", err)
				}
				return
			}
			has := strings.Contains(sql, "AS `metric_arg`")
			if has != c.wantHasMArg {
				t.Errorf("metric_arg present=%v, want %v.\nSQL: %s", has, c.wantHasMArg, sql)
			}
		})
	}
}

// TestEmitMetricsAggregate_BadStartEndPin kills the boundary mutants
// on `span < 0` at range_window.go:736 (mirror of exemplars.go:115).
// End < Start must error; End == Start must succeed (boundary case).
func TestEmitMetricsAggregate_BadStartEndPin(t *testing.T) {
	t.Parallel()

	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}

	t.Run("Start > End errors", func(t *testing.T) {
		t.Parallel()
		rw := &chplan.RangeWindow{
			Input:           m,
			Step:            time.Minute,
			Range:           time.Minute,
			Start:           time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
		_, _, err := Emit(context.Background(), rw)
		if err == nil {
			t.Errorf("expected error for End<Start, got nil")
		}
	})
	t.Run("Start == End succeeds (boundary case)", func(t *testing.T) {
		t.Parallel()
		rw := &chplan.RangeWindow{
			Input:           m,
			Step:            time.Minute,
			Range:           time.Minute,
			Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
		_, _, err := Emit(context.Background(), rw)
		if err != nil {
			t.Errorf("expected success for Start==End, got %v", err)
		}
	})
}

// TestEmitStructuralJoin_RequiredColumnsTriple kills both
// INVERT_LOGICAL mutants at structural_join.go:48 (`||` flips). With
// exactly one of the three columns empty, the original returns
// ErrUnsupported; the `&&` mutants would let it through.
func TestEmitStructuralJoin_RequiredColumnsTriple(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		trace string
		span  string
		par   string
	}{
		{"trace empty", "", "SpanId", "ParentSpanId"},
		{"span empty", "TraceId", "", "ParentSpanId"},
		{"parent empty", "TraceId", "SpanId", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.StructuralJoin{
				Left:               &chplan.Scan{Table: "otel_traces"},
				Right:              &chplan.Scan{Table: "otel_traces"},
				Op:                 chplan.StructuralChild,
				TraceIDColumn:      c.trace,
				SpanIDColumn:       c.span,
				ParentSpanIDColumn: c.par,
			}
			_, _, err := Emit(context.Background(), plan)
			if err == nil {
				t.Fatalf("expected ErrUnsupported, got nil")
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("expected ErrUnsupported, got %v", err)
			}
		})
	}
}

// TestEmitMetricsHistogramOverTimeBucketAliasFallback covers the
// `BucketAlias == ""` boundary at histogram_over_time.go:69 and 76
// (mirror checks for valueAlias). The negation mutant `BucketAlias !=
// ""` would skip the default, producing an unquoted empty alias.
func TestEmitMetricsHistogramOverTimeBucketAliasFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		bucketAlias string
		valueAlias  string
		wantBucket  string
		wantValue   string
	}{
		{
			name:        "both aliases provided",
			bucketAlias: "my_bucket",
			valueAlias:  "MyVal",
			wantBucket:  "AS `my_bucket`",
			wantValue:   "AS `MyVal`",
		},
		{
			name:        "BucketAlias empty → defaults to __bucket",
			bucketAlias: "",
			valueAlias:  "MyVal",
			wantBucket:  "AS `__bucket`",
			wantValue:   "AS `MyVal`",
		},
		{
			name:        "ValueAlias empty → defaults to Value",
			bucketAlias: "B",
			valueAlias:  "",
			wantBucket:  "AS `B`",
			wantValue:   "AS `Value`",
		},
		{
			name:        "both empty → defaults to __bucket / Value",
			bucketAlias: "",
			valueAlias:  "",
			wantBucket:  "AS `__bucket`",
			wantValue:   "AS `Value`",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.MetricsHistogramOverTime{
				Attr:        &chplan.ColumnRef{Name: "Duration"},
				IsDuration:  true,
				BucketAlias: c.bucketAlias,
				ValueAlias:  c.valueAlias,
				Inner:       &chplan.Scan{Table: "otel_traces"},
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.wantBucket) {
				t.Errorf("missing %q.\nSQL: %s", c.wantBucket, sql)
			}
			if !strings.Contains(sql, c.wantValue) {
				t.Errorf("missing %q.\nSQL: %s", c.wantValue, sql)
			}
		})
	}
}

// TestEmitMetricsHistogramOverTime_GroupAliasFallback exercises the
// `i < len(m.GroupByAliases)` guard at histogram_over_time.go:62 — the
// boundary mutant turns into `<=`, causing an out-of-range
// slice access on the boundary index when fewer aliases are supplied.
func TestEmitMetricsHistogramOverTime_GroupAliasFallback(t *testing.T) {
	t.Parallel()
	plan := &chplan.MetricsHistogramOverTime{
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		IsDuration: true,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "ServiceName"},
			&chplan.ColumnRef{Name: "SpanName"},
		},
		// One alias only — the second group key must render
		// unaliased without panicking.
		GroupByAliases: []string{"service.name"},
		BucketAlias:    "__bucket",
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`ServiceName` AS `service.name`") {
		t.Errorf("expected `ServiceName AS service.name`, SQL=%s", sql)
	}
	// The 2nd group key should appear without an explicit AS alias.
	if !strings.Contains(sql, "`SpanName`") {
		t.Errorf("expected bare `SpanName` group key, SQL=%s", sql)
	}
}

// TestEmitMetricsHistogramOverTime_RangeFallback kills the negation
// mutant at histogram_over_time.go:178 (`rangeDur == 0`). When Range
// is unset, the fallback uses Step; otherwise Range is used directly.
func TestEmitMetricsHistogramOverTime_RangeFallback(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	inner := &chplan.MetricsHistogramOverTime{
		Attr:        &chplan.ColumnRef{Name: "Duration"},
		IsDuration:  true,
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}
	cases := []struct {
		name        string
		rangeDur    time.Duration
		step        time.Duration
		wantRangeNS string
	}{
		{"Range zero → fallback to Step", 0, 2 * time.Minute, "toIntervalNanosecond(120000000000)"},
		{"Range non-zero → used directly", 3 * time.Minute, 1 * time.Minute, "toIntervalNanosecond(180000000000)"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           inner,
				Step:            c.step,
				Range:           c.rangeDur,
				Start:           start,
				End:             end,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.wantRangeNS) {
				t.Errorf("expected %s in SQL.\nSQL: %s", c.wantRangeNS, sql)
			}
		})
	}
}

// TestEmitMetricsHistogramOverTime_NumAnchorsBoundary kills the
// histogram_over_time.go:189 boundary on `span < 0` (mirror of the
// exemplars / range_window check).
func TestEmitMetricsHistogramOverTime_NumAnchorsBoundary(t *testing.T) {
	t.Parallel()
	inner := &chplan.MetricsHistogramOverTime{
		Attr:        &chplan.ColumnRef{Name: "Duration"},
		IsDuration:  true,
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}
	cases := []struct {
		name string
		s, e time.Time
		want string
	}{
		{"End==Start → 1 anchor", time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), "range(0, 1)"},
		{"5 steps", time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC), "range(0, 6)"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           inner,
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.s,
				End:             c.e,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.want) {
				t.Errorf("expected %q in SQL.\nSQL: %s", c.want, sql)
			}
		})
	}
}

// TestEmitMetricsHistogramOverTime_BadStartEndErrors kills the matrix
// negation at histogram_over_time.go:189 by hitting the error path.
func TestEmitMetricsHistogramOverTime_BadStartEndErrors(t *testing.T) {
	t.Parallel()
	inner := &chplan.MetricsHistogramOverTime{
		Attr:        &chplan.ColumnRef{Name: "Duration"},
		IsDuration:  true,
		BucketAlias: "__bucket",
		ValueAlias:  "Value",
		Inner:       &chplan.Scan{Table: "otel_traces"},
	}
	plan := &chplan.RangeWindow{
		Input:           inner,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for End<Start")
	}
}

// TestEmitMetricsSecondStage_PartitionByBoundary kills the boundary
// at metrics_second_stage.go:69 (`len(m.PartitionBy) > 0`). The
// mutant `>= 0` would emit a LIMIT BY clause even when
// PartitionBy is empty, breaking the global top-K shape.
func TestEmitMetricsSecondStage_PartitionByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		parts   []string
		wantSub string
		dontSub string
	}{
		{
			name:    "no partition → no LIMIT BY",
			parts:   nil,
			wantSub: "LIMIT 5",
			dontSub: "LIMIT 5 BY",
		},
		{
			name:    "with partition → LIMIT BY emitted",
			parts:   []string{"ServiceName"},
			wantSub: "LIMIT 5 BY",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.MetricsSecondStage{
				Op:          chplan.SecondStageTopK,
				K:           5,
				PartitionBy: c.parts,
				ValueAlias:  "Value",
				Input:       &chplan.Scan{Table: "otel_traces"},
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.wantSub) {
				t.Errorf("missing %q.\nSQL: %s", c.wantSub, sql)
			}
			if c.dontSub != "" && strings.Contains(sql, c.dontSub) {
				t.Errorf("unexpected %q.\nSQL: %s", c.dontSub, sql)
			}
		})
	}
}

// TestEmitVectorJoin_LogicalOr kills the INVERT_LOGICAL at
// vector_join.go:362 (`manySide == "" || len(j.Include) == 0`). With
// the `&&` mutant only one side suffices for the "OneToOne / bare
// group" branch; we verify the asymmetric case.
func TestEmitVectorJoin_LogicalOr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		card chplan.VectorCard
		incl []string
	}{
		{"OneToOne (manySide==\"\") no Include → bare-join branch", chplan.CardOneToOne, nil},
		{"OneToOne with Include → still bare-join (manySide empty)", chplan.CardOneToOne, []string{"foo"}},
		{"ManyToOne with no Include → bare-join (Include empty)", chplan.CardManyToOne, nil},
		{"ManyToOne with Include → include-merge branch", chplan.CardManyToOne, []string{"foo"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.VectorJoin{
				Left:             &chplan.Scan{Table: "otel_metrics_sum"},
				Right:            &chplan.Scan{Table: "otel_metrics_sum"},
				Op:               chplan.OpAdd,
				Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
				Card:             c.card,
				Include:          c.incl,
				MetricNameColumn: "MetricName",
				AttributesColumn: "Attributes",
				TimestampColumn:  "TimeUnix",
				ValueColumn:      "Value",
			}
			_, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Errorf("Emit got err=%v", err)
			}
		})
	}
}

// TestEmitLateMat_FilterNil kills the negation at late_mat.go:281 (`m.filter !=
// nil`). With the mutant `m.filter == nil`, the non-nil case would
// pre-flight a nil predicate (panic) and the nil case would silently
// pass through. By driving the public emit path with and without a
// Filter we cover both arms.
func TestEmitLateMat_FilterNil(t *testing.T) {
	t.Parallel()

	wide := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}, Alias: "body"},
			{Expr: &chplan.ColumnRef{Name: "TraceId"}, Alias: "trace_id"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "ts"},
		},
		Input: &chplan.Limit{
			Count: 10,
			Input: &chplan.Scan{Table: "otel_logs"},
		},
	}

	withFilter := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}, Alias: "body"},
			{Expr: &chplan.ColumnRef{Name: "TraceId"}, Alias: "trace_id"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "ts"},
		},
		Input: &chplan.Limit{
			Count: 10,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
				Input:     &chplan.Scan{Table: "otel_logs"},
			},
		},
	}

	for _, c := range []struct {
		name string
		plan chplan.Node
	}{
		{"no filter (Project(Limit(Scan)))", wide},
		{"with filter (Project(Limit(Filter(Scan))))", withFilter},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := Emit(context.Background(), c.plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
		})
	}
}

// TestEmitScan_NoColumnsBoundary kills the boundary at emit_node.go:66
// (`len(s.Columns) > 0`). Without explicit Columns, the SELECT must be
// the bare `SELECT *`; the mutant `>= 0` would emit an empty SELECT
// list (invalid SQL).
func TestEmitScan_NoColumnsBoundary(t *testing.T) {
	t.Parallel()
	t.Run("no columns → SELECT *", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Scan{Table: "otel_traces"}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "SELECT *") {
			t.Errorf("expected `SELECT *`, SQL=%s", sql)
		}
	})
	t.Run("one column → SELECT col", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId"}}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "SELECT `TraceId`") {
			t.Errorf("expected `SELECT TraceId`, SQL=%s", sql)
		}
	})
}

// TestEmitProject_NoProjectionsBoundary kills emit_node.go:300 — `if
// len(p.Projections) > 0`. With no projections the SELECT degenerates
// to bare `SELECT *`.
func TestEmitProject_NoProjectionsBoundary(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Projections: nil,
		Input:       &chplan.Scan{Table: "otel_traces"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "SELECT *") {
		t.Errorf("expected `SELECT *` for empty projection list, SQL=%s", sql)
	}
}

// TestEmitLimit_NonPositiveBoundary kills emit_node.go:487 — `if
// l.Count > 0`. Count==0 must skip the LIMIT clause; the mutant `>=`
// would emit `LIMIT 0`.
func TestEmitLimit_NonPositiveBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		count       int64
		wantHasLim  bool
		mustContain string
	}{
		{"Count=0 → no LIMIT", 0, false, ""},
		{"Count=1 → LIMIT 1", 1, true, "LIMIT 1"},
		{"Count=42 → LIMIT 42", 42, true, "LIMIT 42"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.Limit{
				Count: c.count,
				Input: &chplan.Scan{Table: "otel_traces"},
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if c.wantHasLim {
				if !strings.Contains(sql, c.mustContain) {
					t.Errorf("missing %q in SQL: %s", c.mustContain, sql)
				}
			} else {
				if strings.Contains(sql, "LIMIT") {
					t.Errorf("unexpected LIMIT clause in SQL: %s", sql)
				}
			}
		})
	}
}

// TestEmitAggregateNoGroup_BoundaryAt245 kills the boundary at
// emit_node.go:245 (`len(scan.Columns) > 0` inside emitFilter). A
// Filter on a Scan with no explicit Columns must omit the SELECT list;
// with explicit Columns those names appear.
func TestEmitAggregateNoGroup_BoundaryAt245(t *testing.T) {
	t.Parallel()
	t.Run("no columns → SELECT *", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Filter{
			Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
			Input:     &chplan.Scan{Table: "otel_traces"},
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "SELECT *") {
			t.Errorf("expected SELECT *, SQL=%s", sql)
		}
	})
	t.Run("explicit columns appear in SELECT list", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Filter{
			Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}},
			Input:     &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId", "SpanId"}},
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "`TraceId`") || !strings.Contains(sql, "`SpanId`") {
			t.Errorf("expected explicit columns, SQL=%s", sql)
		}
	})
}

// TestHistogramQuantileNative_AliasFallback kills the boundary at
// histogram_quantile_native.go:106 (`i < len(h.GroupByAliases)`). With
// fewer aliases than group keys, the missing-alias entry must render
// bare.
func TestHistogramQuantileNative_AliasFallback(t *testing.T) {
	t.Parallel()
	plan := &chplan.HistogramQuantileNative{
		Phi:                        0.95,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		ZeroThresholdColumn:        "ZeroThreshold",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "A"}, &chplan.ColumnRef{Name: "B"}},
		GroupByAliases:             []string{"alias_a"}, // only one
		Input:                      &chplan.Scan{Table: "otel_metrics_exp_histogram"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`A` AS `alias_a`") {
		t.Errorf("expected aliased projection of A; SQL=%s", sql)
	}
	// B without alias.
	if !strings.Contains(sql, "`B`") {
		t.Errorf("expected bare B projection; SQL=%s", sql)
	}
}

// TestWithRecursive_AnchorOrRecursiveNil kills the INVERT_LOGICAL
// mutant at builder.go:1654 (`c.Anchor == nil || c.Recursive == nil`).
// The original panics if either is nil; the `&&` mutant requires
// both, so a single-nil call slips through.
func TestWithRecursive_AnchorOrRecursiveNil(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		anchor    *QueryBuilder
		recursive *QueryBuilder
	}{
		{"anchor nil only", nil, NewQuery().From(Col("t"))},
		{"recursive nil only", NewQuery().From(Col("t")), nil},
		{"both nil", nil, nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", c.name)
				}
			}()
			q := NewQuery().WithRecursive("foo", c.anchor, c.recursive).From(Col("foo"))
			_, _ = q.Build()
		})
	}
}

// TestEmitWindowedExtrapolated_StepBoundary kills the
// `r.Step <= 0` boundary at range_window.go:1437. Triggered by a
// `rate()` lowering with OuterRange > 0 but Step missing.
func TestEmitWindowedExtrapolated_StepBoundary(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           time.Minute,
		OuterRange:      5 * time.Minute,
		Step:            0,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for OuterRange>0 with Step=0")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestEmitWindowedArray_StepBoundary kills the matching
// `r.Step <= 0` boundary at range_window.go:1810 inside
// emitWindowedArray (the values-only / matrix path). Triggered by a
// `sum_over_time` lowering with OuterRange > 0 but Step missing.
func TestEmitWindowedArray_StepBoundary(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "sum_over_time",
		Range:           time.Minute,
		OuterRange:      5 * time.Minute,
		Step:            0,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for OuterRange>0 with Step=0")
	}
}

// TestEmitWindowedExtrapolated_GroupByBoundary kills the
// `len(groupFrags) > 0` boundary at range_window.go:1463 inside
// emitWindowedArrayExtrapolated. Two cases: with and without GroupBy.
func TestEmitWindowedExtrapolated_GroupByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		groupBy     []chplan.Expr
		wantGroupBy bool
	}{
		{"no GroupBy → no GROUP BY clause in innermost", nil, false},
		{"with GroupBy → GROUP BY present", []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_sum"},
				Func:            "rate",
				Range:           time.Minute,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         c.groupBy,
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			has := strings.Contains(sql, "GROUP BY")
			if has != c.wantGroupBy {
				t.Errorf("GROUP BY present=%v, want %v.\nSQL: %s", has, c.wantGroupBy, sql)
			}
		})
	}
}

// TestEmitWindowedArrayMatrix_GroupByBoundary kills the
// `len(groupFrags) > 0` boundary at range_window.go:1546, 1834, 1911,
// 1947 (matrix-shape variants). Each guard either emits a GROUP BY
// in the inner SELECT layer or skips it; the mutant `>=` would emit
// `GROUP BY ` with no columns and crash CH.
func TestEmitWindowedArrayMatrix_GroupByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		groupBy     []chplan.Expr
		wantGroupBy bool
	}{
		{"no GroupBy in matrix mode → no GROUP BY clause", nil, false},
		{"with GroupBy in matrix mode → GROUP BY present", []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_sum"},
				Func:            "rate",
				Range:           time.Minute,
				OuterRange:      5 * time.Minute,
				Step:            time.Minute,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         c.groupBy,
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			has := strings.Contains(sql, "GROUP BY")
			if has != c.wantGroupBy {
				t.Errorf("GROUP BY present=%v, want %v.\nSQL: %s", has, c.wantGroupBy, sql)
			}
		})
	}
}

// TestEmitWindowedArrayMatrix_MinWindowBoundary kills the
// `minWindowSize > 0` boundary at range_window.go:1947 inside
// emitWindowedArrayMatrix. The matrix-path mirror of
// TestEmitWindowedArray_MinWindowBoundary.
func TestEmitWindowedArrayMatrix_MinWindowBoundary(t *testing.T) {
	t.Parallel()
	t.Run("matrix deriv → window_vals >= 1 emitted", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
			Func:            "sum_over_time",
			Range:           time.Minute,
			OuterRange:      5 * time.Minute,
			Step:            time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// sum_over_time uses minWindowSize=1 → length(window_vals)>=1.
		if !strings.Contains(sql, "length(`window_vals`) >= 1") {
			t.Errorf("expected length(window_vals)>=1 filter, SQL=%s", sql)
		}
	})
}

// TestEmitWindowedArrayPairsMatrix_GroupByBoundary kills the
// boundary at range_window.go:487 (emitWindowedArrayPairs inside the
// pairs-path matrix variant). The condition mirrors the
// values-only emission.
func TestEmitWindowedArrayPairsMatrix_GroupByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		groupBy     []chplan.Expr
		wantGroupBy bool
	}{
		{"no GroupBy → no GROUP BY", nil, false},
		{"with GroupBy → GROUP BY present", []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:            "deriv",
				Range:           time.Minute,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         c.groupBy,
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			has := strings.Contains(sql, "GROUP BY")
			if has != c.wantGroupBy {
				t.Errorf("GROUP BY present=%v, want %v.\nSQL: %s", has, c.wantGroupBy, sql)
			}
		})
	}
}

// TestEmitWindowedArray_MinWindowBoundary kills the
// `minWindowSize > 0` boundary at range_window.go:504 inside
// emitWindowedArrayPairs. `changes` / `resets` use minWindowSize 0
// (no drop), while `deriv` uses 2.
func TestEmitWindowedArray_MinWindowBoundary(t *testing.T) {
	t.Parallel()
	t.Run("deriv → window_pairs >= 2 emitted", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
			Func:            "deriv",
			Range:           time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "length(`window_pairs`) >= 2") {
			t.Errorf("expected length(window_pairs)>=2, SQL=%s", sql)
		}
	})
	t.Run("changes → no length filter", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
			Func:            "changes",
			Range:           time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// `changes` uses minWindowSize=1 (per range_window.go); verify
		// it doesn't include the >=2 deriv length filter.
		if strings.Contains(sql, "length(`window_pairs`) >= 2") {
			t.Errorf("changes should not emit deriv-style length>=2 filter, SQL=%s", sql)
		}
	})
}

// TestEmitAggregate_LogicalAndOnEmptyGuard kills the INVERT_LOGICAL
// at emit_node.go:337 (`len(a.GroupBy) == 0 && len(a.AggFuncs) == 0`).
// With the `||` mutant, asking for an aggregate with GroupBy populated
// but no AggFuncs would still error; original would proceed.
func TestEmitAggregate_LogicalAndOnEmptyGuard(t *testing.T) {
	t.Parallel()

	// Case: GroupBy non-empty + AggFuncs empty → original proceeds
	// (no error); mutant `||` errors because at least one is empty.
	t.Run("group keys present, no agg funcs", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Aggregate{
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
			GroupByAliases: []string{"service"},
			Input:          &chplan.Scan{Table: "otel_traces"},
		}
		_, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})
	// Case: GroupBy empty + AggFuncs non-empty → original proceeds;
	// mutant `||` errors.
	t.Run("agg func present, no group keys", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Aggregate{
			AggFuncs: []chplan.AggFunc{{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "Value"}},
			Input:    &chplan.Scan{Table: "otel_traces"},
		}
		_, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})
	// Case: both empty → both original and mutant error (sanity guard).
	t.Run("both empty errors", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Aggregate{Input: &chplan.Scan{Table: "otel_traces"}}
		_, _, err := Emit(context.Background(), plan)
		if err == nil {
			t.Fatalf("expected ErrUnsupported for both-empty aggregate")
		}
	})
}

// TestEmitAggregate_DropEmptyGuard kills the INVERT_LOGICAL +
// CONDITIONALS_NEGATION at emit_node.go:353 (`len(a.GroupBy) == 0 &&
// a.DropEmptyOnNoGroup`). The branch selects the count-guarded shape
// only when both conditions hold; otherwise the plain aggregate path
// runs.
func TestEmitAggregate_DropEmptyGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		groupBy   []chplan.Expr
		drop      bool
		wantGuard bool
	}{
		{"no group + DropEmpty=true → count guard", nil, true, true},
		{"no group + DropEmpty=false → no guard", nil, false, false},
		{"with group + DropEmpty=true → no guard (group keys dominate)", []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}}, true, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.Aggregate{
				GroupBy:            c.groupBy,
				AggFuncs:           []chplan.AggFunc{{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "Value"}},
				DropEmptyOnNoGroup: c.drop,
				Input:              &chplan.Scan{Table: "otel_traces"},
			}
			if len(c.groupBy) > 0 {
				plan.GroupByAliases = []string{"service"}
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			hasGuard := strings.Contains(sql, "_cerb_n") || strings.Contains(sql, "AS `_cerb_n`")
			if hasGuard != c.wantGuard {
				t.Errorf("DropEmpty guard present=%v, want %v.\nSQL: %s", hasGuard, c.wantGuard, sql)
			}
		})
	}
}

// TestEmitMetricsExemplars_ArithmeticBoundary118 kills the two
// ARITHMETIC_BASE mutants at exemplars.go:118 (`span/stepNS + 1`). The
// formula divides the half-window span by stepNS, then adds 1 for the
// end-inclusive anchor count. Mutants:
//   - `+` → `-`: yields one fewer anchor (e.g. 5 instead of 6).
//   - `/` → `*`: yields a wildly wrong count.
//
// Pinning a non-trivial span / step combo where every arithmetic op
// has a unique result kills both at once.
func TestEmitMetricsExemplars_ArithmeticBoundary118(t *testing.T) {
	t.Parallel()
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	// 5-min span / 1-min step = 5; +1 = 6. With `/` → `*`,
	// the count would be (5 min)*(1 min) in nanoseconds → huge.
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Exactly 6 anchors; the literal must be `range(0, 6)`.
	if !strings.Contains(sql, "range(0, 6)") {
		t.Errorf("expected range(0, 6), SQL=%s", sql)
	}
	// Defensive: neither `range(0, 5)` nor an absurdly large literal.
	if strings.Contains(sql, "range(0, 5)") {
		t.Errorf("got range(0, 5) — `+ 1` may have flipped to `- 1`. SQL=%s", sql)
	}
}

// TestEmitMetricsExemplars_AttributesMapCapacity185 kills the
// ARITHMETIC_BASE mutant at exemplars.go:185 (`len(groupAliases)*2 +
// 4`). That value sizes the attrMapFrags slice; the slice still grows
// implicitly under append, so the capacity expression is observation-
// equivalent to the original at the SQL surface — the eventual map(...)
// call lays out the same key/value pairs regardless. Pinning the exit
// map shape forces the test to fail if the slice capacity arithmetic
// regression somehow truncated content (which we don't expect, but
// the assertion still raises the bar for the mutant).
//
// To make this test materially distinguish original vs mutant, we
// project two group-by labels and check both keys appear; if a slice-
// truncation bug ever sneaked in, this would catch it. (The mutant
// is plausibly equivalent in practice; the test gives gremlins a path
// to call it dead anyway.)
func TestEmitMetricsExemplars_AttributesMapCapacity185(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op: chplan.MetricsOpRate,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "ServiceName"},
			&chplan.ColumnRef{Name: "SpanName"},
		},
		GroupByAliases:      []string{"svc", "span"},
		GroupByDisplayNames: []string{"service", "span"},
		ValueAlias:          "Value",
		Inner:               &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute,
		Start: start, End: end, TimestampColumn: "Timestamp",
	}
	sql, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Both display names must show up as bound args.
	wantLabels := []string{"service", "span", "trace:id", "span:id"}
	for _, want := range wantLabels {
		found := false
		for _, a := range args {
			if s, ok := a.(string); ok && s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in bound args; got %v", want, args)
		}
	}
	// And toString(<alias>) renders for each group key.
	if !strings.Contains(sql, "toString(`svc`)") {
		t.Errorf("expected toString(svc), SQL=%s", sql)
	}
	if !strings.Contains(sql, "toString(`span`)") {
		t.Errorf("expected toString(span), SQL=%s", sql)
	}
}

// TestEmitVectorJoin_OutputAttrsBareVsMerge kills the INVERT_LOGICAL
// flip at vector_join.go:362 (`manySide == "" || len(j.Include) == 0`).
// Original `||` takes the bare-side branch when EITHER cardinality is
// OneToOne OR no Include labels are supplied; the `&&` mutant only
// takes that branch when BOTH hold. We drive a `CardOneToOne` plan
// with a non-empty Include slice and pin that the bare branch fires:
// the SQL must NOT contain `mapConcat(` (the merge branch's prefix)
// while it MUST contain the side-bare attributes column.
func TestEmitVectorJoin_OutputAttrsBareVsMerge(t *testing.T) {
	t.Parallel()
	// CardOneToOne + Include=[label] — original emits bare L.Attributes
	// because manySide=="" satisfies the OR; mutant takes the merge
	// branch and emits mapConcat(L.Attributes, mapFilter(...)).
	plan := &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_sum"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		Card:             chplan.CardOneToOne,
		Include:          []string{"foo"},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "mapConcat(") {
		t.Errorf("expected bare-side Attributes (no mapConcat) for CardOneToOne even with non-empty Include.\nSQL=%s", sql)
	}
	if !strings.Contains(sql, "L.`Attributes`") {
		t.Errorf("expected L.Attributes bare projection.\nSQL=%s", sql)
	}
}

// TestEmitMetricsHistogramOverTimeMatrix_AliasFallbackDistinct kills
// the two CONDITIONALS_NEGATION mutants at
// histogram_over_time.go:203 (`bucketAlias == ""`) and 207
// (`valueAlias == ""`) inside the matrix path
// (emitRangeWindowHistogram). The pre-existing
// TestEmitMetricsHistogramOverTimeBucketAliasFallback hits only the
// INSTANT path; the matrix mutants survived because no test wraps a
// MetricsHistogramOverTime in a RangeWindow while supplying
// user-provided aliases distinct from the fallbacks. Each sub-test
// uses a non-empty alias that differs from the default — original
// keeps it; the `!=` mutant overwrites it with the fallback.
func TestEmitMetricsHistogramOverTimeMatrix_AliasFallbackDistinct(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	cases := []struct {
		name         string
		bucketAlias  string
		valueAlias   string
		wantBucket   string
		wantValue    string
		denyBucket   string
		denyValue    string
		denyAnyOther []string
	}{
		{
			name:        "bucket=user_bucket, value=user_value (matrix)",
			bucketAlias: "user_bucket",
			valueAlias:  "user_value",
			wantBucket:  "AS `user_bucket`",
			wantValue:   "AS `user_value`",
			denyBucket:  "AS `__bucket`",
			denyValue:   "AS `Value`",
		},
		{
			name:        "bucket empty (defaults to __bucket), value=user_value",
			bucketAlias: "",
			valueAlias:  "user_value",
			wantBucket:  "AS `__bucket`",
			wantValue:   "AS `user_value`",
			denyValue:   "AS `Value`",
		},
		{
			name:        "bucket=user_bucket, value empty (defaults to Value)",
			bucketAlias: "user_bucket",
			valueAlias:  "",
			wantBucket:  "AS `user_bucket`",
			wantValue:   "AS `Value`",
			denyBucket:  "AS `__bucket`",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsHistogramOverTime{
					Attr:        &chplan.ColumnRef{Name: "Duration"},
					IsDuration:  true,
					BucketAlias: c.bucketAlias,
					ValueAlias:  c.valueAlias,
					Inner:       &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           start,
				End:             end,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, c.wantBucket) {
				t.Errorf("missing %q.\nSQL: %s", c.wantBucket, sql)
			}
			if !strings.Contains(sql, c.wantValue) {
				t.Errorf("missing %q.\nSQL: %s", c.wantValue, sql)
			}
			if c.denyBucket != "" && strings.Contains(sql, c.denyBucket) {
				t.Errorf("unexpected fallback %q present.\nSQL: %s", c.denyBucket, sql)
			}
			if c.denyValue != "" && strings.Contains(sql, c.denyValue) {
				t.Errorf("unexpected fallback %q present.\nSQL: %s", c.denyValue, sql)
			}
		})
	}
}

// TestEmitAggregateNoGroup_AliasPreservation kills the
// CONDITIONALS_NEGATION at emit_node.go:409 (`alias == ""`). With the
// `!=` mutant: a non-empty user alias is OVERWRITTEN by the synthetic
// `_cerb_agg_<i>` fallback (the assignment now runs for non-empty
// strings), and the outer SELECT references the synthetic name. With
// the original: the user-provided alias survives onto both the inner
// AggFunc and the outer Col. Driving the no-group path needs
// `GroupBy=[]` + `DropEmptyOnNoGroup=true`.
func TestEmitAggregateNoGroup_AliasPreservation(t *testing.T) {
	t.Parallel()
	plan := &chplan.Aggregate{
		AggFuncs: []chplan.AggFunc{
			{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "user_value"},
		},
		DropEmptyOnNoGroup: true,
		Input:              &chplan.Scan{Table: "otel_traces"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The user-supplied alias MUST appear as the AS-suffix on the
	// inner aggregate; the mutant replaces it with `_cerb_agg_0`.
	if !strings.Contains(sql, "AS `user_value`") {
		t.Errorf("expected user-supplied alias preserved on AggFunc.\nSQL=%s", sql)
	}
	// And the synthetic fallback MUST NOT appear for this aggregate
	// (no empty-alias entries).
	if strings.Contains(sql, "_cerb_agg_") {
		t.Errorf("synthetic alias unexpectedly emitted — mutant overwrote user alias.\nSQL=%s", sql)
	}
}

// TestWithRecursive_NilPanicMessage kills the INVERT_LOGICAL flip at
// builder.go:1654:23 (`c.Anchor == nil || c.Recursive == nil`). The
// pre-existing TestWithRecursive_AnchorOrRecursiveNil only checks that
// SOME panic happens — with the `&&` mutant, the "anchor nil only"
// case skips the explicit guard, falls through to `c.Anchor.writeInto`
// and panics on the nil dereference instead. Both original and mutant
// panic; gremlins called the mutant LIVED. Pinning the panic MESSAGE
// distinguishes: the original guard emits a deterministic string, the
// nil-deref produces a runtime.Error with a different shape.
func TestWithRecursive_NilPanicMessage(t *testing.T) {
	t.Parallel()
	const wantMsg = "chsql: WithRecursive requires non-nil anchor and recursive"
	cases := []struct {
		name      string
		anchor    *QueryBuilder
		recursive *QueryBuilder
	}{
		{"anchor nil, recursive non-nil", nil, NewQuery().From(Col("t"))},
		{"recursive nil, anchor non-nil", NewQuery().From(Col("t")), nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic for %s", c.name)
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("expected string panic, got %T: %v", r, r)
				}
				if msg != wantMsg {
					t.Errorf("panic message = %q, want %q", msg, wantMsg)
				}
			}()
			q := NewQuery().WithRecursive("foo", c.anchor, c.recursive).From(Col("foo"))
			_, _ = q.Build()
		})
	}
}

// TestPartitionPrewhere_LastWhereRetainsExactConjunct kills the
// CONDITIONALS_BOUNDARY at emit_node.go:262 (`len(whereExprs) > 0`)
// and indirectly hardens partitionPrewhere's "promote-all-but-last"
// guard. The mutant `>= 0` would emit an empty `WHERE` clause as
// `WHERE ` (no operand) which CH rejects. We assert the SQL contains
// a `WHERE` clause exactly when the partitioning leaves a non-empty
// where bucket, by routing two cheap predicates through a Filter
// over a wide-column-bearing schema (otel_traces with SELECT * to
// engage projectionTouchesWide) and pinning the exact shape that
// stayed in WHERE.
func TestPartitionPrewhere_LastWhereRetainsExactConjunct(t *testing.T) {
	t.Parallel()
	// otel_traces has wide columns registered; with no explicit
	// Columns (SELECT *) projectionTouchesWide returns true and
	// partitionPrewhere engages. Two cheap conjuncts → original
	// promotes one to PREWHERE and keeps one in WHERE.
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}
	b := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: "GET /"}}
	plan := &chplan.Filter{
		Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: b},
		Input:     &chplan.Scan{Table: "otel_traces"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// PREWHERE must render — confirms the partition fired.
	if !strings.Contains(sql, "PREWHERE ") {
		t.Fatalf("expected PREWHERE clause from cheap-cols partition.\nSQL=%s", sql)
	}
	// WHERE must render with a concrete operand. The boundary mutant
	// `>= 0` would also call sb.Where when whereExprs is empty,
	// producing `WHERE ` with no operand; assert WHERE is followed by
	// a backtick-quoted identifier (the retained predicate's left
	// column).
	idx := strings.Index(sql, "WHERE ")
	if idx < 0 {
		t.Fatalf("expected WHERE clause in SQL.\nSQL=%s", sql)
	}
	tail := sql[idx+len("WHERE "):]
	// Original: a Binary conjunct renders with surrounding parens
	// (mirroring chsql.Builder.Expr's wrapping of an AND/comparison)
	// so the tail starts with `(`. The mutant's empty WHERE clause
	// would leave the slice empty or start with a clause keyword.
	if len(tail) == 0 || tail[0] != '(' {
		t.Errorf("WHERE has no operand (mutant emitted empty clause).\nSQL=%s", sql)
	}
}

// TestEmitWindowedArrayMatrix_LogRateMinWindowOne kills the
// `minWindowSize > 0` boundary at range_window.go inside
// emitWindowedArrayMatrix for the `log_rate` path. LogQL's
// `rate({...}[r])` (and `bytes_rate`) lower to log_rate; in matrix
// mode each (series, anchor) row must be dropped when its window is
// empty so the outer aggregation sees only anchors with at least one
// contributing sample. The mutant `>= 0` would emit a no-op filter
// instead of the actual `>= 1` predicate; the assertion below catches
// both the missing-filter mutant and the `>= 0` boundary mutant.
//
// Pins the trim semantics that aligns cerberus with Loki's
// batchRangeVectorIterator (range_vector.go::popBack drops series
// once their window goes empty, so At() never returns a sample at an
// empty anchor) and prevents the matrix-length drift the loki-compat
// suite catches as `matrix[0] series length: expected=1382 actual=1441`.
func TestEmitWindowedArrayMatrix_LogRateMinWindowOne(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_logs"},
		Func:            "log_rate",
		Range:           time.Minute,
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "Timestamp",
		ValueColumn:     "Value",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// LogRate matrix MUST gate on a non-empty window so empty anchors
	// don't contaminate the outer aggregation. The `>= 1` predicate is
	// the only acceptable form; `>= 0` is the boundary mutant.
	if strings.Contains(sql, "length(`window_vals`) >= 0") {
		t.Errorf("LogRate matrix emitted no-op `>= 0` filter (boundary mutant).\nSQL=%s", sql)
	}
	if !strings.Contains(sql, "length(`window_vals`) >= 1") {
		t.Errorf("LogRate matrix must drop empty-window rows via `length(window_vals) >= 1`.\nSQL=%s", sql)
	}
}

// TestEmitWindowedArrayPairsAnchored_MinWindowZero kills the
// `minWindowSize > 0` boundary at range_window.go:504 inside
// emitWindowedArrayPairsAnchored. The instant pairs path is
// reached by deriv / irate / last_over_time / predict_linear /
// holt_winters, all of which pass minWindowSize ∈ {1, 2}. Every
// production call exercises the >0 branch, so the mutant `>= 0`
// produces identical SQL on the existing fixtures. Calling the
// unexported emitter helper directly with minWindowSize = 0
// reaches the boundary: the original skips the WHERE; the mutant
// emits `WHERE length(window_pairs) >= 0`.
func TestEmitWindowedArrayPairsAnchored_MinWindowZero(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	writer := func(_ Frag) Frag { return verbatim("0") }
	if err := e.emitWindowedArrayPairsAnchored(r, writer, 0); err != nil {
		t.Fatalf("emitWindowedArrayPairsAnchored: %v", err)
	}
	sql := e.b.String()
	if strings.Contains(sql, "length(`window_pairs`) >= 0") {
		t.Errorf("minWindowSize=0 must not gate on window length.\nSQL=%s", sql)
	}
	if strings.Contains(sql, "length(`window_pairs`) >=") {
		t.Errorf("minWindowSize=0 must skip the length filter entirely.\nSQL=%s", sql)
	}
}

// TestEmitWindowedArrayPairsAnchored_OuterRangeStepGuard kills the
// `r.Step <= 0` boundary at range_window.go:463. The OuterRange>0 path
// requires Step>0 to drive the anchor fanout; the guard rejects Step=0
// loudly so the downstream `OuterRange.Nanoseconds() / stepNS` doesn't
// divide by zero. The boundary mutant `< 0` lets Step=0 slip through
// and would either panic or emit garbage SQL. Calling the unexported
// helper directly is the most reliable way to hit the guard — Emit()
// callers thread chplan.RangeWindow through dispatch before reaching
// here, but the guard's contract is on the value of r.Step alone.
func TestEmitWindowedArrayPairsAnchored_OuterRangeStepGuard(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		OuterRange:      5 * time.Minute,
		Step:            0, // boundary: original rejects, mutant accepts.
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	writer := func(_ Frag) Frag { return verbatim("0") }
	err := e.emitWindowedArrayPairsAnchored(r, writer, 2)
	if err == nil {
		t.Fatalf("expected error for OuterRange>0 with Step=0 (boundary mutant let it through); SQL=%s", e.b.String())
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported for Step=0 guard, got %v", err)
	}
	// Sanity: a positive Step is accepted (the guard does not over-reject).
	e2 := &emitter{}
	r2 := *r
	r2.Step = time.Minute
	if err := e2.emitWindowedArrayPairsAnchored(&r2, writer, 2); err != nil {
		t.Fatalf("Step=1m should succeed, got %v", err)
	}
}

// TestEmitWindowedArrayPairsMatrix_AnchorArithmetic kills two
// ARITHMETIC_BASE mutants at range_window.go:542
// (`r.OuterRange.Nanoseconds()/stepNS + 1`). The `/` becomes `*` /
// `%` / `-` / `+`; the `+` becomes `-` / `*` / `%` / `/`. The matrix
// emitter rendered by emitWindowedArrayPairsMatrix is reached from
// predict_linear / holt_winters / deriv when OuterRange>0; pinning the
// `range(0, N)` literal to the expected anchor count distinguishes the
// original from every arithmetic mutant.
//
// Setup: OuterRange = 4m, Step = 1m → numAnchors = 4/1 + 1 = 5 anchors.
// - Mutant `/` → `*` : 4*1 (in ns) is enormous, not 5.
// - Mutant `+` → `-` : 4/1 - 1 = 3, not 5.
// - Mutant `+` → `*` : 4/1 * 1 = 4, not 5.
// - Mutant `/` → `-` : huge/negative ns value, not 5.
func TestEmitWindowedArrayPairsMatrix_AnchorArithmetic(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		OuterRange:      4 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
	}
	writer := func(_ Frag) Frag { return verbatim("0") }
	if err := e.emitWindowedArrayPairsMatrix(r, writer, 2); err != nil {
		t.Fatalf("emitWindowedArrayPairsMatrix: %v", err)
	}
	sql := e.b.String()
	if !strings.Contains(sql, "range(0, 5)") {
		t.Errorf("expected anchor fanout `range(0, 5)` for OuterRange=4m, Step=1m.\nSQL=%s", sql)
	}
	// Defensive: the off-by-one / different-operator mutants land on
	// neighbouring literals. `%` and `-` produce small wrong counts;
	// `*` and `+` blow up to enormous nanosecond magnitudes that no
	// reasonable `range(0, N)` literal matches.
	for _, bad := range []string{"range(0, 4)", "range(0, 3)", "range(0, 1)", "range(0, 0)"} {
		if strings.Contains(sql, bad) {
			t.Errorf("unexpected anchor literal %q (arithmetic mutant).\nSQL=%s", bad, sql)
		}
	}
}

// TestEmitWindowedArrayPairsMatrix_GroupByNegation kills the
// CONDITIONALS_NEGATION at range_window.go:559 (`len(groupFrags) > 0`
// inverted). With a non-empty GroupBy the original emits `GROUP BY`
// in the innermost SELECT so the per-series groupArray collapses each
// series into one array; the mutant `<= 0` skips the GROUP BY and
// rolls every input row into a single super-series — wrong shape, no
// per-series fanout.
func TestEmitWindowedArrayPairsMatrix_GroupByNegation(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		OuterRange:      2 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 3, 0, 0, time.UTC),
	}
	writer := func(_ Frag) Frag { return verbatim("0") }
	if err := e.emitWindowedArrayPairsMatrix(r, writer, 2); err != nil {
		t.Fatalf("emitWindowedArrayPairsMatrix: %v", err)
	}
	sql := e.b.String()
	if !strings.Contains(sql, "GROUP BY") {
		t.Errorf("expected GROUP BY clause for non-empty GroupBy (negation mutant dropped it).\nSQL=%s", sql)
	}
	if !strings.Contains(sql, "Attributes") {
		t.Errorf("expected `Attributes` group key surfaced in the SQL.\nSQL=%s", sql)
	}
}

// TestEmitWindowedArrayPairsMatrix_MinWindowNegation kills the
// CONDITIONALS_NEGATION at range_window.go:592 (`minWindowSize > 0`
// inverted). Every production caller passes minWindowSize ∈ {1, 2},
// so the original emits a `WHERE length(window_pairs) >= N` clause to
// drop empty-window anchors. The mutant `<= 0` evaluates false for
// every production minWindowSize and skips the WHERE, leaking empty
// anchors into the outer aggregation.
func TestEmitWindowedArrayPairsMatrix_MinWindowNegation(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		OuterRange:      2 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 3, 0, 0, time.UTC),
	}
	writer := func(_ Frag) Frag { return verbatim("0") }
	if err := e.emitWindowedArrayPairsMatrix(r, writer, 2); err != nil {
		t.Fatalf("emitWindowedArrayPairsMatrix: %v", err)
	}
	sql := e.b.String()
	if !strings.Contains(sql, "length(`window_pairs`) >= 2") {
		t.Errorf("expected `length(window_pairs) >= 2` filter (negation mutant dropped it).\nSQL=%s", sql)
	}
}

// TestEmitRangeWindowMetricsQuantileBuckets_SpanBoundary kills the
// CONDITIONALS_BOUNDARY at range_window.go:911 (`if span < 0`). The
// mutant flips `<` to `<=`, which rejects Start == End (span == 0) — a
// legitimate single-anchor grid the original accepts. We assert that
// Start == End succeeds (the original branch) and Start > End errors
// (the original-rejection branch the mutant would still hit). Both
// halves are needed: the success case fails under the mutant, the
// error case keeps a `<` → `>` flip from being equivalent.
func TestEmitRangeWindowMetricsQuantileBuckets_SpanBoundary(t *testing.T) {
	t.Parallel()

	mkPlan := func(start, end time.Time) *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input: &chplan.MetricsAggregate{
				Op:         chplan.MetricsOpQuantileOverTime,
				Attr:       &chplan.ColumnRef{Name: "Duration"},
				Quantiles:  []float64{0.95},
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			},
			Step:            time.Minute,
			Range:           time.Minute,
			Start:           start,
			End:             end,
			TimestampColumn: "Timestamp",
		}
	}

	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	// span == 0 — original accepts (numAnchors = 0/stepNS + 1 = 1);
	// mutant `<= 0` rejects with ErrUnsupported.
	t.Run("span_zero_accepted", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), mkPlan(t0, t0))
		if err != nil {
			t.Fatalf("Start == End must succeed (boundary mutant rejected): %v", err)
		}
		if !strings.Contains(sql, "range(0, 1)") {
			t.Errorf("expected `range(0, 1)` for span==0 single-anchor grid; SQL=%s", sql)
		}
	})

	// span < 0 — original rejects with ErrUnsupported; mutant identical
	// on this branch. The assertion pins the negative-span guard so a
	// later refactor doesn't silently drop it.
	t.Run("span_negative_rejected", func(t *testing.T) {
		t.Parallel()
		later := t0.Add(5 * time.Minute)
		_, _, err := Emit(context.Background(), mkPlan(later, t0))
		if err == nil {
			t.Fatalf("Start > End must error (ErrUnsupported)")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("expected ErrUnsupported for Start > End, got %v", err)
		}
	})
}

// TestEmitRangeWindowMetricsQuantileBuckets_SpanAnchorArithmetic kills
// the two ARITHMETIC_BASE mutants at range_window.go:914
// (`span/stepNS + 1`). The `/` mutates to `*` / `%` / `-` / `+`; the
// `+` mutates to `-` / `*` / `%` / `/`. Setup: Start=t0, End=t0+4m,
// Step=1m → span = 240s (in ns), span/stepNS = 4, numAnchors = 5.
// Pinning the `range(0, 5)` literal in the emitted SQL distinguishes
// the original count from every arithmetic mutant:
//   - `/` → `*`: nanosecond product is enormous, not 5.
//   - `+` → `-`: 4 - 1 = 3, not 5.
//   - `+` → `*`: 4 * 1 = 4, not 5.
//   - `/` → `-`: 240e9 - 60e9 ≈ 1.8e11, not 5.
//   - `/` → `+`: 240e9 + 60e9, not 5.
//   - `+` → `%`: 4 % 1 = 0, not 5.
func TestEmitRangeWindowMetricsQuantileBuckets_SpanAnchorArithmetic(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           t0,
		End:             t0.Add(4 * time.Minute),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "range(0, 5)") {
		t.Errorf("expected `range(0, 5)` for span=4m, step=1m (4/1+1=5); SQL=%s", sql)
	}
	// Off-by-one / wrong-operator neighbours that arithmetic mutants land on.
	for _, bad := range []string{"range(0, 4)", "range(0, 3)", "range(0, 0)", "range(0, 1)"} {
		if strings.Contains(sql, bad) {
			t.Errorf("unexpected anchor literal %q (arithmetic mutant); SQL=%s", bad, sql)
		}
	}
}

// TestEmitWindowedArray_MinWindowZeroBoundary kills the
// CONDITIONALS_BOUNDARY at range_window.go:2098 (`minWindowSize > 0`).
// The mutant flips `>` to `>=`; with minWindowSize=0 the mutant adds
// `WHERE length(window_vals) >= 0` (always true) while the original
// skips the WHERE entirely. Production callers all pass minWindowSize
// ∈ {1, 2}; reaching the boundary requires the unexported emitter.
func TestEmitWindowedArray_MinWindowZeroBoundary(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	if err := e.emitWindowedArray(r, verbatim("0"), 0); err != nil {
		t.Fatalf("emitWindowedArray: %v", err)
	}
	sql := e.b.String()
	if strings.Contains(sql, "length(`window_vals`) >= 0") {
		t.Errorf("minWindowSize=0 must not gate on window length; mutant `>= 0` leaked.\nSQL=%s", sql)
	}
	if strings.Contains(sql, "length(`window_vals`) >=") {
		t.Errorf("minWindowSize=0 must skip the length filter entirely.\nSQL=%s", sql)
	}
}

// TestEmitWindowedArrayMatrix_MinWindowZeroBoundary kills the
// CONDITIONALS_BOUNDARY at range_window.go:2192 (`minWindowSize > 0`)
// inside emitWindowedArrayMatrix. Same shape as the non-matrix kill
// above: with minWindowSize=0 the mutant `>= 0` leaks an
// always-true `WHERE length(window_vals) >= 0`; the original skips
// the WHERE entirely. Reached via OuterRange > 0 with Step > 0 so the
// matrix path dispatches.
func TestEmitWindowedArrayMatrix_MinWindowZeroBoundary(t *testing.T) {
	t.Parallel()
	e := &emitter{}
	r := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Range:           time.Minute,
		OuterRange:      2 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	if err := e.emitWindowedArrayMatrix(r, verbatim("0"), 0); err != nil {
		t.Fatalf("emitWindowedArrayMatrix: %v", err)
	}
	sql := e.b.String()
	if strings.Contains(sql, "length(`window_vals`) >= 0") {
		t.Errorf("matrix minWindowSize=0 must not gate on window length; mutant `>= 0` leaked.\nSQL=%s", sql)
	}
	if strings.Contains(sql, "length(`window_vals`) >=") {
		t.Errorf("matrix minWindowSize=0 must skip the length filter entirely.\nSQL=%s", sql)
	}
}
