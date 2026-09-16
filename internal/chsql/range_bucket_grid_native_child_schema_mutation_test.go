package chsql

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins resolveRangeBucketGridNativeInputColumns' child-schema
// contract (range_bucket_grid_native.go): which repeated name is benign, which
// repetition is a contradiction the emitter must refuse, and which roles are
// mandatory. It exists so the gremlins phase2-compare leg cannot flip one of
// those guards without a test going red.
//
// Why the guards survived mutation until now (cerberus issue #3467). Every
// other test of this resolver drives it through chsql.Emit and asserts only
// that a malformed child is rejected at all. Under each surviving mutant SOME
// guard still rejects those same inputs — just a different one, with a
// different message — so an `err != nil` assertion cannot tell the mutant from
// the original. The tests below assert on the resolver's own return, and name
// the guard that must produce it, which is exactly where the mutants differ.
//
// They call the resolver directly rather than through chsql.Emit for the same
// reason: a mutant that lets a malformed schema PAST this resolver may still
// trip some later emitter step, and an Emit-level `err != nil` would then read
// that unrelated rejection as a kill.

// The child column names these tests speak in. Only their roles matter to the
// resolver; the names are the canonical OTel-CH ones so a failure message
// reads like a real plan.
const (
	gridNativeChildSeriesColumn    = "SeriesID"
	gridNativeChildTimestampColumn = "TimeUnix"
	gridNativeChildCountsColumn    = "BucketCounts"
	gridNativeChildBoundsColumn    = "ExplicitBounds"
)

// gridNativeChildRoles declares the role of every name these tests use: an
// opaque series key, the per-sample timestamp, and the two classic-histogram
// arrays.
func gridNativeChildRoles() []chplan.Column {
	return []chplan.Column{
		{Name: gridNativeChildSeriesColumn},
		{Name: gridNativeChildTimestampColumn, Role: chplan.RoleTimestamp},
		{Name: gridNativeChildCountsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: gridNativeChildBoundsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}
}

// gridNativeChild returns a CLOSED child publishing exactly `columns`, in that
// order, each carrying the role gridNativeChildRoles gives its name. A Scan
// with an explicit column list is the node that publishes a named list
// verbatim (chplan.Scan.RowType), so it is how these tests state a schema —
// including one that names the same column twice, which is legal SQL and
// therefore a shape the resolver has to have an answer for.
func gridNativeChild(columns ...string) chplan.Node {
	return &chplan.Scan{Table: "otel_metrics_histogram", Columns: columns, Roles: gridNativeChildRoles()}
}

// Both contradictory children below need a node that APPENDS a fixed-role
// column to a child already publishing that name, because chplan.Node is
// sealed and neither Scan nor Project can express the shape: each resolves a
// repeated name to ONE role (chplan.Scan.RowType, chplan.roleColumn), so a
// contradiction cannot come from either. The two vehicles below are two such
// nodes, one per half of the guard — and the hazard they stand for is real
// rather than synthetic:
// RangeWindowGridNativeInstant.InputValueColumn already fails closed on the
// mirror image of it, a RoleValue name that some other output shares.

// gridNativeRoleContradictionChild publishes gridNativeChildSeriesColumn twice
// under contradicting ROLES and the same (absent) histogram field: once as the
// opaque series key it groups by, once as its own reduced value alias, which
// RangeWindowGridNativeInstant appends as `{ValueColumn, RoleValue}` after its
// group keys (chplan.windowSchema).
func gridNativeRoleContradictionChild() chplan.Node {
	return &chplan.RangeWindowGridNativeInstant{
		Input: gridNativeChild(
			gridNativeChildSeriesColumn, gridNativeChildTimestampColumn,
			gridNativeChildCountsColumn, gridNativeChildBoundsColumn,
		),
		Func:            "rate",
		Range:           time.Minute,
		Anchor:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TimestampColumn: gridNativeChildTimestampColumn,
		ValueColumn:     gridNativeChildSeriesColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: gridNativeChildSeriesColumn},
			&chplan.ColumnRef{Name: gridNativeChildTimestampColumn},
			&chplan.ColumnRef{Name: gridNativeChildCountsColumn},
			&chplan.ColumnRef{Name: gridNativeChildBoundsColumn},
		},
	}
}

// gridNativeFieldContradictionChild publishes chplan.HistogramCountColumn
// twice under the SAME role and contradicting HISTOGRAM FIELDS: the child
// carries that name as its classic bucket-count array, and HistogramProjection
// appends the exponential-histogram payload (chplan.histogramColumns), whose
// own column of that name is the observation count.
func gridNativeFieldContradictionChild() chplan.Node {
	return &chplan.HistogramProjection{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{gridNativeChildTimestampColumn, chplan.HistogramCountColumn, gridNativeChildBoundsColumn},
			Roles: []chplan.Column{
				{Name: gridNativeChildTimestampColumn, Role: chplan.RoleTimestamp},
				{Name: chplan.HistogramCountColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
				{Name: gridNativeChildBoundsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
			},
		},
		TimestampColumn: gridNativeChildTimestampColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: gridNativeChildTimestampColumn},
			&chplan.ColumnRef{Name: chplan.HistogramCountColumn},
			&chplan.ColumnRef{Name: gridNativeChildBoundsColumn},
		},
	}
}

// requireGridNativeChildColumns fails t unless the resolver accepted the child
// and bound each of the three mandatory roles to the expected name.
func requireGridNativeChildColumns(t *testing.T, input chplan.Node) {
	t.Helper()

	columns, err := resolveRangeBucketGridNativeInputColumns(input)
	if err != nil {
		t.Fatalf("resolveRangeBucketGridNativeInputColumns: %v", err)
	}
	for _, binding := range []struct {
		role string
		got  string
		want string
	}{
		{"timestamp", columns.timestamp, gridNativeChildTimestampColumn},
		{"bucket-count", columns.bucketCounts, gridNativeChildCountsColumn},
		{"explicit-bound", columns.explicitBounds, gridNativeChildBoundsColumn},
	} {
		if binding.got != binding.want {
			t.Errorf("%s role resolved to %q, want %q", binding.role, binding.got, binding.want)
		}
	}
}

// TestResolveRangeBucketGridNativeChildResolvesEveryRole is the positive
// control the two rejection tests below lean on: the canonical child resolves,
// so a rejection elsewhere in this file is caused by the one thing that test
// changed and not by the base schema.
func TestResolveRangeBucketGridNativeChildResolvesEveryRole(t *testing.T) {
	t.Parallel()

	requireGridNativeChildColumns(t, gridNativeChild(
		gridNativeChildSeriesColumn, gridNativeChildTimestampColumn,
		gridNativeChildCountsColumn, gridNativeChildBoundsColumn,
	))
}

// TestResolveRangeBucketGridNativeChildAcceptsRepeatedIdenticalColumn defends
//
//	range_bucket_grid_native.go:resolveRangeBucketGridNativeInputColumns:`seen.HistogramField != column.HistogramField`
//
// against CONDITIONALS_NEGATION (`!=` -> `==`).
//
// A name published twice under the SAME role and the SAME histogram field
// names one column twice — `SELECT SeriesID, …, SeriesID` — and says nothing
// contradictory about it, so the resolver admits it. The mutant reads the
// identical field as the contradiction and rejects exactly the schemas the
// guard is meant to let through, which would make a duplicated projection an
// unsupported query.
func TestResolveRangeBucketGridNativeChildAcceptsRepeatedIdenticalColumn(t *testing.T) {
	t.Parallel()

	requireGridNativeChildColumns(t, gridNativeChild(
		gridNativeChildSeriesColumn, gridNativeChildTimestampColumn,
		gridNativeChildCountsColumn, gridNativeChildBoundsColumn,
		gridNativeChildSeriesColumn,
	))
}

// TestResolveRangeBucketGridNativeChildRejectsContradictingRoles defends the
// whole ambiguity guard
//
//	range_bucket_grid_native.go:resolveRangeBucketGridNativeInputColumns:`if column.Name != ""`
//	range_bucket_grid_native.go:resolveRangeBucketGridNativeInputColumns:`seen.Role != column.Role`
//	range_bucket_grid_native.go:resolveRangeBucketGridNativeInputColumns:`seen.Role != column.Role || seen.HistogramField != column.HistogramField`
//
// against CONDITIONALS_NEGATION on either comparison and INVERT_LOGICAL on the
// `||` between them. Every one of those rewrites makes the guard miss a child
// that binds one name to two contradicting identities, and is caught here
// because the resolver then ACCEPTS the child instead of rejecting it.
//
// The two subtests are the disjunction's two halves, and a contradiction in
// only ONE of them is what tells `||` from `&&`: a child whose repeated name
// contradicts on BOTH halves at once is rejected under either operator.
//
// Accepting such a child is not a cosmetic difference. The emit reads the
// child's columns by name, so a name with two different columns answering to
// it leaves which one ClickHouse resolves the reference to outside anything
// the plan states — the ladder is then built over whichever column won, with
// no error anywhere.
func TestResolveRangeBucketGridNativeChildRejectsContradictingRoles(t *testing.T) {
	t.Parallel()

	for name, child := range map[string]chplan.Node{
		"a contradicting role under the same histogram field": gridNativeRoleContradictionChild(),
		"contradicting histogram fields under the same role":  gridNativeFieldContradictionChild(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveRangeBucketGridNativeInputColumns(child)
			if err == nil {
				t.Fatalf("resolveRangeBucketGridNativeInputColumns accepted a child whose repeated "+
					"name carries %s; the two readings of that name cannot both be the one the emit means", name)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("rejection must wrap ErrUnsupported, got %v", err)
			}
			if !strings.Contains(err.Error(), "ambiguous roles") {
				t.Errorf("the ambiguity guard must be the one that rejected this child, got %v", err)
			}
		})
	}
}

// TestResolveRangeBucketGridNativeChildRequiresEveryRole defends
//
//	range_bucket_grid_native.go:resolveRangeBucketGridNativeInputColumns:`columns.timestamp == "" || columns.bucketCounts == "" || columns.explicitBounds == ""`
//
// against INVERT_LOGICAL on either `||`. Either rewrite pairs one of the three
// comparisons with its neighbour, so a child missing exactly one role from that
// pair is accepted and the emit then reads a column named "" — a reference no
// ClickHouse table answers. Each role therefore gets its own case: only a
// subtest that drops ONE role distinguishes the conjunction from the
// disjunction, and which single role it drops decides which `||` it catches.
func TestResolveRangeBucketGridNativeChildRequiresEveryRole(t *testing.T) {
	t.Parallel()

	for name, columns := range map[string][]string{
		"timestamp": {
			gridNativeChildSeriesColumn, gridNativeChildCountsColumn, gridNativeChildBoundsColumn,
		},
		"bucket-count": {
			gridNativeChildSeriesColumn, gridNativeChildTimestampColumn, gridNativeChildBoundsColumn,
		},
		"explicit-bound": {
			gridNativeChildSeriesColumn, gridNativeChildTimestampColumn, gridNativeChildCountsColumn,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveRangeBucketGridNativeInputColumns(gridNativeChild(columns...))
			if err == nil {
				t.Fatalf("resolveRangeBucketGridNativeInputColumns accepted a child publishing no %s "+
					"role; the emit would then read a column named \"\"", name)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("rejection must wrap ErrUnsupported, got %v", err)
			}
			if !strings.Contains(err.Error(), "missing timestamp, bucket-count, or explicit-bound roles") {
				t.Errorf("the mandatory-role guard must be the one that rejected this child, got %v", err)
			}
		})
	}
}
