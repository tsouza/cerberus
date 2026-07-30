package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

const pushdownSpansTable = "otel_traces"

// pushdownPred is the predicate the tests below fold into a spans scan.
func pushdownPred() chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "TraceId"},
		Right: &chplan.LitString{V: "aabb"},
	}
}

// TestMutation_PushPredicateToSpansScanFilter_RecursesThroughNestedFilter pins
// that the walker keeps descending when a Filter's input is NOT a Scan. The
// prod shape it serves is a stack of Filters over one spans Scan (PREWHERE
// promotion and the optimizer both produce it), so a walker that only
// recognises the Filter-directly-over-Scan case would fail to fold the window
// bound and leave the root leg reading full retention.
//
// Kills metrics_compare.go's `ok && sc.Table == spansTable` INVERT_LOGICAL
// (`||`): when the type assertion fails, `sc` is a nil *chplan.Scan, so the
// mutant's second operand dereferences nil and panics on exactly the nested
// shape this test builds.
func TestMutation_PushPredicateToSpansScanFilter_RecursesThroughNestedFilter(t *testing.T) {
	t.Parallel()

	inner := &chplan.Filter{
		Input:     &chplan.Scan{Table: pushdownSpansTable},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ParentSpanId"}, Right: &chplan.LitString{V: ""}},
	}
	outer := &chplan.Filter{
		Input:     inner,
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "StatusCode"}, Right: &chplan.LitString{V: "Error"}},
	}

	if !pushPredicateToSpansScanFilter(outer, pushdownSpansTable, pushdownPred()) {
		t.Fatal("a Filter stack over a spans Scan must accept the pushed predicate")
	}
	// The fold must land on the Filter that actually sits on the Scan — that is
	// the only position from which ClickHouse can prune the MergeTree read.
	if _, ok := inner.Predicate.(*chplan.Binary); !ok || inner.Predicate == nil {
		t.Fatalf("inner Filter predicate not conjoined: %#v", inner.Predicate)
	}
	if inner.Predicate.(*chplan.Binary).Op != chplan.OpAnd {
		t.Fatalf("expected the scan-adjacent Filter to gain an AND conjunct, got op %v", inner.Predicate.(*chplan.Binary).Op)
	}
	if outer.Predicate.(*chplan.Binary).Op == chplan.OpAnd {
		t.Fatal("the predicate was folded into the outer Filter, which does not prune the scan")
	}
}

// TestMutation_PushPredicateToSpansScanFilter_IgnoresForeignTable pins the
// table half of the same guard: a Filter over a scan of some OTHER table is not
// a fold target, so the walker reports failure rather than bounding an
// unrelated relation. Without this the recursion assertion above could be
// satisfied by a walker that folds into the first Filter it meets.
func TestMutation_PushPredicateToSpansScanFilter_IgnoresForeignTable(t *testing.T) {
	t.Parallel()

	n := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SeverityText"}, Right: &chplan.LitString{V: "ERROR"}},
	}
	if pushPredicateToSpansScanFilter(n, pushdownSpansTable, pushdownPred()) {
		t.Fatal("a scan of a non-spans table must not accept the spans predicate")
	}
	if n.Predicate.(*chplan.Binary).Op == chplan.OpAnd {
		t.Fatal("the predicate was folded into a foreign table's Filter")
	}
}
