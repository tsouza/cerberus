package chplan_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEqual exercises the structural equality methods on a handful of trees;
// it's the primary contract optimizer-rule tests will rely on.
func TestEqual(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	scanSame := &chplan.Scan{Table: "otel_metrics_gauge"}
	scanOther := &chplan.Scan{Table: "otel_logs"}
	if !scan.Equal(scanSame) {
		t.Fatalf("Scan: identical trees should be Equal")
	}
	if scan.Equal(scanOther) {
		t.Fatalf("Scan: different tables should not be Equal")
	}

	filter := func() *chplan.Filter {
		return &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "http_requests_total"},
			},
		}
	}
	if !filter().Equal(filter()) {
		t.Fatalf("Filter: identical trees should be Equal")
	}
	differentPredicate := filter()
	differentPredicate.Predicate = &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "other_metric"},
	}
	if filter().Equal(differentPredicate) {
		t.Fatalf("Filter: different predicates should not be Equal")
	}

	mwk := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"instance", "pod"},
	}
	mwkSame := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"instance", "pod"},
	}
	mwkReordered := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"pod", "instance"},
	}
	if !mwk.Equal(mwkSame) {
		t.Fatalf("MapWithoutKeys: identical keys should be Equal")
	}
	if mwk.Equal(mwkReordered) {
		t.Fatalf("MapWithoutKeys: reordered keys are observably different (groups would differ)")
	}

	vj := &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_sum"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	vjSame := &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_sum"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	vjDifferentMatch := *vjSame
	vjDifferentMatch.Match = chplan.VectorMatch{Labels: []string{"job"}, On: false}
	if !vj.Equal(vjSame) {
		t.Fatalf("VectorJoin: identical trees should be Equal")
	}
	if vj.Equal(&vjDifferentMatch) {
		t.Fatalf("VectorJoin: on(job) vs ignoring(job) should not be Equal")
	}

	rw := &chplan.RangeWindow{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Func:  "rate",
		Range: 5 * time.Minute,
		Step:  time.Minute,
	}
	rwDifferentRange := &chplan.RangeWindow{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Func:  "rate",
		Range: 10 * time.Minute,
		Step:  time.Minute,
	}
	if rw.Equal(rwDifferentRange) {
		t.Fatalf("RangeWindow: different Range should not be Equal")
	}
}

// TestWalk confirms the visitor descends in pre-order and respects the
// false-skip signal.
func TestWalk(t *testing.T) {
	t.Parallel()

	tree := &chplan.Filter{
		Input: &chplan.Project{
			Input:       &chplan.Scan{Table: "otel_logs"},
			Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Body"}}},
		},
		Predicate: &chplan.LitBool{V: true},
	}

	var got []string
	chplan.Walk(tree, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.Filter:
			got = append(got, "Filter")
		case *chplan.Project:
			got = append(got, "Project")
		case *chplan.Scan:
			got = append(got, "Scan")
		}
		return true
	})

	want := []string{"Filter", "Project", "Scan"}
	if len(got) != len(want) {
		t.Fatalf("Walk: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Walk[%d]: got %s, want %s", i, got[i], want[i])
		}
	}

	// false-skip: stop at Project.
	got = got[:0]
	chplan.Walk(tree, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.Filter:
			got = append(got, "Filter")
			return true
		case *chplan.Project:
			got = append(got, "Project")
			return false // skip children
		case *chplan.Scan:
			got = append(got, "Scan")
			_ = v
		}
		return true
	})
	want = []string{"Filter", "Project"}
	if len(got) != len(want) {
		t.Fatalf("Walk skip: got %v, want %v", got, want)
	}
}
