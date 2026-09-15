package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func rangeBucketGridNativeRoleProject(columns ...chplan.Column) chplan.Node {
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{Expr: &chplan.LitFloat{V: 1}, Alias: column.Name}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Roles: columns, Projections: projections}
}

func rangeBucketGridNativeTestRoles() []chplan.Column {
	return []chplan.Column{
		{Name: "SeriesID"},
		{Name: "TimeUnix", Role: chplan.RoleTimestamp},
		{Name: "BucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: "ExplicitBounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}
}

func TestRangeBucketGridNativeResolvesPhysicalChildColumns(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeBucketGridNative{
		Input: rangeBucketGridNativeRoleProject(
			chplan.Column{Name: "series", Role: chplan.RoleAttributes},
			chplan.Column{Name: "physical_timestamp", Role: chplan.RoleTimestamp},
			chplan.Column{Name: "physical_counts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
			chplan.Column{Name: "physical_bounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
		),
		Start: at, End: at.Add(time.Minute), Step: time.Minute, Range: 5 * time.Minute,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "series"}}, GroupByAliases: []string{"series"},
		AnchorAlias: "anchor_ts", TimestampCol: "TimeUnix",
		BucketCountsCol: "BucketCounts", ExplicitBoundsCol: "ExplicitBounds",
	}
	row := plan.RowType()
	for field, want := range map[chplan.HistogramField]string{
		chplan.HistogramFieldBucketCounts: "BucketCounts", chplan.HistogramFieldExplicitBounds: "ExplicitBounds",
	} {
		column, ok := row.FindHistogramField(field)
		if !ok || column.Name != want {
			t.Fatalf("RowType histogram field %v = (%+v, %v), want %q", field, column, ok, want)
		}
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, physical := range []string{
		"arrayMap(hc -> toFloat64(hc), `physical_counts`)",
		"arrayPushBack(`physical_bounds`, inf)",
		"(`physical_timestamp`, `_hqn_cum`)",
	} {
		if !strings.Contains(sql, physical) {
			t.Errorf("SQL does not read resolved child column %s: %s", physical, sql)
		}
	}
	for _, public := range []string{"AS `BucketCounts`", "AS `ExplicitBounds`"} {
		if !strings.Contains(sql, public) {
			t.Errorf("SQL does not preserve public output alias %s: %s", public, sql)
		}
	}
}

func TestRangeBucketGridNativeRejectsMalformedChildSchema(t *testing.T) {
	t.Parallel()
	canonical := []chplan.Column{
		{Name: "timestamp", Role: chplan.RoleTimestamp},
		{Name: "counts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
		{Name: "bounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
	}
	tests := map[string]chplan.Node{
		"open":    &chplan.Scan{Roles: canonical},
		"missing": rangeBucketGridNativeRoleProject(canonical[0], canonical[1]),
		"duplicate": rangeBucketGridNativeRoleProject(canonical[0], canonical[1], canonical[2],
			chplan.Column{Name: "other_counts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts}),
		"unnamed": rangeBucketGridNativeRoleProject(canonical[0], canonical[1],
			chplan.Column{Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds}),
		"ambiguous": rangeBucketGridNativeRoleProject(canonical[0], canonical[1], canonical[2],
			chplan.Column{Name: "timestamp", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds}),
	}
	for name, child := range tests {
		child := child
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			plan := &chplan.RangeBucketGridNative{
				Input: child, Start: at, End: at.Add(time.Minute), Step: time.Minute, Range: time.Minute,
				AnchorAlias: "anchor_ts", TimestampCol: "TimeUnix",
				BucketCountsCol: "BucketCounts", ExplicitBoundsCol: "ExplicitBounds",
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err == nil {
				t.Fatal("RangeBucketGridNative accepted a malformed child schema")
			}
		})
	}
}
