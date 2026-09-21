//go:build chdb

package promql

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestExpHistogramBucketSliceBounds_WideScaleGapChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	for _, gap := range []int64{0, 7, 8, 17} {
		t.Run(fmt.Sprintf("scale_gap=%d", gap), func(t *testing.T) {
			zero := &chplan.LitInt{V: 0}
			buckets := &chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.LitFloat{V: 1}, &chplan.LitFloat{V: 1}}}
			start, length := expHistogramBucketSliceBoundsExpr(&chplan.LitInt{V: gap}, zero, buckets, zero, zero, zero)
			query := chsql.NewQuery().SelectAs(func(b *chsql.Builder) { _ = b.Expr(start) }, "slice_start").
				SelectAs(func(b *chsql.Builder) { _ = b.Expr(length) }, "slice_length")
			sql, args := query.Build()
			var gotStart, gotLength int64
			if err := db.QueryRow(sql, args...).Scan(&gotStart, &gotLength); err != nil {
				t.Fatal(err)
			}
			wantLength := int64(len(buckets.Args))
			if gap == 0 {
				wantLength = 1
			}
			if gotStart != 1 || gotLength != wantLength {
				t.Fatalf("got (%d,%d), want (1,%d)", gotStart, gotLength, wantLength)
			}
		})
	}
}
