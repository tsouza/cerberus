package optimizer_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// Shared fixtures.
func scan() *chplan.Scan {
	return &chplan.Scan{Table: "otel_metrics_gauge"}
}

func filterScan() *chplan.Filter {
	return &chplan.Filter{
		Input: scan(),
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}
}

func TestAny_MatchesAnyNonNilNode(t *testing.T) {
	t.Parallel()
	p := optimizer.Any()

	for _, n := range []chplan.Node{scan(), filterScan(), &chplan.Limit{Input: scan(), Count: 10}} {
		if b, ok := p.Match(n); !ok || b == nil {
			t.Fatalf("Any().Match(%T) = (%v, %v); want match", n, b, ok)
		}
	}
}

func TestAny_RejectsNil(t *testing.T) {
	t.Parallel()
	if _, ok := optimizer.Any().Match(nil); ok {
		t.Fatal("Any().Match(nil) matched; want no match")
	}
}

func TestKind_MatchesSameKind(t *testing.T) {
	t.Parallel()
	if _, ok := optimizer.Kind(optimizer.KindScan).Match(scan()); !ok {
		t.Fatal("Kind(KindScan).Match(Scan) failed")
	}
	if _, ok := optimizer.Kind(optimizer.KindFilter).Match(filterScan()); !ok {
		t.Fatal("Kind(KindFilter).Match(Filter) failed")
	}
}

func TestKind_RejectsDifferentKind(t *testing.T) {
	t.Parallel()
	if _, ok := optimizer.Kind(optimizer.KindFilter).Match(scan()); ok {
		t.Fatal("Kind(KindFilter).Match(Scan) matched; want no match")
	}
	if _, ok := optimizer.Kind(optimizer.KindScan).Match(filterScan()); ok {
		t.Fatal("Kind(KindScan).Match(Filter) matched; want no match")
	}
}

func TestKind_RejectsNil(t *testing.T) {
	t.Parallel()
	if _, ok := optimizer.Kind(optimizer.KindScan).Match(nil); ok {
		t.Fatal("Kind(KindScan).Match(nil) matched; want no match")
	}
}

func TestKindOf_RoundTrip(t *testing.T) {
	t.Parallel()
	s := scan()
	if got := optimizer.KindOf(s); got != optimizer.KindScan {
		t.Fatalf("KindOf(Scan) = %v; want KindScan", got)
	}
	f := filterScan()
	if got := optimizer.KindOf(f); got != optimizer.KindFilter {
		t.Fatalf("KindOf(Filter) = %v; want KindFilter", got)
	}
}

func TestCapture_BindsMatchedNode(t *testing.T) {
	t.Parallel()
	s := scan()
	p := optimizer.Capture("root", optimizer.Kind(optimizer.KindScan))

	b, ok := p.Match(s)
	if !ok {
		t.Fatal("Capture(...Kind(KindScan)).Match(Scan) failed")
	}
	got, ok := b.Get("root")
	if !ok {
		t.Fatal("Bindings.Get(root) missing")
	}
	if got != chplan.Node(s) {
		t.Fatalf("captured node = %v; want the scan itself", got)
	}
}

func TestCapture_DoesNotBindOnMissMatch(t *testing.T) {
	t.Parallel()
	p := optimizer.Capture("root", optimizer.Kind(optimizer.KindFilter))
	if b, ok := p.Match(scan()); ok {
		t.Fatalf("Capture over miss matched: bindings=%v", b)
	}
}

func TestWithChildren_MatchesParentAndChildrenInOrder(t *testing.T) {
	t.Parallel()
	f := filterScan()

	p := optimizer.WithChildren(
		optimizer.Kind(optimizer.KindFilter),
		optimizer.Capture("input", optimizer.Kind(optimizer.KindScan)),
	)

	b, ok := p.Match(f)
	if !ok {
		t.Fatal("WithChildren(Filter, Scan).Match(Filter(Scan)) failed")
	}
	got, ok := b.Get("input")
	if !ok || got != chplan.Node(f.Input) {
		t.Fatalf("captured input = %v; want Filter.Input", got)
	}
}

func TestWithChildren_RejectsWrongChildKind(t *testing.T) {
	t.Parallel()
	f := filterScan()
	// Filter has a Scan child, not a Limit.
	p := optimizer.WithChildren(
		optimizer.Kind(optimizer.KindFilter),
		optimizer.Kind(optimizer.KindLimit),
	)
	if _, ok := p.Match(f); ok {
		t.Fatal("WithChildren(Filter, Limit).Match(Filter(Scan)) matched; want no match")
	}
}

func TestWithChildren_RejectsArityMismatch(t *testing.T) {
	t.Parallel()
	// Scan has 0 children; specifying 1 child must reject.
	p := optimizer.WithChildren(
		optimizer.Kind(optimizer.KindScan),
		optimizer.Any(),
	)
	if _, ok := p.Match(scan()); ok {
		t.Fatal("WithChildren(Scan, Any).Match(Scan) matched; want no match (arity)")
	}
}

func TestWithChildren_RejectsParentMiss(t *testing.T) {
	t.Parallel()
	p := optimizer.WithChildren(
		optimizer.Kind(optimizer.KindLimit),
		optimizer.Any(),
	)
	if _, ok := p.Match(filterScan()); ok {
		t.Fatal("WithChildren(Limit, Any).Match(Filter(Scan)) matched; want no match")
	}
}

func TestWithChildren_MergesBindings(t *testing.T) {
	t.Parallel()
	f := filterScan()
	p := optimizer.WithChildren(
		optimizer.Capture("parent", optimizer.Kind(optimizer.KindFilter)),
		optimizer.Capture("child", optimizer.Any()),
	)
	b, ok := p.Match(f)
	if !ok {
		t.Fatal("WithChildren capture-parent + capture-child failed to match")
	}
	if got, _ := b.Get("parent"); got != chplan.Node(f) {
		t.Fatalf("parent binding = %v; want Filter", got)
	}
	if got, _ := b.Get("child"); got != chplan.Node(f.Input) {
		t.Fatalf("child binding = %v; want Scan", got)
	}
}

func TestBindings_GetMissingReturnsFalse(t *testing.T) {
	t.Parallel()
	b, ok := optimizer.Any().Match(scan())
	if !ok {
		t.Fatal("Any().Match(Scan) failed")
	}
	if _, present := b.Get("never-bound"); present {
		t.Fatal("Bindings.Get(missing) reported present")
	}
}
