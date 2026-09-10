//go:build chdb

package spec

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestRowTypeCrossJoinDriver(t *testing.T) {
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()
	db := OpenChDB(t)
	left := &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "shared"}}}
	right := &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 2}, Alias: "shared"}}}
	open := &chplan.Scan{Database: "system", Table: "one"}
	dummy := &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 2}, Alias: "dummy"}}}
	cases := map[string]*chplan.CrossJoin{
		"closed collision":       {Left: left, Right: right},
		"open hidden collision":  {Left: open, Right: dummy},
		"open without collision": {Left: open, Right: right},
		"closed left open right": {Left: left, Right: &chplan.Scan{Database: "system", Table: "one", Roles: []chplan.Column{{Name: "dummy"}}}},
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			query, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.Query(query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			AssertRowTypeMatchesDriver(t, plan, RoundTripResult{seeded: true, projectionColumns: columns})
		})
	}
}
