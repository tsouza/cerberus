package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The native timeSeries*ToGrid family bounds its innermost MergeTree read at
// EMIT time (maybePushRangeScanTimeBound), not via an IR flag the way the
// instant windowed-array leaf does (chplan.RangeWindow.InstantScanBounded plus
// the fail-closed RequireScanTimeBound analyzer). An emit-time-only bound is
// exactly the shape that gets silently dropped: remove the call and the
// emitters still produce CORRECT SQL, because the aggregate's own (start, end,
// step, window) parameters already discard out-of-window samples — only the
// rows READ change. No test fails, no result changes; the goldens simply record
// a scan with no time predicate, and regenerating them accepts it. That is the
// mechanism by which this bound has to be re-remembered per emitter (see the
// history in internal/chplan/scan_time_bound.go).
//
// These tests close the hole for the family as a CLASS rather than per member.
// The case list is driven by the emitter's OWN registry (nativeTSGridFn), and a
// registered function with no case here is a hard failure — so a native
// function added later cannot ship without a scan-bound pin. The assertions are
// on the PREDICATE rather than on golden text, so they survive unrelated SQL
// churn and cannot be satisfied by regenerating a golden.

// nativeScanBoundTSCol is the canonical OTel-CH sample-timestamp column the
// lowered fixtures scan on.
const nativeScanBoundTSCol = "TimeUnix"

// nativeScanBoundLower / nativeScanBoundUpper are the two halves of the
// inner-scan time bound as innerScanTsBoundsFrags renders it: a half-open
// `(Start - Offset - window, End - Offset]` span on the scan's timestamp
// column. BOTH must be present — an upper bound alone still lets ClickHouse
// read the whole retention below the window.
var (
	nativeScanBoundLower = "`" + nativeScanBoundTSCol + "` > "
	nativeScanBoundUpper = "`" + nativeScanBoundTSCol + "` <= "
)

// nativeScanBoundWindowSub is the interval subtraction that widens the lower
// bound by the trailing window, so the earliest anchors still see a full
// window's worth of samples. A lower bound without it would silently truncate
// the first anchors' windows.
const nativeScanBoundWindowSub = "toIntervalNanosecond"

const nativeScanBoundStep = 30 * time.Second

func nativeScanBoundGrid() (time.Time, time.Time) {
	start := time.Date(2029, 2, 3, 4, 0, 0, 0, time.UTC)
	return start, start.Add(time.Hour)
}

// nativeScanBoundCase pairs a PromQL expression that lowers to the native node
// for one registered function with the boot-wired strategy table that enables
// it. Both are needed: the native lowering is opt-in per function.
type nativeScanBoundCase struct {
	query    string
	lowerers promql.RangeLowerers
}

// nativeScanBoundCases maps each nativeTSGridFn member to an expression that
// exercises it. The registry loop below fails when a registered function is
// missing from this map, which is what makes the coverage a ratchet rather than
// a snapshot.
var nativeScanBoundCases = map[string]nativeScanBoundCase{
	"rate": {
		query:    "rate(requests_total[5m])",
		lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}},
	},
	"changes": {
		query:    "changes(queue_depth[5m])",
		lowerers: promql.RangeLowerers{Changes: promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}},
	},
	"resets": {
		query:    "resets(requests_total[5m])",
		lowerers: promql.RangeLowerers{Resets: promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}},
	},
	"deriv": {
		query:    "deriv(queue_depth[5m])",
		lowerers: promql.RangeLowerers{Deriv: promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}},
	},
	"predict_linear": {
		query:    "predict_linear(queue_depth[5m], 600)",
		lowerers: promql.RangeLowerers{PredictLinear: promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}},
	},
}

// lowerNativeScanBound lowers q in range mode over the default OTel-CH metrics
// schema with the supplied boot-wired strategy table. Going through the REAL
// lowering (rather than hand-building the node) is what makes these tests
// evidence about the shape the server actually emits.
func lowerNativeScanBound(t *testing.T, q string, lowerers promql.RangeLowerers) chplan.Node {
	t.Helper()

	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	start, end := nativeScanBoundGrid()
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, end, nativeScanBoundStep, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

// requireNativeNodeCount fails unless plan carries exactly want native nodes —
// the guard against a test that silently proves nothing because the expression
// fell back to the fan-out lowering.
func requireNativeNodeCount(t *testing.T, plan chplan.Node, want int) {
	t.Helper()

	got := 0
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.RangeWindowNative, *chplan.RangeWindowResample:
			got++
		}
		return true
	})
	if got != want {
		t.Fatalf("plan carries %d native ts-grid nodes, want %d — the expression fell back to the "+
			"fan-out lowering, so this case proves nothing about the native scan bound", got, want)
	}
}

// assertNativeScanBounded fails unless sql carries BOTH halves of the
// inner-scan time bound plus the trailing-window widening.
func assertNativeScanBounded(t *testing.T, name, sql string) {
	t.Helper()

	if !strings.Contains(sql, nativeScanBoundLower) {
		t.Errorf("%s: emitted SQL carries no lower scan bound (%s) — the inner read is unbounded below, so "+
			"ClickHouse cannot prune granules and the aggregate consumes every retained sample of every "+
			"matching series\nSQL: %s", name, nativeScanBoundLower, sql)
	}
	if !strings.Contains(sql, nativeScanBoundUpper) {
		t.Errorf("%s: emitted SQL carries no upper scan bound (%s) — the inner read is unbounded above\nSQL: %s",
			name, nativeScanBoundUpper, sql)
	}
	if !strings.Contains(sql, nativeScanBoundWindowSub) {
		t.Errorf("%s: the lower scan bound does not widen by the trailing window (%s) — the earliest "+
			"anchors would evaluate over a truncated window\nSQL: %s", name, nativeScanBoundWindowSub, sql)
	}
}

// TestNativeTSGrid_ScanBound_EveryRegisteredFunc is the class-level ratchet:
// EVERY function in the emitter's native registry lowers to a node whose inner
// scan is time-bounded. Registering a new native aggregate without a case here
// fails; registering one whose emitter drops the bound fails too.
func TestNativeTSGrid_ScanBound_EveryRegisteredFunc(t *testing.T) {
	t.Parallel()

	if len(nativeTSGridFn) == 0 {
		t.Fatal("nativeTSGridFn is empty — the ratchet below would vacuously pass")
	}

	for fn := range nativeTSGridFn {
		tc, ok := nativeScanBoundCases[fn]
		if !ok {
			t.Errorf("nativeTSGridFn registers %q but nativeScanBoundCases has no expression for it — "+
				"add one, or the new native aggregate ships with no scan-bound coverage", fn)
			continue
		}
		t.Run(fn, func(t *testing.T) {
			t.Parallel()

			plan := lowerNativeScanBound(t, tc.query, tc.lowerers)
			requireNativeNodeCount(t, plan, 1)

			sql, _, err := Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			assertNativeScanBounded(t, fn, sql)
		})
	}
}

// TestNativeTSGrid_ScanBound_Resample covers the family's other member, the
// native staleness resample. It is a distinct node type with its own emitter,
// so the registry loop above cannot reach it.
func TestNativeTSGrid_ScanBound_Resample(t *testing.T) {
	t.Parallel()

	plan := lowerNativeScanBound(t, "process_open_fds", promql.RangeLowerers{
		Staleness: promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}},
	})
	requireNativeNodeCount(t, plan, 1)

	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	assertNativeScanBounded(t, "resample", sql)
}

// TestNativeTSGrid_ScanBound_PerJoinOperand pins that the bound is applied to
// EACH operand of a vector-vector join independently, not once at the top. A
// binary op over two native windows is two separate scans, and a bound rendered
// on only one of them leaves the other reading unbounded — which the
// single-operand cases above cannot detect, because they would still find the
// one bound that is present.
func TestNativeTSGrid_ScanBound_PerJoinOperand(t *testing.T) {
	t.Parallel()

	plan := lowerNativeScanBound(t,
		"sum(rate(requests_total[5m])) / sum(rate(attempts_total[5m]))",
		promql.RangeLowerers{Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}})

	const joinOperands = 2
	requireNativeNodeCount(t, plan, joinOperands)

	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	assertNativeScanBounded(t, "join", sql)

	// One aggregate and one bound pair per operand — a bound count below the
	// operand count means some operand scans unbounded.
	for _, probe := range []struct {
		what string
		s    string
	}{
		{"native aggregate", nativeTSGridFn["rate"]},
		{"lower scan bound", nativeScanBoundLower},
		{"upper scan bound", nativeScanBoundUpper},
	} {
		if got := strings.Count(sql, probe.s); got != joinOperands {
			t.Errorf("join emits %d %s(s), want %d (one per operand)\nSQL: %s",
				got, probe.what, joinOperands, sql)
		}
	}
}
