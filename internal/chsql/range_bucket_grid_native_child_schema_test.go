package chsql

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The resolver's own contract (which shapes resolve, which fail and why) is
// pinned once in chplan (schema_role_resolution_test.go). What this file pins
// is the WIRING: that resolveRangeBucketGridNativeInputColumns routes each of
// its three mandatory identities through that resolver, so dropping any one
// of them is reported by the resolver itself rather than by whichever later
// emitter step happens to trip over an empty name.

const (
	gridNativeChildSeriesColumn    = "SeriesID"
	gridNativeChildTimestampColumn = "TimeUnix"
	gridNativeChildCountsColumn    = "BucketCounts"
	gridNativeChildBoundsColumn    = "ExplicitBounds"
)

// gridNativeChild returns a CLOSED child publishing exactly `columns`, in that
// order, each carrying the role its name is declared with below. A Scan with
// an explicit column list publishes a named list verbatim
// (chplan.Scan.RowType), including one that names the same column twice.
func gridNativeChild(columns ...string) chplan.Node {
	return &chplan.Scan{Table: "otel_metrics_histogram", Columns: columns, Roles: []chplan.Column{
		{Name: gridNativeChildSeriesColumn},
		{Name: gridNativeChildTimestampColumn, Role: chplan.RoleTimestamp},
		{Name: gridNativeChildCountsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: gridNativeChildBoundsColumn, Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}}
}

func TestResolveRangeBucketGridNativeChildBindsEveryRole(t *testing.T) {
	t.Parallel()

	// The series key repeated is legal SQL (`SELECT SeriesID, …, SeriesID`)
	// and says nothing about the three identities the operator reads.
	columns, err := resolveRangeBucketGridNativeInputColumns(gridNativeChild(
		gridNativeChildSeriesColumn, gridNativeChildTimestampColumn,
		gridNativeChildCountsColumn, gridNativeChildBoundsColumn,
		gridNativeChildSeriesColumn,
	))
	if err != nil {
		t.Fatalf("resolveRangeBucketGridNativeInputColumns: %v", err)
	}
	want := rangeBucketGridNativeInputColumns{
		timestamp:      gridNativeChildTimestampColumn,
		bucketCounts:   gridNativeChildCountsColumn,
		explicitBounds: gridNativeChildBoundsColumn,
	}
	if columns != want {
		t.Fatalf("resolved %+v, want %+v", columns, want)
	}
}

func TestResolveRangeBucketGridNativeChildRequiresEveryRoleThroughTheResolver(t *testing.T) {
	t.Parallel()

	for name, columns := range map[string][]string{
		"timestamp":       {gridNativeChildSeriesColumn, gridNativeChildCountsColumn, gridNativeChildBoundsColumn},
		"bucket-counts":   {gridNativeChildSeriesColumn, gridNativeChildTimestampColumn, gridNativeChildBoundsColumn},
		"explicit-bounds": {gridNativeChildSeriesColumn, gridNativeChildTimestampColumn, gridNativeChildCountsColumn},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveRangeBucketGridNativeInputColumns(gridNativeChild(columns...))
			if !errors.Is(err, ErrUnsupported) || !errors.Is(err, chplan.ErrRoleMissing) {
				t.Fatalf("a child publishing no %s must be rejected by the resolver as ErrUnsupported+ErrRoleMissing, got %v", name, err)
			}
		})
	}
}
