package chsql

import "github.com/tsouza/cerberus/internal/chplan"

func metricsTimestampInternalTestScan(table, timestamp string) *chplan.Scan {
	return &chplan.Scan{Table: table, Roles: []chplan.Column{{Name: timestamp, Role: chplan.RoleTimestamp}}}
}
