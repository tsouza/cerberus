package promql

import "github.com/tsouza/cerberus/internal/chplan"

func rangeWindowTemporalityColumn(r *chplan.RangeWindow) string {
	if r.IgnoreInputTemporality {
		return ""
	}
	column, ok := r.Input.RowType().Find(chplan.RoleTemporality)
	if !ok {
		return ""
	}
	return column.Name
}
