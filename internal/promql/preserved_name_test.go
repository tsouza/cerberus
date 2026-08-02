package promql

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func mustMatcher(t *testing.T, typ labels.MatchType, name, value string) *labels.Matcher {
	t.Helper()
	m, err := labels.NewMatcher(typ, name, value)
	if err != nil {
		t.Fatalf("NewMatcher(%v, %q, %q): %v", typ, name, value, err)
	}
	return m
}

// newAttrOnlyWindow builds the RangeWindow shape the range-vector
// lowering hands to the preserve-name wrappers: grouped on the
// Attributes map alone, which is exactly why a per-series metric name
// cannot survive it unaided.
func newAttrOnlyWindow(s schema.Metrics) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func:    "last_over_time",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
}

// TestPreservedNameExpr_EqualityMatcherPinsLiteral: one name for every
// row, so the window's grouping key must be left alone — widening it
// would split nothing and cost a GROUP BY key.
func TestPreservedNameExpr_EqualityMatcherPinsLiteral(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	rw := newAttrOnlyWindow(s)

	got := preservedNameExpr(rw, []*labels.Matcher{
		mustMatcher(t, labels.MatchEqual, model.MetricNameLabel, "cpu_temp"),
		mustMatcher(t, labels.MatchEqual, "host", "a"),
	}, s)

	lit, ok := got.(*chplan.LitString)
	if !ok {
		t.Fatalf("got %T, want *chplan.LitString", got)
	}
	if lit.V != "cpu_temp" {
		t.Fatalf("literal name: got %q, want %q", lit.V, "cpu_temp")
	}
	if len(rw.GroupBy) != 1 {
		t.Fatalf("grouping key widened for a pinned name: got %d keys, want 1", len(rw.GroupBy))
	}
}

// TestPreservedNameExpr_RegexMatcherCarriesColumn is the case #1492
// reported: the name differs per series, so it has to join the grouping
// key or the metrics collapse into one another.
func TestPreservedNameExpr_RegexMatcherCarriesColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	rw := newAttrOnlyWindow(s)

	got := preservedNameExpr(rw, []*labels.Matcher{
		mustMatcher(t, labels.MatchRegexp, model.MetricNameLabel, "cpu_temp|gpu_temp"),
	}, s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	if len(rw.GroupBy) != 2 {
		t.Fatalf("grouping key: got %d keys, want 2 (Attributes + %s)", len(rw.GroupBy), s.MetricNameColumn)
	}
	if !isIdentityColumnRef(rw.GroupBy[1], s.MetricNameColumn) {
		t.Fatalf("appended key: got %#v, want ColumnRef(%q)", rw.GroupBy[1], s.MetricNameColumn)
	}
	// The Attributes key must survive alongside it, not be replaced.
	if !isIdentityColumnRef(rw.GroupBy[0], s.AttributesColumn) {
		t.Fatalf("original key clobbered: got %#v, want ColumnRef(%q)", rw.GroupBy[0], s.AttributesColumn)
	}
}

// TestPreservedNameExpr_NoNameMatcherCarriesColumn: a selector matching
// purely on labels (`last_over_time({host="a"}[5m])`) spans every metric
// carrying that label, so it needs the same per-row treatment as the
// regex case.
func TestPreservedNameExpr_NoNameMatcherCarriesColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	rw := newAttrOnlyWindow(s)

	got := preservedNameExpr(rw, []*labels.Matcher{
		mustMatcher(t, labels.MatchEqual, "host", "a"),
	}, s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	if len(rw.GroupBy) != 2 {
		t.Fatalf("grouping key: got %d keys, want 2", len(rw.GroupBy))
	}
}

// TestPreservedNameExpr_DoesNotDuplicateExistingKey pins idempotence: a
// window that already groups on MetricName must not gain a second copy,
// which would be a redundant GROUP BY key and a churned golden.
func TestPreservedNameExpr_DoesNotDuplicateExistingKey(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	rw := newAttrOnlyWindow(s)
	rw.GroupBy = append(rw.GroupBy, &chplan.ColumnRef{Name: s.MetricNameColumn})

	got := preservedNameExpr(rw, []*labels.Matcher{
		mustMatcher(t, labels.MatchRegexp, model.MetricNameLabel, "cpu_temp|gpu_temp"),
	}, s)

	if !isIdentityColumnRef(got, s.MetricNameColumn) {
		t.Fatalf("projected name expr: got %#v, want ColumnRef(%q)", got, s.MetricNameColumn)
	}
	if len(rw.GroupBy) != 2 {
		t.Fatalf("grouping key gained a duplicate: got %d keys, want 2", len(rw.GroupBy))
	}
}

// TestPreservedNameExpr_SchemaWithoutMetricNameColumn: there is no
// per-series name to carry, so the wrapper keeps its canonical
// 4-column shape with an empty literal rather than emitting a
// ColumnRef to a column that does not exist.
func TestPreservedNameExpr_SchemaWithoutMetricNameColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = ""
	rw := newAttrOnlyWindow(s)

	got := preservedNameExpr(rw, []*labels.Matcher{
		mustMatcher(t, labels.MatchRegexp, model.MetricNameLabel, "cpu_temp|gpu_temp"),
	}, s)

	lit, ok := got.(*chplan.LitString)
	if !ok {
		t.Fatalf("got %T, want *chplan.LitString", got)
	}
	if lit.V != "" {
		t.Fatalf("literal name: got %q, want empty", lit.V)
	}
	if len(rw.GroupBy) != 1 {
		t.Fatalf("grouping key widened with a nameless column: got %d keys, want 1", len(rw.GroupBy))
	}
}

// TestRangeFnPreservesName pins the exact upstream membership. Widening
// it silently restores `__name__` on functions Prometheus strips it
// from; narrowing it re-opens #1492 for the other half of the pair.
func TestRangeFnPreservesName(t *testing.T) {
	t.Parallel()
	preserving := []string{"last_over_time", "first_over_time"}
	dropping := []string{
		"rate", "increase", "delta", "irate", "idelta",
		"avg_over_time", "sum_over_time", "min_over_time", "max_over_time",
		"count_over_time", "stddev_over_time", "quantile_over_time",
		"predict_linear", "holt_winters", "present_over_time",
	}
	for _, fn := range preserving {
		if !rangeFnPreservesName(fn) {
			t.Errorf("%s: got drop, want preserve", fn)
		}
	}
	for _, fn := range dropping {
		if rangeFnPreservesName(fn) {
			t.Errorf("%s: got preserve, want drop", fn)
		}
	}
}
