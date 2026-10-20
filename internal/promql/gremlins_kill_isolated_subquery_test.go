package promql

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// isolatedEval is the request instant the isolated lowerings below
// evaluate at.
var isolatedEval = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// lowerIsolated lowers q for an instant query at [isolatedEval] with a
// guard sink, and returns the plan, the runs of its one isolated
// subquery, and the guards recorded.
func lowerIsolated(t *testing.T, q string) (chplan.Node, []chplan.Node, []ScalarGuard) {
	t.Helper()
	var guards []ScalarGuard
	plan, err := LowerAtRangeOpts(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(), isolatedEval, isolatedEval, 0, LowerOpts{Guards: &guards})
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	var runs []chplan.Node
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if u, ok := n.(*chplan.UnionAll); ok && runs == nil {
			runs = u.Inputs
		}
		return true
	})
	return plan, runs, guards
}

// runWindow returns the evaluation instants of a run: the span of the
// first window it evaluates, the inner expression's own.
func runWindow(t *testing.T, run chplan.Node) [2]time.Time {
	t.Helper()
	var span *[2]time.Time
	chplan.WalkDeep(run, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && span == nil {
			span = &[2]time.Time{rw.Start.UTC(), rw.End.UTC()}
		}
		return span == nil
	})
	if span == nil {
		t.Fatal("run evaluates no window")
	}
	return *span
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// TestIsolatedSubquery_RunsSplitTheGrid pins the runs an isolated
// subquery lowers its inner expression over. The 292y grid of 100-day
// steps holds 1066 instants, from 1734-04-08 to 2025-11-08, and a run
// holds at most 533 steps of 100 days (maxRangeSpan), so it splits in two
// runs of 533 instants each, in Prometheus's frame. A step longer than a
// run gives each instant a run of its own.
func TestIsolatedSubquery_RunsSplitTheGrid(t *testing.T) {
	plan, runs, _ := lowerIsolated(t, `count_over_time(count_over_time(vector(1)[292y:100d])[292y:100d])`)
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if f, ok := n.(*chplan.Filter); ok {
			if b, ok := f.Predicate.(*chplan.LitBool); ok && !b.V {
				t.Fatal("a grid holding instants is answered as empty")
			}
		}
		return true
	})
	want := [][2]time.Time{{day(1734, 4, 8), day(1879, 12, 4)}, {day(1880, 3, 13), day(2025, 11, 8)}}
	if len(runs) != len(want) {
		t.Fatalf("got %d runs, want %d", len(runs), len(want))
	}
	for i, run := range runs {
		if got := runWindow(t, run); got != want[i] {
			t.Fatalf("run %d evaluates %v, want %v", i, got, want[i])
		}
	}
	_, runs, _ = lowerIsolated(t, `count_over_time(count_over_time(vector(1)[292y:200y])[292y:200y])`)
	if len(runs) != 2 {
		t.Fatalf("got %d runs of one instant, want 2", len(runs))
	}
	for i, run := range runs {
		if got := runWindow(t, run); !got[0].Equal(got[1]) {
			t.Fatalf("run %d evaluates %v, want one instant", i, got)
		}
	}
}

// TestIsolatedSubquery_RunsKeepRequestPins pins that `@ end()` inside an
// isolated subquery names the request's end in every run, not the run's.
func TestIsolatedSubquery_RunsKeepRequestPins(t *testing.T) {
	plan, runs, _ := lowerIsolated(t, `count_over_time((count_over_time(vector(1)[292y:100d]) + count_over_time(vector(1)[1d:1d] @ end()))[292y:100d])`)
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	pinned := 0
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && rw.Range == 24*time.Hour {
			pinned++
			if !rw.End.Equal(isolatedEval) {
				t.Fatalf("`@ end()` window ends at %s, want the request's end %s", rw.End, isolatedEval)
			}
		}
		return true
	})
	if pinned != len(runs) {
		t.Fatalf("got %d `@ end()` windows, want one per run", pinned)
	}
}

// TestIsolatedSubquery_RunsRecordGuards pins that every run records the
// scalar guards of its inner expression on the request's sink.
func TestIsolatedSubquery_RunsRecordGuards(t *testing.T) {
	_, runs, guards := lowerIsolated(t, `count_over_time(count_over_time(topk(scalar(vector(1)), vector(1))[292y:100d])[292y:100d])`)
	if len(runs) != 2 || len(guards) != len(runs) {
		t.Fatalf("got %d guards over %d runs, want one per run", len(guards), len(runs))
	}
}

// TestIsolatedSubquery_RunsReportInEnclosingFrame pins that each run
// reports its instants moved back to the frame of the region enclosing
// the subquery. The pinned outer grid runs from -18429120000s, in 1385,
// in runs of 533 steps of 100 days; each run is lowered in its own frame,
// in which it starts on 1970-07-20 (17280000s); the region is moved by
// 16139520000s. Each run therefore moves its instants by its own frame's
// shift less the region's:
//
//	17280000 + 18429120000 - 16139520000 = 2306880000s
//	17280000 + 18429120000 - 533*8640000 - 16139520000 = -2298240000s
func TestIsolatedSubquery_RunsReportInEnclosingFrame(t *testing.T) {
	_, runs, _ := lowerIsolated(t, `sum_over_time(count_over_time(vector(1)[292y:100d])[292y:100d] @ -9223371000)`)
	want := []int64{2306880000 * int64(time.Second), -2298240000 * int64(time.Second)}
	if len(runs) != len(want) {
		t.Fatalf("got %d runs, want %d", len(runs), len(want))
	}
	for i, run := range runs {
		if got := runWindow(t, run); !got[0].Equal(day(1970, 7, 20)) {
			t.Fatalf("run %d starts at %s in its frame, want 1970-07-20", i, got[0])
		}
		var moved []int64
		chplan.WalkDeep(run, func(n chplan.Node) bool {
			p, ok := n.(*chplan.Project)
			if !ok {
				return true
			}
			for _, pr := range p.Projections {
				if v, ok := unframedNanos(pr); ok {
					moved = append(moved, v)
				}
			}
			return true
		})
		if len(moved) != 1 || moved[0] != want[i] {
			t.Fatalf("run %d moves its instants by %v ns, want %d", i, moved, want[i])
		}
	}
}

// unframedNanos returns the shift, in nanoseconds, a timestamp projection
// built by [unframeInstant] moves its instant back by.
func unframedNanos(pr chplan.Projection) (int64, bool) {
	call, ok := pr.Expr.(*chplan.FuncCall)
	if !ok || pr.Alias != schema.DefaultOTelMetrics().TimestampColumn || call.Fn != chplan.FnFromUnixNanos {
		return 0, false
	}
	sub, ok := call.Args[0].(*chplan.Binary)
	if !ok || sub.Op != chplan.OpSub {
		return 0, false
	}
	lit, ok := sub.Right.(*chplan.LitInt)
	if !ok {
		return 0, false
	}
	return lit.V, true
}

// TestUnshiftRoot_MovesOnlyTheTimestamp pins that a root lowered with a
// shift reports its timestamp moved back and every other column as it
// is: a subquery root moved by its region's shift, one moved by the
// frame's, and a selector and an aggregation in a shifted frame.
func TestUnshiftRoot_MovesOnlyTheTimestamp(t *testing.T) {
	near := instantDomainMin.Add(2 * time.Minute)
	ts := schema.DefaultOTelMetrics().TimestampColumn
	for _, tc := range []struct {
		q  string
		at time.Time
	}{
		{`vector(1)[5m:1m] @ -9223372036`, isolatedEval},
		{`m[5m:1m]`, near},
		{`m`, near},
		{`sum(m)`, near},
	} {
		plan, err := LowerAt(context.Background(), mustParseExperimental(t, tc.q), schema.DefaultOTelMetrics(), tc.at, tc.at)
		if err != nil {
			t.Fatalf("lower %q: %v", tc.q, err)
		}
		p, ok := plan.(*chplan.Project)
		if !ok {
			t.Fatalf("%q: root is %T, want the projection moving its timestamp", tc.q, plan)
		}
		columns := p.Input.RowType().Columns
		if len(p.Projections) != len(columns) {
			t.Fatalf("%q: %d projections over %d columns, want one per column", tc.q, len(p.Projections), len(columns))
		}
		moved := 0
		for i, pr := range p.Projections {
			if pr.Alias != columns[i].Name {
				t.Fatalf("%q: projection %d is %s, want column %s", tc.q, i, pr.Alias, columns[i].Name)
			}
			if pr.Alias == ts {
				if _, isRef := pr.Expr.(*chplan.ColumnRef); isRef {
					t.Fatalf("%q: timestamp is not moved", tc.q)
				}
				moved++
				continue
			}
			if ref, isRef := pr.Expr.(*chplan.ColumnRef); !isRef || ref.Name != pr.Alias {
				t.Fatalf("%q: column %s is %#v, want itself", tc.q, pr.Alias, pr.Expr)
			}
		}
		if moved != 1 {
			t.Fatalf("%q: %d timestamp projections, want 1", tc.q, moved)
		}
	}
}
