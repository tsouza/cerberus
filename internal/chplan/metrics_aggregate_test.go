package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMetricsAggregateEqual exercises structural equality across the
// node's fields — Op, Attr, GroupBy, GroupByAliases, Quantiles,
// ValueAlias, Inner.
func TestMetricsAggregateEqual(t *testing.T) {
	t.Parallel()

	base := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		GroupByAliases: []string{"service"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	same := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		GroupByAliases: []string{"service"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	if !base.Equal(same) {
		t.Fatalf("identical MetricsAggregate trees should be Equal")
	}

	// Different Op.
	diffOp := *same
	diffOp.Op = chplan.MetricsOpCountOverTime
	if base.Equal(&diffOp) {
		t.Errorf("different Op should not be Equal")
	}

	// Different ValueAlias.
	diffAlias := *same
	diffAlias.ValueAlias = "other"
	if base.Equal(&diffAlias) {
		t.Errorf("different ValueAlias should not be Equal")
	}

	// Attr presence diff.
	withAttr := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpSumOverTime,
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	withoutAttr := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpSumOverTime,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	if withAttr.Equal(withoutAttr) {
		t.Errorf("Attr presence should differentiate Equal")
	}
	if withoutAttr.Equal(withAttr) {
		t.Errorf("Attr presence should differentiate Equal (other side)")
	}

	// Attr value diff.
	otherAttr := *withAttr
	otherAttr.Attr = &chplan.ColumnRef{Name: "Other"}
	if withAttr.Equal(&otherAttr) {
		t.Errorf("different Attr exprs should not be Equal")
	}

	// Quantile diff.
	q1 := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpQuantileOverTime,
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		Quantiles:  []float64{0.95},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	q2 := *q1
	q2.Quantiles = []float64{0.99}
	if q1.Equal(&q2) {
		t.Errorf("different Quantiles should not be Equal")
	}

	// GroupByAliases diff.
	diffGroupAlias := *same
	diffGroupAlias.GroupByAliases = []string{"other"}
	if base.Equal(&diffGroupAlias) {
		t.Errorf("different GroupByAliases should not be Equal")
	}

	// GroupByDisplayNames diff — populated on one side, empty on the
	// other (the legacy / non-TraceQL path leaves it empty).
	withDisplay := *same
	withDisplay.GroupByDisplayNames = []string{"resource.service.name"}
	if base.Equal(&withDisplay) {
		t.Errorf("display-name presence should differentiate Equal")
	}
	if withDisplay.Equal(base) {
		t.Errorf("display-name presence should differentiate Equal (other side)")
	}

	// GroupByDisplayNames diff — different prefixes (resource vs. span).
	otherDisplay := withDisplay
	otherDisplay.GroupByDisplayNames = []string{"span.service.name"}
	if withDisplay.Equal(&otherDisplay) {
		t.Errorf("different GroupByDisplayNames should not be Equal")
	}

	// GroupBy len diff.
	moreGroup := *same
	moreGroup.GroupBy = append([]chplan.Expr{}, same.GroupBy...)
	moreGroup.GroupBy = append(moreGroup.GroupBy, &chplan.ColumnRef{Name: "Host"})
	moreGroup.GroupByAliases = []string{"service", "host"}
	if base.Equal(&moreGroup) {
		t.Errorf("different GroupBy length should not be Equal")
	}

	// Inner diff.
	diffInner := *same
	diffInner.Inner = &chplan.Scan{Table: "other_traces"}
	if base.Equal(&diffInner) {
		t.Errorf("different Inner should not be Equal")
	}

	// Different node type.
	scan := &chplan.Scan{Table: "otel_traces"}
	if base.Equal(scan) {
		t.Errorf("MetricsAggregate.Equal of *Scan should be false")
	}
}

// TestMetricsAggregateChildren confirms Walk descends through Inner.
func TestMetricsAggregateChildren(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_traces"}
	ma := &chplan.MetricsAggregate{
		Op:    chplan.MetricsOpRate,
		Inner: scan,
	}
	kids := ma.Children()
	if len(kids) != 1 {
		t.Fatalf("expected 1 child, got %d", len(kids))
	}
	if kids[0] != scan {
		t.Errorf("Children[0] should be the Inner scan, got %T", kids[0])
	}

	var visited []string
	chplan.Walk(ma, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.MetricsAggregate:
			visited = append(visited, "MetricsAggregate")
		case *chplan.Scan:
			visited = append(visited, "Scan")
		}
		return true
	})
	want := []string{"MetricsAggregate", "Scan"}
	if len(visited) != len(want) {
		t.Fatalf("Walk visited %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Errorf("Walk[%d] = %s, want %s", i, visited[i], want[i])
		}
	}
}

// TestMetricsOpString round-trips the enum to its TraceQL-source name.
func TestMetricsOpString(t *testing.T) {
	t.Parallel()

	cases := map[chplan.MetricsOp]string{
		chplan.MetricsOpInvalid:          "invalid",
		chplan.MetricsOpRate:             "rate",
		chplan.MetricsOpCountOverTime:    "count_over_time",
		chplan.MetricsOpSumOverTime:      "sum_over_time",
		chplan.MetricsOpAvgOverTime:      "avg_over_time",
		chplan.MetricsOpMinOverTime:      "min_over_time",
		chplan.MetricsOpMaxOverTime:      "max_over_time",
		chplan.MetricsOpQuantileOverTime: "quantile_over_time",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("MetricsOp(%d).String() = %q, want %q", op, got, want)
		}
	}
}
