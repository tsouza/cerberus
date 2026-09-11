package prom

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestRangeNamePreservingPlansPublishMatrixAnchor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		query         string
		wantTimestamp chplan.Expr
	}{
		{
			name:          "first over offset selector",
			query:         "first_over_time(http_requests_total[5m] offset 1m)",
			wantTimestamp: chplan.OffsetReanchoredAnchorExpr(time.Minute),
		},
		{
			name:          "last over offset selector",
			query:         "last_over_time(http_requests_total[5m] offset 1m)",
			wantTimestamp: chplan.OffsetReanchoredAnchorExpr(time.Minute),
		},
		{
			name:          "first over subquery",
			query:         "first_over_time(http_requests_total[10m:1m])",
			wantTimestamp: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
		},
		{
			name:          "last over subquery",
			query:         "last_over_time(http_requests_total[10m:1m])",
			wantTimestamp: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := langForTest()
			l.Step = time.Minute
			plan, meta, err := l.Parse(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			anchor, ok := plan.RowType().Find(chplan.RoleAnchor)
			if !ok || anchor.Name == "" {
				t.Fatalf("Parse(%q) schema = %#v, want named anchor role", tc.query, plan.RowType())
			}

			wrapped, err := l.ProjectSamples(plan, meta)
			if err != nil {
				t.Fatalf("ProjectSamples(%q): %v", tc.query, err)
			}
			projected, ok := wrapped.(*chplan.Project)
			if !ok {
				t.Fatalf("ProjectSamples(%q) = %T, want *chplan.Project", tc.query, wrapped)
			}
			if got := projected.Projections[2].Expr; !got.Equal(tc.wantTimestamp) {
				t.Errorf("ProjectSamples(%q) timestamp = %#v, want %#v", tc.query, got, tc.wantTimestamp)
			}
		})
	}
}
