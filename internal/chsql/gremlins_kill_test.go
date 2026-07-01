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
		plan.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1, "")
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
		plan.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1, "")
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
	_, _, err := EmitMetricsExemplars(context.Background(), rw, nil, "TraceId", "SpanId", 1, "")
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
		rw.Input.(*chplan.MetricsAggregate), "TraceId", "SpanId", 1, "")
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
			sql, _, err := EmitMetricsExemplars(context.Background(), c.rw, m, "TraceId", "SpanId", 1, "")
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
			name:  "End==Start (span=0)",
			start: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			// The anchor count caps the sample-side fanout's upper
			// index bound (see sampleAnchorFanoutFrag).
			wantAnchors: "least(1, intDiv(dateDiff('nanosecond'",
		},
		{
			name:        "End-Start = 1 step",
			start:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:         time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC),
			wantAnchors: "least(2, intDiv(dateDiff('nanosecond'",
		},
		{
			name:        "End-Start = 5 steps",
			start:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
			end:         time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
			wantAnchors: "least(6, intDiv(dateDiff('nanosecond'",
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
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
			_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
			sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 0, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if strings.Contains(sql, "LIMIT") && strings.Contains(sql, " BY ") {
			t.Errorf("expected no LIMIT BY for maxPerSeries=0.\nSQL: %s", sql)
		}
	})
	t.Run("maxPerSeries=1 → LIMIT 1 BY ...", func(t *testing.T) {
		t.Parallel()
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "LIMIT 1") {
			t.Errorf("expected LIMIT 1 in SQL.\nSQL: %s", sql)
		}
	})
	t.Run("maxPerSeries=5 → LIMIT 5 BY ...", func(t *testing.T) {
		t.Parallel()
		sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 5, "")
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
	sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
		// The effective range surfaces as the lower index bound's
		// `- <rangeNS>, <stepNS>)` shift in the sample-side fanout
		// (see sampleAnchorFanoutFrag).
		{"Range zero → fallback to Step", 0, 2 * time.Minute, " - 120000000000, toInt64(120000000000))"},
		{"Range non-zero → used directly", 3 * time.Minute, 1 * time.Minute, " - 180000000000, toInt64(60000000000))"},
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
		// The anchor count caps the sample-side fanout's upper index
		// bound (see sampleAnchorFanoutFrag).
		{"End==Start → 1 anchor", time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), "least(1, intDiv(dateDiff('nanosecond'"},
		{"5 steps", time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC), "least(6, intDiv(dateDiff('nanosecond'"},
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
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
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

// TestEmitWindowedArrayMatrix_GroupByBoundary pins the matrix-shape
// regroup GROUP BY key list: the per-(series, anchor) regroup layer
// always GROUPs BY anchor_ts (the sample-side fanout requires it to
// rebuild the window array), with the series-identity columns
// prepended when GroupBy is set. A mutant that dropped the group
// columns from the key list would collapse all series into one window
// per anchor; one that dropped anchor_ts would collapse the grid.
func TestEmitWindowedArrayMatrix_GroupByBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		groupBy []chplan.Expr
		want    string
		dont    string
	}{
		{
			name:    "no GroupBy in matrix mode → regroup keys on anchor_ts only",
			groupBy: nil,
			want:    "GROUP BY `anchor_ts`",
			dont:    "GROUP BY `Attributes`, `anchor_ts`",
		},
		{
			name:    "with GroupBy in matrix mode → series key prepended",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			want:    "GROUP BY `Attributes`, `anchor_ts`",
			dont:    "",
		},
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
			if !strings.Contains(sql, c.want) {
				t.Errorf("expected %q in SQL.\nSQL: %s", c.want, sql)
			}
			if c.dont != "" && strings.Contains(sql, c.dont) {
				t.Errorf("did not expect %q in SQL.\nSQL: %s", c.dont, sql)
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
	t.Run("matrix stddev → window_vals >= 1 emitted", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindow{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			// stddev_over_time stays on the array (windowed) path — the
			// incremental funcs (sum/avg/min/max/count/present) now emit a
			// direct CH group aggregate with no window_vals array, so they
			// no longer exercise this minWindowSize boundary. stddev needs
			// the materialised array (two-pass moments) and so still routes
			// through emitWindowedArrayMatrix with minWindowSize=1.
			Func:            "stddev_over_time",
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
		// stddev_over_time uses minWindowSize=1 → length(window_vals)>=1.
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
	sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Exactly 6 anchors; the count caps the sample-side fanout's upper
	// index bound (see sampleAnchorFanoutFrag).
	if !strings.Contains(sql, "least(6, intDiv(dateDiff('nanosecond'") {
		t.Errorf("expected least(6, ...) anchor cap, SQL=%s", sql)
	}
	// Defensive: neither `least(5, ...)` nor an absurdly large literal.
	if strings.Contains(sql, "least(5, intDiv(dateDiff('nanosecond'") {
		t.Errorf("got least(5, ...) — `+ 1` may have flipped to `- 1`. SQL=%s", sql)
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
	sql, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
	// The instant windowed-array emitter fail-closes unless the inner scan
	// carries the IR-level time bound that Emit establishes; attach it here
	// the same way Emit does so the emitter sees a production-shaped plan.
	r = chplan.AttachInstantScanTimeBounds(r).(*chplan.RangeWindow)
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
	// The anchor count caps the sample-side fanout's upper index bound
	// (see sampleAnchorFanoutFrag).
	if !strings.Contains(sql, "least(5, intDiv(dateDiff('nanosecond'") {
		t.Errorf("expected anchor cap `least(5, ...)` for OuterRange=4m, Step=1m.\nSQL=%s", sql)
	}
	// Defensive: the off-by-one / different-operator mutants land on
	// neighbouring literals. `%` and `-` produce small wrong counts;
	// `*` and `+` blow up to enormous nanosecond magnitudes that no
	// reasonable `least(N, ...)` literal matches.
	for _, bad := range []string{"least(4, intDiv(", "least(3, intDiv(", "least(1, intDiv(", "least(0, intDiv("} {
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
	// Attach the IR-level scan time bound the instant emitter now requires
	// (Emit does this in production).
	r = chplan.AttachInstantScanTimeBounds(r).(*chplan.RangeWindow)
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

// TestValidateScanShape_BothEmpty kills the CONDITIONALS_NEGATION at
// emit_node.go:86 (`len(s.UnionTables) == 0`). With both Table and
// UnionTables empty the emitter has nothing to read from and must
// reject the Scan. The mutant flips the equality so the both-empty
// case slips through, then L89's check also passes (Table is "") and
// validateScanShape returns nil — the emitter then renders an empty
// FROM clause that ClickHouse parses as `SELECT *` over the literal
// “ (the bare Col(""))` which CH rejects at parse time with a
// non-deterministic message. Asserting the synchronous validation
// error pins the mutual-exclusion contract.
func TestValidateScanShape_BothEmpty(t *testing.T) {
	t.Parallel()
	_, _, err := Emit(context.Background(), &chplan.Scan{})
	if err == nil {
		t.Fatalf("Emit(Scan{}) returned nil error; want validateScanShape rejection")
	}
	if !strings.Contains(err.Error(), "neither Table nor UnionTables") {
		t.Errorf("error %q must name the missing-table contract", err.Error())
	}
}

// TestValidateScanShape_BothSet kills the CONDITIONALS_NEGATION at
// emit_node.go:89 (`s.Table != ""`). The mutant flips the inequality
// so a Scan with BOTH Table and UnionTables set is no longer rejected
// — the emitter would then route to the merge() table function and
// silently drop the Table field, masking a planning bug. Asserting
// the rejection pins the mutual-exclusion contract.
func TestValidateScanShape_BothSet(t *testing.T) {
	t.Parallel()
	_, _, err := Emit(context.Background(), &chplan.Scan{
		Table:       "otel_metrics_gauge",
		UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum"},
	})
	if err == nil {
		t.Fatalf("Emit(Scan{Table+UnionTables}) returned nil error; want validateScanShape rejection")
	}
	if !strings.Contains(err.Error(), "both Table=") || !strings.Contains(err.Error(), "UnionTables=") {
		t.Errorf("error %q must name both fields", err.Error())
	}
}

// TestValidateScanShape_BothSet_FilterScan kills the same
// CONDITIONALS_NEGATION at emit_node.go:89 reached via the
// emitFilterScan path (which re-validates so a Filter(Scan{...})
// node can't smuggle a malformed Scan past). Filter-with-Scan input
// is the codegen-specialised PREWHERE shape and a separate caller of
// validateScanShape.
func TestValidateScanShape_BothSet_FilterScan(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "ServiceName"},
			Right: &chplan.LitString{V: "api"},
		},
		Input: &chplan.Scan{
			Table:       "otel_metrics_gauge",
			UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum"},
		},
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("Emit(Filter(Scan{both set})) returned nil error; want validateScanShape rejection")
	}
	if !strings.Contains(err.Error(), "both Table=") {
		t.Errorf("error %q must report the Filter-side rejection", err.Error())
	}
}

// TestValidateScanShape_BothEmpty_FilterScan kills the
// CONDITIONALS_NEGATION at emit_node.go:86 reached via emitFilterScan.
// With both Table and UnionTables empty the Filter path must surface
// the same error as the bare-Scan path.
func TestValidateScanShape_BothEmpty_FilterScan(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "ServiceName"},
			Right: &chplan.LitString{V: "api"},
		},
		Input: &chplan.Scan{},
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("Emit(Filter(Scan{})) returned nil error; want validateScanShape rejection")
	}
	if !strings.Contains(err.Error(), "neither Table nor UnionTables") {
		t.Errorf("error %q must name the missing-table contract", err.Error())
	}
}

// TestEmitFilterScan_UnionTablesShapeLookup kills the cluster of
// mutants at emit_node.go:341 — the `shapeKey == "" && len(scan.
// UnionTables) > 0` guard that resolves the table shape from the
// first member of a UnionTables Scan. The mutants:
//
//   - CONDITIONALS_NEGATION col 14 (`==` → `!=`): with Table empty,
//     mutant condition becomes false; shapeKey stays "" and
//     tableShapeFor("") returns the zero TableShape. No wide columns
//     means projectionTouchesWide returns false → all conjuncts route
//     to WHERE and no PREWHERE is emitted.
//   - CONDITIONALS_NEGATION col 45 (`>` → `<=`): same observable —
//     the guard becomes false, shapeKey stays "", PREWHERE drops.
//
// The original behaviour shape-looks-up against UnionTables[0]
// (otel_metrics_gauge — registered with the metrics shape) so a cheap
// non-wide predicate is promoted to PREWHERE. Asserting the PREWHERE
// keyword in the rendered SQL kills both mutants in one shot.
//
// The companion INVERT_LOGICAL col 20 (`&&` → `||`) and
// CONDITIONALS_BOUNDARY col 45 (`>` → `>=`) at the same site cannot
// observationally diverge from the original after validateScanShape:
// the Table-empty path forces both conjuncts true, the Table-set path
// is rejected outright when UnionTables is also set, and the
// Table-set + UnionTables-empty path leaves the second conjunct false
// either way. Killing the two reachable mutants is sufficient to drop
// LIVED below the phase2 threshold.
func TestEmitFilterScan_UnionTablesShapeLookup(t *testing.T) {
	t.Parallel()
	// A wide-column-touching projection (no explicit Columns →
	// SELECT *) over a UnionTables Scan whose first member is
	// otel_metrics_gauge (registered with the metrics shape).
	// ServiceName + MetricName are both metrics sort-key columns so
	// each conjunct classifies cheap + non-wide; partitionPrewhere
	// promotes all-but-last to PREWHERE.
	a := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "ServiceName"},
		Right: &chplan.LitString{V: "api"},
	}
	b := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "system_cpu_time"},
	}
	plan := &chplan.Filter{
		Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: b},
		Input: &chplan.Scan{
			UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum"},
		},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')") {
		t.Errorf("expected merge() table function from UnionTables; SQL=%s", sql)
	}
	// Original: MetricName lands in PREWHERE because the metrics
	// shape lookup against UnionTables[0] succeeds. Both reachable
	// mutants force shapeKey="" → empty shape → no PREWHERE.
	if !strings.Contains(sql, "PREWHERE ") {
		t.Errorf("expected PREWHERE promotion via UnionTables[0] shape lookup; SQL=%s", sql)
	}
}

// TestRegexQuoteMeta_EscapesEachMetacharacter pins the escape set of
// the unexported regexQuoteMeta helper introduced by PR #710. The
// merge() table-function regex argument is RE2 — accidental
// metacharacters in a user-overridden table name (the OTel-CH default
// names are plain `[a-z_0-9]+` but the schema override surface is
// config-driven) would either widen the regex (matching unintended
// tables) or fail to compile. Asserting every documented
// metacharacter is escaped catches future drift in the meta string.
func TestRegexQuoteMeta_EscapesEachMetacharacter(t *testing.T) {
	t.Parallel()
	// One char at a time so a missing escape fails its own subtest.
	for _, r := range []rune{'\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$'} {
		r := r
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			got := regexQuoteMeta(string(r))
			want := `\` + string(r)
			if got != want {
				t.Errorf("regexQuoteMeta(%q) = %q, want %q", string(r), got, want)
			}
		})
	}
	// Plain identifier characters must pass through verbatim.
	for _, s := range []string{"a", "z", "0", "_", "abc_123", "otel_metrics_gauge"} {
		s := s
		t.Run("plain/"+s, func(t *testing.T) {
			t.Parallel()
			if got := regexQuoteMeta(s); got != s {
				t.Errorf("regexQuoteMeta(%q) = %q, want unchanged", s, got)
			}
		})
	}
}

// TestMergeTableFrag_DefaultDatabase pins the merge() rendering when
// Scan.Database is empty: the database argument is the CH built-in
// `currentDatabase()` call and the regex enumerates UnionTables anchored
// at both ends. The exact-string assertion catches any drift in the
// surface — a missing anchor, a swapped separator, or a dropped database
// fallback would all fail.
func TestMergeTableFrag_DefaultDatabase(t *testing.T) {
	t.Parallel()
	plan := &chplan.Scan{
		UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	const want = "FROM merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')"
	if !strings.Contains(sql, want) {
		t.Errorf("SQL missing %q.\nfull SQL=%s", want, sql)
	}
}

// TestMergeTableFrag_ExplicitDatabase pins the merge() rendering when
// Scan.Database is non-empty: the database argument is a single-quoted
// SQL literal (not currentDatabase()) and the regex anchors stay in
// place. Single quotes in the database name are doubled per
// escapeSingleQuotes.
func TestMergeTableFrag_ExplicitDatabase(t *testing.T) {
	t.Parallel()
	plan := &chplan.Scan{
		Database:    "obs",
		UnionTables: []string{"otel_metrics_gauge", "otel_metrics_sum"},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	const want = "FROM merge('obs', '^(otel_metrics_gauge|otel_metrics_sum)$')"
	if !strings.Contains(sql, want) {
		t.Errorf("SQL missing %q.\nfull SQL=%s", want, sql)
	}
}

// --- metrics_compare.go survivors -----------------------------------------

// compareNodeInternal builds a minimal valid *chplan.MetricsCompare for
// the internal-package tests below. Aliases are left EMPTY so the
// alias-fallback helpers (compareSelOut / compareAttrOut / compareValOut
// / compareValueOut) take their default branch — the branch every
// external-package test skips because it always pins explicit aliases.
func compareNodeInternal() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN: 10,
		Pairs: &chplan.FuncCall{Name: "array", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{
				&chplan.LitString{V: "name"},
				&chplan.ColumnRef{Name: "SpanName"},
			}},
		}},
		Inner: &chplan.Scan{Table: "otel_traces"},
	}
}

// TestCompareOutAliasFallbacks kills the four CONDITIONALS_NEGATION
// mutants at metrics_compare.go:126,133,140,147 — the `m.SelAlias != ""`
// / `AttrAlias` / `ValAlias` / `ValueAlias` guards that fall back to the
// canonical default name when the alias is empty. The mutant `== ""`
// would return the (empty) alias instead of the default; we assert both
// the empty→default mapping AND the explicit→passthrough mapping so the
// flip is observable on each helper independently.
func TestCompareOutAliasFallbacks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		set      func(*chplan.MetricsCompare)
		out      func(*chplan.MetricsCompare) string
		wantDflt string
		explicit string
	}{
		{"sel", func(m *chplan.MetricsCompare) { m.SelAlias = "" }, compareSelOut, "is_selection", "cohort"},
		{"attr", func(m *chplan.MetricsCompare) { m.AttrAlias = "" }, compareAttrOut, "attr", "akey"},
		{"val", func(m *chplan.MetricsCompare) { m.ValAlias = "" }, compareValOut, "val", "vval"},
		{"value", func(m *chplan.MetricsCompare) { m.ValueAlias = "" }, compareValueOut, "Value", "Count"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mDflt := compareNodeInternal()
			c.set(mDflt)
			if got := c.out(mDflt); got != c.wantDflt {
				t.Errorf("empty alias must fall back to %q, got %q", c.wantDflt, got)
			}
			mExp := compareNodeInternal()
			switch c.name {
			case "sel":
				mExp.SelAlias = c.explicit
			case "attr":
				mExp.AttrAlias = c.explicit
			case "val":
				mExp.ValAlias = c.explicit
			case "value":
				mExp.ValueAlias = c.explicit
			}
			if got := c.out(mExp); got != c.explicit {
				t.Errorf("explicit alias must pass through as %q, got %q", c.explicit, got)
			}
		})
	}
}

// TestEmitRangeWindowCompare_RangeFallback kills the
// CONDITIONALS_NEGATION at metrics_compare.go:212 (`if rangeDur == 0`).
// When the RangeWindow carries no explicit Range, rangeDur falls back to
// Step, so the inner scan-bound pushdown subtracts one Step (60s) from
// End. The mutant `!= 0` would skip the fallback, leaving rangeDur=0 and
// emitting a `- toIntervalNanosecond(0)` lower bound. We assert the
// fallback's observable artefact: the `- toIntervalNanosecond(60000000000)`
// window subtraction in the scan-bound PREWHERE/WHERE.
func TestEmitRangeWindowCompare_RangeFallback(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input: compareNodeInternal(),
		Step:  time.Minute,
		// Range deliberately omitted (0) → must fall back to Step.
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "- toIntervalNanosecond(60000000000)") {
		t.Errorf("rangeDur must fall back to Step (60s window); missing the 60e9ns subtraction.\nSQL: %s", sql)
	}
	if strings.Contains(sql, "- toIntervalNanosecond(0)") {
		t.Errorf("rangeDur==0 fallback passed over: emitted a zero-width window.\nSQL: %s", sql)
	}
}

// TestEmitRangeWindowCompare_SpanBoundaryAndArithmetic kills three
// mutants on the Start/End anchor-count path:
//   - CONDITIONALS_BOUNDARY at 223:11 (`if span < 0`) — Start==End is a
//     valid single-anchor window, not an error.
//   - ARITHMETIC_BASE at 226:20 (`span/stepNS`) and 226:28 (`+ 1`) — the
//     `least(<numAnchors>, …)` literal in the sample-side fanout pins the
//     exact anchor count, so `/`→`*`/`%`/`±` and `+`→`-`/`*` all diverge.
//
// Setup: Start=10:00, End=10:03, Step=1m → span=3m, numAnchors=3/1+1=4.
func TestEmitRangeWindowCompare_SpanBoundaryAndArithmetic(t *testing.T) {
	t.Parallel()

	t.Run("span boundary Start==End → numAnchors=1, no error", func(t *testing.T) {
		t.Parallel()
		ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
		rw := &chplan.RangeWindow{
			Input: compareNodeInternal(), Step: time.Minute, Range: time.Minute,
			Start: ts, End: ts, TimestampColumn: "Timestamp",
		}
		sql, _, err := Emit(context.Background(), rw)
		if err != nil {
			t.Fatalf("Start==End must be accepted (span=0, not <0): %v", err)
		}
		if !strings.Contains(sql, "least(1,") {
			t.Errorf("span=0 → numAnchors=0/step+1=1; expected least(1,.\nSQL: %s", sql)
		}
	})

	t.Run("numAnchors arithmetic span=3m step=1m → 4", func(t *testing.T) {
		t.Parallel()
		rw := &chplan.RangeWindow{
			Input: compareNodeInternal(), Step: time.Minute, Range: time.Minute,
			Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
		sql, _, err := Emit(context.Background(), rw)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "least(4,") {
			t.Errorf("span=3m, step=1m → 3/1+1=4 anchors; expected least(4,.\nSQL: %s", sql)
		}
	})

	t.Run("Start>End → error", func(t *testing.T) {
		t.Parallel()
		rw := &chplan.RangeWindow{
			Input: compareNodeInternal(), Step: time.Minute, Range: time.Minute,
			Start:           time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
		_, _, err := Emit(context.Background(), rw)
		if err == nil {
			t.Fatalf("Start>End (span<0) must error")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	})
}

// TestEmitRangeWindowCompare_OuterRangePath covers + kills the
// NOT_COVERED mutants on the OuterRange anchor branch:
//   - CONDITIONALS_BOUNDARY / CONDITIONALS_NEGATION at 219:20
//     (`case r.OuterRange > 0`).
//   - ARITHMETIC_BASE at 220:42/50 (`OuterRange.Nanoseconds()/stepNS + 1`).
//   - INVERT_LOGICAL at 221:25 (the `&&` joining the Start/End fallback
//     case — covered by exercising both branches: OuterRange>0 takes the
//     first case, so the second-case guard never fires here, but the
//     anchor-count assertion distinguishes the OuterRange formula from the
//     Start/End one).
//
// Setup: OuterRange=4m, Step=1m → numAnchors = 4/1 + 1 = 5.
func TestEmitRangeWindowCompare_OuterRangePath(t *testing.T) {
	t.Parallel()
	// Start/End carry the request window: a compare over the spans table must
	// be partition-bounded (requireInnerSpansScanBound), and the OuterRange
	// branch is selected first in the anchor-count switch regardless, so the
	// least(5, ...) assertion still pins the OuterRange formula.
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	rw := &chplan.RangeWindow{
		Input:           compareNodeInternal(),
		Step:            time.Minute,
		Range:           time.Minute,
		OuterRange:      4 * time.Minute,
		Start:           start,
		End:             start.Add(4 * time.Minute),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "least(5,") {
		t.Errorf("OuterRange=4m, step=1m → 4/1+1=5 anchors; expected least(5,.\nSQL: %s", sql)
	}
}

// TestEmitRangeWindowCompare_RootLookupTraceIDGuard covers + kills the
// NOT_COVERED CONDITIONALS_NEGATION at metrics_compare.go:95
// (`if m.TraceIDColumn == ""` inside the RootLookup branch). With a
// RootLookup set but no TraceIDColumn the emitter must error; a non-empty
// TraceIDColumn must render the LEFT JOIN on the qualified id columns.
func TestEmitRangeWindowCompare_RootLookupTraceIDGuard(t *testing.T) {
	t.Parallel()

	t.Run("RootLookup set, TraceIDColumn empty → error", func(t *testing.T) {
		t.Parallel()
		m := compareNodeInternal()
		m.RootLookup = &chplan.Scan{Table: "otel_traces"}
		m.TraceIDColumn = ""
		_, _, err := Emit(context.Background(), m)
		if err == nil {
			t.Fatalf("RootLookup with empty TraceIDColumn must error")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	})

	t.Run("RootLookup set, TraceIDColumn present → LEFT JOIN on it", func(t *testing.T) {
		t.Parallel()
		m := compareNodeInternal()
		m.RootLookup = &chplan.Scan{Table: "otel_traces"}
		m.TraceIDColumn = "TraceId"
		sql, _, err := Emit(context.Background(), m)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "LEFT JOIN") {
			t.Errorf("RootLookup must render a LEFT JOIN.\nSQL: %s", sql)
		}
		if !strings.Contains(sql, "s.`TraceId` = r.`TraceId`") {
			t.Errorf("LEFT JOIN must key on the qualified TraceId columns.\nSQL: %s", sql)
		}
	})
}

// --- exemplars.go attribute-map keying branches ---------------------------

// exemplarArgsContain reports whether the bound-arg slice carries the
// given string literal (the Attributes-map keys/values bind positionally
// as args).
func exemplarArgsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestEmitMetricsExemplars_OuterRangeNumAnchors covers + kills the
// CONDITIONALS_BOUNDARY/NEGATION at exemplars.go:112 (`case rw.OuterRange
// > 0`) and the ARITHMETIC_BASE at 113 (`OuterRange.Nanoseconds()/stepNS
// + 1`). The existing NumAnchorsBoundary test only exercises the
// Start/End span branch; this one drives the OuterRange branch.
//
// OuterRange=4m, Step=1m → numAnchors = 4/1 + 1 = 5 → `least(5,`.
func TestEmitMetricsExemplars_OuterRangeNumAnchors(t *testing.T) {
	t.Parallel()
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		OuterRange:      4 * time.Minute,
		TimestampColumn: "Timestamp",
	}
	sql, _, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "least(5, intDiv(dateDiff('nanosecond'") {
		t.Errorf("OuterRange=4m, step=1m → 4/1+1=5 anchors; expected least(5,.\nSQL: %s", sql)
	}
}

// TestEmitMetricsExemplars_QuantileKeyBranch covers + kills the
// CONDITIONALS_NEGATION / INVERT_LOGICAL mutants at exemplars.go:215
// (`m.Op == QuantileOverTime && len(m.Quantiles) == 1`) and the
// ARITHMETIC/format at 219. A single-quantile quantile_over_time exemplar
// must carry a `p="<phi>"` Attributes entry; a non-quantile op must NOT.
func TestEmitMetricsExemplars_QuantileKeyBranch(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)

	t.Run("quantile_over_time, 1 quantile → p key + phi value", func(t *testing.T) {
		t.Parallel()
		m := &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		}
		rw := &chplan.RangeWindow{Input: m, Step: time.Minute, Range: time.Minute, Start: start, End: end, TimestampColumn: "Timestamp"}
		_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !exemplarArgsContain(args, "p") {
			t.Errorf("single-quantile exemplar must carry the `p` map key; args=%v", args)
		}
		if !exemplarArgsContain(args, "0.95") {
			t.Errorf("single-quantile exemplar must carry the phi value 0.95; args=%v", args)
		}
		if exemplarArgsContain(args, "__name__") {
			t.Errorf("quantile branch must NOT emit the __name__ key; args=%v", args)
		}
	})

	t.Run("two quantiles → no p key (len != 1)", func(t *testing.T) {
		t.Parallel()
		m := &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.5, 0.9},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		}
		rw := &chplan.RangeWindow{Input: m, Step: time.Minute, Range: time.Minute, Start: start, End: end, TimestampColumn: "Timestamp"}
		_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if exemplarArgsContain(args, "p") {
			t.Errorf("multi-quantile exemplar must NOT carry the `p` key (len != 1); args=%v", args)
		}
	})
}

// TestEmitMetricsExemplars_UngroupedNameKeyBranch covers + kills the
// CONDITIONALS_NEGATION / INVERT_LOGICAL mutants at exemplars.go:221
// (`len(groupAliases) == 0 && m.Op != QuantileOverTime`). An ungrouped
// non-quantile op must carry a `__name__="<op>"` Attributes entry; a
// grouped op must NOT (the group labels key the series instead).
func TestEmitMetricsExemplars_UngroupedNameKeyBranch(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)

	t.Run("ungrouped rate → __name__ key + op value", func(t *testing.T) {
		t.Parallel()
		m := &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		}
		rw := &chplan.RangeWindow{Input: m, Step: time.Minute, Range: time.Minute, Start: start, End: end, TimestampColumn: "Timestamp"}
		_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !exemplarArgsContain(args, "__name__") {
			t.Errorf("ungrouped exemplar must carry the __name__ key; args=%v", args)
		}
		if !exemplarArgsContain(args, chplan.MetricsOpRate.String()) {
			t.Errorf("ungrouped exemplar must carry the op name %q; args=%v", chplan.MetricsOpRate.String(), args)
		}
	})

	t.Run("grouped rate → no __name__ key", func(t *testing.T) {
		t.Parallel()
		m := &chplan.MetricsAggregate{
			Op:             chplan.MetricsOpRate,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
			GroupByAliases: []string{"service"},
			ValueAlias:     "Value",
			Inner:          &chplan.Scan{Table: "otel_traces"},
		}
		rw := &chplan.RangeWindow{Input: m, Step: time.Minute, Range: time.Minute, Start: start, End: end, TimestampColumn: "Timestamp"}
		_, args, err := EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if exemplarArgsContain(args, "__name__") {
			t.Errorf("grouped exemplar must NOT carry the __name__ key (len(groupAliases) != 0); args=%v", args)
		}
	})
}

// --- nested_set_annotate.go survivors -------------------------------------

func nsAnnotateInternal() *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              &chplan.Scan{Table: "otel_traces"},
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
	}
}

// TestEmitNestedSetAnnotate_ColumnGuardDisjunction kills the four
// INVERT_LOGICAL mutants at nested_set_annotate.go:108-109 — the
// `SpansTable == "" || TraceIDColumn == "" || SpanIDColumn == "" ||
// ParentSpanIDColumn == "" || TimestampColumn == ""` guard. Each `||`
// flip to `&&` is killed by an input that blanks exactly ONE column:
// with the disjunction the emitter must still error (one empty column is
// enough); the `&&` mutant would require ALL columns empty to error, so
// the single-blank case slips through and emits SQL instead.
func TestEmitNestedSetAnnotate_ColumnGuardDisjunction(t *testing.T) {
	t.Parallel()
	blanks := []struct {
		name string
		set  func(*chplan.NestedSetAnnotate)
	}{
		{"SpansTable", func(n *chplan.NestedSetAnnotate) { n.SpansTable = "" }},
		{"TraceIDColumn", func(n *chplan.NestedSetAnnotate) { n.TraceIDColumn = "" }},
		{"SpanIDColumn", func(n *chplan.NestedSetAnnotate) { n.SpanIDColumn = "" }},
		{"ParentSpanIDColumn", func(n *chplan.NestedSetAnnotate) { n.ParentSpanIDColumn = "" }},
		{"TimestampColumn", func(n *chplan.NestedSetAnnotate) { n.TimestampColumn = "" }},
	}
	for _, b := range blanks {
		b := b
		t.Run(b.name+" empty → error", func(t *testing.T) {
			t.Parallel()
			n := nsAnnotateInternal()
			b.set(n)
			_, _, err := Emit(context.Background(), n)
			if err == nil {
				t.Fatalf("a single empty %s must surface ErrUnsupported (|| guard)", b.name)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("want ErrUnsupported, got %v", err)
			}
		})
	}
	t.Run("all columns set → no guard error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := Emit(context.Background(), nsAnnotateInternal()); err != nil {
			t.Fatalf("fully-populated NestedSetAnnotate must not error: %v", err)
		}
	})
}

// TestProjectsBareColumn_ContinueScansAllProjections kills the
// INVERT_LOOPCTRL mutant at nested_set_annotate.go:379 (`continue` →
// `break` inside projectsBareColumn). A non-matching projection AHEAD of
// the matching one must be passed over (continue), not abort the scan
// (break). The break mutant returns false because it bails at the first
// non-match before ever reaching the bare column.
func TestProjectsBareColumn_ContinueScansAllProjections(t *testing.T) {
	t.Parallel()
	p := &chplan.Project{
		Projections: []chplan.Projection{
			// First projection: NOT a bare TraceId ref (aliased rename) —
			// the loop must `continue` past it.
			{Expr: &chplan.ColumnRef{Name: "SpanId"}, Alias: "sid"},
			// A non-ColumnRef expr — also passed over.
			{Expr: &chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: "X"}}}},
			// The bare TraceId we want to find sits LAST.
			{Expr: &chplan.ColumnRef{Name: "TraceId"}},
		},
	}
	if !projectsBareColumn(p, "TraceId") {
		t.Errorf("projectsBareColumn must find a bare TraceId behind earlier non-matches (continue, not break)")
	}
	// Negative: a Project that renames TraceId away exposes no bare ref.
	pRenamed := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "TraceId"}, Alias: "tid"},
		},
	}
	if projectsBareColumn(pRenamed, "TraceId") {
		t.Errorf("an aliased-away TraceId is not a bare projection")
	}
}

// TestWriteOptQualCol_QualifierBranch kills the CONDITIONALS_NEGATION
// mutant at nested_set_annotate.go:391 (`if qual == ""`). The empty-qual
// branch must emit a BARE backtick-quoted identifier; the non-empty
// branch a `qual`.`col` pair. The `!=` mutant swaps the two, so the
// empty-qual call would render the broken “ “.`col` “ form.
func TestWriteOptQualCol_QualifierBranch(t *testing.T) {
	t.Parallel()
	t.Run("empty qual → bare ident", func(t *testing.T) {
		t.Parallel()
		b := &Builder{}
		writeOptQualCol(b, "", "Timestamp")
		got, _ := b.Build()
		if got != "`Timestamp`" {
			t.Errorf("empty qual must render bare `Timestamp`, got %q", got)
		}
	})
	t.Run("non-empty qual → qualified ident", func(t *testing.T) {
		t.Parallel()
		b := &Builder{}
		writeOptQualCol(b, "t", "Timestamp")
		got, _ := b.Build()
		if got != "`t`.`Timestamp`" {
			t.Errorf("qual=t must render `t`.`Timestamp`, got %q", got)
		}
	})
}

// --- prewhere.go survivors ------------------------------------------------

// TestOrderedConjuncts_SkipBucketContinue kills the INVERT_LOOPCTRL
// mutant at prewhere.go:193 (`continue` → `break` after appending a
// skip-index conjunct). A skip-bucket conjunct ahead of a plain "rest"
// conjunct must NOT abort the loop — the `break` mutant would drop every
// conjunct after the first skip hit, losing them from the output.
//
// The shape registers a SkipIndexColumn but no SortColumns, so the first
// conjunct lands in the skip bucket and the second in the rest bucket.
// orderedConjuncts concatenates prefix ++ skip ++ rest, so the output
// must contain BOTH conjuncts (skip first, rest second).
func TestOrderedConjuncts_SkipBucketContinue(t *testing.T) {
	t.Parallel()
	shape := TableShape{SkipIndexColumns: []string{"TraceId"}}
	skipC := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "TraceId"}, Right: &chplan.LitString{V: "abc"}}
	restC := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Plain"}, Right: &chplan.LitInt{V: 1}}
	got := orderedConjuncts([]chplan.Expr{skipC, restC}, shape)
	if len(got) != 2 {
		t.Fatalf("orderedConjuncts dropped a conjunct (break instead of continue): got %d, want 2", len(got))
	}
	if got[0] != skipC || got[1] != restC {
		t.Errorf("ordered output = %v, want [skip, rest] (skip bucket precedes rest)", got)
	}
}

// TestOrderedConjuncts_StableSameRankTiebreak kills the insertion-sort
// tiebreaker mutants at prewhere.go:201 (CONDITIONALS_NEGATION on the
// `rank ==` equality + CONDITIONALS_BOUNDARY on the `idx >` tiebreaker)
// and the INVERT_LOOPCTRL at 203 (`continue` → `break` after a swap).
//
// Three conjuncts all reference the SAME sort column (ServiceName, rank
// 0), so the rank comparison is always a tie and the stable sort must
// fall back to the idx (input-order) tiebreaker. The original keeps them
// in input order; a broken tiebreaker or an early `break` permutes them.
func TestOrderedConjuncts_StableSameRankTiebreak(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName"}}
	a := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "a"}}
	b := &chplan.Binary{Op: chplan.OpNe, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "b"}}
	c := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "c"}}
	// All rank 0 → stable sort must preserve input order [a, b, c].
	got := orderedConjuncts([]chplan.Expr{a, b, c}, shape)
	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("same-rank conjuncts must keep input order [a,b,c]; got %v", got)
	}
}

// TestIsSkipIndexColumn_Match kills the CONDITIONALS_NEGATION at
// tableshape.go:60 (`c == name` → `!=`). With a matching name the lookup
// must report true; a non-member must report false. Also covers the same
// equality flip at tableshape.go:26 (IsSortColumn) below.
func TestIsSkipIndexColumn_Match(t *testing.T) {
	t.Parallel()
	shape := TableShape{SkipIndexColumns: []string{"TraceId", "SpanId"}}
	if !shape.IsSkipIndexColumn("SpanId") {
		t.Errorf("IsSkipIndexColumn(SpanId) must be true for a registered skip column")
	}
	if shape.IsSkipIndexColumn("Body") {
		t.Errorf("IsSkipIndexColumn(Body) must be false for an unregistered column")
	}
}

// TestIsSortColumn_Match kills the CONDITIONALS_NEGATION at
// tableshape.go:26 (`c == name` → `!=`) in IsSortColumn.
func TestIsSortColumn_Match(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "Timestamp"}}
	if !shape.IsSortColumn("Timestamp") {
		t.Errorf("IsSortColumn(Timestamp) must be true")
	}
	if shape.IsSortColumn("Nope") {
		t.Errorf("IsSortColumn(Nope) must be false")
	}
}

// --- vector_join.go / vector_set_op.go column-validation switches ---------

// TestValidateVectorJoinCols_EachEmptyErrors covers + kills the four
// CONDITIONALS_NEGATION mutants at vector_join.go:97-103 — the
// `AttributesColumn`/`MetricNameColumn`/`TimestampColumn`/`ValueColumn ==
// ""` switch cases. Each blank-one-column variant must surface
// ErrUnsupported; the fully-populated control must not.
func TestValidateVectorJoinCols_EachEmptyErrors(t *testing.T) {
	t.Parallel()
	base := func() *chplan.VectorJoin {
		return &chplan.VectorJoin{
			Left:             &chplan.Scan{Table: "otel_metrics_sum"},
			Right:            &chplan.Scan{Table: "otel_metrics_sum"},
			Op:               chplan.OpAdd,
			Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}
	blanks := []struct {
		name string
		set  func(*chplan.VectorJoin)
	}{
		{"AttributesColumn", func(j *chplan.VectorJoin) { j.AttributesColumn = "" }},
		{"MetricNameColumn", func(j *chplan.VectorJoin) { j.MetricNameColumn = "" }},
		{"TimestampColumn", func(j *chplan.VectorJoin) { j.TimestampColumn = "" }},
		{"ValueColumn", func(j *chplan.VectorJoin) { j.ValueColumn = "" }},
	}
	for _, b := range blanks {
		b := b
		t.Run(b.name+" empty → error", func(t *testing.T) {
			t.Parallel()
			j := base()
			b.set(j)
			_, _, err := Emit(context.Background(), j)
			if err == nil {
				t.Fatalf("empty %s must error", b.name)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("want ErrUnsupported for empty %s, got %v", b.name, err)
			}
		})
	}
	t.Run("all set → no validation error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := Emit(context.Background(), base()); err != nil {
			t.Fatalf("fully-populated VectorJoin must not error: %v", err)
		}
	})
}

// TestValidateVectorSetOpCols_EachEmptyErrors covers + kills the four
// CONDITIONALS_NEGATION mutants at vector_set_op.go:371-377 plus the
// `if err != nil` guard at 50 — the column-validation switch on a
// VectorSetOp. Same blank-one-column shape as the VectorJoin test.
func TestValidateVectorSetOpCols_EachEmptyErrors(t *testing.T) {
	t.Parallel()
	base := func() *chplan.VectorSetOp {
		return &chplan.VectorSetOp{
			Left:             &chplan.Scan{Table: "otel_metrics_sum"},
			Right:            &chplan.Scan{Table: "otel_metrics_sum"},
			Op:               chplan.VectorSetAnd,
			Match:            chplan.VectorMatch{On: true},
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}
	blanks := []struct {
		name string
		set  func(*chplan.VectorSetOp)
	}{
		{"AttributesColumn", func(s *chplan.VectorSetOp) { s.AttributesColumn = "" }},
		{"MetricNameColumn", func(s *chplan.VectorSetOp) { s.MetricNameColumn = "" }},
		{"TimestampColumn", func(s *chplan.VectorSetOp) { s.TimestampColumn = "" }},
		{"ValueColumn", func(s *chplan.VectorSetOp) { s.ValueColumn = "" }},
	}
	for _, b := range blanks {
		b := b
		t.Run(b.name+" empty → error", func(t *testing.T) {
			t.Parallel()
			s := base()
			b.set(s)
			_, _, err := Emit(context.Background(), s)
			if err == nil {
				t.Fatalf("empty %s must error", b.name)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("want ErrUnsupported for empty %s, got %v", b.name, err)
			}
		})
	}
	t.Run("all set → no validation error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := Emit(context.Background(), base()); err != nil {
			t.Fatalf("fully-populated VectorSetOp must not error: %v", err)
		}
	})
}

// TestVectorSetOpProjectionOutputName_AliasBranch kills the
// CONDITIONALS_NEGATION at vector_set_op.go:343 (`if p.Alias != ""`).
// An explicit Alias must win; a bare ColumnRef with no Alias yields the
// column name; a non-ColumnRef with no Alias yields "".
func TestVectorSetOpProjectionOutputName_AliasBranch(t *testing.T) {
	t.Parallel()
	if got := vectorSetOpProjectionOutputName(chplan.Projection{
		Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "renamed",
	}); got != "renamed" {
		t.Errorf("aliased projection output name = %q, want renamed", got)
	}
	if got := vectorSetOpProjectionOutputName(chplan.Projection{
		Expr: &chplan.ColumnRef{Name: "Value"},
	}); got != "Value" {
		t.Errorf("bare ColumnRef output name = %q, want Value", got)
	}
	if got := vectorSetOpProjectionOutputName(chplan.Projection{
		Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitInt{V: 1}}},
	}); got != "" {
		t.Errorf("non-ColumnRef unaliased projection output name = %q, want empty", got)
	}
}

// TestVectorSetOpArmIsMatrixRangeWindow_OuterRangeBoundary kills the
// CONDITIONALS_BOUNDARY/NEGATION at vector_set_op.go:270
// (`v.OuterRange > 0`). A matrix RangeWindow (OuterRange>0) is matrix
// shape; an instant RangeWindow (OuterRange==0) is not. The boundary
// mutant `>= 0` would mis-classify the instant case as matrix.
func TestVectorSetOpArmIsMatrixRangeWindow_OuterRangeBoundary(t *testing.T) {
	t.Parallel()
	matrix := &chplan.RangeWindow{OuterRange: 5 * time.Minute}
	if !vectorSetOpArmIsMatrixRangeWindow(matrix) {
		t.Errorf("OuterRange>0 RangeWindow must be matrix shape")
	}
	instant := &chplan.RangeWindow{OuterRange: 0}
	if vectorSetOpArmIsMatrixRangeWindow(instant) {
		t.Errorf("OuterRange==0 RangeWindow must NOT be matrix shape (boundary)")
	}
	// Recurses past a wrapping Project / Filter.
	if !vectorSetOpArmIsMatrixRangeWindow(&chplan.Project{Input: matrix}) {
		t.Errorf("matrix detection must recurse through a Project wrapper")
	}
	if !vectorSetOpArmIsMatrixRangeWindow(&chplan.Filter{Input: matrix, Predicate: &chplan.LitBool{V: true}}) {
		t.Errorf("matrix detection must recurse through a Filter wrapper")
	}
}

// --- structural_join.go recursive MaxDepth + sibling path -----------------

func structuralRecursiveNode(op chplan.StructuralOp, maxDepth int) *chplan.StructuralJoin {
	return &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 op,
		MaxDepth:           maxDepth,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
}

// TestEmitStructuralRecursive_MaxDepthBoundary kills the
// CONDITIONALS_BOUNDARY + CONDITIONALS_NEGATION at effectiveRecursionDepth
// (`if maxDepth > 0`). The recursive arm is ALWAYS bounded now (#78):
// MaxDepth==0 falls back to the package default cap
// (defaultStructuralRecursionDepth); MaxDepth>0 overrides it. The
// boundary mutant `>= 0` would let MaxDepth=0 emit `c._depth < 0`
// (an always-false bound that empties the closure); the negation
// `<= 0` would emit the default for positive inputs too.
func TestEmitStructuralRecursive_MaxDepthBoundary(t *testing.T) {
	t.Parallel()
	t.Run("MaxDepth=0 → default safety cap", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), structuralRecursiveNode(chplan.StructuralDescendant, 0))
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		want := "c._depth < " + strconv.Itoa(defaultStructuralRecursionDepth)
		if !strings.Contains(sql, want) {
			t.Errorf("MaxDepth=0 must emit the default safety cap %q.\nSQL: %s", want, sql)
		}
		// The unbounded `> 0` boundary mutant would emit `c._depth < 0`.
		if strings.Contains(sql, "c._depth < 0") {
			t.Errorf("MaxDepth=0 must NOT emit a zero (always-false) cap.\nSQL: %s", sql)
		}
	})
	t.Run("MaxDepth=3 → c._depth < 3", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), structuralRecursiveNode(chplan.StructuralDescendant, 3))
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "c._depth < 3") {
			t.Errorf("MaxDepth=3 must cap at c._depth < 3.\nSQL: %s", sql)
		}
		// Must override, not co-emit, the default.
		if strings.Contains(sql, "c._depth < "+strconv.Itoa(defaultStructuralRecursionDepth)) {
			t.Errorf("MaxDepth=3 must override the default cap, not co-emit it.\nSQL: %s", sql)
		}
	})
}

// TestEffectiveRecursionDepth pins the #78 cap resolution directly:
// positive MaxDepth passes through; 0 (and defensively negative) falls
// back to the package default. Kills the CONDITIONALS_BOUNDARY /
// CONDITIONALS_NEGATION mutants on the `maxDepth > 0` guard.
func TestEffectiveRecursionDepth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want int
	}{
		{0, defaultStructuralRecursionDepth},
		{-1, defaultStructuralRecursionDepth},
		{1, 1},
		{3, 3},
		{1000, 1000},
	}
	for _, c := range cases {
		if got := effectiveRecursionDepth(c.in); got != c.want {
			t.Errorf("effectiveRecursionDepth(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestEmitStructuralRecursiveUnion_InverseClosureMaxDepth covers + kills
// the MaxDepth boundary in buildStructuralInverseClosure (the INVERSE
// closure built for the union recursive arm). The union op (`&>>`)
// builds both the forward and inverse closures, so the bound must
// appear TWICE: MaxDepth>0 → two `c._depth < N`; MaxDepth=0 → two
// `c._depth < <default>` (both closures bounded by the #78 safety cap,
// never unbounded).
func TestEmitStructuralRecursiveUnion_InverseClosureMaxDepth(t *testing.T) {
	t.Parallel()
	t.Run("union MaxDepth=2 → both closures capped at 2", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), structuralRecursiveNode(chplan.StructuralUnionDescendant, 2))
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if got := strings.Count(sql, "c._depth < 2"); got != 2 {
			t.Errorf("union recursive MaxDepth=2 must cap BOTH the forward and inverse closures (2 occurrences), got %d.\nSQL: %s", got, sql)
		}
	})
	t.Run("union MaxDepth=0 → both closures at default cap", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), structuralRecursiveNode(chplan.StructuralUnionDescendant, 0))
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		want := "c._depth < " + strconv.Itoa(defaultStructuralRecursionDepth)
		if got := strings.Count(sql, want); got != 2 {
			t.Errorf("union recursive MaxDepth=0 must bound BOTH closures with the default cap %q (2 occurrences), got %d.\nSQL: %s", want, got, sql)
		}
		if strings.Contains(sql, "c._depth < 0") {
			t.Errorf("union recursive MaxDepth=0 must NOT emit a zero (always-false) cap.\nSQL: %s", sql)
		}
	})
}

// TestEmitStructuralSiblingJoin_Succeeds covers + kills the
// CONDITIONALS_NEGATION mutants at structural_join.go:184 and 188 (the
// `if err != nil` guards around the left/right subqueryFrag calls in
// emitStructuralSiblingJoin). A valid sibling (`~`) join must emit
// non-empty SQL that joins L and R on shared parent; the `err == nil`
// mutant would return early (empty SQL) on the success path.
func TestEmitStructuralSiblingJoin_Succeeds(t *testing.T) {
	t.Parallel()
	j := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralSibling,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	sql, _, err := Emit(context.Background(), j)
	if err != nil {
		t.Fatalf("sibling join must emit cleanly: %v", err)
	}
	// The success path must produce a real SELECT that references both
	// sides — a mutant that bailed on err==nil would yield empty SQL.
	if !strings.Contains(sql, "SELECT") {
		t.Fatalf("sibling join produced no SELECT (early-return mutant?).\nSQL: %q", sql)
	}
	for _, want := range []string{"L.`ParentSpanId`", "R.`ParentSpanId`"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sibling join must key on shared parent; missing %q.\nSQL: %s", want, sql)
		}
	}
}

// =====================================================================
// Phase-2 (chsql) LIVED-mutant kills from the push-to-main mutation run
// 27469747395 (commit fef5109a). The typed-Frag sweep (#850) plus
// accumulated test-quality gaps left 40 LIVED chsql mutants — efficacy
// 94.13% < the 95% bar. The block below kills the clearly-killable set
// (validation disjunct chains, off-by-one / sign boundaries on the
// emitted SQL, and the computed-phi / qualifier branches), each by
// asserting the EXACT emitted SQL or error so the mutated operator
// produces observably-different output.
// =====================================================================

// --- range_lwr.go column-validation disjunct (65:26/46/71) ---

// TestEmitRangeLWR_EachColumnEmptyErrors kills the INVERT_LOGICAL
// mutants on the `TimestampCol == "" || ValueCol == "" || MetricNameCol
// == "" || AttributesCol == ""` validation chain. Each case blanks
// exactly ONE column; `||` → `&&` would require every column blank
// before erroring, so a single missing name must still be rejected.
func TestEmitRangeLWR_EachColumnEmptyErrors(t *testing.T) {
	t.Parallel()
	base := func() *chplan.RangeLWR {
		return &chplan.RangeLWR{
			Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
			Step:          30 * time.Second,
			MetricNameCol: "MetricName",
			AttributesCol: "Attributes",
			TimestampCol:  "TimeUnix",
			ValueCol:      "Value",
		}
	}
	cases := []struct {
		name  string
		blank func(r *chplan.RangeLWR)
	}{
		{"timestamp", func(r *chplan.RangeLWR) { r.TimestampCol = "" }},
		{"value", func(r *chplan.RangeLWR) { r.ValueCol = "" }},
		{"metricName", func(r *chplan.RangeLWR) { r.MetricNameCol = "" }},
		{"attributes", func(r *chplan.RangeLWR) { r.AttributesCol = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := base()
			tc.blank(r)
			if _, _, err := Emit(context.Background(), r); err == nil {
				t.Errorf("RangeLWR with empty %s column must error", tc.name)
			}
		})
	}
}

// --- range_lwr.go Start/End guard + span boundary (76:23, 78:11) ---

// TestEmitRangeLWR_AnchorCountBounds kills two mutants in the anchor-
// count computation:
//   - 76:23 INVERT_LOGICAL on `!Start.IsZero() && !End.IsZero()`: with a
//     pinned [Start,End] grid the anchor count is computed from the span
//     (least(11, …)); the `&&` → `||` flip would still take the computed
//     branch when only one bound is set, but more importantly the
//     pinned-grid case below proves the computed branch runs (least(11)).
//   - 78:11 CONDITIONALS_BOUNDARY on `span < 0` → `span <= 0`: a
//     ZERO-span grid (Start == End) is legal — one anchor — and must NOT
//     error; the `<=` mutant would reject it.
func TestEmitRangeLWR_AnchorCountBounds(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Pinned grid → computed anchor count least(11, …) (5m / 30s + 1).
	pinned := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName", AttributesCol: "Attributes",
		TimestampCol: "TimeUnix", ValueCol: "Value",
	}
	sql, _, err := Emit(context.Background(), pinned)
	if err != nil {
		t.Fatalf("pinned-grid RangeLWR: %v", err)
	}
	if !strings.Contains(sql, "least(11,") {
		t.Errorf("pinned [Start,End] grid must compute 11 anchors (least(11, …)); got:\n%s", sql)
	}

	// Zero-span grid (Start == End): exactly one anchor, must NOT error.
	zeroSpan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start, // span == 0
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName", AttributesCol: "Attributes",
		TimestampCol: "TimeUnix", ValueCol: "Value",
	}
	zsSQL, _, err := Emit(context.Background(), zeroSpan)
	if err != nil {
		t.Fatalf("zero-span RangeLWR must emit (1 anchor), got error: %v", err)
	}
	if !strings.Contains(zsSQL, "least(1,") {
		t.Errorf("zero-span grid must yield exactly one anchor (least(1, …)); got:\n%s", zsSQL)
	}

	// 76:23 INVERT_LOGICAL on `!Start.IsZero() && !End.IsZero()`: only
	// when BOTH bounds are pinned is the span computed. With Start set
	// but End zero the guard is false → single anchor, no span math, must
	// emit cleanly. The `&&` → `||` mutant would enter the span branch on
	// the zero End, computing a negative span and erroring.
	oneBound := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           time.Time{}, // zero
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName", AttributesCol: "Attributes",
		TimestampCol: "TimeUnix", ValueCol: "Value",
	}
	obSQL, _, err := Emit(context.Background(), oneBound)
	if err != nil {
		t.Fatalf("RangeLWR with only Start pinned must emit (1 anchor), got error: %v", err)
	}
	if !strings.Contains(obSQL, "least(1,") {
		t.Errorf("single pinned bound must yield exactly one anchor (least(1, …)); got:\n%s", obSQL)
	}
}

// --- range_lwr.go lookback sign in the floor-index numerator (189:72) ---

// TestEmitRangeLWR_LookbackSign kills the INVERT_NEGATIVES /
// ARITHMETIC_BASE on `-lookbackNS` in the floor-index numerator. The
// window floor walks BACK by the lookback, so the emitted numerator is
// `dist - <lookbackNS>`; flipping the sign to `+ <lookbackNS>` (or
// changing the op) would walk the window the wrong way and is caught by
// the exact subtraction substring.
func TestEmitRangeLWR_LookbackSign(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute, // 300000000000 ns
		MetricNameCol: "MetricName", AttributesCol: "Attributes",
		TimestampCol: "TimeUnix", ValueCol: "Value",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The floor numerator subtracts the lookback (greatest(0, intDiv(dist
	// - lookbackNS, …))). A sign flip would render "+ 300000000000".
	if !strings.Contains(sql, "- 300000000000, toInt64(30000000000)) - (modulo") {
		t.Errorf("floor index must subtract the lookback (dist - 300000000000); got:\n%s", sql)
	}
	if strings.Contains(sql, "+ 300000000000") {
		t.Errorf("lookback must be subtracted, never added; got:\n%s", sql)
	}
}

// --- histogram_quantile_native.go computed-phi guard (196:15) ---

// TestEmitHistogramQuantileNative_ComputedPhiNaNGuard kills the
// CONDITIONALS_NEGATION on `if h.PhiExpr == nil`. With a computed phi
// (PhiExpr set) the emitter wraps the core in an `isNaN(phi)` guard
// (Prometheus's NaN-phi contract); the literal-phi path omits it. The
// `== nil` → `!= nil` flip would swap which path gets the wrapper, so
// the isNaN token must be PRESENT for computed phi and ABSENT for
// literal phi.
func TestEmitHistogramQuantileNative_ComputedPhiNaNGuard(t *testing.T) {
	t.Parallel()
	build := func(phiExpr chplan.Expr) string {
		t.Helper()
		plan := &chplan.HistogramQuantileNative{
			Phi:                        0.9,
			PhiExpr:                    phiExpr,
			ScaleColumn:                "Scale",
			ZeroCountColumn:            "ZeroCount",
			PositiveOffsetColumn:       "PositiveOffset",
			PositiveBucketCountsColumn: "PositiveBucketCounts",
			NegativeOffsetColumn:       "NegativeOffset",
			NegativeBucketCountsColumn: "NegativeBucketCounts",
			Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		}
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		return sql
	}
	computed := build(&chplan.FuncCall{Name: "scalar"})
	if !strings.Contains(computed, "isNaN") {
		t.Errorf("computed-phi native quantile must carry the isNaN NaN-guard; got:\n%s", computed)
	}
	literal := build(nil)
	if strings.Contains(literal, "isNaN") {
		t.Errorf("literal-phi native quantile must NOT carry the isNaN guard; got:\n%s", literal)
	}
}

// --- nested_set_annotate.go optQualColFrag qualifier branch (410:10) ---

// TestOptQualColFrag_QualifierBranch kills the CONDITIONALS_NEGATION on
// `if qual == ""` in optQualColFrag: an empty qualifier renders the bare
// `col`, a non-empty one renders `qual`.`col`. The `== ""` → `!= ""`
// flip would swap the two branches.
func TestOptQualColFrag_QualifierBranch(t *testing.T) {
	t.Parallel()
	render := func(f Frag) string {
		b := &Builder{}
		f(b)
		return b.String()
	}
	if got := render(optQualColFrag("", "col")); got != "`col`" {
		t.Errorf("empty qualifier should render bare `col`, got %q", got)
	}
	if got := render(optQualColFrag("c", "col")); got != "`c`.`col`" {
		t.Errorf("non-empty qualifier should render `c`.`col`, got %q", got)
	}
}

// --- range_window.go over-time matrix anchor count + mode split ---

// TestEmitRangeWindowOverTimeMatrix_AnchorArithmetic kills the
// ARITHMETIC_BASE mutants on `OuterRange/Step + 1` (the anchor count) in
// the direct-matrix over-time emitter. A 5m OuterRange at 30s Step is 11
// anchors → `least(11, …)`; the `/` or `+1` mutation moves that count.
func TestEmitRangeWindowOverTimeMatrix_AnchorArithmetic(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "max_over_time",
		Range:           time.Minute,
		Step:            30 * time.Second,
		OuterRange:      5 * time.Minute, // 5m / 30s + 1 = 11
		Start:           start,
		End:             start.Add(5 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "least(11,") {
		t.Errorf("matrix over-time must compute 11 anchors (least(11, …)); got:\n%s", sql)
	}
	// The +1 mutation would yield 10; assert the off-by-one neighbour is absent.
	if strings.Contains(sql, "least(10,") {
		t.Errorf("anchor count must be 11, not 10 (off-by-one mutant); got:\n%s", sql)
	}
}

// TestEmitRangeWindowOverTime_OuterRangeBoundary kills the
// CONDITIONALS_BOUNDARY on `if r.OuterRange > 0` (instant vs matrix
// mode). With OuterRange == 0 the emitter takes the INSTANT path — a
// single windowed aggregate, NO anchor fan-out. The `> 0` → `>= 0`
// mutant would wrongly route the instant case through the matrix
// variant, introducing the `arrayJoin(arrayMap` / `least(` fan-out.
func TestEmitRangeWindowOverTime_OuterRangeBoundary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "max_over_time",
		Range:           time.Minute,
		Step:            30 * time.Second,
		OuterRange:      0, // instant mode
		End:             start.Add(5 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "arrayJoin(arrayMap(i ->") || strings.Contains(sql, "least(") {
		t.Errorf("instant over-time (OuterRange=0) must NOT fan out across anchors; got:\n%s", sql)
	}
}

// --- builder.go Window PARTITION-BY boundary (1575:23) ---

// TestWindowFrag_PartitionByBoundary kills the CONDITIONALS_BOUNDARY on
// `if len(partitionBy) > 0` inside the Window OVER frag. An empty
// partition list must omit `PARTITION BY` entirely; the `> 0` → `>= 0`
// mutant would emit a dangling `PARTITION BY ` with no columns.
func TestWindowFrag_PartitionByBoundary(t *testing.T) {
	t.Parallel()
	render := func(f Frag) string {
		b := &Builder{}
		f(b)
		return b.String()
	}
	with := render(Window(Call("row_number"), []Frag{Col("a")}, nil))
	if !strings.Contains(with, "OVER (PARTITION BY `a`)") {
		t.Errorf("non-empty partition list must render PARTITION BY; got %q", with)
	}
	without := render(Window(Call("row_number"), nil, nil))
	if without != "row_number() OVER ()" {
		t.Errorf("empty partition list must omit PARTITION BY entirely; got %q", without)
	}
	if strings.Contains(without, "PARTITION BY") {
		t.Errorf("empty partition list must NOT emit a dangling PARTITION BY; got %q", without)
	}
}
