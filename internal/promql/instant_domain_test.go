package promql

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// instantDomainEvalEnd is the request instant every case below lowers at
// unless the case names its own.
var instantDomainEvalEnd = time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)

// instantDomainMode is one way a case is lowered: an instant query at
// instantDomainEvalEnd, or a range query ending there.
type instantDomainMode struct {
	name  string
	start time.Time
	step  time.Duration
}

var instantDomainModes = []instantDomainMode{
	{name: "instant", start: instantDomainEvalEnd},
	{name: "range", start: instantDomainEvalEnd.Add(-5 * time.Minute), step: time.Minute},
}

// The request instants a few minutes inside the int64-nanosecond floor and
// ceiling, to which the API's request parsing clamps a request.
var (
	instantDomainNearFloor   = time.Date(1677, 9, 21, 0, 20, 0, 0, time.UTC)
	instantDomainNearCeiling = time.Date(2262, 4, 11, 23, 46, 0, 0, time.UTC)
)

func lowerInstantDomainCase(t *testing.T, q string, m instantDomainMode) error {
	t.Helper()
	_, err := LowerAtRange(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(),
		m.start, instantDomainEvalEnd, m.step)
	return err
}

// fitAt fits q for an instant query at end.
func fitAt(t *testing.T, q string, end time.Time) (parser.Expr, *domainFit, error) {
	t.Helper()
	return fitInstantDomain(mustParseExperimental(t, q), lowerCtx{start: end, end: end})
}

// mustUTC parses an RFC 3339 instant.
func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// isInstantDomainError reports whether err is the instant-domain rejection.
func isInstantDomainError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "outside the range cerberus supports")
}

// TestPromPinnedInstant pins the transcription of Prometheus's `@`
// arithmetic: a pin more than about 292 years from the evaluator's start
// wraps time.Duration, and Prometheus evaluates at the wrapped instant.
func TestPromPinnedInstant(t *testing.T) {
	start := instantDomainEvalEnd.UnixMilli()
	ev := promEval{start: start, offEval: start, req: &[2]int64{start, start}}
	cases := []struct {
		name        string
		pin         int64
		offset      time.Duration
		wantInstant int64
		wantWrapped bool
	}{
		// In range: the pin minus the offset.
		{"in_range", 1767225600000, 0, 1767225600000, false},
		{"in_range_offset", 1767225600000, time.Hour, 1767222000000, false},
		// 2286 is 260 years from 2026: no wrap, although DateTime64(9)
		// ends in 2262.
		{"2286_does_not_wrap", 10000000000000, 0, 10000000000000, false},
		// Year one runs in 1754, one wrap of 2^64 nanoseconds per 584
		// years, three of them.
		{"year_one_runs_in_1754", -62135596800000, 0, -6795364578871, true},
		// 1675 runs in 2259.
		{"1675_runs_in_2259", -9300000000000, 0, 9146744073709, true},
		{"floor_offset_runs_in_2262", -9223372036000, time.Hour, 9223368437709, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, wrapped := promPinnedInstant(ev, tc.pin, tc.offset)
			if got != tc.wantInstant || wrapped != tc.wantWrapped {
				t.Fatalf("promPinnedInstant(%d, %s) = %d, %v; want %d, %v", tc.pin, tc.offset, got, wrapped, tc.wantInstant, tc.wantWrapped)
			}
		})
	}
}

// TestPromSubqueryStart pins Prometheus's inner-evaluator start: the first
// multiple of the step strictly after start - offset - range, on both sides
// of the epoch.
func TestPromSubqueryStart(t *testing.T) {
	cases := []struct {
		start, offset, rng, step, want int64
	}{
		{start: 600_000, rng: 300_000, step: 60_000, want: 360_000},
		{start: 630_000, rng: 300_000, step: 60_000, want: 360_000},
		{start: -600_000, rng: 300_000, step: 60_000, want: -840_000},
		{start: -630_000, rng: 300_000, step: 60_000, want: -900_000},
		{start: 600_000, offset: 30_000, rng: 300_000, step: 60_000, want: 300_000},
	}
	for _, tc := range cases {
		if got := promSubqueryStart(tc.start, tc.offset, tc.rng, tc.step); got != tc.want {
			t.Errorf("promSubqueryStart(%d, %d, %d, %d) = %d, want %d", tc.start, tc.offset, tc.rng, tc.step, got, tc.want)
		}
	}
}

// TestInstantDomain_ResolvesPins pins how each pin of the issue is
// prepared: a wrapped pin is re-pinned at the instant Prometheus evaluates
// it at, and a region whose instants leave the domain is shifted into it.
func TestInstantDomain_ResolvesPins(t *testing.T) {
	cases := []struct {
		query   string
		want    string
		shifted bool
	}{
		{`count_over_time(vector(1)[5m:1m] @ -62135596800)`, `count_over_time(vector(1)[5m:1m] @ -6795364578.871)`, false},
		{`count_over_time(vector(1)[5m:1m] @ -9300000000)`, `count_over_time(vector(1)[5m:1m] @ 9146744073.709)`, false},
		{`count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`, `count_over_time(vector(1)[5m:1m] @ 9223368437.709)`, false},
		{`count_over_time(vector(1)[5m:1m] @ 10000000000)`, ``, true},
		{`count_over_time(vector(1)[5m:1m] @ -9223372036.854)`, ``, true},
		{`count_over_time((vector(1) * 2)[5m:1m] @ -62135596800)`, `count_over_time((vector(1) * 2)[5m:1m] @ -6795364578.871)`, false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			expr := mustParseExperimental(t, tc.query)
			before := expr.String()
			got, fit, err := fitInstantDomain(expr, lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			if expr.String() != before {
				t.Fatalf("fitInstantDomain modified its input: %q became %q", before, expr.String())
			}
			if tc.want != "" && got.String() != tc.want {
				t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got, tc.want)
			}
			sub := got.(*parser.Call).Args[0].(*parser.SubqueryExpr)
			shift, ok := fit.shiftOf(sub)
			if ok != tc.shifted {
				t.Fatalf("fitInstantDomain(%q): shifted = %v, want %v", tc.query, ok, tc.shifted)
			}
			if !tc.shifted {
				return
			}
			if shift == 0 || shift%time.Minute.Milliseconds() != 0 {
				t.Fatalf("fitInstantDomain(%q): shift %d ms is not a nonzero multiple of the 1m step", tc.query, shift)
			}
			// Every node of the region carries the shift.
			parser.Inspect(sub, func(node parser.Node, _ []parser.Node) error {
				if node == nil {
					return nil
				}
				if s, ok := fit.shiftOf(node); !ok || s != shift {
					t.Errorf("node %q: shift %d, %v; want %d", node, s, ok, shift)
				}
				return nil
			})
			at := time.UnixMilli(*sub.Timestamp).UTC()
			if at.Before(instantDomainMin) || at.After(instantDomainMax) {
				t.Fatalf("fitInstantDomain(%q): shifted pin %s is outside the domain", tc.query, at)
			}
		})
	}
}

// TestInstantDomain_ShiftsUnpinnedGridNearBounds pins an unpinned constant
// grid that leaves the domain at a request near its bounds: the subquery
// is moved into the domain by an offset, a multiple of its step.
func TestInstantDomain_ShiftsUnpinnedGridNearBounds(t *testing.T) {
	cases := []struct {
		end    time.Time
		query  string
		offset time.Duration
	}{
		{instantDomainNearFloor, `count_over_time(vector(1)[5m:1m] offset 3m)`, 3 * time.Minute},
		{instantDomainNearCeiling, `count_over_time(vector(1)[5m:1m] offset -3m)`, -3 * time.Minute},
	}
	for _, tc := range cases {
		end, q := tc.end, tc.query
		got, fit, err := fitAt(t, q, end)
		if err != nil {
			t.Fatalf("fitInstantDomain(%q) at %s: %v", q, end, err)
		}
		sub := got.(*parser.Call).Args[0].(*parser.SubqueryExpr)
		shift, ok := fit.shiftOf(sub)
		if !ok || shift == 0 || shift%time.Minute.Milliseconds() != 0 {
			t.Fatalf("at %s: shift %d, %v; want a nonzero multiple of the 1m step", end, shift, ok)
		}
		if want := tc.offset - time.Duration(shift)*time.Millisecond; sub.OriginalOffset != want {
			t.Fatalf("at %s: offset %s, want %s", end, sub.OriginalOffset, want)
		}
	}
}

// TestInstantDomain_ShiftsNestedAndRequestPinRegions pins the regions
// holding a nested subquery or an `@ start()` / `@ end()` pin that leave
// the domain: each is answered, moved into the domain, alone or with the
// whole query, by a nonzero multiple of every grid step in it, or
// evaluated unshifted at the wrapped instant Prometheus evaluates its pin
// at.
func TestInstantDomain_ShiftsNestedAndRequestPinRegions(t *testing.T) {
	cases := []struct {
		name  string
		query string
		end   time.Time
		// unit is the step every shift is a multiple of; zero for a region
		// evaluated at a wrapped instant, which no shift moves.
		unit time.Duration
	}{
		{"nested_grid_near_floor", `max_over_time(max_over_time(timestamp(vector(1))[5m:1m])[3m:1m])`, instantDomainNearFloor.Add(-6 * time.Minute), time.Minute},
		{"nested_grid_past_ceiling", `max_over_time(ts_of_last_over_time(vector(1)[2m:1m])[5m:1m] @ 10000000000)`, instantDomainEvalEnd, time.Minute},
		{"parenthesised_nested_grid_past_ceiling", `max_over_time((ts_of_last_over_time(vector(1)[2m:1m]))[5m:1m] @ 10000000000)`, instantDomainEvalEnd, time.Minute},
		{"nested_steps_past_ceiling", `max_over_time(max_over_time(timestamp(vector(1))[2m:7s])[5m:1m] @ 10000000000)`, instantDomainEvalEnd, 7 * time.Minute},
		{"nested_grid_at_wrapped_pin", `max_over_time(max_over_time(timestamp(vector(1))[2m:7s])[5m:1m] @ -62135596800)`, instantDomainEvalEnd, 0},
		{"request_pin_in_grid_past_ceiling", `count_over_time((m{job="api"} @ end())[5m:1m] @ 10000000000)`, instantDomainEvalEnd, time.Minute},
		{"request_pin_in_wrapped_grid", `count_over_time((vector(1) and on() (m @ end()))[5m:1m] @ -62135596800)`, instantDomainEvalEnd, 0},
		{"request_pin_in_grid_near_floor", `count_over_time((vector(1) and on() (m @ end()))[5m:1m])`, instantDomainNearFloor.Add(-6 * time.Minute), time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LowerAt(context.Background(), mustParseExperimental(t, tc.query), schema.DefaultOTelMetrics(), tc.end, tc.end); err != nil {
				t.Fatalf("lower %q: %v", tc.query, err)
			}
			_, f, err := fitAt(t, tc.query, tc.end)
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			shifts := []int64{f.frameShift}
			for _, shift := range f.shifts {
				shifts = append(shifts, shift)
			}
			shifted := false
			for _, shift := range shifts {
				if tc.unit != 0 && shift%tc.unit.Milliseconds() != 0 {
					t.Fatalf("shifted by %d ms, not a multiple of %s", shift, tc.unit)
				}
				shifted = shifted || shift != 0
			}
			if shifted != (tc.unit != 0) {
				t.Fatalf("some node shifted: %v, want %v", shifted, tc.unit != 0)
			}
		})
	}
}

// TestInstantDomain_MarkShiftKeepsOwnPins pins the shifts a region records
// below its subquery: a pinned selector, matrix selector or subquery, `@
// end()` included, names instants of its own and is recorded unshifted
// with its whole subtree, every other node takes the region's shift, and
// the grid of each subquery takes the shift of the subquery itself.
func TestInstantDomain_MarkShiftKeepsOwnPins(t *testing.T) {
	expr := mustParseExperimental(t, `max_over_time((ts_of_last_over_time(vector(1)[2m:1m]) + max_over_time((vector(1) and on() n @ end())[2m:1m] @ 5) + count_over_time(o[1m] @ 7))[5m:1m] @ 10000000000)`)
	sub := expr.(*parser.Call).Args[0].(*parser.SubqueryExpr)
	const shift = 600000
	f := &domainFit{}
	f.shiftRegion(sub, shift, [2]time.Time{})
	sum := unwrapParens(sub.Expr).(*parser.BinaryExpr)
	left := sum.LHS.(*parser.BinaryExpr)
	nested := left.LHS.(*parser.Call).Args[0].(*parser.SubqueryExpr)
	pinnedSub := left.RHS.(*parser.Call).Args[0].(*parser.SubqueryExpr)
	pinnedMatrix := sum.RHS.(*parser.Call).Args[0].(*parser.MatrixSelector)
	for _, tc := range []struct {
		name string
		node parser.Node
		want int64
	}{
		{"region", sub, shift},
		{"nested subquery", nested, shift},
		{"nested subquery's inner", nested.Expr, shift},
		{"pinned subquery", pinnedSub, 0},
		{"pinned subquery's inner", pinnedSub.Expr, 0},
		{"pinned matrix", pinnedMatrix, 0},
		{"pinned matrix's selector", pinnedMatrix.VectorSelector, 0},
	} {
		if got, ok := f.shifts[tc.node]; !ok || got != tc.want {
			t.Errorf("%s: shift %d, %v; want %d", tc.name, got, ok, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		sub  *parser.SubqueryExpr
		want int64
	}{
		{"region", sub, shift},
		{"nested subquery", nested, shift},
		{"pinned subquery", pinnedSub, 0},
	} {
		if got := f.gridShift(tc.sub); got != tc.want {
			t.Errorf("%s: grid shift %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestInstantDomain_CutsWindowsAtStorageEdges pins the rewrite of a window
// reaching past the stored range: a function fixed by the samples the
// window holds reads the window cut back to the last stored millisecond
// or to the first one, and an instant selector whose lookback ends past
// the ceiling keeps the newest stored sample when it lies inside the
// lookback.
func TestInstantDomain_CutsWindowsAtStorageEdges(t *testing.T) {
	cases := []struct {
		name  string
		query string
		end   time.Time
		want  string
	}{
		{"window_past_ceiling", `count_over_time(m[10m] @ 9223372300)`, instantDomainEvalEnd, `count_over_time(m[5m36s854ms] @ 9223372036.854)`},
		{"window_past_ceiling_by_offset", `sum_over_time(m[10m] @ 9223372400 offset 100s)`, instantDomainEvalEnd, `sum_over_time(m[5m36s854ms] @ 9223372036.854)`},
		{"window_below_floor", `max_over_time(m[20m] @ -9223371200)`, instantDomainNearFloor, `max_over_time(m[13m56s854ms] @ -9223371200.000)`},
		{"window_below_floor_at_request", `max_over_time(m[10m])`, instantDomainNearFloor, `max_over_time(m[7m16s854ms] @ -9223371600.000)`},
		{"selector_past_ceiling", `m @ 9223372096.854`, instantDomainEvalEnd, `m @ 9223372036.854 and timestamp(m @ 9223372036.854) > 9223371796.8545`},
		{"rate_is_not_cut", `rate(m[10m] @ 9223371900)`, instantDomainEvalEnd, `rate(m[10m] @ 9223371900.000)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lowerCtx{start: tc.end, end: tc.end}
			got, _, err := fitInstantDomain(mustParseExperimental(t, tc.query), ctx)
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			if got.String() != tc.want {
				t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got.String(), tc.want)
			}
		})
	}
	// The printed range drops nanoseconds: a window cut at the floor
	// starts one nanosecond before the first stored millisecond, so that a
	// sample there stays inside it.
	got, _, err := fitInstantDomain(mustParseExperimental(t, `max_over_time(m[20m] @ -9223371200)`), lowerCtx{start: instantDomainNearFloor, end: instantDomainNearFloor})
	if err != nil {
		t.Fatal(err)
	}
	const wantRange = 13*time.Minute + 56854*time.Millisecond + time.Nanosecond
	if r := got.(*parser.Call).Args[0].(*parser.MatrixSelector).Range; r != wantRange {
		t.Fatalf("window cut at the floor has range %v, want %v", r, wantRange)
	}
}

// TestInstantDomain_Boundaries pins the exact edges at the ceiling, where
// a pin is not wrapped. A subquery grid reaches a step past its end; a
// grid that reads nothing is shifted instead. The last accepted pin and
// the first rejected one sit one millisecond apart in Prometheus's frame;
// the lowering answers both, the rejected one in a shifted frame.
func TestInstantDomain_Boundaries(t *testing.T) {
	instant := instantDomainModes[0]
	cases := []struct {
		name   string
		query  string
		accept bool
	}{
		// 23:46:16.854 + 1m step = 23:47:16.854.
		{"subquery_highest_accepted", `count_over_time(m[5m:1m] @ 9223371976.854)`, true},
		{"subquery_lowest_rejected", `count_over_time(m[5m:1m] @ 9223371976.855)`, false},
		{"constant_subquery_is_shifted", `count_over_time(vector(1)[5m:1m] @ 9223371976.855)`, true},
		// A subquery without a step uses the 1m default.
		{"default_step_highest_accepted", `count_over_time(m[5m:] @ 9223371976.854)`, true},
		{"default_step_lowest_rejected", `count_over_time(m[5m:] @ 9223371976.855)`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lowerCtx{start: instant.start, end: instantDomainEvalEnd}
			_, _, err := fitDomain(mustParseExperimental(t, tc.query), ctx, 0)
			if tc.accept && err != nil {
				t.Fatalf("fit %q in Prometheus's frame: unexpected error %v", tc.query, err)
			}
			if !tc.accept && !isInstantDomainError(err) {
				t.Fatalf("fit %q in Prometheus's frame: err = %v, want the instant-domain rejection", tc.query, err)
			}
			// A query rejected there is answered in a shifted frame.
			if err := lowerInstantDomainCase(t, tc.query, instant); err != nil {
				t.Fatalf("lower %q: unexpected error %v", tc.query, err)
			}
		})
	}
}

// TestInstantDomain_RangeStepMargin pins that a range query's own step
// widens every window it evaluates, on top of a subquery's step, at the
// int64-nanosecond floor in Prometheus's frame; the lowering answers a
// query rejected there in a shifted frame.
func TestInstantDomain_RangeStepMargin(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		start  string
		accept bool
	}{
		// start - 5m lookback - 1m query step = the floor.
		{"selector_lowest_accepted", `m`, "1677-09-21T00:18:43.145224192Z", true},
		{"selector_highest_rejected", `m`, "1677-09-21T00:18:43.145224191Z", false},
		// An unpinned window is evaluated at every step, so it is not cut
		// back to the floor.
		{"window_highest_rejected", `count_over_time(m[10m])`, "1677-09-21T00:23:43.145224191Z", false},
		// start - 5m range - 5m lookback of the grid's selector - 1m
		// subquery step.
		{"subquery_lowest_accepted", `count_over_time(m[5m:1m])`, "1677-09-21T00:23:43.145224192Z", true},
		{"subquery_highest_rejected", `count_over_time(m[5m:1m])`, "1677-09-21T00:23:43.145224191Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := mustUTC(t, tc.start)
			ctx := lowerCtx{start: start, end: start.Add(time.Hour), step: time.Minute}
			_, _, err := fitDomain(mustParseExperimental(t, tc.query), ctx, 0)
			if tc.accept && err != nil {
				t.Fatalf("fit %q from %s in Prometheus's frame: unexpected error %v", tc.query, tc.start, err)
			}
			if !tc.accept && !isInstantDomainError(err) {
				t.Fatalf("fit %q from %s in Prometheus's frame: err = %v, want the instant-domain rejection", tc.query, tc.start, err)
			}
			// A query rejected there is answered in a shifted frame.
			if _, err := LowerAtRange(context.Background(), mustParseExperimental(t, tc.query), schema.DefaultOTelMetrics(),
				ctx.start, ctx.end, ctx.step); err != nil {
				t.Fatalf("lower %q from %s: unexpected error %v", tc.query, tc.start, err)
			}
		})
	}
}

// TestInstantDomain_RequestInstantsBefore1900 pins that a request instant
// before 1900, which a parsed DateTime64 string cannot name, is lowered.
func TestInstantDomain_RequestInstantsBefore1900(t *testing.T) {
	at := mustUTC(t, "1800-01-01T00:00:00Z")
	for _, q := range []string{`vector(1)`, `rate(m[5m])`, `count_over_time(vector(1)[5m:1m])`, `m @ -5000000000`} {
		if _, err := LowerAtRange(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(), at, at.Add(time.Hour), time.Minute); err != nil {
			t.Fatalf("lower %q at %s: %v", q, at, err)
		}
	}
}

// TestInstantDomain_AnswersEmptyReadsInSQL pins the rewrite of a read that
// finds no sample: one under a wrapped pin, whose storage read and
// evaluation window never meet, or one whose window lies wholly outside
// what storage holds. The expression it makes empty is replaced by
// `vector(0) < -1` and everything around it still lowers to SQL.
func TestInstantDomain_AnswersEmptyReadsInSQL(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{`m @ -62135596800`, `vector(0) < -1`},
		{`count_over_time(m[5m:1m] @ -62135596800)`, `count_over_time(vector(0) < -1[5m:1m] @ -6795364578.871)`},
		{`max_over_time(rate(m[5m])[10m:1m] @ -62135596800)`, `max_over_time(vector(0) < -1[10m:1m] @ -6795364578.871)`},
		{`sum by (job) (rate(m[5m] @ 10000000000))`, `sum by (job) (vector(0) < -1)`},
		{`m @ -62135596800 or vector(7)`, `vector(0) < -1 or vector(7)`},
		{`scalar(m @ -62135596800)`, `scalar(vector(0) < -1)`},
		{`-(m offset -250y)`, `-(vector(0) < -1)`},
		{`quantile_over_time(0.5, m[5m] @ -62135596800)`, `vector(0) < -1`},
		{`topk(scalar(m @ -62135596800), m)`, `topk(scalar(vector(0) < -1), m)`},
		{`info(m @ -62135596800, {job="a"})`, `info(vector(0) < -1, {job="a"})`},
		// A root range vector that reads nothing is the empty range vector.
		{`m[5m] @ -62135596800`, `(vector(0) < -1)[1m:1m]`},
		{`m[5m] @ 10000000000`, `(vector(0) < -1)[1m:1m]`},
		// Mixes in a pinned region: the empty read is replaced, the
		// constant arm stays.
		{`count_over_time((m @ -62135596800 or vector(1))[5m:1m] @ -62135596800)`, `count_over_time((vector(0) < -1 or vector(1))[5m:1m] @ -6795364578.871)`},
		{`count_over_time((m @ -62135596800 * vector(1))[5m:1m] @ -62135596800)`, `count_over_time((vector(0) < -1 * vector(1))[5m:1m] @ -6795364578.871)`},
		{`count_over_time(histogram_quantile(0.9, rate(m[5m]))[5m:1m] @ -62135596800)`, `count_over_time(histogram_quantile(0.9, vector(0) < -1)[5m:1m] @ -6795364578.871)`},
	}
	for _, tc := range cases {
		for _, m := range instantDomainModes {
			t.Run(tc.query+"/"+m.name, func(t *testing.T) {
				expr := mustParseExperimental(t, tc.query)
				before := expr.String()
				ctx := lowerCtx{start: m.start, end: instantDomainEvalEnd, step: m.step}
				got, _, err := fitInstantDomain(expr, ctx)
				if err != nil {
					t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
				}
				if got.String() != tc.want {
					t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got.String(), tc.want)
				}
				if expr.String() != before {
					t.Fatalf("fitInstantDomain modified its input: %q became %q", before, expr.String())
				}
				if err := lowerInstantDomainCase(t, tc.query, m); err != nil {
					t.Fatalf("lower %q: %v", tc.query, err)
				}
			})
		}
	}
}

// TestInstantDomain_AbsentReadsMatchers pins that absent and
// absent_over_time of a selector that reads nothing keep the selector,
// whose matchers label the answer, and record it as reading nothing.
func TestInstantDomain_AbsentReadsMatchers(t *testing.T) {
	for _, q := range []string{`absent(m{job="a"} @ -62135596800)`, `absent_over_time(m{job="a"}[5m] @ 1e17)`, `absent((m @ -62135596800))`} {
		t.Run(q, func(t *testing.T) {
			expr := mustParseExperimental(t, q)
			got, fit, err := fitInstantDomain(expr, lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", q, err)
			}
			if got != expr {
				t.Fatalf("fitInstantDomain(%q) rewrote absent's argument into %q", q, got)
			}
			vs, _, ok := absentSelector(expr.(*parser.Call))
			if !ok || !fit.readsNothing(vs) {
				t.Fatalf("fitInstantDomain(%q): selector not recorded as reading nothing", q)
			}
		})
	}
}

// TestInstantDomain_InfoPinIsRejected pins that an `@` pin on info's
// second argument, which must stay a selector, is still rejected when it
// leaves the domain. The reference rejects every pin there.
func TestInstantDomain_InfoPinIsRejected(t *testing.T) {
	_, _, err := fitAt(t, `info(m, {job="a"} @ -62135596800)`, instantDomainEvalEnd)
	if !isInstantDomainError(err) {
		t.Fatalf("err = %v, want the instant-domain rejection", err)
	}
}

// TestInstantDomain_InDomainQueryIsUntouched pins that a query whose
// instants all lie inside the domain is lowered exactly as written.
func TestInstantDomain_InDomainQueryIsUntouched(t *testing.T) {
	for _, q := range []string{
		`rate(m[5m])`,
		`count_over_time(m[5m:1m] @ 1767225600)`,
		`absent(m @ 1767225600) or info(m, {job="a"})`,
		`-sum by (job) (rate(m[5m]))`,
		`topk(5, m)`,
		`(m)`,
		`m @ -5000000000`,
	} {
		expr := mustParseExperimental(t, q)
		got, fit, err := fitInstantDomain(expr, lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd})
		if err != nil {
			t.Fatalf("fitInstantDomain(%q): %v", q, err)
		}
		if got != expr {
			t.Fatalf("fitInstantDomain(%q) rewrote an in-domain query into %q", q, got)
		}
		if len(fit.shifts) != 0 || len(fit.emptyReads) != 0 {
			t.Fatalf("fitInstantDomain(%q) recorded shifts %v or empty reads %v", q, fit.shifts, fit.emptyReads)
		}
	}
}

// TestInstantDomain_NoContextChecksOnlyAbsolutePins pins the bare [Lower]
// path: with no request instant an unpinned selector renders now64(9), and
// a pin is judged as written, since no evaluator start exists to wrap it.
func TestInstantDomain_NoContextChecksOnlyAbsolutePins(t *testing.T) {
	if _, err := Lower(context.Background(), mustParseExperimental(t, `rate(m[5m] offset 1h)`), schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("Lower(unpinned): %v", err)
	}
	if _, err := Lower(context.Background(), mustParseExperimental(t, `count_over_time(vector(1)[5m:1m] @ -62135596800)`), schema.DefaultOTelMetrics()); err != nil {
		t.Fatalf("Lower(year-one constant): %v", err)
	}
	_, err := Lower(context.Background(), mustParseExperimental(t, `count_over_time(m[1h:1m] @ 9223373836.854)`), schema.DefaultOTelMetrics())
	if !isInstantDomainError(err) {
		t.Fatalf("Lower(read past the ceiling): err = %v, want the instant-domain rejection", err)
	}
}

// TestEvalAnchor_ZeroTimePinIsAnchored pins that "no anchor" is its own
// state, not the zero time.Time: an `@` pin at 0001-01-01T00:00:00Z
// (`@ -62135596800`) is Go's zero time and is still a pin.
func TestEvalAnchor_ZeroTimePinIsAnchored(t *testing.T) {
	vs := mustParseExperimental(t, `m @ -62135596800`).(*parser.VectorSelector)
	a, err := anchorFromSelector(vs, lowerCtx{})
	if err != nil {
		t.Fatalf("anchorFromSelector: %v", err)
	}
	if !a.anchored() || !a.End().IsZero() {
		t.Fatalf("selector pinned at the zero time: anchored=%v End=%v, want anchored at the zero time", a.anchored(), a.End())
	}

	sub := mustParseExperimental(t, `vector(1)[5m:1m] @ -62135596800`).(*parser.SubqueryExpr)
	sa, err := subqueryAnchor(sub, lowerCtx{})
	if err != nil {
		t.Fatalf("subqueryAnchor: %v", err)
	}
	if !sa.anchored() || !sa.End().IsZero() {
		t.Fatalf("subquery pinned at the zero time: anchored=%v End=%v, want anchored at the zero time", sa.anchored(), sa.End())
	}

	unpinned := mustParseExperimental(t, `m`).(*parser.VectorSelector)
	ua, err := anchorFromSelector(unpinned, lowerCtx{})
	if err != nil {
		t.Fatalf("anchorFromSelector(unpinned): %v", err)
	}
	if ua.anchored() {
		t.Fatalf("unpinned selector with no request instant reports an anchor at %v", ua.End())
	}
}

// TestInstantDomain_EmitsNoOutOfDomainInstant pins that the SQL for every
// answered shape carries no literal from a century DateTime64(9) cannot
// hold, and no string literal before 1900, which ClickHouse would clamp.
// nanosInstantPattern matches an instant the SQL names by its Unix
// nanoseconds, capturing them.
var nanosInstantPattern = regexp.MustCompile(`fromUnixTimestamp64Nano\(toInt64\((-?\d+)\)\)`)

func TestInstantDomain_EmitsNoOutOfDomainInstant(t *testing.T) {
	for _, q := range []string{
		`count_over_time(m[5m:1m] @ -62135596800)`,
		`count_over_time(vector(1)[5m:1m] @ -62135596800)`,
		`count_over_time(vector(1)[5m:1m] @ 10000000000)`,
		`max_over_time(timestamp(vector(1))[5m:1m] @ 1e17)`,
		`absent(m{job="a"} @ 1e17)`,
		`m @ -5000000000`,
	} {
		for _, m := range instantDomainModes {
			plan, err := LowerAtRange(context.Background(), mustParseExperimental(t, q),
				schema.DefaultOTelMetrics(), m.start, instantDomainEvalEnd, m.step)
			if err != nil {
				t.Fatalf("%s %q: lower: %v", m.name, q, err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("%s %q: emit: %v", m.name, q, err)
			}
			texts := []string{sql}
			for _, a := range args {
				if s, ok := a.(string); ok {
					texts = append(texts, s)
				}
			}
			for _, text := range texts {
				for _, bad := range []string{"'0001-", "'0000-", "'-", "'18", "'17", "'16", "'2286-", "'2263-"} {
					if strings.Contains(text, bad) {
						t.Fatalf("%s %q: SQL carries an out-of-domain literal %s… in:\n%s", m.name, q, bad, text)
					}
				}
				// An instant before 1900 is written as Unix nanoseconds; one
				// clamped to the int64 bound would name the wrong instant.
				for _, match := range nanosInstantPattern.FindAllStringSubmatch(text, -1) {
					if match[1] == strconv.FormatInt(math.MinInt64, 10) || match[1] == strconv.FormatInt(math.MaxInt64, 10) {
						t.Fatalf("%s %q: SQL carries an instant clamped to the int64 bound %s in:\n%s", m.name, q, match[0], text)
					}
				}
			}
		}
	}
}

// TestInstantDomain_StepInvariantIsUntouched pins the StepInvariantExpr
// arm, a node the engine adds and the parser never produces.
func TestInstantDomain_StepInvariantIsUntouched(t *testing.T) {
	ctx := lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd}
	in := &parser.StepInvariantExpr{Expr: mustParseExperimental(t, `m`)}
	got, _, err := fitInstantDomain(in, ctx)
	if err != nil || got != in {
		t.Fatalf("fitInstantDomain(in-domain step-invariant) = %v, %v; want the input unchanged", got, err)
	}
	out := &parser.StepInvariantExpr{Expr: mustParseExperimental(t, `m @ -62135596800`)}
	got, _, err = fitInstantDomain(out, ctx)
	if err != nil || got.String() != `vector(0) < -1` {
		t.Fatalf("fitInstantDomain(year-one step-invariant) = %v, %v; want vector(0) < -1", got, err)
	}
	if out.Expr.String() != `m @ -62135596800.000` {
		t.Fatalf("fitInstantDomain modified its input into %q", out.Expr)
	}
}

// TestInstantDomain_FirstErrorWins pins that a call with two arguments
// outside the domain reports the first one when fitted in Prometheus's
// frame.
func TestInstantDomain_FirstErrorWins(t *testing.T) {
	q := `predict_linear(m[1h:1m] @ 9223373836.854, scalar(count_over_time(m[1h:1m] @ 9223374836.854)))`
	_, _, err := fitDomain(mustParseExperimental(t, q), lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd}, 0)
	want := instantDomainError(time.UnixMilli(9223373836854)).Error()
	if err == nil || err.Error() != want {
		t.Fatalf("fitInstantDomain(%q): err = %v, want %q", q, err, want)
	}
}

// TestInstantDomain_StartEndPinsWithoutRange pins that `@ start()` and
// `@ end()` with no request range are left to the lowering's own
// "requires query range context" error rather than judged as instants.
func TestInstantDomain_StartEndPinsWithoutRange(t *testing.T) {
	for _, q := range []string{`m @ start()`, `m @ end()`, `count_over_time(vector(1)[5m:1m] @ start())`} {
		expr := mustParseExperimental(t, q)
		if _, _, err := fitInstantDomain(expr, lowerCtx{}); err != nil {
			t.Fatalf("fitInstantDomain(%q) with no request range: %v", q, err)
		}
		_, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics())
		if err == nil || !strings.Contains(err.Error(), "requires query range context") {
			t.Fatalf("Lower(%q): err = %v, want the query-range-context error", q, err)
		}
	}
}

// TestShiftIntoDomain pins the shift that moves a region into the domain:
// a whole number of the region's step, and none when the region is wider
// than the domain.
func TestShiftIntoDomain(t *testing.T) {
	const minute = int64(60_000)
	over := func(lo, hi time.Time) fitResult {
		return fitResult{lo: lo, hi: hi, spanned: true, stepMillis: minute}
	}
	for _, r := range []fitResult{
		over(time.UnixMilli(10_000_000_000_000), time.UnixMilli(10_000_000_600_000)),
		over(instantDomainMin.Add(-time.Hour), instantDomainMin.Add(time.Hour)),
		over(instantDomainMax.Add(-time.Hour), instantDomainMax.Add(time.Hour)),
	} {
		shift, ok := shiftIntoDomain(r, anyShift)
		if !ok || shift%minute != 0 {
			t.Fatalf("shiftIntoDomain(%s..%s) = %d, %v; want a multiple of the step", r.lo, r.hi, shift, ok)
		}
		d := time.Duration(shift) * time.Millisecond
		if r.lo.Add(d).Before(instantDomainMin) || r.hi.Add(d).After(instantDomainMax) {
			t.Fatalf("shiftIntoDomain(%s..%s) = %d leaves the domain", r.lo, r.hi, shift)
		}
	}
	if _, ok := shiftIntoDomain(over(time.UnixMilli(-10_000_000_000_000), time.UnixMilli(10_000_000_000_000)), anyShift); ok {
		t.Fatal("shiftIntoDomain shifted a region wider than the domain")
	}
}
