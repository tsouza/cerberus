package chsql_test

import "github.com/tsouza/cerberus/internal/chplan"

func closedRangeWindowTestScan(table, timestamp string, columns ...string) *chplan.Scan {
	columns = append(columns, timestamp)
	return &chplan.Scan{
		Table:   table,
		Columns: columns,
		Roles:   []chplan.Column{{Name: timestamp, Role: chplan.RoleTimestamp}},
	}
}
