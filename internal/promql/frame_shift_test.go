package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestFrameShift_FitsInShiftedFrame pins the frame a query rejected in
// Prometheus's own frame is fitted in: a nonzero multiple of every
// subquery step, which moves every instant the query needs into the
// domain.
func TestFrameShift_FitsInShiftedFrame(t *testing.T) {
	frameShiftFarBelowFloor := time.Date(1100, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		query string
		start time.Time
		end   time.Time
		step  time.Duration
		unit  time.Duration
	}{
		{"selector_near_floor", `m`, instantDomainNearFloor.Add(-6 * time.Minute), instantDomainNearFloor.Add(-6 * time.Minute), 0, time.Millisecond},
		{"window_near_floor_range", `count_over_time(m[10m])`, instantDomainNearFloor, instantDomainNearFloor.Add(time.Hour), time.Minute, time.Millisecond},
		{"subquery_past_ceiling", `count_over_time(m[1h:1m] @ 9223373836.854)`, instantDomainEvalEnd, instantDomainEvalEnd, 0, time.Minute},
		{"steps_past_ceiling", `count_over_time(m[1h:7m] @ 9223373836.854) + count_over_time(m[1h:11s] @ 9223373836.854)`, instantDomainEvalEnd, instantDomainEvalEnd, 0, 77 * time.Minute},
		{"nested_subquery_near_floor", `max_over_time(max_over_time(m[2m:1m])[5m:1m])`, instantDomainNearFloor.Add(-6 * time.Minute), instantDomainNearFloor.Add(-6 * time.Minute), 0, time.Minute},
		{"nested_steps_past_ceiling", `max_over_time(max_over_time(m[5m:7s])[1h:1m] @ 9223373836.854)`, instantDomainEvalEnd, instantDomainEvalEnd, 0, 7 * time.Minute},
		{"request_pin_in_subquery_past_ceiling", `count_over_time((m and on() (n @ end()))[1h:1m] @ 9223373836.854)`, instantDomainEvalEnd, instantDomainEvalEnd, 0, time.Minute},
		// The instants span nearly the whole domain: only the least shift
		// that brings the earliest of them inside fits.
		{"window_spanning_nearly_the_domain", `count_over_time(m[290y] offset 290y)`, instantDomainEvalEnd.Add(-2 * time.Minute), instantDomainEvalEnd, time.Minute, time.Millisecond},
		// A request in 1100: the shift that brings it inside moves it by
		// more than a time.Duration holds.
		{"request_beyond_a_duration_below_floor", `count_over_time(vector(1)[300d:100d])`, frameShiftFarBelowFloor, frameShiftFarBelowFloor, 0, 100 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lowerCtx{start: tc.start, end: tc.end, step: tc.step}
			expr := mustParseExperimental(t, tc.query)
			before := expr.String()
			if _, _, err := fitDomain(expr, ctx, 0, false); !isInstantDomainError(err) {
				t.Fatalf("fit %q in Prometheus's frame: err = %v, want the instant-domain rejection", tc.query, err)
			}
			_, f, err := fitInstantDomain(expr, ctx)
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			if expr.String() != before {
				t.Fatalf("fitInstantDomain modified its input: %q became %q", before, expr.String())
			}
			if f.frameShift == 0 || f.frameShift%tc.unit.Milliseconds() != 0 {
				t.Fatalf("frame shift %d ms is not a nonzero multiple of %s", f.frameShift, tc.unit)
			}
			lctx := ctx.withDomain(f)
			if !lctx.start.Equal(moveMillis(tc.start, f.frameShift)) || !lctx.end.Equal(moveMillis(tc.end, f.frameShift)) || lctx.shiftMillis != f.frameShift {
				t.Fatalf("lowering context %s..%s shift %d, want the request moved by %d ms", lctx.start, lctx.end, lctx.shiftMillis, f.frameShift)
			}
		})
	}
}

// TestFrameShift_SpanningTheDomain pins the queries no one shifted frame
// answers, their instants with the steps the lowering reaches past a grid
// spanning more than the domain. Prometheus's own frame rejects each; the
// lowering answers each by evaluating every outermost subquery over its
// grid on its own. TestInstantDomainParity_IsolatedSubqueries_ChDB pins
// the answers.
func TestFrameShift_SpanningTheDomain(t *testing.T) {
	for _, query := range []string{
		`count_over_time(count_over_time(vector(1)[292y:100d])[292y:100d])`,
		`max_over_time(max_over_time(m[292y:100d])[292y:100d])`,
		`max_over_time(m[292y:100d] offset 292y)`,
	} {
		t.Run(query, func(t *testing.T) {
			ctx := lowerCtx{start: instantDomainEvalEnd, end: instantDomainEvalEnd}
			if _, _, err := fitDomain(mustParseExperimental(t, query), ctx, 0, false); !isInstantDomainError(err) {
				t.Fatalf("fit %q in Prometheus's frame: err = %v, want the instant-domain rejection", query, err)
			}
			if _, err := LowerAt(context.Background(), mustParseExperimental(t, query), schema.DefaultOTelMetrics(), instantDomainEvalEnd, instantDomainEvalEnd); err != nil {
				t.Fatalf("lower %q: %v", query, err)
			}
		})
	}
}

// TestFrameShift_ShiftPins pins that every `@` pin moves by the shift,
// that a node without one is shared, and that the input is unchanged.
func TestFrameShift_ShiftPins(t *testing.T) {
	expr := mustParseExperimental(t, `sum(rate(m[5m] @ 100)) + count_over_time((n @ 200)[5m:1m] @ 300) - o offset 1m`)
	got := shiftPins(expr, 1000)
	const want = `sum(rate(m[5m] @ 101.000)) + count_over_time((n @ 201.000)[5m:1m] @ 301.000) - o offset 1m`
	if got.String() != want {
		t.Fatalf("shiftPins = %q, want %q", got, want)
	}
	if expr.String() != `sum(rate(m[5m] @ 100.000)) + count_over_time((n @ 200.000)[5m:1m] @ 300.000) - o offset 1m` {
		t.Fatalf("shiftPins modified its input into %q", expr)
	}
	if got.(*parser.BinaryExpr).RHS != expr.(*parser.BinaryExpr).RHS {
		t.Fatal("shiftPins copied a node that holds no pin")
	}
}

// TestFrameShift_FrameStorage pins that every scan of a table with a
// sample timestamp reads it moved by the frame's shift, that every range
// window, shared or not, takes the shift once, and that a scan of a table
// without a sample timestamp is refused.
func TestFrameShift_FrameStorage(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	scan := &chplan.Scan{Table: s.GaugeTable, Roles: metricScanRoles(s, s.GaugeTable)}
	rw := &chplan.RangeWindow{Input: scan, TimeShiftMillis: 5}
	root := &chplan.CrossJoin{Left: rw, Right: rw}
	guard := &chplan.Filter{Input: rw, Predicate: &chplan.LitBool{V: true}}
	f := &domainFit{frameShift: 60000}
	if err := frameStorage(f, root, guard); err != nil {
		t.Fatal(err)
	}
	if scan.TimeShiftMillis != 60000 || rw.TimeShiftMillis != 60005 {
		t.Fatalf("scan shift %d, window shift %d; want 60000 and 60005", scan.TimeShiftMillis, rw.TimeShiftMillis)
	}
	bucket := &chplan.Scan{Table: schema.DownsampleTierTable, Roles: metricScanRoles(s, schema.DownsampleTierTable)}
	if err := frameStorage(f, bucket); err != errFramedStorage {
		t.Fatalf("frameStorage(bucket table) = %v, want %v", err, errFramedStorage)
	}
	if err := frameStorage(&domainFit{}, bucket); err != nil || bucket.TimeShiftMillis != 0 {
		t.Fatalf("frameStorage outside a frame = %v, shift %d; want no change", err, bucket.TimeShiftMillis)
	}
}

// TestFrameShift_LowersStorageInFrame pins the lowering of a framed query:
// every scan reads in the frame and the root reports its timestamps moved
// back.
func TestFrameShift_LowersStorageInFrame(t *testing.T) {
	end := instantDomainNearFloor.Add(-6 * time.Minute)
	plan, err := LowerAtRange(context.Background(), mustParseExperimental(t, `rate(m[10m])`), schema.DefaultOTelMetrics(), end.Add(-time.Hour), end, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	scans, shift := 0, int64(0)
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if sc, ok := n.(*chplan.Scan); ok && (sc.Table != "" || len(sc.UnionTables) > 0) {
			scans++
			if shift == 0 {
				shift = sc.TimeShiftMillis
			}
			if sc.TimeShiftMillis == 0 || sc.TimeShiftMillis != shift {
				t.Errorf("scan of %s%v reads with shift %d, want the frame's %d", sc.Table, sc.UnionTables, sc.TimeShiftMillis, shift)
			}
		}
		return true
	})
	if scans == 0 {
		t.Fatal("no scan in the lowered plan")
	}
	root, ok := plan.(*chplan.Project)
	if !ok || len(root.Projections) == 0 {
		t.Fatalf("root %T is not the projection moving timestamps back", plan)
	}
}

// TestFrameShift_SplitsRangeSpanningTheDomain pins a range query whose
// steps span more than one lowering covers: it is lowered as the union of
// runs of its steps, each spanning at most maxRangeSpan, consecutive and
// covering every step, with a pin naming the request range resolved to
// the instant it names. A range within maxRangeSpan is not split.
func TestFrameShift_SplitsRangeSpanningTheDomain(t *testing.T) {
	start, end, step := instantDomainNearFloor.Add(-6*time.Minute), instantDomainNearCeiling, 24*time.Hour
	for _, q := range []string{`m`, `m @ start() + m`, `max_over_time(max_over_time(m[2m:1m])[5m:1m])`} {
		plan, err := LowerAtRange(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(), start, end, step)
		if err != nil {
			t.Fatalf("lower %q: %v", q, err)
		}
		union, ok := plan.(*chplan.UnionAll)
		if !ok || len(union.Inputs) < 2 {
			t.Fatalf("lower %q: root %T, want a union of runs", q, plan)
		}
	}
	var runs [][2]time.Time
	chplan.WalkDeep(mustLowerRange(t, `m`, start, end, step), func(n chplan.Node) bool {
		if lwr, ok := n.(*chplan.RangeLWR); ok {
			runs = append(runs, [2]time.Time{lwr.Start, lwr.End})
		}
		return true
	})
	if len(runs) < 2 {
		t.Fatalf("got %d runs, want several", len(runs))
	}
	got := resolveRequestPins(mustParseExperimental(t, `m @ start() + n @ end() + o @ 5`), 1000, 2000)
	if want := `m @ 1.000 + n @ 2.000 + o @ 5.000`; got.String() != want {
		t.Fatalf("resolveRequestPins = %q, want %q", got, want)
	}
	short := mustLowerRange(t, `m`, instantDomainEvalEnd.Add(-maxRangeSpan.Truncate(time.Millisecond)), instantDomainEvalEnd, 24*time.Hour)
	if _, ok := short.(*chplan.UnionAll); ok {
		t.Fatal("a range within maxRangeSpan was split")
	}
}

// TestFrameShift_RunsCoverEveryStep pins that the runs a split range is
// lowered as are consecutive steps of the request, each spanning at most
// maxRangeSpan, from the request's first step to its last.
func TestFrameShift_RunsCoverEveryStep(t *testing.T) {
	start, end, step := instantDomainNearFloor, instantDomainNearCeiling, 7*time.Hour
	plan := mustLowerRange(t, `m`, start, end, step)
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("root %T, want a union of runs", plan)
	}
	want := start
	for i, arm := range union.Inputs {
		var lwr *chplan.RangeLWR
		chplan.WalkDeep(arm, func(n chplan.Node) bool {
			if r, ok := n.(*chplan.RangeLWR); ok && lwr == nil {
				lwr = r
			}
			return true
		})
		if lwr == nil {
			t.Fatalf("run %d holds no RangeLWR", i)
		}
		scan := armShift(arm)
		first := lwr.Start.Add(-time.Duration(scan) * time.Millisecond)
		last := lwr.End.Add(-time.Duration(scan) * time.Millisecond)
		if !first.Equal(want) {
			t.Fatalf("run %d starts at %s, want %s", i, first, want)
		}
		if last.Sub(first) > maxRangeSpan {
			t.Fatalf("run %d spans %s, more than %s", i, last.Sub(first), maxRangeSpan)
		}
		want = last.Add(step)
	}
	if last := want.Add(-step); last.After(end) || end.Sub(last) >= step {
		t.Fatalf("runs end at %s, want the request's last step before %s", last, end)
	}
}

// TestFrameShift_RunsOfOneInstant pins a split range whose runs hold one
// instant: a step longer than a run makes every instant a run, and a run
// of one instant reports its instant as every other run does.
func TestFrameShift_RunsOfOneInstant(t *testing.T) {
	const year = 365 * 24 * time.Hour
	start, end := instantDomainNearFloor, instantDomainNearCeiling
	for _, step := range []time.Duration{200 * year, 100 * year} {
		plan := mustLowerRange(t, `count_over_time(m[5m])`, start, end, step)
		union, ok := plan.(*chplan.UnionAll)
		if !ok {
			t.Fatalf("step %s: root %T, want a union of runs", step, plan)
		}
		if want := int((end.UnixMilli()-start.UnixMilli())/step.Milliseconds()) + 1; len(union.Inputs) != want {
			t.Fatalf("step %s: %d runs, want one per instant, %d", step, len(union.Inputs), want)
		}
		for i, arm := range union.Inputs {
			if _, ok := arm.RowType().ByName(schema.DefaultOTelMetrics().TimestampColumn); !ok {
				t.Fatalf("step %s: run %d reports no timestamp", step, i)
			}
		}
	}
}

// TestFrameShift_RunsOfOneLongStep pins a split range whose step is too
// long for any frame to hold its margins: two steps of about 292 years
// span the stored range from a first step whose window and step reach
// below the floor, and each run of one instant is lowered at
// [oneInstantStep].
func TestFrameShift_RunsOfOneLongStep(t *testing.T) {
	start, end := time.Date(1677, 9, 21, 0, 15, 0, 0, time.UTC), instantDomainNearCeiling
	step := time.Duration((end.UnixMilli()-start.UnixMilli())/2) * time.Millisecond
	ctx := lowerCtx{start: start.Add(step), end: start.Add(step), step: step}
	if _, _, err := fitInstantDomain(mustParseExperimental(t, `count_over_time(m[5m])`), ctx); !isInstantDomainError(err) {
		t.Fatalf("fit at the middle instant with its own step: err = %v, want the instant-domain rejection", err)
	}
	plan := mustLowerRange(t, `count_over_time(m[5m])`, start, end, step)
	union, ok := plan.(*chplan.UnionAll)
	if !ok || len(union.Inputs) != 3 {
		t.Fatalf("root %#v, want a union of three runs", plan)
	}
	var windows []*chplan.RangeWindow
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok {
			windows = append(windows, rw)
		}
		return true
	})
	if len(windows) == 0 {
		t.Fatal("no range window")
	}
	for _, rw := range windows {
		if rw.Step > oneInstantStep {
			t.Fatalf("a run's window steps by %s, want at most %s", rw.Step, oneInstantStep)
		}
	}
}

// TestLowerCtx_OneInstant pins which evaluation [lowerCtx.oneInstant]
// lowers at [oneInstantStep]: a range of one instant whose step is longer.
func TestLowerCtx_OneInstant(t *testing.T) {
	at := instantDomainEvalEnd
	const century = 100 * 365 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		ctx  lowerCtx
		want bool
	}{
		{"one instant, long step", lowerCtx{start: at, end: at, step: century}, true},
		{"one instant, step just longer", lowerCtx{start: at, end: at, step: oneInstantStep + time.Millisecond}, true},
		{"one instant, step of one second", lowerCtx{start: at, end: at, step: oneInstantStep}, false},
		{"two instants", lowerCtx{start: at.Add(-century), end: at, step: century}, false},
		{"instant query", lowerCtx{end: at}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.ctx.oneInstant()
			if ok != tc.want {
				t.Fatalf("oneInstant reports %v, want %v", ok, tc.want)
			}
			want := tc.ctx
			if tc.want {
				want.step = oneInstantStep
			}
			if got.step != want.step || !got.start.Equal(want.start) || !got.end.Equal(want.end) {
				t.Fatalf("oneInstant = %s..%s by %s, want %s..%s by %s", got.start, got.end, got.step, want.start, want.end, want.step)
			}
		})
	}
}

// armShift returns the frame shift a run's scans read with.
func armShift(arm chplan.Node) int64 {
	shift := int64(0)
	chplan.WalkDeep(arm, func(n chplan.Node) bool {
		if sc, ok := n.(*chplan.Scan); ok && sc.TimeShiftMillis != 0 {
			shift = sc.TimeShiftMillis
		}
		return true
	})
	return shift
}

// mustLowerRange lowers q as a range query.
func mustLowerRange(t *testing.T, q string, start, end time.Time, step time.Duration) chplan.Node {
	t.Helper()
	plan, err := LowerAtRange(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

// TestFrameShift_TimestampReportsPrometheusInstants pins which date
// function reads its instant back out of the frame. In a framed lowering,
// `timestamp` of a selector reports the selected sample's instant and
// `timestamp` of any other expression the evaluation instant, each moved
// back by the frame shift in milliseconds; every other date function
// reads the sample value and takes neither path.
func TestFrameShift_TimestampReportsPrometheusInstants(t *testing.T) {
	at := instantDomainNearFloor.Add(-6 * time.Minute)
	for _, tc := range []struct {
		q    string
		want bool
	}{
		{`timestamp(m)`, true},
		{`timestamp(sum(m))`, true},
		{`minute(m)`, false},
		{`minute(sum(m))`, false},
	} {
		plan, err := LowerAt(context.Background(), mustParseExperimental(t, tc.q), schema.DefaultOTelMetrics(), at, at)
		if err != nil {
			t.Fatalf("lower %q: %v", tc.q, err)
		}
		shift := armShift(plan)
		if shift == 0 {
			t.Fatalf("lower %q: no scan reads in a frame", tc.q)
		}
		if got := holdsFrameSeconds(plan, shift); got != tc.want {
			t.Errorf("lower %q: reads seconds moved back by the frame = %v, want %v", tc.q, got, tc.want)
		}
	}
}

// holdsFrameSeconds reports whether plan computes seconds from an instant
// moved back by shift milliseconds: toFloat64(toUnixMillis(x) - shift) /
// 1000.
func holdsFrameSeconds(plan chplan.Node, shift int64) bool {
	found := false
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		chplan.InspectNodeExprs(n, func(e chplan.Expr) {
			chplan.InspectExpr(e, func(x chplan.Expr) bool {
				div, ok := x.(*chplan.Binary)
				if !ok || div.Op != chplan.OpDiv {
					return true
				}
				conv, ok := div.Left.(*chplan.FuncCall)
				if !ok || conv.Fn != chplan.FnToFloat64 || len(conv.Args) != 1 {
					return true
				}
				sub, ok := conv.Args[0].(*chplan.Binary)
				if !ok || sub.Op != chplan.OpSub {
					return true
				}
				ms, ok := sub.Left.(*chplan.FuncCall)
				lit, isLit := sub.Right.(*chplan.LitInt)
				found = found || (ok && ms.Fn == chplan.FnToUnixMillis && isLit && lit.V == shift)
				return true
			})
		})
		return true
	})
	return found
}
