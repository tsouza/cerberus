package chplan

import (
	"errors"
	"testing"
)

// TestRequireSpansScansBounded_DescendsIntoEmbeddedSubqueries pins that the
// emit-time resource-bound gate sees a spans scan wherever it lives.
//
// The descent walked Children() only, and a plan embedded in an Expr slot —
// an InSubquery's or a ScalarSubquery's input — is not reachable that way.
// So an unbounded scan of the whole traces table inside a subquery passed
// the gate vacuously, and that is exactly where TraceQL puts a spanset
// aggregate's cohort and its trace-scoped intrinsics: the blind spot
// covered the shapes most likely to read the entire table.
//
// The subquery is descended with a FRESH context because it is its own
// root: the outer spine's window and top-N do not govern what it reads.
func TestRequireSpansScansBounded_DescendsIntoEmbeddedSubqueries(t *testing.T) {
	t.Parallel()

	unbounded := func() Node { return &Scan{Table: spansTable} }

	cases := []struct {
		name string
		plan Node
	}{
		{
			name: "in_subquery_under_a_filter_predicate",
			plan: &Filter{
				Input: &Scan{Table: metricsTable},
				Predicate: &InSubquery{
					Left:     &ColumnRef{Name: "TraceId"},
					Subquery: unbounded(),
				},
			},
		},
		{
			name: "scalar_subquery_under_a_filter_predicate",
			plan: &Filter{
				Input: &Scan{Table: metricsTable},
				Predicate: &Binary{
					Op:    OpGt,
					Left:  &ColumnRef{Name: "Value"},
					Right: &ScalarSubquery{Input: unbounded()},
				},
			},
		},
		{
			name: "scalar_subquery_in_a_projection",
			plan: &Project{
				Input: &Scan{Table: metricsTable},
				Projections: []Projection{
					{Expr: &ScalarSubquery{Input: unbounded()}, Alias: "n"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := RequireSpansScansBounded(spansTable, tc.plan)
			var v *ScanResourceBoundViolation
			if !errors.As(err, &v) {
				t.Fatalf(
					"an unbounded spans scan inside a subquery passed the gate (err=%v): "+
						"the descent does not reach plans embedded in Expr slots",
					err,
				)
			}
			if v.Table != spansTable {
				t.Errorf("violation names table %q, want %q", v.Table, spansTable)
			}
		})
	}
}

// TestRequireSpansScansBounded_OuterBoundDoesNotVouchForASubquery pins the
// reason the embedded plan gets a fresh context: an outer window or top-N
// constrains the outer scan, not what a subquery reads. Treating the outer
// bound as governing would turn the fix into a different vacuous pass.
func TestRequireSpansScansBounded_OuterBoundDoesNotVouchForASubquery(t *testing.T) {
	t.Parallel()

	// The outer spans scan IS bounded — by a top-N Limit — while the
	// subquery's is not.
	plan := &Limit{
		Count: 10,
		Input: &Filter{
			Input: &Scan{Table: spansTable},
			Predicate: &InSubquery{
				Left:     &ColumnRef{Name: "TraceId"},
				Subquery: &Scan{Table: spansTable},
			},
		},
	}

	var v *ScanResourceBoundViolation
	if err := RequireSpansScansBounded(spansTable, plan); !errors.As(err, &v) {
		t.Fatalf(
			"the outer LIMIT was read as bounding the subquery's own scan (err=%v); "+
				"a subquery must carry its own bound",
			err,
		)
	}
}

// TestRequireSpansScansBounded_BoundedSubqueryIsAccepted is the other half:
// the new descent must not false-reject a subquery that IS bounded, or the
// gate stops being usable.
func TestRequireSpansScansBounded_BoundedSubqueryIsAccepted(t *testing.T) {
	t.Parallel()

	plan := &Filter{
		Input: &Scan{Table: metricsTable},
		Predicate: &InSubquery{
			Left: &ColumnRef{Name: "TraceId"},
			Subquery: &Limit{
				Count: 25,
				Input: &Scan{Table: spansTable},
			},
		},
	}

	if err := RequireSpansScansBounded(spansTable, plan); err != nil {
		t.Fatalf("a bounded subquery scan must pass, got %v", err)
	}
}
