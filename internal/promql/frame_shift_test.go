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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lowerCtx{start: tc.start, end: tc.end, step: tc.step}
			expr := mustParseExperimental(t, tc.query)
			before := expr.String()
			if _, _, err := fitDomain(expr, ctx, 0); !isInstantDomainError(err) {
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
			by := time.Duration(f.frameShift) * time.Millisecond
			if !lctx.start.Equal(tc.start.Add(by)) || !lctx.end.Equal(tc.end.Add(by)) || lctx.shiftMillis != f.frameShift {
				t.Fatalf("lowering context %s..%s shift %d, want the request moved by %d ms", lctx.start, lctx.end, lctx.shiftMillis, f.frameShift)
			}
		})
	}
}

// TestFrameShift_KeepsRejections pins the queries no shifted frame
// answers: one whose instants span more than the domain, and one holding
// a subquery region that diverges from Prometheus whatever its instants.
// Each keeps the rejection of Prometheus's own frame.
func TestFrameShift_KeepsRejections(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		start, end time.Time
		step       time.Duration
	}{
		{"range_spanning_the_domain", `m`, instantDomainNearFloor.Add(-6 * time.Minute), instantDomainNearCeiling, 24 * time.Hour},
		{"nested_subquery_near_floor", `max_over_time(max_over_time(m[2m:1m])[5m:1m])`, instantDomainNearFloor.Add(-6 * time.Minute), instantDomainNearFloor.Add(-6 * time.Minute), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := lowerCtx{start: tc.start, end: tc.end, step: tc.step}
			_, _, want := fitDomain(mustParseExperimental(t, tc.query), ctx, 0)
			if !isInstantDomainError(want) {
				t.Fatalf("fit %q in Prometheus's frame: err = %v, want the instant-domain rejection", tc.query, want)
			}
			_, err := LowerAtRange(context.Background(), mustParseExperimental(t, tc.query), schema.DefaultOTelMetrics(), tc.start, tc.end, tc.step)
			if err == nil || err.Error() != want.Error() {
				t.Fatalf("lower %q: err = %v, want %v", tc.query, err, want)
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
