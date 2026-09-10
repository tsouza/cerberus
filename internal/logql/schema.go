package logql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func logSampleRoles() []chplan.Column {
	return []chplan.Column{
		{Name: sampleMetricNameCol, Role: chplan.RoleMetricName},
		{Name: sampleAttributesCol, Role: chplan.RoleAttributes},
		{Name: sampleTimeUnixCol, Role: chplan.RoleTimestamp},
		{Name: sampleValueCol, Role: chplan.RoleValue},
	}
}

func logRoles(s schema.Logs) []chplan.Column {
	return append(
		logSampleRoles(),
		chplan.Column{Name: s.ResourceAttributesColumn, Role: chplan.RoleAttributes},
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
	)
}
