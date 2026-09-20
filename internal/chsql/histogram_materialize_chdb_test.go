//go:build chdb

package chsql

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestMaterializeHistogramInput_PreservesRowsChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	row := chplan.Schema{Columns: []chplan.Column{{Name: "stamp"}, {Name: "labels"}, {Name: "buckets"}, {Name: "optional"}}}
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprintf("rows=%d", count), func(t *testing.T) {
			source := NewQuery().Select(
				As(Col("number"), "stamp"),
				As(Call("map", InlineLit("series"), Call("toString", Col("number"))), "labels"),
				As(If(Eq(Col("number"), InlineLit(0)), Array(), Array(InlineLit(1), InlineLit(2))), "buckets"),
				As(Call("nullIf", InlineLit(1), InlineLit(1)), "optional"),
			).From(Call("numbers", InlineLit(count)))
			query := NewQuery().Select(Call("toJSONString", Tuple(Col("stamp"), Col("labels"), Col("buckets"), Col("optional")))).
				From(materializeHistogramInput(Subquery(source), row)).OrderBy(Col("stamp"), false)
			sql, args := query.Build()
			rows, err := db.Query(sql, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			want := []string{`[0,{"series":"0"},[],null]`, `[1,{"series":"1"},[1,2],null]`}
			seen := 0
			for rows.Next() {
				var got string
				if err := rows.Scan(&got); err != nil {
					t.Fatal(err)
				}
				if seen >= count || got != want[seen] {
					t.Fatalf("row %d: unexpected %s", seen, got)
				}
				seen++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if seen != count {
				t.Fatalf("got %d rows, want %d", seen, count)
			}
		})
	}
}
