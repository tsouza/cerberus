package promql

import (
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestCutSelector_InDomainLookbackIsKept pins that a selector whose
// lookback lies wholly in the stored range is not cut.
func TestCutSelector_InDomainLookbackIsKept(t *testing.T) {
	vs := mustParseExperimental(t, `m`).(*parser.VectorSelector)
	if cut, ok := cutSelector(vs, fitEnvAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))); ok {
		t.Fatalf("cutSelector cut an in-domain lookback to %s", cut)
	}
}

// TestCheckBeyondRead_SkipsReadWindows pins that a cut window some
// lowering read is passed over and a later unread one still rejects the
// query.
func TestCheckBeyondRead_SkipsReadWindows(t *testing.T) {
	end := time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC)
	f := &domainFit{beyond: []*beyondCut{
		{cut: storageCut{endMillis: end.UnixMilli(), rng: time.Hour, endBeyond: time.Hour}, read: true},
		{cut: storageCut{endMillis: end.UnixMilli(), rng: time.Hour, endBeyond: time.Hour}},
	}}
	if err := f.checkBeyondRead(); err == nil {
		t.Fatal("an unread cut window after a read one was accepted")
	}
}

// TestStaleLayoutWindowFor_RangeOffset pins the layout window of a range
// query's selector with an offset: the request range moved back by the
// offset, widened by the lookback and a step.
func TestStaleLayoutWindowFor_RangeOffset(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	vs := mustParseExperimental(t, `m_bucket offset 1h`).(*parser.VectorSelector)
	w, err := staleLayoutWindowFor(vs, lowerCtx{start: start, end: end, step: time.Minute})
	if err != nil {
		t.Fatalf("staleLayoutWindowFor: %v", err)
	}
	if !w.ranged || !w.lo.Equal(start.Add(-time.Hour-instantLookback-time.Minute)) || !w.hi.Equal(end.Add(-time.Hour)) {
		t.Fatalf("layout window = %+v, want (%s, %s]", w, start.Add(-time.Hour-instantLookback-time.Minute), end.Add(-time.Hour))
	}
}

// TestBoundStaleMarkerLayoutsToIdentity pins the input window of a
// subquery's Identity window that bounds the stale-marker layouts below
// it: the outer range, the range and a step back from its end.
func TestBoundStaleMarkerLayoutsToIdentity(t *testing.T) {
	filter := &chplan.Filter{Input: &chplan.Scan{Table: "otel_metrics_histogram"}, Predicate: staleLayoutPendingBound()}
	layout := &chplan.Project{
		Input:       &chplan.Project{Input: filter},
		Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 0}, Alias: staleLayoutCountsAlias}},
	}
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rw := &chplan.RangeWindow{Input: layout, TimestampColumn: "TimeUnix", OuterRange: 10 * time.Minute, Range: 5 * time.Minute, Step: time.Minute, End: end}
	boundStaleMarkerLayoutsToIdentity(rw)
	anchor := evalAnchor{}
	anchor.setEnd(end)
	if want := staleLayoutAnchoredBound("TimeUnix", anchor, 16*time.Minute); !reflect.DeepEqual(filter.Predicate, want) {
		t.Fatalf("layout bound = %#v, want the 16 minutes before the end", filter.Predicate)
	}
}
