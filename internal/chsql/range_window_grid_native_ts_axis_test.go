package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The native ts-grid aggregates take their timestamp axis as the FIRST
// argument of the second (argument) paren group:
//
//	timeSeriesRateToGrid(start, end, step_s, window_s)(<ts axis>, value)
//
// nativeGridTsAxisFrag decides what that axis is per function, and the choice
// is NOT cosmetic. The regression pair (deriv, predict_linear) is handed a
// whole-second `toDateTime(<ts>)` axis so the emitted values stay BIT-IDENTICAL
// to the fan-out lowering, whose own x-axis is the floored
// `dateDiff('second', anchor, ts)`; the rest of the family reads the raw
// DateTime64 column. See nativeGridTsAxisFrag's own doc for the numerical
// argument behind each half.
//
// Inverting the `||` in
//
//	if fn == "deriv" || fn == "predict_linear" {
//
// makes the test unsatisfiable, so the regression pair silently drops to the
// raw axis. That is a change of emitted SQL — and of the values ClickHouse
// computes — that nothing failed on: the existing deriv / predict_linear
// activation tests assert the aggregate NAME and its parametric prefix, both of
// which are unchanged (cerberus issue #2943; the mutant reached a verdict for
// the first time once #2940 stopped `go vet`'s bools analyzer rejecting it as
// "suspect and").
//
// nativeGridTsAxisFrag has TWO callers — the range emitter
// (range_window_grid_native.go) and the instant one
// (range_window_grid_native_instant.go), which build different node types and
// so cannot share one driver. Both are covered below, mirroring the way
// range_window_grid_native_scan_bound_test.go pairs its registry loop with
// TestNativeTSGrid_ScanBound_Resample for the node type that loop cannot reach.

// nativeWholeSecondTsAxis records, per registered native function, whether its
// timestamp axis is the whole-second `toDateTime(<ts>)` form (true) or the raw
// timestamp column (false). Only the least-squares regression pair takes the
// whole-second axis; see the file header.
var nativeWholeSecondTsAxis = map[string]bool{
	"rate":           false,
	"increase":       false,
	"changes":        false,
	"resets":         false,
	"deriv":          true,
	"predict_linear": true,
	"delta":          false,
	"irate":          false,
	"idelta":         false,
}

// nativeTsAxisRaw / nativeTsAxisWholeSecond are the two renderings of the axis
// argument as it appears at the head of the aggregate's argument group. Under
// strings.HasPrefix over the argument group they are mutually exclusive in both
// directions, so asserting the expected one is enough — no negative half is
// needed to tell them apart.
const (
	nativeTsAxisRaw         = "`" + nativeScanBoundTSCol + "`"
	nativeTsAxisWholeSecond = "toDateTime(`" + nativeScanBoundTSCol + "`)"
)

// nativeGridArgGroup returns the text INSIDE the argument (second) paren group
// of the first `fn(params...)(args...)` parametric call in sql. Both groups
// nest parens of their own (`toDateTime(…, 'UTC')` in the params,
// `toDateTime(<ts>)` in the args), so each is found by matching parens rather
// than by searching for the next `)`.
//
// It reads the FIRST such call. Every caller below emits exactly one native
// node — the range cases are guarded by requireNativeNodeCount(t, plan, 1) and
// the instant case builds a single node by hand — which matters because `rate`
// and `increase` share the aggregate name `timeSeriesRateToGrid`, so a plan
// carrying two native nodes would silently be asserted on its first one only.
// The `fn+"("` match also keeps the `…ToGridState(` / `…ToGridMerge(`
// combinators from being mistaken for the aggregate itself.
func nativeGridArgGroup(sql, fn string) (string, bool) {
	open := strings.Index(sql, fn+"(")
	if open < 0 {
		return "", false
	}
	paramsEnd, ok := matchParen(sql, open+len(fn))
	if !ok || paramsEnd+1 >= len(sql) || sql[paramsEnd+1] != '(' {
		return "", false
	}
	argsEnd, ok := matchParen(sql, paramsEnd+1)
	if !ok {
		return "", false
	}
	return sql[paramsEnd+2 : argsEnd], true
}

// matchParen returns the index of the `)` closing the `(` at position open, or
// false when the group is unterminated.
func matchParen(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// assertNativeTsAxis fails unless the aggregate's argument group leads with the
// axis form recorded for fn.
func assertNativeTsAxis(t *testing.T, fn, aggFn, sql string, wholeSecond bool) {
	t.Helper()

	args, ok := nativeGridArgGroup(sql, aggFn)
	if !ok {
		t.Fatalf("%s: emitted SQL contains no complete %q parametric call — the expression did not reach the native emitter\nSQL: %s",
			fn, aggFn, sql)
	}
	want := nativeTsAxisRaw
	if wholeSecond {
		want = nativeTsAxisWholeSecond
	}
	if !strings.HasPrefix(args, want+",") {
		t.Errorf("%s: %s's timestamp axis is not %s — the emitted grid would be computed on the wrong x-axis\nargument group: %s",
			fn, aggFn, want, args)
	}
}

// TestNativeTSGrid_TsAxis_EveryRegisteredFunc pins the per-function timestamp
// axis for every member of the emitter's native registry, through the RANGE
// emitter. Driven by the registry itself, so a native aggregate registered
// later cannot ship without recording an axis choice here.
func TestNativeTSGrid_TsAxis_EveryRegisteredFunc(t *testing.T) {
	t.Parallel()

	if len(nativeTSGridFn) == 0 {
		t.Fatal("nativeTSGridFn is empty — the ratchet below would vacuously pass")
	}

	for fn, agg := range nativeTSGridFn {
		wholeSecond, recorded := nativeWholeSecondTsAxis[fn]
		if !recorded {
			t.Errorf("nativeTSGridFn registers %q but nativeWholeSecondTsAxis records no timestamp-axis "+
				"choice for it — record one, or the new native aggregate ships with no axis coverage", fn)
			continue
		}
		tc, ok := nativeScanBoundCases[fn]
		if !ok {
			t.Errorf("nativeTSGridFn registers %q but nativeScanBoundCases has no expression for it", fn)
			continue
		}
		t.Run(fn, func(t *testing.T) {
			t.Parallel()

			plan := lowerNativeScanBound(t, tc.query, tc.lowerers)
			requireNativeNodeCount(t, plan, 1)
			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("emit %q: %v", tc.query, err)
			}
			assertNativeTsAxis(t, fn, agg.Fn, sql, wholeSecond)
		})
	}
}

// nativeTsAxisInstantWindow / nativeTsAxisInstantHorizon are the instant
// fixture's lookback window and predict_linear's whole-second horizon t.
const (
	nativeTsAxisInstantWindow  = 5 * time.Minute
	nativeTsAxisInstantHorizon = 3600
)

// nativeTsAxisInstantOutOfScope names the two registry members the INSTANT
// emitter rejects outright rather than emitting: the lowering never builds an
// instant node for them, and the emitter fails loudly instead of accepting a
// shape nothing has differentially proven (cerberus issue #2748). They are
// therefore skipped by the loop below rather than left silently uncovered.
var nativeTsAxisInstantOutOfScope = map[string]bool{"increase": true, "delta": true}

// TestNativeTSGrid_TsAxis_InstantEmitter covers nativeGridTsAxisFrag's OTHER
// caller. RangeWindowGridNativeInstant is a distinct node type with its own
// emitter, so the registry loop above cannot reach it — the same gap
// TestNativeTSGrid_ScanBound_Resample closes for the scan bound.
//
// The node is built directly rather than lowered: instant native routing is
// per-function opt-in at the lowering seam, and this test is about the
// emitter's axis choice, not about which queries reach it.
func TestNativeTSGrid_TsAxis_InstantEmitter(t *testing.T) {
	t.Parallel()

	anchor := time.Date(2029, 2, 3, 4, 0, 0, 0, time.UTC)

	for fn, agg := range nativeTSGridFn {
		if nativeTsAxisInstantOutOfScope[fn] {
			continue
		}
		wholeSecond, recorded := nativeWholeSecondTsAxis[fn]
		if !recorded {
			t.Errorf("nativeTSGridFn registers %q but nativeWholeSecondTsAxis records no timestamp-axis choice for it", fn)
			continue
		}
		t.Run(fn, func(t *testing.T) {
			t.Parallel()

			node := &chplan.RangeWindowGridNativeInstant{
				Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:            fn,
				Range:           nativeTsAxisInstantWindow,
				Anchor:          anchor,
				TimestampColumn: nativeScanBoundTSCol,
				ValueColumn:     "Value",
				GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			}
			if fn == "predict_linear" {
				node.Scalars = []float64{nativeTsAxisInstantHorizon}
			}
			sql, _, err := Emit(context.Background(), node)
			if err != nil {
				t.Fatalf("emit instant %q: %v", fn, err)
			}
			assertNativeTsAxis(t, fn, agg.Fn, sql, wholeSecond)
		})
	}
}

// TestNativeTSGrid_TsAxis_InstantOutOfScopeStillRejected keeps the skip list
// above honest: the two functions it excludes are excluded because the instant
// emitter REFUSES them, not because covering them was inconvenient. If either
// ever starts emitting, this fails and the skip has to be reconsidered rather
// than quietly hiding an uncovered axis choice.
func TestNativeTSGrid_TsAxis_InstantOutOfScopeStillRejected(t *testing.T) {
	t.Parallel()

	anchor := time.Date(2029, 2, 3, 4, 0, 0, 0, time.UTC)

	if len(nativeTsAxisInstantOutOfScope) == 0 {
		t.Fatal("nativeTsAxisInstantOutOfScope is empty — this test would vacuously pass")
	}
	for fn := range nativeTsAxisInstantOutOfScope {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()

			node := &chplan.RangeWindowGridNativeInstant{
				Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:            fn,
				Range:           nativeTsAxisInstantWindow,
				Anchor:          anchor,
				TimestampColumn: nativeScanBoundTSCol,
				ValueColumn:     "Value",
				GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			}
			if _, _, err := Emit(context.Background(), node); err == nil {
				t.Fatalf("instant emitter accepted %q — it is listed as out of scope, so either the "+
					"exclusion in nativeTsAxisInstantOutOfScope is stale or the axis choice is now uncovered", fn)
			}
		})
	}
}
