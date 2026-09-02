package chsql

import (
	"context"
	"strings"
	"testing"
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
// The assertion below is a class-level ratchet driven by the emitter's OWN
// registry, in the same shape as TestNativeTSGrid_ScanBound_EveryRegisteredFunc
// in range_window_grid_native_scan_bound_test.go: every registered native
// function must have its axis choice recorded here, so a function added later
// cannot ship without one, and the two axis forms are mutually exclusive so
// neither expectation can be satisfied vacuously.

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
// argument as it appears at the head of the aggregate's argument group.
const (
	nativeTsAxisRaw          = "`" + nativeScanBoundTSCol + "`"
	nativeTsAxisWholeSecond  = "toDateTime(`" + nativeScanBoundTSCol + "`)"
	errNativeAggNotRendered  = "emitted SQL contains no %q call — the expression did not reach the native emitter"
	errNativeAggUnterminated = "emitted SQL's %q call is unterminated — cannot read its argument group"
)

// nativeGridArgGroup returns the text of the argument (second) paren group of
// the first `fn(params...)(args...)` parametric call in sql. The params group
// nests parens of its own (`toDateTime(…, 'UTC')`), so the scan matches them
// rather than searching for the first `)`.
func nativeGridArgGroup(sql, fn string) (string, bool) {
	i := strings.Index(sql, fn+"(")
	if i < 0 {
		return "", false
	}
	depth := 0
	for j := i + len(fn); j < len(sql); j++ {
		switch sql[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				// End of the params group; the args group opens next.
				if j+1 >= len(sql) || sql[j+1] != '(' {
					return "", false
				}
				return sql[j+2:], true
			}
		}
	}
	return "", false
}

// TestNativeTSGrid_TsAxis_EveryRegisteredFunc pins the per-function timestamp
// axis for every member of the emitter's native registry.
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
			args, ok := nativeGridArgGroup(sql, agg.Fn)
			if !ok {
				if !strings.Contains(sql, agg.Fn+"(") {
					t.Fatalf(errNativeAggNotRendered+"\nSQL: %s", agg.Fn, sql)
				}
				t.Fatalf(errNativeAggUnterminated+"\nSQL: %s", agg.Fn, sql)
			}

			want, unwanted := nativeTsAxisRaw, nativeTsAxisWholeSecond
			if wholeSecond {
				want, unwanted = nativeTsAxisWholeSecond, nativeTsAxisRaw
			}
			if !strings.HasPrefix(args, want+",") {
				t.Errorf("%s: %s's timestamp axis is not %s — the emitted grid would be computed on the "+
					"wrong x-axis\nargument group: %s", fn, agg.Fn, want, args)
			}
			// Mutually exclusive by construction: the whole-second form
			// CONTAINS the raw form, so the negative half is asserted only
			// for the raw expectation, where it is discriminating.
			if !wholeSecond && strings.HasPrefix(args, unwanted+",") {
				t.Errorf("%s: %s's timestamp axis is %s, but this function reads the raw column\n"+
					"argument group: %s", fn, agg.Fn, unwanted, args)
			}
		})
	}
}
