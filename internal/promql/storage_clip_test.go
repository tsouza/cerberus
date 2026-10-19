package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestStorageCut_ExtrapolatedWindowsCarryTheirEdges pins that rate,
// increase and delta over a window reaching past the stored range read the
// cut window, and that the fit records how far Prometheus's own edges lie
// beyond it: past the ceiling, the pin's distance from the last stored
// millisecond; below the floor, the window's start's distance from the
// first instant a cut window starts at.
func TestStorageCut_ExtrapolatedWindowsCarryTheirEdges(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		end         time.Time
		want        string
		startBeyond time.Duration
		endBeyond   time.Duration
	}{
		{
			"rate_past_ceiling", `rate(m[10m] @ 9223372140)`, instantDomainEvalEnd,
			`rate(m[8m16s854ms] @ 9223372036.854)`, 0, 103146 * time.Millisecond,
		},
		{
			"increase_past_ceiling", `increase(m[10m] @ 9223372140)`, instantDomainEvalEnd,
			`increase(m[8m16s854ms] @ 9223372036.854)`, 0, 103146 * time.Millisecond,
		},
		{
			"delta_past_ceiling", `delta(m[10m] @ 9223372140)`, instantDomainEvalEnd,
			`delta(m[8m16s854ms] @ 9223372036.854)`, 0, 103146 * time.Millisecond,
		},
		{
			"rate_below_floor", `rate(m[14m] @ -9223371270)`, instantDomainNearFloor,
			`rate(m[12m46s854ms] @ -9223371270.000)`, 73146*time.Millisecond - time.Nanosecond, 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fit, err := fitAt(t, tc.query, tc.end)
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			if got.String() != tc.want {
				t.Fatalf("fitInstantDomain(%q) = %q, want %q", tc.query, got.String(), tc.want)
			}
			ms := got.(*parser.Call).Args[0].(*parser.MatrixSelector)
			start, end := fit.windowBeyond(ms)
			if start != tc.startBeyond || end != tc.endBeyond {
				t.Fatalf("windowBeyond = %v, %v; want %v, %v", start, end, tc.startBeyond, tc.endBeyond)
			}
			if err := fit.checkBeyondRead(); err != nil {
				t.Fatalf("checkBeyondRead after the lowering read the cut: %v", err)
			}
		})
	}
}

// TestStorageCut_LoweringCarriesEdgesIntoThePlan pins that the lowering
// hands the edges the fit recorded to the RangeWindow it builds.
func TestStorageCut_LoweringCarriesEdgesIntoThePlan(t *testing.T) {
	q := `rate(m[10m] @ 9223372140)`
	plan, err := LowerAt(context.Background(), mustParseExperimental(t, q), schema.DefaultOTelMetrics(), instantDomainEvalEnd, instantDomainEvalEnd)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", q, err)
	}
	var found []*chplan.RangeWindow
	chplan.Walk(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok {
			found = append(found, rw)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("LowerAt(%q) built %d RangeWindows, want 1", q, len(found))
	}
	rw := found[0]
	if rw.StartBeyond != 0 || rw.EndBeyond != 103146*time.Millisecond {
		t.Fatalf("RangeWindow beyond = %v, %v; want 0, 1m43.146s", rw.StartBeyond, rw.EndBeyond)
	}
	if rw.DownsampleTier || rw.DeltaPrefixAggregateInput != nil {
		t.Fatalf("a cut window carries a bucket arm: downsample %v, delta prefix %v", rw.DownsampleTier, rw.DeltaPrefixAggregateInput)
	}
}

// TestStorageCut_UnreadEdgesAreRejected pins the fail-closed check: a cut
// whose edges no lowering read rejects the query with the instant of
// Prometheus's window that lies outside the stored range.
func TestStorageCut_UnreadEdgesAreRejected(t *testing.T) {
	cases := []struct {
		name  string
		query string
		end   time.Time
		want  time.Time
	}{
		{"past_ceiling", `rate(m[10m] @ 9223372140)`, instantDomainEvalEnd, time.Unix(9223372140, 0)},
		{"below_floor", `rate(m[14m] @ -9223371270)`, instantDomainNearFloor, time.Unix(-9223371270-14*60, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, fit, err := fitAt(t, tc.query, tc.end)
			if err != nil {
				t.Fatalf("fitInstantDomain(%q): %v", tc.query, err)
			}
			want := instantDomainError(tc.want.UTC()).Error()
			if err := fit.checkBeyondRead(); err == nil || err.Error() != want {
				t.Fatalf("checkBeyondRead = %v, want %q", err, want)
			}
		})
	}
	var none *domainFit
	if err := none.checkBeyondRead(); err != nil {
		t.Fatalf("checkBeyondRead on no fit: %v", err)
	}
	if start, end := none.windowBeyond(&parser.MatrixSelector{}); start != 0 || end != 0 {
		t.Fatalf("windowBeyond on no fit = %v, %v; want 0, 0", start, end)
	}
}

// TestStorageCut_NativeGridRefusesCutWindows pins that neither native
// timeSeries*ToGrid funnel takes a cut window: the aggregate extrapolates
// to the edges of the window it reads, not to Prometheus's.
func TestStorageCut_NativeGridRefusesCutWindows(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, beyond := range []struct {
		name string
		set  func(*chplan.RangeWindow)
	}{
		{"start", func(rw *chplan.RangeWindow) { rw.StartBeyond = time.Second }},
		{"end", func(rw *chplan.RangeWindow) { rw.EndBeyond = time.Second }},
	} {
		t.Run(beyond.name, func(t *testing.T) {
			matrix := acceptedGridWindow(s)
			if nativeTSGridMatrixNode(matrix, "rate", s, false) == nil {
				t.Fatal("baseline matrix window was refused")
			}
			beyond.set(matrix)
			if got := nativeTSGridMatrixNode(matrix, "rate", s, false); got != nil {
				t.Fatalf("nativeTSGridMatrixNode took a cut window: %#v", got)
			}
			instant := nativeGridInstantAcceptedWindow(s)
			if nativeTSGridInstantNode(instant, "rate", s) == nil {
				t.Fatal("baseline instant window was refused")
			}
			beyond.set(instant)
			if got := nativeTSGridInstantNode(instant, "rate", s); got != nil {
				t.Fatalf("nativeTSGridInstantNode took a cut window: %#v", got)
			}
		})
	}
}
