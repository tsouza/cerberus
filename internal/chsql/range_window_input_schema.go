package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

func rangeWindowInputTimestampColumn(r *chplan.RangeWindow) string {
	if r == nil || r.Input == nil {
		return ""
	}
	column, _ := r.Input.RowType().Find(rangeWindowInputTimestampRole(r))
	return column.Name
}

// rangeWindowInputTimestampRole identifies the child output that owns window
// membership. An ordinary range window consumes physical samples and therefore
// reads RoleTimestamp. A range window stacked over another matrix range window
// consumes the inner evaluation grid instead: the inner public timestamp alias
// is an output convenience, while RoleAnchor is the actual per-row grid key.
func rangeWindowInputTimestampRole(r *chplan.RangeWindow) chplan.ColumnRole {
	if _, nested := r.Input.(*chplan.RangeWindow); nested {
		return chplan.RoleAnchor
	}
	return chplan.RoleTimestamp
}

func validateRangeWindowInputTimestamp(r *chplan.RangeWindow) error {
	if r.Input == nil {
		return fmt.Errorf("%w: RangeWindow.Input is nil", ErrUnsupported)
	}
	row := r.Input.RowType()
	role := rangeWindowInputTimestampRole(r)
	var found chplan.Column
	count := 0
	for _, column := range row.Columns {
		if column.Role == role {
			found = column
			count++
		}
	}
	if row.Open || count != 1 || found.Name == "" {
		return fmt.Errorf("%w: RangeWindow requires one named temporal driver in a closed child schema", ErrUnsupported)
	}
	for _, column := range row.Columns {
		if column.Name == found.Name && column.Role != role {
			return fmt.Errorf("%w: RangeWindow child temporal driver is ambiguous", ErrUnsupported)
		}
	}
	return nil
}
