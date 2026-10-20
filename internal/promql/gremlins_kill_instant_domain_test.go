package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The domain bounds in whole milliseconds, rounded inward, as
// [shiftIntoDomain] computes them: the floor 1677-09-21T00:12:43.145224192Z
// rounds up to its next millisecond, the ceiling down to its own.
const (
	domainFloorMillis   = int64(-9223372036854)
	domainCeilingMillis = int64(9223372036854)
)

// TestShiftIntoDomain_ExactShifts pins the shift [shiftIntoDomain] picks
// for a span reaching past either bound: half the way to the centre when
// that fits, else the fewest units that bring the span inside, and none
// for a span whose width leaves no unit of slack in the domain.
func TestShiftIntoDomain_ExactShifts(t *testing.T) {
	width := domainCeilingMillis - domainFloorMillis
	span := func(lo, hi, unit int64) fitResult {
		return fitResult{lo: time.UnixMilli(lo), hi: time.UnixMilli(hi), spanned: true, stepMillis: unit}
	}
	cases := []struct {
		name  string
		r     fitResult
		shift int64
		ok    bool
	}{
		// Half the way to the centre leaves the span 50ms below the floor,
		// so the span moves the 100ms that bring it onto the floor.
		{"floor, least shift", span(domainFloorMillis-100, domainFloorMillis-100+width-1, 1), 100, true},
		{"ceiling, least shift", span(domainCeilingMillis+100-(width-1), domainCeilingMillis+100, 1), -100, true},
		// The least shift rounds up to whole units of 7ms.
		{"floor, least shift in units", span(domainFloorMillis-100, domainFloorMillis-100+width-14, 7), 105, true},
		// A short span 10 minutes below the floor moves halfway to the
		// centre, a whole number of minutes.
		{"floor, halfway", span(domainFloorMillis-600_000, domainFloorMillis, 60_000), 4611686160000, true},
		{"no slack", span(domainFloorMillis-100, domainFloorMillis-100+width, 1), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shift, ok := shiftIntoDomain(tc.r, anyShift)
			if shift != tc.shift || ok != tc.ok {
				t.Fatalf("shiftIntoDomain = %d, %v; want %d, %v", shift, ok, tc.shift, tc.ok)
			}
		})
	}
}

// TestCeilUnitsAndLcmMillis pins the unit arithmetic of the shifts.
func TestCeilUnitsAndLcmMillis(t *testing.T) {
	for _, tc := range []struct{ d, unit, want int64 }{{1, 7, 7}, {7, 7, 7}, {8, 7, 14}, {100, 1, 100}} {
		if got := ceilUnits(tc.d, tc.unit); got != tc.want {
			t.Fatalf("ceilUnits(%d, %d) = %d, want %d", tc.d, tc.unit, got, tc.want)
		}
	}
	for _, tc := range []struct{ a, b, want int64 }{{0, 5, 5}, {5, 0, 5}, {4, 6, 12}, {60_000, 60_000, 60_000}} {
		if got := lcmMillis(tc.a, tc.b); got != tc.want {
			t.Fatalf("lcmMillis(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestOffsetFits pins the bounds of a shift a time.Duration offset holds.
func TestOffsetFits(t *testing.T) {
	for _, tc := range []struct {
		shift int64
		want  bool
	}{{maxDurationMillis, true}, {maxDurationMillis + 1, false}, {-maxDurationMillis, true}, {-maxDurationMillis - 1, false}, {0, true}} {
		if got := offsetFits(tc.shift); got != tc.want {
			t.Fatalf("offsetFits(%d) = %v, want %v", tc.shift, got, tc.want)
		}
	}
}

// TestFramePinsFit pins the bound of the pins a shifted frame admits.
func TestFramePinsFit(t *testing.T) {
	at := func(ts int64) parser.Expr {
		return &parser.VectorSelector{Name: "m", Timestamp: &ts}
	}
	sub := func(ts int64) parser.Expr {
		return &parser.SubqueryExpr{Expr: &parser.VectorSelector{Name: "m"}, Range: time.Minute, Timestamp: &ts}
	}
	for _, tc := range []struct {
		e    parser.Expr
		want bool
	}{
		{at(framePinBound), true},
		{at(-framePinBound), true},
		{at(framePinBound + 1), false},
		{at(-framePinBound - 1), false},
		{sub(framePinBound + 1), false},
		{sub(-framePinBound), true},
	} {
		if got := framePinsFit(tc.e); got != tc.want {
			t.Fatalf("framePinsFit(%s) = %v, want %v", tc.e, got, tc.want)
		}
	}
}

// TestFrameMargin pins the widening a fit gives a query's instants: the
// request step, and twice each subquery's step.
func TestFrameMargin(t *testing.T) {
	e := mustParseExperimental(t, `max_over_time(max_over_time(m[1m:10s])[5m:30s])`)
	if got, want := frameMargin(e, 2*time.Minute), int64(120_000+2*10_000+2*30_000); got != want {
		t.Fatalf("frameMargin = %d, want %d", got, want)
	}
}

// TestFitInFrame_ExactShifts pins the frame [fitInFrame] picks: half the
// way to the domain's centre for a short query below the floor, and for
// a query whose two halves reach past both bounds, the least shift that
// brings the rejected instant inside.
func TestFitInFrame_ExactShifts(t *testing.T) {
	const years290 = 105850 * 24 * time.Hour
	cases := []struct {
		name  string
		q     string
		at    time.Time
		shift int64
	}{
		{"halfway", `max_over_time(m[5m:1m])`, instantDomainMin.Add(2 * time.Minute), 4611686100000},
		{"least, below the ceiling", `max_over_time(m[10m:1m] offset 290y) + max_over_time(m[10m:1m] offset -290y)`, instantDomainMax.Add(-years290 + 5*time.Minute), -600000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, f, err := fitInFrame(mustParseExperimental(t, tc.q), lowerCtx{start: tc.at, end: tc.at}, false)
			if err != nil {
				t.Fatalf("fit: %v", err)
			}
			if f.frameShift != tc.shift {
				t.Fatalf("frame shift = %d, want %d", f.frameShift, tc.shift)
			}
		})
	}
}

// TestMapPins pins that mapPins moves the pins under every node kind and
// returns a node whose subtree keeps its pins as it is.
func TestMapPins(t *testing.T) {
	later := func(p pin) pin {
		if p.ts == nil {
			return p
		}
		ts := *p.ts + 1000
		return pin{ts: &ts, startOrEnd: p.startOrEnd}
	}
	keep := func(p pin) pin { return p }
	for _, tc := range []struct{ q, want string }{
		{`max_over_time((m @ 100)[5m:1m])`, `max_over_time((m @ 101.000)[5m:1m])`},
		{`max_over_time(m[5m:1m] @ 100)`, `max_over_time(m[5m:1m] @ 101.000)`},
		{`topk(scalar(m @ 100), n)`, `topk(scalar(m @ 101.000), n)`},
		{`n + m @ 100`, `n + m @ 101.000`},
		{`clamp_min(m, scalar(n @ 100))`, `clamp_min(m, scalar(n @ 101.000))`},
	} {
		e := mustParseExperimental(t, tc.q)
		if got := mapPins(e, later).String(); got != tc.want {
			t.Fatalf("mapPins(%s) = %s, want %s", tc.q, got, tc.want)
		}
	}
	for _, q := range []string{
		`max_over_time(m[5m:1m] @ 100)`, `max_over_time(m[5m:1m] @ end())`, `topk(2, m @ 100)`,
		`m @ 100 + n`, `rate(m[5m] @ 100)`, `-(m @ 100)`,
	} {
		e := mustParseExperimental(t, q)
		if got := mapPins(e, keep); got != e {
			t.Fatalf("mapPins(%s) with every pin kept returned a copy", q)
		}
	}
}

// TestUnframeInstant pins that an instant moves back by the shift in
// nanoseconds.
func TestUnframeInstant(t *testing.T) {
	call, ok := unframeInstant(&chplan.ColumnRef{Name: "TimeUnix"}, 1500).(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnFromUnixNanos {
		t.Fatalf("unframeInstant = %#v, want fromUnixTimestamp64Nano", call)
	}
	sub, ok := call.Args[0].(*chplan.Binary)
	if !ok || sub.Op != chplan.OpSub {
		t.Fatalf("unframeInstant argument = %#v, want a subtraction", call.Args[0])
	}
	if lit, ok := sub.Right.(*chplan.LitInt); !ok || lit.V != 1500*int64(time.Millisecond) {
		t.Fatalf("unframeInstant subtracts %#v, want 1500ms in nanoseconds", sub.Right)
	}
}

// TestBucketRowsAnswer pins that bucket rows answer only a window cut at
// neither edge and lowered in Prometheus's own frame.
func TestBucketRowsAnswer(t *testing.T) {
	plain, framed := lowerCtx{}, lowerCtx{domain: &domainFit{frameShift: 60_000}}
	for _, tc := range []struct {
		name string
		ctx  lowerCtx
		rw   chplan.RangeWindow
		want bool
	}{
		{"uncut", plain, chplan.RangeWindow{}, true},
		{"start cut", plain, chplan.RangeWindow{StartBeyond: time.Minute}, false},
		{"end cut", plain, chplan.RangeWindow{EndBeyond: time.Minute}, false},
		{"framed", framed, chplan.RangeWindow{}, false},
	} {
		if got := tc.ctx.bucketRowsAnswer(&tc.rw); got != tc.want {
			t.Fatalf("%s: bucketRowsAnswer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRootGridInDomain pins that a root subquery's grid is judged in
// Prometheus's frame, and that a root with no recorded grid is in the
// domain.
func TestRootGridInDomain(t *testing.T) {
	root := mustParseExperimental(t, `m[5m:1m]`)
	sub := root.(*parser.SubqueryExpr)
	if !(&domainFit{frameShift: 60_000}).rootGridInDomain(root) {
		t.Fatal("a root subquery with no recorded grid is judged outside the domain")
	}
	century := (100 * 365 * 24 * time.Hour).Milliseconds()
	at := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	// Fitted a century ahead, the grid in 2200 lies in 2100 in
	// Prometheus's frame; moved the other way it would lie past 2262.
	f := &domainFit{frameShift: century, grids: map[*parser.SubqueryExpr][2]time.Time{sub: {at, at}}}
	if !f.rootGridInDomain(root) {
		t.Fatal("a grid in 2100 in Prometheus's frame is judged outside the domain")
	}
}

// TestFitResultMerge_ReadsAccumulate pins that a region reads storage
// when either part of it does.
func TestFitResultMerge_ReadsAccumulate(t *testing.T) {
	for _, tc := range []struct{ r, o, want bool }{{true, false, true}, {false, true, true}, {false, false, false}} {
		r := fitResult{reads: tc.r}
		r.merge(fitResult{reads: tc.o})
		if r.reads != tc.want {
			t.Fatalf("merge reads %v into %v = %v, want %v", tc.o, tc.r, r.reads, tc.want)
		}
	}
}

// TestMarkShift_MatrixSelector pins that an unpinned range selector keeps
// the region's shift and a pinned one names instants of its own.
func TestMarkShift_MatrixSelector(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want int64
	}{{`m[5m]`, 60_000}, {`m[5m] @ 100`, 0}} {
		ms := mustParseExperimental(t, tc.q).(*parser.MatrixSelector)
		f := &domainFit{shifts: map[parser.Node]int64{}, gridShifts: map[parser.Expr]int64{}}
		f.markShift(ms, 60_000)
		if got := f.shifts[ms]; got != tc.want {
			t.Fatalf("markShift(%s) recorded %d, want %d", tc.q, got, tc.want)
		}
	}
}

// TestIsolate_KeepsStateInPrometheusFrame pins that an isolated subquery's
// evaluator state is recorded moved back out of the fit's frame.
func TestIsolate_KeepsStateInPrometheusFrame(t *testing.T) {
	n := mustParseExperimental(t, `m[5m:1m]`).(*parser.SubqueryExpr)
	f := &domainFit{frameShift: 60_000}
	f.isolate(n, n.Expr, domainEnv{ev: promEval{start: 600_000, offEval: 600_000}})
	if got := f.isolated[n.Expr]; got.start != 540_000 || got.offEval != 540_000 {
		t.Fatalf("isolated state starts at %d, %d; want 540000", got.start, got.offEval)
	}
}

// fitEnvAt is the environment [fitDomain] fits an instant query at end in.
func fitEnvAt(end time.Time) domainEnv {
	at := promTimeMilliseconds(end)
	return domainEnv{
		known: true, lo: end, hi: end, start: end, end: end, hasEv: true,
		ev: promEval{start: at, offEval: at, req: &[2]int64{at, at}},
	}
}

// TestFitBinary_EmptyOperands pins which binary expressions answer no
// sample: a set union when both operands answer none, `unless` when its
// left operand does, and any other operator when either does.
func TestFitBinary_EmptyOperands(t *testing.T) {
	const beyond = `(m @ 9300000000)`
	env := fitEnvAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		q     string
		empty bool
	}{
		{beyond + ` or n`, false},
		{beyond + ` or ` + beyond, true},
		{beyond + ` + n`, true},
		{`n + ` + beyond, true},
		{`n + n`, false},
		{beyond + ` unless n`, true},
		{`n unless ` + beyond, false},
	} {
		f := &domainFit{}
		_, res := f.fitBinary(mustParseExperimental(t, tc.q).(*parser.BinaryExpr), env)
		if res.empty != tc.empty {
			t.Fatalf("fitBinary(%s) empty = %v, want %v", tc.q, res.empty, tc.empty)
		}
	}
}

// TestPromSelectorHint_SubqueryOffset pins that the subquery offsets move
// both ends of the read.
func TestPromSelectorHint_SubqueryOffset(t *testing.T) {
	ev := promEval{req: &[2]int64{10_000_000, 20_000_000}, sums: promReadSums{off: time.Hour, rng: time.Minute}}
	want := [2]int64{10_000_000 - 3_600_000 - 60_000 - (300_000 - 1), 20_000_000 - 3_600_000}
	if got := promSelectorHint(ev, &parser.VectorSelector{}, 0); got != want {
		t.Fatalf("promSelectorHint = %v, want %v", got, want)
	}
}

// TestPromPinMillis pins the pin `@ start()` and `@ end()` name, which
// exists only with a request.
func TestPromPinMillis(t *testing.T) {
	req := &[2]int64{1000, 2000}
	if _, ok := promPinMillis(nil, parser.END, nil); ok {
		t.Fatal("`@ end()` has a pin without a request")
	}
	if got, ok := promPinMillis(nil, parser.END, req); !ok || got != 2000 {
		t.Fatalf("`@ end()` = %d, %v; want 2000", got, ok)
	}
	if got, ok := promPinMillis(nil, parser.START, req); !ok || got != 1000 {
		t.Fatalf("`@ start()` = %d, %v; want 1000", got, ok)
	}
}

// TestPromAtOffset_SubqueryOffset pins that the offsets of the subqueries
// on the path come off a pinned node's offset.
func TestPromAtOffset_SubqueryOffset(t *testing.T) {
	ev := promEval{offEval: 100_000, offPath: []*parser.SubqueryExpr{{Range: time.Minute, OriginalOffset: time.Minute}}}
	if got := promAtOffset(ev, 40_000, 0); got != 0 {
		t.Fatalf("promAtOffset = %s, want 0", got)
	}
}

// TestPromSubqueryEval pins both evaluator states of a subquery's inner
// expression: a fresh one when its evaluator starts elsewhere, and the
// enclosing one with the subquery on its offset path when it starts at
// the same instant.
func TestPromSubqueryEval(t *testing.T) {
	n := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute}
	ev := promEval{start: 600_000, offEval: 777}
	moved := promSubqueryEval(ev, n, 0, time.Minute)
	if moved.start != 360_000 || moved.offEval != 360_000 || moved.offPath != nil {
		t.Fatalf("inner evaluator = %+v, want a fresh one at 360000", moved)
	}
	same := promSubqueryEval(ev, n, -5*time.Minute+time.Millisecond, time.Minute)
	if same.start != 600_000 || same.offEval != 777 || len(same.offPath) != 1 || same.offPath[0] != n {
		t.Fatalf("inner evaluator = %+v, want the enclosing one with n on its path", same)
	}
}

// TestPromEvalMoved pins that moving an evaluator state moves every
// instant it holds, and leaves an empty offset path empty.
func TestPromEvalMoved(t *testing.T) {
	pinAt := int64(100)
	ev := promEval{
		start: 1, offEval: 2, req: &[2]int64{3, 4},
		sums:    promReadSums{pinned: true, pinMillis: 5},
		offPath: []*parser.SubqueryExpr{{Expr: &parser.VectorSelector{Name: "m"}, Range: time.Minute, Timestamp: &pinAt}},
	}
	got := ev.moved(10)
	if got.start != 11 || got.offEval != 12 || *got.req != [2]int64{13, 14} || got.sums.pinMillis != 15 || *got.offPath[0].Timestamp != 110 {
		t.Fatalf("moved = %+v", got)
	}
	if pinAt != 100 || *ev.req != [2]int64{3, 4} {
		t.Fatal("moved modified the state it moved")
	}
	if bare := (promEval{start: 1}).moved(10); bare.offPath != nil {
		t.Fatal("moved gave an empty offset path a slice")
	}
}

// TestFitSubquery_PinnedEmptyReadIsReplaced pins that a pinned subquery
// whose every read lies before storage, at instants the domain does not
// hold, answers no sample and is replaced by the empty vector rather than
// moved into the domain.
func TestFitSubquery_PinnedEmptyReadIsReplaced(t *testing.T) {
	// The pin lies a second above the floor; the grid and its lookbacks
	// reach below it.
	e := mustParseExperimental(t, `max_over_time(m[5m:1m] @ -9223372036)`)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out, _, err := fitDomain(e, lowerCtx{start: at, end: at}, 0, false)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if got, want := out.String(), emptyInstantVector().String(); got != want {
		t.Fatalf("fit = %s, want %s", got, want)
	}
}

// TestUnshiftRoot_RowTypes pins how a framed root's timestamp is found:
// by its role, else by the schema's timestamp name, which an open row
// type is taken to hold; a closed root with neither reports no timestamp
// and is returned as it is.
func TestUnshiftRoot_RowTypes(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{domain: &domainFit{frameShift: 60_000}}
	root := mustParseExperimental(t, `m`)
	closed := &chplan.Scan{Table: "t", Columns: []string{"Attributes", "Value"}}
	if got := unshiftRoot(closed, root, s, ctx); got != chplan.Node(closed) {
		t.Fatalf("a root with no timestamp became %#v", got)
	}
	open := &chplan.Scan{Table: "t"}
	p, ok := unshiftRoot(open, root, s, ctx).(*chplan.Project)
	if !ok || len(p.Replacements) != 1 || p.Replacements[0].Alias != s.TimestampColumn {
		t.Fatalf("an open root's timestamp is not moved: %#v", p)
	}
	named := &chplan.Scan{Table: "t", Columns: []string{"Attributes", s.TimestampColumn, "Value"}}
	p, ok = unshiftRoot(named, root, s, ctx).(*chplan.Project)
	if !ok || len(p.Projections) != 3 {
		t.Fatalf("a root naming its timestamp without its role is not moved: %#v", p)
	}
	if v, ok := unframedNanos(p.Projections[1]); !ok || v != 60_000*int64(time.Millisecond) {
		t.Fatalf("the named timestamp is %#v, want it moved back a minute", p.Projections[1].Expr)
	}
}
