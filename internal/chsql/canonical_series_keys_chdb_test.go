//go:build chdb

// chDB-backed proof that the emit chokepoint's Map key-order repair is
// semantic and not cosmetic.
//
// A ClickHouse Map compares POSITIONALLY over its (keys, values) arrays, so
// `map('job','api','dc','eu')` and `map('dc','eu','job','api')` are two
// distinct GROUP BY keys even though they carry identical labels. The OTel-CH
// exporter preserves OTLP wire order and never sorts, so one logical series
// really can land under two physical key orders — and an aggregation keyed on
// the whole Map then reports two series where there is one. That is a silently
// wrong NUMBER, which is why the assertion here is on the row count and the
// summed value rather than on the presence of a token in the SQL.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package chsql_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// keyOrderSplitRows is the two-row inline dataset: the SAME logical label set
// delivered under the two physical key orders the exporter can produce.
const keyOrderSplitRows = `SELECT map('job', 'api', 'dc', 'eu') AS Attributes, 1.0 AS Value ` +
	`UNION ALL SELECT map('dc', 'eu', 'job', 'api') AS Attributes, 2.0 AS Value`

func TestEmitAggregate_RawMapKeyDoesNotSplitSeries(t *testing.T) {
	// A hand-built plan, i.e. one that never crossed a head's lowering — the
	// exact shape the emit-time repair exists to cover.
	plan := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "kv", Columns: []string{"Attributes", "Value"}},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{{
			Name:  "sum",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
			Alias: "total",
		}},
	}

	gotSQL, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("plan has no parameters, got args %v", args)
	}

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	defer db.Close()

	// Substitute the scan's table for the inline two-row dataset. Both orders
	// carry the same labels, so a correct emit sees ONE series summing to 3.
	query := replaceScanTable(t, gotSQL, "`kv`", "("+keyOrderSplitRows+")")

	// Count the GROUPS and the total the emitted aggregate produces. Both are
	// read back as strings so the assertion turns on ClickHouse's answer, not
	// on the chDB driver's column-type mapping.
	var groups, total string
	summary := "SELECT toString(count()), toString(sum(`total`)) FROM (" + query + ")"
	if err := db.QueryRow(summary).Scan(&groups, &total); err != nil {
		t.Fatalf("query %s: %v", summary, err)
	}
	if groups != "1" {
		t.Fatalf("got %s series, want 1: a whole-Map group key split one logical series across its physical key orders", groups)
	}
	if total != "3" {
		t.Fatalf("summed value = %s, want 3", total)
	}
}

// replaceScanTable swaps the single scan table reference in sql for src,
// failing the test when the reference is absent so a future emit change cannot
// silently turn this into a test of some other query.
func replaceScanTable(t *testing.T, sql, table, src string) string {
	t.Helper()
	idx := strings.Index(sql, table)
	if idx < 0 {
		t.Fatalf("emitted SQL does not reference %s: %s", table, sql)
	}
	return sql[:idx] + src + sql[idx+len(table):]
}
